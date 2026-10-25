package queue

import (
	"context"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/testutil/natsjs"
)

func TestPublishDedupesOnMsgID(t *testing.T) {
	ctx := context.Background()
	c, err := Connect(ctx, natsjs.Start(t), WithMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	const subject = "relay.sync.account"
	body := []byte(`{"account_id":"acct-1","provider":"google"}`)
	if err := c.Publish(ctx, subject, "google:ch-1:4", body); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(ctx, subject, "google:ch-1:4", body); err != nil {
		t.Fatal(err)
	}
	n, err := c.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("messages = %d, want 1", n)
	}
	got, err := c.Collect(ctx, subject, 5, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0]) != string(body) {
		t.Fatalf("collected %#v", got)
	}
}
