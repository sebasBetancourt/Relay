package calendar

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/sebasBetancourt/relay/internal/oauth"
)

// ErrInvalidPageToken: page_token is malformed, too long, or belongs to an
// account of another provider.
var ErrInvalidPageToken = errors.New("invalid page token")

const maxPageTokenLen = 4096

// pageCursor is what Relay's opaque page_token carries. Wrapping the
// provider token keeps the API provider-neutral and lets Relay check it
// before use. The window and page size travel with it, so page 2 queries
// exactly what page 1 did even if the client omitted time_min (which
// defaults to "now" and would otherwise move between requests).
type pageCursor struct {
	V   int       `json:"v"`
	P   string    `json:"p"`   // provider
	T   string    `json:"t"`   // provider page token (opaque, provider-specific)
	Min time.Time `json:"min"` // time_min of page 1
	Max time.Time `json:"max"` // time_max of page 1
	N   int       `json:"n"`   // max_results of page 1
}

func encodePageToken(provider oauth.Provider, providerToken string, q Query) string {
	if providerToken == "" {
		return ""
	}
	raw, _ := json.Marshal(pageCursor{
		V: 1, P: string(provider), T: providerToken,
		Min: q.TimeMin.UTC(), Max: q.TimeMax.UTC(), N: q.MaxResults,
	})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodePageToken(token string) (pageCursor, error) {
	if len(token) > maxPageTokenLen {
		return pageCursor{}, ErrInvalidPageToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return pageCursor{}, ErrInvalidPageToken
	}
	var c pageCursor
	if err := json.Unmarshal(raw, &c); err != nil || c.V != 1 || c.P == "" || c.T == "" ||
		!c.Max.After(c.Min) || c.N < 1 {
		return pageCursor{}, ErrInvalidPageToken
	}
	return c, nil
}
