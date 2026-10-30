package natsq

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sebasBetancourt/relay/internal/queue"
	"github.com/sebasBetancourt/relay/internal/testutil/natsjs"
)

func testClient(t *testing.T, opts ...Option) *Client {
	t.Helper()
	all := append([]Option{WithMemory()}, opts...)
	c, err := Connect(context.Background(), natsjs.Start(t), all...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestStreamsUseWorkQueueAndTwoMinuteDedupe(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	info, err := c.jobs.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.Retention != jetstream.WorkQueuePolicy {
		t.Fatalf("retention = %s", info.Config.Retention)
	}
	if info.Config.Storage != jetstream.MemoryStorage {
		t.Fatalf("storage = %s", info.Config.Storage)
	}
	if info.Config.Duplicates != 2*time.Minute {
		t.Fatalf("duplicates = %s", info.Config.Duplicates)
	}
	dlq, err := c.dlq.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dlq.Config.Retention != jetstream.LimitsPolicy || dlq.Config.MaxAge != 14*24*time.Hour {
		t.Fatalf("dlq retention %s max age %s", dlq.Config.Retention, dlq.Config.MaxAge)
	}
}

func TestFileStorageOnDisk(t *testing.T) {
	c, err := Connect(context.Background(), natsjs.Start(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	info, err := c.jobs.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.Storage != jetstream.FileStorage {
		t.Fatalf("storage = %s, want file", info.Config.Storage)
	}
}

func TestPublishOnceThenGone(t *testing.T) {
	c := testClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const subject = "relay.sync.account"
	body := []byte(`{"account_id":"acct-1"}`)
	if err := c.Publish(ctx, subject, "job-1", body); err != nil {
		t.Fatal(err)
	}
	n, err := c.Pending(ctx)
	if err != nil || n != 1 {
		t.Fatalf("pending = %d, %v", n, err)
	}
	got := make(chan []byte, 1)
	done := consume(ctx, c, subject, func(_ context.Context, msg queue.Message) error {
		got <- append([]byte(nil), msg.Payload...)
		return nil
	})
	select {
	case payload := <-got:
		if string(payload) != string(body) {
			t.Fatalf("payload %s", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("not delivered")
	}
	waitPending(t, c, 0)
	cancel()
	waitDone(t, done)
}

func TestPublishDedupesOnMsgID(t *testing.T) {
	c := testClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const subject = "relay.sync.account"
	body := []byte(`{"account_id":"acct-1","provider":"google"}`)
	var calls atomic.Int32
	done := consume(ctx, c, subject, func(context.Context, queue.Message) error {
		calls.Add(1)
		return nil
	})
	if err := c.Publish(ctx, subject, "google:ch-1:4", body); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(ctx, subject, "google:ch-1:4", body); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("not delivered")
		}
		time.Sleep(15 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("processed %d times, want 1", calls.Load())
	}
	cancel()
	waitDone(t, done)
	waitPending(t, c, 0)
}

func TestCollectWithoutAWorker(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	const subject = "relay.sync.account"
	body := []byte(`{"account_id":"acct-1"}`)
	if err := c.Publish(ctx, subject, "google:ch-1:4", body); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(ctx, subject, "google:ch-1:4", body); err != nil {
		t.Fatal(err)
	}
	n, err := c.Pending(ctx)
	if err != nil || n != 1 {
		t.Fatalf("pending = %d, %v", n, err)
	}
	got, err := c.Collect(ctx, subject, 5, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0]) != string(body) {
		t.Fatalf("collected %#v", got)
	}
	waitPending(t, c, 0)
}

func TestRetryBackoffThenDLQ(t *testing.T) {
	c := testClient(t,
		WithAckWait(40*time.Millisecond),
		WithBackOff(40*time.Millisecond, 40*time.Millisecond),
		WithMaxDeliver(3),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const subject = "relay.sync.account"
	body := []byte(`{"account_id":"acct-9"}`)
	var (
		mu    sync.Mutex
		calls []time.Time
	)
	done := consume(ctx, c, subject, func(context.Context, queue.Message) error {
		mu.Lock()
		calls = append(calls, time.Now())
		mu.Unlock()
		return errors.New("provider 503")
	})
	if err := c.Publish(ctx, subject, "job-9", body); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		letters, err := c.ListDLQ(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(letters) == 1 {
			if letters[0].Error != "provider 503" || string(letters[0].Payload) != string(body) {
				t.Fatalf("letter %+v", letters[0])
			}
			if letters[0].Subject != subject || letters[0].Delivered != 3 {
				t.Fatalf("letter %+v", letters[0])
			}
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			n := len(calls)
			mu.Unlock()
			t.Fatalf("dlq empty after 3s, attempts %d", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	got := append([]time.Time(nil), calls...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("attempts = %d, want 3", len(got))
	}
	if gap := got[1].Sub(got[0]); gap < 30*time.Millisecond {
		t.Fatalf("first retry gap = %s, want at least 30ms", gap)
	}
	waitPending(t, c, 0)
	cancel()
	waitDone(t, done)
}

func TestPermanentGoesStraightToDLQ(t *testing.T) {
	c := testClient(t, WithAckWait(200*time.Millisecond), WithBackOff(200*time.Millisecond), WithMaxDeliver(5))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	done := consume(ctx, c, "relay.sync.account", func(context.Context, queue.Message) error {
		calls.Add(1)
		return queue.Permanent(errors.New("invalid json"))
	})
	if err := c.Publish(ctx, "relay.sync.account", "bad-1", []byte(`{`)); err != nil {
		t.Fatal(err)
	}
	waitDLQ(t, c, 1)
	time.Sleep(350 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("attempts = %d, permanent errors must not retry", calls.Load())
	}
	letters, err := c.ListDLQ(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if letters[0].Error != "invalid json" || string(letters[0].Payload) != "{" {
		t.Fatalf("letter %+v", letters[0])
	}
	cancel()
	waitDone(t, done)
}

func TestRedeliveryAfterAckDeadline(t *testing.T) {
	c := testClient(t,
		WithAckWait(300*time.Millisecond),
		WithBackOff(300*time.Millisecond, time.Second),
		WithMaxDeliver(5),
	)
	ctx := context.Background()
	const subject = "relay.sync.account"
	body := []byte(`{"account_id":"acct-1"}`)
	if err := c.Publish(ctx, subject, "job-1", body); err != nil {
		t.Fatal(err)
	}
	cons, err := c.consumer(ctx, subject)
	if err != nil {
		t.Fatal(err)
	}
	first, err := one(cons.Fetch(1, jetstream.FetchMaxWait(2*time.Second)))
	if err != nil || first == nil {
		t.Fatalf("first fetch: %v", err)
	}
	// No ack and no InProgress: this is a worker that died mid-message.
	time.Sleep(700 * time.Millisecond)
	second, err := one(cons.Fetch(1, jetstream.FetchMaxWait(2*time.Second)))
	if err != nil || second == nil {
		t.Fatalf("not redelivered: %v", err)
	}
	meta, err := second.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	if meta.NumDelivered < 2 {
		t.Fatalf("num delivered = %d, want at least 2", meta.NumDelivered)
	}
	if string(second.Data()) != string(body) {
		t.Fatalf("body %s", second.Data())
	}
	if err := second.Ack(); err != nil {
		t.Fatal(err)
	}
	waitPending(t, c, 0)
}

func TestDrainWaitsForInFlightHandler(t *testing.T) {
	c := testClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	const subject = "relay.sync.account"
	started := make(chan struct{})
	release := make(chan struct{})
	done := consume(ctx, c, subject, func(context.Context, queue.Message) error {
		close(started)
		<-release
		return nil
	})
	if err := c.Publish(context.Background(), subject, "job-1", []byte(`{"account_id":"a"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("consume returned while the handler was still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	waitDone(t, done)
	waitPending(t, c, 0)
	letters, err := c.ListDLQ(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(letters) != 0 {
		t.Fatalf("dlq = %+v", letters)
	}
}

func TestTwoConsumersShareADurable(t *testing.T) {
	url := natsjs.Start(t)
	opts := []Option{
		WithMemory(),
		WithAckWait(time.Second),
		WithBackOff(time.Second),
		WithMaxDeliver(5),
	}
	c1, err := Connect(context.Background(), url, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c1.Close)
	c2, err := Connect(context.Background(), url, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c2.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const subject = "relay.sync.account"
	var first, second atomic.Int32
	seen := make(chan string, 32)
	done1 := consume(ctx, c1, subject, func(_ context.Context, msg queue.Message) error {
		first.Add(1)
		seen <- string(msg.Payload)
		return nil
	})
	done2 := consume(ctx, c2, subject, func(_ context.Context, msg queue.Message) error {
		second.Add(1)
		seen <- string(msg.Payload)
		return nil
	})
	waitPullers(t, c1, subject, 2)

	const n = 20
	for i := 0; i < n; i++ {
		id := "job-" + string(rune('a'+i))
		body := []byte(id)
		if err := c1.Publish(ctx, subject, id, body); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]int{}
	deadline := time.After(5 * time.Second)
	for len(got) < n {
		select {
		case id := <-seen:
			got[id]++
			if got[id] > 1 {
				t.Fatalf("%s processed twice", id)
			}
		case <-deadline:
			t.Fatalf("processed %d of %d", len(got), n)
		}
	}
	if first.Load() == 0 || second.Load() == 0 {
		t.Fatalf("work was not shared: c1=%d c2=%d", first.Load(), second.Load())
	}
	cancel()
	waitDone(t, done1)
	waitDone(t, done2)
}

func TestReplayPutsTheJobBack(t *testing.T) {
	c := testClient(t, WithMaxDeliver(2), WithAckWait(30*time.Millisecond), WithBackOff(30*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	const subject = "relay.sync.account"
	body := []byte(`{"account_id":"acct-1"}`)
	done := consume(ctx, c, subject, func(context.Context, queue.Message) error {
		return errors.New("nope")
	})
	if err := c.Publish(context.Background(), subject, "job-1", body); err != nil {
		t.Fatal(err)
	}
	waitDLQ(t, c, 1)
	cancel()
	waitDone(t, done)

	letters, err := c.ListDLQ(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReplayDLQ(context.Background(), letters[0].Seq); err != nil {
		t.Fatal(err)
	}
	if again, err := c.ListDLQ(context.Background()); err != nil || len(again) != 0 {
		t.Fatalf("dlq after replay: %v %+v", err, again)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	got := make(chan []byte, 1)
	done2 := consume(ctx2, c, subject, func(_ context.Context, msg queue.Message) error {
		got <- append([]byte(nil), msg.Payload...)
		return nil
	})
	select {
	case payload := <-got:
		if string(payload) != string(body) {
			t.Fatalf("replayed %s", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replayed job was not delivered")
	}
	cancel2()
	waitDone(t, done2)
}

func TestPublishFailsFastWhenNATSIsDown(t *testing.T) {
	s := natsjs.StartServer(t)
	c, err := Connect(context.Background(), s.ClientURL(), WithMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	s.Shutdown()
	s.WaitForShutdown()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	err = c.Publish(ctx, "relay.sync.account", "job-1", []byte(`{}`))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected publish error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("publish blocked for %s: %v", elapsed, err)
	}
}

func TestPublishFailsWhenStreamIsFull(t *testing.T) {
	c := testClient(t, WithMaxMsgs(1))
	ctx := context.Background()
	if err := c.Publish(ctx, "relay.sync.account", "one", []byte(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	err := c.Publish(ctx, "relay.sync.account", "two", []byte(`{"n":2}`))
	if err == nil {
		t.Fatal("expected a full-stream error")
	}
	if n, nerr := c.Pending(ctx); nerr != nil || n != 1 {
		t.Fatalf("pending = %d, %v (full stream must keep the first job)", n, nerr)
	}
}

func consume(ctx context.Context, c *Client, subject string, h queue.Handler) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Consume(ctx, subject, h)
	}()
	return done
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("consume did not return")
	}
}

func waitPending(t *testing.T, c *Client, want uint64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last uint64
	var err error
	for time.Now().Before(deadline) {
		last, err = c.Pending(context.Background())
		if err == nil && last == want {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("pending = %d, %v, want %d", last, err, want)
}

func waitDLQ(t *testing.T, c *Client, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		letters, err := c.ListDLQ(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(letters) == n {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("dlq length != %d", n)
}

func waitPullers(t *testing.T, c *Client, subject string, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cons, err := c.jobs.Consumer(context.Background(), durableName(subject))
		if err == nil {
			info, err := cons.Info(context.Background())
			if err == nil && info.NumWaiting >= n {
				return
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("wanted %d pullers on %s", n, subject)
}

func one(batch jetstream.MessageBatch, err error) (jetstream.Msg, error) {
	if err != nil {
		return nil, err
	}
	var got jetstream.Msg
	for msg := range batch.Messages() {
		got = msg
		break
	}
	if err := batch.Error(); err != nil && !errors.Is(err, jetstream.ErrNoMessages) {
		return got, err
	}
	return got, nil
}
