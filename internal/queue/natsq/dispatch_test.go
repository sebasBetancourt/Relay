package natsq

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sebasBetancourt/relay/internal/queue"
)

// fakeMsg is an in-memory JetStream delivery. It lets dispatch tests
// classify errors without a broker.
type fakeMsg struct {
	subject  string
	data     []byte
	header   nats.Header
	meta     *jetstream.MsgMetadata
	ack      int
	nak      int
	nakDelay time.Duration
	term     int
}

func (f *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) { return f.meta, nil }
func (f *fakeMsg) Data() []byte                              { return f.data }
func (f *fakeMsg) Headers() nats.Header                      { return f.header }
func (f *fakeMsg) Subject() string                           { return f.subject }
func (f *fakeMsg) Reply() string                             { return "" }
func (f *fakeMsg) Ack() error                                { f.ack++; return nil }
func (f *fakeMsg) DoubleAck(context.Context) error           { return f.Ack() }
func (f *fakeMsg) Nak() error                                { f.nak++; return nil }
func (f *fakeMsg) NakWithDelay(d time.Duration) error {
	f.nak++
	f.nakDelay = d
	return nil
}
func (f *fakeMsg) InProgress() error           { return nil }
func (f *fakeMsg) Term() error                 { f.term++; return nil }
func (f *fakeMsg) TermWithReason(string) error { return f.Term() }

func testDispatchClient(dlq func(queue.DeadLetter) error) *Client {
	return &Client{
		cfg: settings{
			ackWait:        time.Second,
			maxDeliver:     5,
			backOff:        []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute},
			handlerTimeout: time.Second,
		},
		toDLQ: dlq,
	}
}

func TestDeliverClassifiesAndKeepsPayload(t *testing.T) {
	body := []byte(`{"account_id":"acct-1"}`)
	msg := &fakeMsg{
		subject: "relay.sync.account",
		data:    body,
		header:  nats.Header{nats.MsgIdHdr: []string{"google:ch-1:4"}},
		meta:    &jetstream.MsgMetadata{NumDelivered: 1},
	}
	c := testDispatchClient(func(queue.DeadLetter) error { return nil })
	var got queue.Message
	c.deliver(context.Background(), msg, func(_ context.Context, m queue.Message) error {
		got = m
		return nil
	})
	if msg.ack != 1 || msg.nak != 0 || msg.term != 0 {
		t.Fatalf("ack %d nak %d term %d", msg.ack, msg.nak, msg.term)
	}
	if got.Subject != msg.subject || got.MsgID != "google:ch-1:4" || string(got.Payload) != string(body) {
		t.Fatalf("message %+v", got)
	}
}

func TestDeliverNaksTransientWithBackoff(t *testing.T) {
	steps := []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute}
	for i, want := range steps {
		msg := &fakeMsg{
			subject: "relay.sync.account",
			data:    []byte(`{"account_id":"a"}`),
			meta:    &jetstream.MsgMetadata{NumDelivered: uint64(i + 1)},
		}
		c := testDispatchClient(nil)
		c.deliver(context.Background(), msg, func(context.Context, queue.Message) error {
			return errors.New("provider timeout")
		})
		if msg.nak != 1 || msg.term != 0 || msg.ack != 0 {
			t.Fatalf("delivery %d: ack %d nak %d term %d", i+1, msg.ack, msg.nak, msg.term)
		}
		if msg.nakDelay != want {
			t.Fatalf("delivery %d delay = %s, want %s", i+1, msg.nakDelay, want)
		}
	}
}

func TestDeliverPermanentSkipsRetries(t *testing.T) {
	var letters []queue.DeadLetter
	msg := &fakeMsg{
		subject: "relay.sync.account",
		data:    []byte(`not-json`),
		header:  nats.Header{nats.MsgIdHdr: []string{"bad-1"}},
		meta:    &jetstream.MsgMetadata{NumDelivered: 1, Sequence: jetstream.SequencePair{Stream: 9}},
	}
	c := testDispatchClient(func(letter queue.DeadLetter) error {
		letters = append(letters, letter)
		return nil
	})
	c.deliver(context.Background(), msg, func(context.Context, queue.Message) error {
		return queue.Permanent(errors.New("invalid json"))
	})
	if msg.term != 1 || msg.nak != 0 || msg.ack != 0 {
		t.Fatalf("ack %d nak %d term %d", msg.ack, msg.nak, msg.term)
	}
	if len(letters) != 1 || letters[0].Error != "invalid json" || string(letters[0].Payload) != "not-json" {
		t.Fatalf("dlq %+v", letters)
	}
	if letters[0].MsgID != "bad-1" || letters[0].Subject != "relay.sync.account" || letters[0].Delivered != 1 {
		t.Fatalf("dlq %+v", letters[0])
	}
}

func TestDeliverLastAttemptGoesToDLQ(t *testing.T) {
	var n int
	msg := &fakeMsg{
		subject: "relay.notify.lifecycle",
		data:    []byte(`{"account_id":"a","action":"renew"}`),
		meta:    &jetstream.MsgMetadata{NumDelivered: 5},
	}
	c := testDispatchClient(func(letter queue.DeadLetter) error {
		n++
		if letter.Error != "still down" {
			t.Errorf("error = %q", letter.Error)
		}
		return nil
	})
	c.deliver(context.Background(), msg, func(context.Context, queue.Message) error {
		return errors.New("still down")
	})
	if n != 1 || msg.term != 1 || msg.nak != 0 {
		t.Fatalf("dlq calls %d term %d nak %d", n, msg.term, msg.nak)
	}
}

func TestDeliverKeepsMessageWhenDLQPublishFails(t *testing.T) {
	msg := &fakeMsg{
		subject: "relay.sync.account",
		data:    []byte(`{}`),
		meta:    &jetstream.MsgMetadata{NumDelivered: 5},
	}
	c := testDispatchClient(func(queue.DeadLetter) error {
		return errors.New("nats down")
	})
	c.deliver(context.Background(), msg, func(context.Context, queue.Message) error {
		return errors.New("still down")
	})
	if msg.term != 0 || msg.nak != 1 {
		t.Fatalf("term %d nak %d, message must stay when the dlq publish fails", msg.term, msg.nak)
	}
}

func TestDeliverPanicIsPermanent(t *testing.T) {
	var got string
	msg := &fakeMsg{
		subject: "relay.sync.account",
		data:    []byte(`{"account_id":"a"}`),
		meta:    &jetstream.MsgMetadata{NumDelivered: 1},
	}
	c := testDispatchClient(func(letter queue.DeadLetter) error {
		got = letter.Error
		return nil
	})
	c.deliver(context.Background(), msg, func(context.Context, queue.Message) error {
		panic("bad payload")
	})
	if msg.term != 1 || got != "handler panic: bad payload" {
		t.Fatalf("term %d err %q", msg.term, got)
	}
}
