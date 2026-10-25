package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const providerBodyLimit = 1 << 20

var (
	// ErrProviderUnavailable is a network error or a 5xx from the provider.
	// The subscription row is left in place so the next cycle retries.
	ErrProviderUnavailable = errors.New("provider unavailable")

	// ErrGone means the provider no longer has this subscription (404).
	// The caller creates a new one.
	ErrGone = errors.New("subscription gone")
)

// Channel is a provider push registration. Secret is plaintext and only
// lives until the caller hashes it. Empty Secret on renew means "keep the
// stored hash".
type Channel struct {
	ExternalID string
	ResourceID string
	Secret     string
	ExpiresAt  time.Time
}

// Provider talks to one vendor. *Google and *Microsoft implement it.
type Provider interface {
	// Subscribe creates a channel. publicBase is the HTTPS origin providers call.
	Subscribe(ctx context.Context, accessToken, publicBase string) (Channel, error)
	// Renew extends the channel. replaced is true when ExternalID and Secret
	// are new (Google has no in-place renew). The caller stops the previous
	// channel after the new one is stored.
	Renew(ctx context.Context, accessToken, publicBase string, current Channel) (Channel, bool, error)
	Stop(ctx context.Context, accessToken string, current Channel) error
}

func callJSON(ctx context.Context, client *http.Client, method, endpoint, token string, reqBody, respBody any) error {
	var buf io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		buf = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, providerBodyLimit))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	if resp.StatusCode >= 300 {
		return classifyStatus(resp.StatusCode, body)
	}
	if respBody != nil && len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, respBody); err != nil {
			return fmt.Errorf("provider response: %w", err)
		}
	}
	return nil
}

func classifyStatus(status int, body []byte) error {
	snip := string(body)
	if len(snip) > 180 {
		snip = snip[:180]
	}
	snip = strings.TrimSpace(snip)
	switch {
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrGone, snip)
	case status >= 500 || status == http.StatusTooManyRequests:
		return fmt.Errorf("%w: status %d: %s", ErrProviderUnavailable, status, snip)
	default:
		return fmt.Errorf("provider status %d: %s", status, snip)
	}
}

func hook(publicBase, path string) string {
	return strings.TrimRight(publicBase, "/") + path
}
