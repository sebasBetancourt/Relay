package sync

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type memPub struct{ ids []string }

func (m *memPub) Publish(_ context.Context, subject, msgID string, body []byte) error {
	if subject != subjectSync {
		return errPublish(subject)
	}
	var job struct {
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(body, &job); err != nil {
		return err
	}
	m.ids = append(m.ids, job.AccountID+" "+msgID)
	return nil
}

type errPublish string

func (e errPublish) Error() string { return "bad subject " + string(e) }

func TestEnqueueStalePublishesOneJobPerAccount(t *testing.T) {
	claims := staleClaim{ids: []string{"a", "b"}}
	pub := &memPub{}
	n, err := enqueueStale(context.Background(), claims, pub, time.Minute, 10)
	if err != nil || n != 2 || len(pub.ids) != 2 {
		t.Fatalf("n %d err %v ids %v", n, err, pub.ids)
	}
	if pub.ids[0] == pub.ids[1] {
		t.Fatal("msg ids collided")
	}
}

type staleClaim struct{ ids []string }

func (s staleClaim) ClaimStale(_ context.Context, _ time.Duration, _ int, fn func([]string) error) error {
	return fn(append([]string(nil), s.ids...))
}
