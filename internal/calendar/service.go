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

// CreateEvent validates in and creates it on the account's provider.
func (s *Service) CreateEvent(ctx context.Context, tenantID, accountID string, in EventInput) (Event, error) {
	change, err := ChangeFromInput(in)
	if err != nil {
		return Event{}, err
	}
	var ev Event
	err = s.write(ctx, tenantID, accountID, func(w Writer, token string) error {
		var err error
		ev, err = w.CreateEvent(ctx, token, change)
		return err
	})
	return ev, err
}

// UpdateEvent applies p. version is the client's If-Match, "" for none.
// The extra read is what refuses a single occurrence: the provider would
// accept the edit and leave the series split.
func (s *Service) UpdateEvent(ctx context.Context, tenantID, accountID, eventID string, p EventPatch, version string) (Event, error) {
	change, err := ChangeFromPatch(p)
	if err != nil {
		return Event{}, err
	}
	var ev Event
	err = s.write(ctx, tenantID, accountID, func(w Writer, token string) error {
		kind, err := w.EventKind(ctx, token, eventID)
		if err != nil {
			return err
		}
		if kind == KindOccurrence {
			return errOccurrence
		}
		// A new time without its pattern may leave the series starting
		// outside it, and the providers disagree on what happens then.
		if kind == KindSeries && change.schedule != nil && change.rule == nil {
			return unsupported("moving a recurring event needs its recurrence in the same request")
		}
		ev, err = w.UpdateEvent(ctx, token, eventID, change, version)
		return err
	})
	return ev, err
}

// DeleteEvent removes a single event or a whole series.
func (s *Service) DeleteEvent(ctx context.Context, tenantID, accountID, eventID string) error {
	return s.write(ctx, tenantID, accountID, func(w Writer, token string) error {
		kind, err := w.EventKind(ctx, token, eventID)
		if err != nil {
			return err
		}
		if kind == KindOccurrence {
			return errOccurrence
		}
		return w.DeleteEvent(ctx, token, eventID)
	})
}

var errOccurrence = unsupported("a single occurrence cannot be changed: change the whole series with the id returned on create")

// write checks the stored scopes first, so an account connected read-only gets
// 403 with the way out, and retries once after a 401 like list does. A retry
// is safe here: a 401 means the provider processed nothing.
func (s *Service) write(ctx context.Context, tenantID, accountID string, fn func(Writer, string) error) error {
	tok, err := s.tokens.AccessToken(ctx, tenantID, accountID)
	if err != nil {
		return err
	}
	missing := &InsufficientScopeError{
		Provider: string(tok.Provider),
		Required: []string{oauth.FeatureScope(tok.Provider, oauth.FeatureCalendarWrite)},
	}
	if !oauth.HasFeature(tok.Provider, oauth.FeatureCalendarWrite, tok.Scopes) {
		return missing
	}
	w, ok := s.sources[tok.Provider].(Writer)
	if !ok {
		return fmt.Errorf("write event: no writer for provider %q", tok.Provider)
	}

	err = fn(w, tok.Value)
	if errors.Is(err, ErrTokenRejected) {
		tok, err = s.tokens.RefreshAccessToken(ctx, tenantID, accountID)
		if err != nil {
			return err
		}
		err = fn(w, tok.Value)
		if errors.Is(err, ErrTokenRejected) {
			return fmt.Errorf("%w: still 401 after refresh", ErrProviderBadResponse)
		}
	}
	if errors.Is(err, ErrScopeRejected) {
		// The user can narrow a grant after connecting.
		return missing
	}
	return err
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
