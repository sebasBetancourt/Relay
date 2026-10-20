package calendar

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
)

type fakeTokens struct {
	access  oauth.AccessToken
	accessN int
	refresh oauth.AccessToken
	refErr  error
	accErr  error
	refN    int
}

func (f *fakeTokens) AccessToken(context.Context, string, string) (oauth.AccessToken, error) {
	f.accessN++
	if f.accErr != nil {
		return oauth.AccessToken{}, f.accErr
	}
	return f.access, nil
}

func (f *fakeTokens) RefreshAccessToken(context.Context, string, string) (oauth.AccessToken, error) {
	f.refN++
	if f.refErr != nil {
		return oauth.AccessToken{}, f.refErr
	}
	return f.refresh, nil
}

type fakeSource struct {
	pages []Page
	errs  []error
	n     int
	toks  []string
}

func (f *fakeSource) ListEvents(_ context.Context, accessToken string, _ Query) (Page, error) {
	f.toks = append(f.toks, accessToken)
	i := f.n
	f.n++
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	var page Page
	if i < len(f.pages) {
		page = f.pages[i]
	}
	return page, err
}

func TestServiceListEventsOK(t *testing.T) {
	tok := &fakeTokens{access: oauth.AccessToken{Value: "a1", Provider: oauth.ProviderGoogle}}
	src := &fakeSource{pages: []Page{{Events: []Event{{ID: "1", Title: "Hi", Provider: "google"}}}}}
	svc := NewService(tok, map[oauth.Provider]Source{oauth.ProviderGoogle: src})
	page, err := svc.ListEvents(context.Background(), "t", "a", Query{})
	if err != nil || len(page.Events) != 1 || src.n != 1 || tok.refN != 0 {
		t.Fatalf("page=%+v err=%v calls=%d refresh=%d", page, err, src.n, tok.refN)
	}
}

func TestServiceRetriesOnceOn401(t *testing.T) {
	tok := &fakeTokens{
		access:  oauth.AccessToken{Value: "old", Provider: oauth.ProviderGoogle},
		refresh: oauth.AccessToken{Value: "new", Provider: oauth.ProviderGoogle},
	}
	src := &fakeSource{
		errs:  []error{ErrTokenRejected, nil},
		pages: []Page{{}, {Events: []Event{{ID: "ok"}}}},
	}
	svc := NewService(tok, map[oauth.Provider]Source{oauth.ProviderGoogle: src})
	page, err := svc.ListEvents(context.Background(), "t", "a", Query{})
	if err != nil || page.Events[0].ID != "ok" || tok.refN != 1 || src.n != 2 {
		t.Fatalf("page=%+v err=%v ref=%d n=%d", page, err, tok.refN, src.n)
	}
	if src.toks[0] != "old" || src.toks[1] != "new" {
		t.Fatalf("toks = %v", src.toks)
	}
}

func TestServiceDouble401(t *testing.T) {
	tok := &fakeTokens{
		access:  oauth.AccessToken{Value: "a", Provider: oauth.ProviderGoogle},
		refresh: oauth.AccessToken{Value: "b", Provider: oauth.ProviderGoogle},
	}
	src := &fakeSource{errs: []error{ErrTokenRejected, ErrTokenRejected}}
	svc := NewService(tok, map[oauth.Provider]Source{oauth.ProviderGoogle: src})
	_, err := svc.ListEvents(context.Background(), "t", "a", Query{})
	if !errors.Is(err, ErrProviderBadResponse) || src.n != 2 || tok.refN != 1 {
		t.Fatalf("err=%v n=%d ref=%d", err, src.n, tok.refN)
	}
}

func TestServicePropagatesNotFoundAndRevoked(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"not found", store.ErrNotFound},
		{"revoked", oauth.ErrAccountRevoked},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tok := &fakeTokens{accErr: tt.err}
			svc := NewService(tok, map[oauth.Provider]Source{oauth.ProviderGoogle: &fakeSource{}})
			_, err := svc.ListEvents(context.Background(), "t", "a", Query{TimeMin: time.Now(), TimeMax: time.Now().Add(time.Hour)})
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestServiceMissingSource(t *testing.T) {
	tok := &fakeTokens{access: oauth.AccessToken{Value: "a", Provider: oauth.ProviderMicrosoft}}
	svc := NewService(tok, map[oauth.Provider]Source{})
	_, err := svc.ListEvents(context.Background(), "t", "a", Query{})
	if err == nil {
		t.Fatal("expected error")
	}
}
