// Package natsq is the NATS JetStream queue (ADR 0003).
//
// RELAY_JOBS is a work queue: a message is deleted when it is acked or
// terminated. RELAY_DLQ keeps failures for 14 days so they can be listed
// and replayed. JetStream has no native dead-letter queue; this package
// publishes the failed body there and then Term()s the original.
//
// Job subjects are relay.sync.>, relay.notify.> and relay.subscription.>.
// They are not the single wildcard relay.>, because relay.dlq.> has to
// belong to RELAY_DLQ and a subject can sit on only one stream.
package natsq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sebasBetancourt/relay/internal/queue"
)

const (
	// JobsStream holds work that is still to be done.
	JobsStream = "RELAY_JOBS"
	// DLQStream holds jobs that will not be retried again.
	DLQStream = "RELAY_DLQ"

	defaultAckWait      = 30 * time.Second
	defaultMaxDeliver   = 5
	duplicateWindow     = 2 * time.Minute
	dlqMaxAge           = 14 * 24 * time.Hour
	defaultHandlerLimit = 2 * time.Minute
	defaultPublishLimit = 2 * time.Second
	dlqSubjectPrefix    = "relay.dlq."
)

// defaultBackOff is applied with NakWithDelay. Consumer-level BackOff does
// not affect a nak (NATS only uses it when AckWait expires), and setting it
// also replaces AckWait with the first step. Both are still configured: a
// crashed worker is redelivered on that schedule, and a handler error naks
// with the same delays instead of immediately.
var defaultBackOff = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute}

// jobSubjects are the work prefixes. relay.dlq.> is reserved for DLQStream.
var jobSubjects = []string{
	"relay.sync.>",
	"relay.notify.>",
	"relay.subscription.>",
}

// Client is one connection, RELAY_JOBS and RELAY_DLQ.
type Client struct {
	nc     *nats.Conn
	js     jetstream.JetStream
	jobs   jetstream.Stream
	dlq    jetstream.Stream
	cfg    settings
	logger *slog.Logger
	mu     sync.Mutex

	// toDLQ, when set, replaces the network publish. Dispatch tests use it.
	toDLQ func(queue.DeadLetter) error
}

type settings struct {
	memory         bool
	ackWait        time.Duration
	maxDeliver     int
	backOff        []time.Duration
	duplicates     time.Duration
	maxMsgs        int64
	handlerTimeout time.Duration
	publishTimeout time.Duration
	logger         *slog.Logger
}

// Option configures Connect.
type Option func(*settings)

// WithMemory keeps stream data in memory. Tests use it. Production uses disk.
func WithMemory() Option {
	return func(s *settings) { s.memory = true }
}

// WithAckWait sets how long a crashed worker may hold a message.
// When BackOff is also set, the server uses the first backoff step as the
// ack deadline. InProgress heartbeats use the shorter of the two so a live
// handler is not redelivered underneath itself.
func WithAckWait(d time.Duration) Option {
	return func(s *settings) { s.ackWait = d }
}

// WithMaxDeliver caps attempts. The last failed attempt is dead-lettered.
func WithMaxDeliver(n int) Option {
	return func(s *settings) { s.maxDeliver = n }
}

// WithBackOff replaces the retry schedule. The length should be at most
// MaxDeliver; extra steps are dropped when the consumer is created.
func WithBackOff(steps ...time.Duration) Option {
	return func(s *settings) {
		s.backOff = append([]time.Duration(nil), steps...)
	}
}

// WithMaxMsgs caps RELAY_JOBS. Further publishes fail instead of evicting
// an older job. Tests use it for the full-stream case.
func WithMaxMsgs(n int64) Option {
	return func(s *settings) { s.maxMsgs = n }
}

// WithLogger receives dead-letter warnings. A nil logger stays quiet.
func WithLogger(log *slog.Logger) Option {
	return func(s *settings) { s.logger = log }
}

func (s *settings) applyDefaults() {
	if s.ackWait <= 0 {
		s.ackWait = defaultAckWait
	}
	if s.maxDeliver <= 0 {
		s.maxDeliver = defaultMaxDeliver
	}
	if s.backOff == nil {
		s.backOff = append([]time.Duration(nil), defaultBackOff...)
	}
	if s.duplicates <= 0 {
		s.duplicates = duplicateWindow
	}
	if s.handlerTimeout <= 0 {
		s.handlerTimeout = defaultHandlerLimit
	}
	if s.publishTimeout <= 0 {
		s.publishTimeout = defaultPublishLimit
	}
}

// Connect dials NATS and ensures both streams exist. url empty is an error.
// A dial that cannot finish returns within a few seconds; it does not block
// the process on reconnect.
func Connect(ctx context.Context, url string, opts ...Option) (*Client, error) {
	if url == "" {
		return nil, errors.New("nats url is empty")
	}
	var cfg settings
	for _, opt := range opts {
		opt(&cfg)
	}
	cfg.applyDefaults()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}

	nc, err := nats.Connect(url,
		nats.Name("relay"),
		nats.Timeout(2*time.Second),
		// A running process should survive a broker blip. Publish still
		// returns when its own context expires, so a request is not stuck.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats jetstream: %w", err)
	}
	storage := jetstream.FileStorage
	if cfg.memory {
		storage = jetstream.MemoryStorage
	}
	jobsCfg := jetstream.StreamConfig{
		Name:       JobsStream,
		Subjects:   jobSubjects,
		Retention:  jetstream.WorkQueuePolicy,
		Storage:    storage,
		Duplicates: cfg.duplicates,
	}
	if cfg.maxMsgs > 0 {
		jobsCfg.MaxMsgs = cfg.maxMsgs
		// The default discard policy drops the oldest message and accepts
		// the new one. A full queue must surface an error instead.
		jobsCfg.Discard = jetstream.DiscardNew
	}
	jobs, err := js.CreateOrUpdateStream(ctx, jobsCfg)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats stream %s: %w", JobsStream, err)
	}
	dlq, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       DLQStream,
		Subjects:   []string{dlqSubjectPrefix + ">"},
		Retention:  jetstream.LimitsPolicy,
		Storage:    storage,
		MaxAge:     dlqMaxAge,
		Duplicates: cfg.duplicates,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats stream %s: %w", DLQStream, err)
	}
	return &Client{nc: nc, js: js, jobs: jobs, dlq: dlq, cfg: cfg, logger: cfg.logger}, nil
}

var (
	_ queue.Publisher = (*Client)(nil)
	_ queue.Consumer  = (*Client)(nil)
)

// Publish sends payload to subject. A duplicate msgID inside the dedupe
// window returns nil and does not store a second copy. The call fails
// within publishTimeout (or sooner, if ctx expires first) when NATS is down.
func (c *Client) Publish(ctx context.Context, subject, msgID string, payload []byte) error {
	if subject == "" {
		return errors.New("empty subject")
	}
	pctx, cancel := context.WithTimeout(ctx, c.cfg.publishTimeout)
	defer cancel()
	msg := &nats.Msg{Subject: subject, Data: payload}
	if msgID != "" {
		msg.Header = nats.Header{nats.MsgIdHdr: []string{msgID}}
	}
	if _, err := c.js.PublishMsg(pctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

// Consume handles subject until ctx is cancelled, then drains the callback
// already running. A handler that is still working is allowed to finish and
// ack; the message is not abandoned on the floor.
func (c *Client) Consume(ctx context.Context, subject string, h queue.Handler) error {
	if subject == "" {
		return errors.New("empty subject")
	}
	if h == nil {
		return errors.New("nil handler")
	}
	cons, err := c.consumer(ctx, subject)
	if err != nil {
		return err
	}
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		c.deliver(ctx, msg, h)
	})
	if err != nil {
		return fmt.Errorf("consume %s: %w", subject, err)
	}
	select {
	case <-ctx.Done():
		cc.Drain()
		<-cc.Closed()
		return nil
	case <-cc.Closed():
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("consume %s: consumer stopped", subject)
	}
}

func (c *Client) consumer(ctx context.Context, subject string) (jetstream.Consumer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cons, err := c.jobs.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       durableName(subject),
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       c.cfg.ackWait,
		MaxDeliver:    c.cfg.maxDeliver,
		BackOff:       c.backoff(),
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("consumer %s: %w", subject, err)
	}
	return cons, nil
}

// deliver acks, naks with the backoff step, or dead-letters.
// A panic is permanent: a poison payload must not crash the process and
// then be redelivered forever.
func (c *Client) deliver(parent context.Context, msg jetstream.Msg, h queue.Handler) {
	meta, err := msg.Metadata()
	if err != nil || meta == nil {
		_ = msg.NakWithDelay(c.delay(1))
		return
	}
	stop := make(chan struct{})
	var beats sync.WaitGroup
	beats.Add(1)
	go func() {
		defer beats.Done()
		c.heartbeat(msg, stop)
	}()
	defer beats.Wait()
	defer close(stop)

	hctx, cancel := context.WithTimeout(context.WithoutCancel(parent), c.cfg.handlerTimeout)
	defer cancel()

	var herr error
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				herr = queue.Permanent(fmt.Errorf("handler panic: %v", rec))
			}
		}()
		herr = h(hctx, queue.Message{
			Subject: msg.Subject(),
			MsgID:   headerID(msg),
			Payload: append([]byte(nil), msg.Data()...),
		})
	}()

	delivered := meta.NumDelivered
	switch queue.OutcomeFor(herr, delivered, uint64(c.cfg.maxDeliver)) {
	case queue.OutcomeAck:
		_ = msg.Ack()
	case queue.OutcomeRetry:
		_ = msg.NakWithDelay(c.delay(delivered))
	default:
		if werr := c.deadLetter(hctx, msg, herr, delivered); werr != nil {
			c.log(slog.LevelError, "dlq publish failed", "err", werr, "subject", msg.Subject())
			// Leave it un-terminated so it is not deleted. Nak may not
			// redeliver once MaxDeliver is exhausted; the body is still
			// in the stream until an operator can see the error.
			_ = msg.NakWithDelay(c.delay(delivered))
			return
		}
		c.log(slog.LevelWarn, "job moved to dlq", "subject", msg.Subject(), "err", herr, "delivered", delivered)
		_ = msg.Term()
	}
}

func (c *Client) heartbeat(msg jetstream.Msg, stop <-chan struct{}) {
	every := c.progressEvery()
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			// Resets the ack timer while the handler is still working.
			// A dead process stops calling this, and the message is
			// redelivered after the ack deadline.
			_ = msg.InProgress()
		}
	}
}

func (c *Client) progressEvery() time.Duration {
	wait := c.cfg.ackWait
	if len(c.cfg.backOff) > 0 && (wait <= 0 || c.cfg.backOff[0] < wait) {
		wait = c.cfg.backOff[0]
	}
	if wait <= 0 {
		return 0
	}
	return wait / 2
}

func (c *Client) delay(delivered uint64) time.Duration {
	bo := c.backoff()
	if len(bo) == 0 {
		if c.cfg.ackWait > 0 {
			return c.cfg.ackWait
		}
		return time.Second
	}
	i := int(delivered) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(bo) {
		i = len(bo) - 1
	}
	return bo[i]
}

func (c *Client) backoff() []time.Duration {
	n := len(c.cfg.backOff)
	if c.cfg.maxDeliver > 0 && n > c.cfg.maxDeliver {
		n = c.cfg.maxDeliver
	}
	if n <= 0 {
		return nil
	}
	out := make([]time.Duration, n)
	copy(out, c.cfg.backOff)
	return out
}

func (c *Client) deadLetter(ctx context.Context, msg jetstream.Msg, herr error, delivered uint64) error {
	meta, err := msg.Metadata()
	seq := uint64(0)
	if err == nil && meta != nil {
		seq = meta.Sequence.Stream
	}
	reason := "unknown"
	if herr != nil {
		reason = herr.Error()
	}
	letter := queue.DeadLetter{
		Subject:   msg.Subject(),
		MsgID:     headerID(msg),
		Payload:   append([]byte(nil), msg.Data()...),
		Error:     reason,
		Delivered: delivered,
		FailedAt:  time.Now().UTC(),
	}
	if c.toDLQ != nil {
		return c.toDLQ(letter)
	}
	body, err := json.Marshal(letter)
	if err != nil {
		return err
	}
	// Stable id: a retried dead-letter publish inside the dedupe window
	// does not store a second copy of the same failure.
	id := fmt.Sprintf("dlq:%d:%s", seq, letter.MsgID)
	return c.Publish(ctx, dlqSubjectPrefix+msg.Subject(), id, body)
}

// ListDLQ returns every dead letter currently stored, oldest sequence first.
// Gaps left by a replay are skipped.
func (c *Client) ListDLQ(ctx context.Context) ([]queue.DeadLetter, error) {
	info, err := c.dlq.Info(ctx)
	if err != nil {
		return nil, err
	}
	if info.State.Msgs == 0 {
		return nil, nil
	}
	var out []queue.DeadLetter
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
		raw, err := c.dlq.GetMsg(ctx, seq)
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("dlq seq %d: %w", seq, err)
		}
		letter, err := decodeLetter(raw.Data)
		if err != nil {
			letter = queue.DeadLetter{
				Subject: raw.Subject,
				Payload: append([]byte(nil), raw.Data...),
				Error:   "invalid dead letter: " + err.Error(),
			}
		}
		letter.Seq = raw.Sequence
		out = append(out, letter)
	}
	return out, nil
}

// ReplayDLQ publishes the original payload back onto its subject and
// removes that sequence from RELAY_DLQ. The new Nats-Msg-Id is distinct
// so the dedupe window does not swallow the replay.
func (c *Client) ReplayDLQ(ctx context.Context, seq uint64) error {
	raw, err := c.dlq.GetMsg(ctx, seq)
	if err != nil {
		return fmt.Errorf("dlq seq %d: %w", seq, err)
	}
	letter, err := decodeLetter(raw.Data)
	if err != nil {
		return fmt.Errorf("dlq seq %d: invalid message: %w", seq, err)
	}
	if letter.Subject == "" {
		return fmt.Errorf("dlq seq %d: missing subject", seq)
	}
	id := letter.MsgID
	if id == "" {
		id = fmt.Sprintf("seq-%d", seq)
	}
	if err := c.Publish(ctx, letter.Subject, fmt.Sprintf("replay:%d:%s", seq, id), letter.Payload); err != nil {
		return err
	}
	if err := c.dlq.DeleteMsg(ctx, seq); err != nil {
		return fmt.Errorf("replayed onto %s but not removed from the dlq: %w", letter.Subject, err)
	}
	return nil
}

func decodeLetter(data []byte) (queue.DeadLetter, error) {
	var letter queue.DeadLetter
	if err := json.Unmarshal(data, &letter); err != nil {
		return queue.DeadLetter{}, err
	}
	return letter, nil
}

// Pending is how many messages RELAY_JOBS currently holds.
// Acked work-queue messages are already gone.
func (c *Client) Pending(ctx context.Context) (uint64, error) {
	info, err := c.jobs.Info(ctx)
	if err != nil {
		return 0, err
	}
	return info.State.Msgs, nil
}

// Collect reads up to max messages on subject and acks them.
// It has to be the only consumer on that subject: a work queue rejects
// a second consumer whose filter overlaps the worker's.
func (c *Client) Collect(ctx context.Context, subject string, max int, wait time.Duration) ([][]byte, error) {
	if max <= 0 {
		max = 1
	}
	cons, err := c.jobs.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Name:          "collect-" + durableName(subject),
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return nil, err
	}
	batch, err := cons.Fetch(max, jetstream.FetchMaxWait(wait))
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for msg := range batch.Messages() {
		out = append(out, append([]byte(nil), msg.Data()...))
		_ = msg.Ack()
	}
	if err := batch.Error(); err != nil && !errors.Is(err, jetstream.ErrNoMessages) {
		return out, err
	}
	return out, nil
}

// Drain flushes the connection and closes it. Call it only after Consume
// has returned, so an in-flight handler can still ack.
func (c *Client) Drain() error {
	if c == nil || c.nc == nil || c.nc.IsClosed() {
		return nil
	}
	return c.nc.Drain()
}

// Close drops the connection without waiting for in-flight handlers.
// A message that was not acked is redelivered after the ack deadline.
func (c *Client) Close() {
	if c != nil && c.nc != nil {
		c.nc.Close()
	}
}

func (c *Client) log(level slog.Level, msg string, args ...any) {
	if c.logger == nil {
		return
	}
	c.logger.Log(context.Background(), level, msg, args...)
}

func headerID(msg jetstream.Msg) string {
	h := msg.Headers()
	if h == nil {
		return ""
	}
	return h.Get(nats.MsgIdHdr)
}

func durableName(subject string) string {
	var b strings.Builder
	b.WriteString("jobs-")
	for i := 0; i < len(subject); i++ {
		c := subject[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
