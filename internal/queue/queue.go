// Package queue is the durable job contract (ADR 0003).
//
// Business packages depend on Publisher and Consumer. The NATS JetStream
// implementation lives in internal/queue/natsq and can be replaced later
// (for example with SQS) without changing those packages.
//
// Delivery is at least once. Handlers must be idempotent. NATS deduplicates
// only inside a short window, and it does not order work across consumers.
package queue

import (
	"context"
	"errors"
	"time"
)

// Publisher enqueues one job. msgID is a deterministic id from the producer
// (provider, external id, notification number). A second publish of the same
// id inside the broker's dedupe window is stored once and returns nil.
type Publisher interface {
	Publish(ctx context.Context, subject string, msgID string, payload []byte) error
}

// Message is one delivery. Payload is only valid for the handler call.
type Message struct {
	Subject string
	MsgID   string
	Payload []byte
}

// Handler processes one delivery. A nil error acknowledges it.
// A permanent error is dead-lettered immediately. Any other error is retried
// with backoff until the attempt limit, then dead-lettered.
type Handler func(ctx context.Context, msg Message) error

// Consumer runs h for each message on subject until ctx is cancelled.
// Cancel drains the delivery already in the handler; it does not drop it.
type Consumer interface {
	Consume(ctx context.Context, subject string, h Handler) error
}

// DeadLetter is one failed job stored for inspection and replay.
// Payload is the original body, not a re-encoding of a struct.
type DeadLetter struct {
	Seq       uint64    `json:"seq,omitempty"`
	Subject   string    `json:"subject"`
	MsgID     string    `json:"msg_id,omitempty"`
	Payload   []byte    `json:"payload"`
	Error     string    `json:"error"`
	Delivered uint64    `json:"delivered"`
	FailedAt  time.Time `json:"failed_at"`
}

// Outcome is what the consumer should do with a delivery.
type Outcome int

const (
	// OutcomeAck removes the job. The handler succeeded.
	OutcomeAck Outcome = iota
	// OutcomeRetry asks the broker to deliver again later.
	OutcomeRetry
	// OutcomeDead records the job on the dead-letter stream and stops retries.
	OutcomeDead
)

// OutcomeFor classifies a handler result. delivered is 1 on the first try.
// A permanent error skips the remaining attempts. Reaching maxDeliver also
// stops, so a poison message cannot be retried forever.
func OutcomeFor(err error, delivered, maxDeliver uint64) Outcome {
	if err == nil {
		return OutcomeAck
	}
	if IsPermanent(err) || (maxDeliver > 0 && delivered >= maxDeliver) {
		return OutcomeDead
	}
	return OutcomeRetry
}

// permanentError marks a failure that another attempt cannot fix
// (the account is gone, the body is not JSON).
type permanentError struct{ err error }

func (e permanentError) Error() string {
	if e.err == nil {
		return "permanent"
	}
	return e.err.Error()
}

func (e permanentError) Unwrap() error { return e.err }

// Permanent wraps err so the consumer dead-letters it without retrying.
// A nil err stays nil. Wrapping an error that is already permanent is a no-op.
func Permanent(err error) error {
	if err == nil || IsPermanent(err) {
		return err
	}
	return permanentError{err: err}
}

// IsPermanent reports whether err, or something it wraps, was marked Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}
