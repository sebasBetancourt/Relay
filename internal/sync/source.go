// Package sync applies provider calendar changes one account at a time.
// A webhook only says that something changed. Delta sync asks for the
// changes since the stored cursor and commits them with that cursor.
package sync

import (
	"context"
	"errors"
	"time"

	"github.com/sebasBetancourt/relay/internal/calendar"
)

const (
	// DefaultWindow matches GET /v1/calendars/{id}/events. The cursor is
	// bound to this span: a different span is a full resync.
	DefaultWindow = 30 * 24 * time.Hour
	maxDeltaPages = 100
)

// Window is the range a full sync asked the provider for. Google bakes it
// into the syncToken. Graph bakes it into the deltaLink.
type Window struct {
	Start time.Time
	End   time.Time
}

// Change is one added, updated or removed event. Version is the Google
// etag or the Graph changeKey. An empty version is filled by the applier
// from the event body so a replay still matches.
type Change struct {
	Event   calendar.Event
	Version string
	Deleted bool
}

var (
	// ErrCursorExpired: the provider rejected the cursor (HTTP 410).
	// The caller does one full sync and does not loop.
	ErrCursorExpired = errors.New("sync cursor expired")
)

// DeltaSource reads one provider's incremental feed.
type DeltaSource interface {
	// Delta returns every change since cursor and the cursor to store.
	// An empty cursor is a full sync of window. next is empty only on error.
	Delta(ctx context.Context, accessToken, cursor string, window Window) (changes []Change, next string, err error)
}
