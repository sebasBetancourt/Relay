// Package queue publishes durable jobs to NATS JetStream.
// Workers depend on this small surface so the broker can change later
// (ADR 0003). Relay does not use the queue as a lock.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// StreamName is the single Relay stream.
	StreamName = "RELAY"
	// StreamSubjects captures sync jobs and subscription lifecycle jobs.
	StreamSubjects = "relay.>"
)

// Client is one connection and the RELAY stream.
type Client struct {
	nc     *nats.Conn
	js     jetstream.JetStream
	stream jetstream.Stream
	name   string
}

type option struct {
	memory bool
}

// Option configures Connect.
type Option func(*option)

// WithMemory keeps stream data in memory. Tests use it.
func WithMemory() Option {
	return func(o *option) { o.memory = true }
}

// Connect dials NATS and ensures the RELAY stream exists. A second publish
// of the same Nats-Msg-Id inside two hours is stored once and returns nil.
func Connect(ctx context.Context, url string, opts ...Option) (*Client, error) {
	var o option
	for _, opt := range opts {
		opt(&o)
	}
	nc, err := nats.Connect(url,
		nats.Name("relay"),
		nats.Timeout(2*time.Second),
		nats.MaxReconnects(5),
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
	if o.memory {
		storage = jetstream.MemoryStorage
	}
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       StreamName,
		Subjects:   []string{StreamSubjects},
		Storage:    storage,
		Duplicates: 2 * time.Hour,
		MaxAge:     7 * 24 * time.Hour,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats stream: %w", err)
	}
	return &Client{nc: nc, js: js, stream: stream, name: StreamName}, nil
}

// Publish sends body to subject. msgID is the JetStream Nats-Msg-Id.
func (c *Client) Publish(ctx context.Context, subject, msgID string, body []byte) error {
	msg := &nats.Msg{Subject: subject, Data: body}
	if msgID != "" {
		msg.Header = nats.Header{nats.MsgIdHdr: []string{msgID}}
	}
	if _, err := c.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

// Lifecycle is a subscription maintenance job. Sync jobs are a different
// subject and are left in the stream for the delta-sync worker.
type Lifecycle struct {
	AccountID string `json:"account_id"`
	Action    string `json:"action"`
}

// RunLifecycle consumes subject until ctx is cancelled. handle errors are
// nak'd so JetStream redelivers. Renew and resubscribe run here, not in
// the webhook request.
func (c *Client) RunLifecycle(ctx context.Context, subject string, handle func(context.Context, Lifecycle) error) error {
	cons, err := c.stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "subscription-lifecycle",
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    8,
	})
	if err != nil {
		return fmt.Errorf("lifecycle consumer: %w", err)
	}
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		var job Lifecycle
		if err := json.Unmarshal(msg.Data(), &job); err != nil || job.AccountID == "" {
			_ = msg.Ack()
			return
		}
		hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		err := handle(hctx, job)
		cancel()
		if err != nil && ctx.Err() == nil {
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return fmt.Errorf("lifecycle consume: %w", err)
	}
	defer cc.Stop()
	<-ctx.Done()
	return nil
}

// Pending is how many messages the stream currently holds.
func (c *Client) Pending(ctx context.Context) (uint64, error) {
	info, err := c.stream.Info(ctx)
	if err != nil {
		return 0, err
	}
	return info.State.Msgs, nil
}

// Collect reads up to max messages on subject, waiting up to wait for the
// first ones. Fewer than max messages returns when wait elapses.
func (c *Client) Collect(ctx context.Context, subject string, max int, wait time.Duration) ([][]byte, error) {
	if max <= 0 {
		max = 1
	}
	cons, err := c.stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Name:          "collect-" + subjectName(subject),
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

func subjectName(subject string) string {
	b := make([]byte, 0, len(subject))
	for i := 0; i < len(subject); i++ {
		c := subject[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b = append(b, c)
		} else {
			b = append(b, '-')
		}
	}
	return string(b)
}

// Close closes the connection.
func (c *Client) Close() {
	if c != nil && c.nc != nil {
		c.nc.Close()
	}
}
