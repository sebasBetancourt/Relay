package calendar

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/sebasBetancourt/relay/internal/connect"
)

// Service lists events using a TokenProvider and per-provider Sources.
type Service struct {
	tokens  TokenProvider
	sources map[connect.Provider]Source
}

// NewService wires token access to calendar Sources.
func NewService(tokens TokenProvider, sources map[connect.Provider]Source) *Service {
	return &Service{
		tokens:  tokens,
		sources: maps.Clone(sources),
	}
}

// ListEvents returns a page of events for the tenant's connected account.
func (s *Service) ListEvents(ctx context.Context, tenantID, accountID string, q Query) (Page, error) {
	tok, err := s.tokens.AccessToken(ctx, tenantID, accountID)
	if err != nil {
		return Page{}, err
	}
	src, ok := s.sources[tok.Provider]
	if !ok {
		return Page{}, fmt.Errorf("list events: no source for provider %q", tok.Provider)
	}

	page, err := src.ListEvents(ctx, tok.Value, q)
	if !errors.Is(err, ErrTokenRejected) {
		return page, err
	}

	// One forced refresh + one retry. No loops.
	tok, err = s.tokens.RefreshAccessToken(ctx, tenantID, accountID)
	if err != nil {
		return Page{}, err
	}
	page, err = src.ListEvents(ctx, tok.Value, q)
	if errors.Is(err, ErrTokenRejected) {
		return Page{}, fmt.Errorf("%w: still 401 after refresh", ErrProviderBadResponse)
	}
	return page, err
}
