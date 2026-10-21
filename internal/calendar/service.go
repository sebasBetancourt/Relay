package calendar

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/sebasBetancourt/relay/internal/oauth"
)

// Service lists events using a TokenProvider and per-provider Sources.
type Service struct {
	tokens  TokenProvider
	sources map[oauth.Provider]Source
}

// NewService wires token access to calendar Sources.
func NewService(tokens TokenProvider, sources map[oauth.Provider]Source) *Service {
	return &Service{
		tokens:  tokens,
		sources: maps.Clone(sources),
	}
}

// ListEvents returns a page of events for the tenant's connected account.
// page_token is Relay's own opaque cursor: it is unwrapped here (and must
// belong to this account's provider) and the provider's next token is
// wrapped again on the way out.
func (s *Service) ListEvents(ctx context.Context, tenantID, accountID string, q Query) (Page, error) {
	var cursor *pageCursor
	if q.PageToken != "" {
		c, err := decodePageToken(q.PageToken)
		if err != nil {
			return Page{}, err
		}
		cursor = &c
	}

	tok, err := s.tokens.AccessToken(ctx, tenantID, accountID)
	if err != nil {
		return Page{}, err
	}
	src, ok := s.sources[tok.Provider]
	if !ok {
		return Page{}, fmt.Errorf("list events: no source for provider %q", tok.Provider)
	}

	if cursor != nil {
		if cursor.P != string(tok.Provider) {
			return Page{}, ErrInvalidPageToken
		}
		// Page N repeats page 1's window and size.
		q = Query{TimeMin: cursor.Min, TimeMax: cursor.Max, MaxResults: cursor.N, PageToken: cursor.T}
	}

	page, err := s.list(ctx, src, tenantID, accountID, tok.Value, q)
	// Sources drop events the client must not see (Graph returns cancelled
	// occurrences), so a provider page can end up empty while more exist.
	// A client seeing "events": [] would stop paging and miss events, so
	// skip empty pages, a bounded number of times.
	for hops := 0; err == nil && len(page.Events) == 0 && page.NextPageToken != "" && hops < maxEmptyPageHops; hops++ {
		q.PageToken = page.NextPageToken
		page, err = s.list(ctx, src, tenantID, accountID, tok.Value, q)
	}
	if err != nil {
		return Page{}, err
	}
	page.NextPageToken = encodePageToken(tok.Provider, page.NextPageToken, q)
	return page, nil
}

// maxEmptyPageHops bounds how many empty provider pages one request skips.
const maxEmptyPageHops = 5

func (s *Service) list(ctx context.Context, src Source, tenantID, accountID, accessToken string, q Query) (Page, error) {
	page, err := src.ListEvents(ctx, accessToken, q)
	if !errors.Is(err, ErrTokenRejected) {
		return page, err
	}

	// One forced refresh + one retry. No loops.
	tok, err := s.tokens.RefreshAccessToken(ctx, tenantID, accountID)
	if err != nil {
		return Page{}, err
	}
	page, err = src.ListEvents(ctx, tok.Value, q)
	if errors.Is(err, ErrTokenRejected) {
		return Page{}, fmt.Errorf("%w: still 401 after refresh", ErrProviderBadResponse)
	}
	return page, err
}
