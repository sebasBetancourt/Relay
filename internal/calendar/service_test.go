package calendar

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	pages   []Page
	errs    []error
	n       int
	toks    []string
	queries []Query
}

func (f *fakeSource) ListEvents(_ context.Context, accessToken string, q Query) (Page, error) {
	f.toks = append(f.toks, accessToken)
	f.queries = append(f.queries, q)
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

func TestServicePageTokenRoundTrip(t *testing.T) {
	tokens := &fakeTokens{access: oauth.AccessToken{Value: "a", Provider: oauth.ProviderMicrosoft}}
	src := &fakeSource{pages: []Page{
		{Events: []Event{{ID: "e1"}}, NextPageToken: "skiptoken:abc"},
		{Events: []Event{{ID: "e2"}}},
	}}
	svc := NewService(tokens, map[oauth.Provider]Source{oauth.ProviderMicrosoft: src})

	page1 := Query{
		TimeMin:    time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
		TimeMax:    time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC),
		MaxResults: 10,
	}
	p1, err := svc.ListEvents(context.Background(), "t", "acc", page1)
	if err != nil {
		t.Fatal(err)
	}
	if p1.NextPageToken == "" || p1.NextPageToken == "skiptoken:abc" {
		t.Fatalf("next token must be Relay's opaque wrapper, got %q", p1.NextPageToken)
	}

	// Page 2: the client sends only page_token (its own time_min would
	// default to a different "now"). Relay replays page 1's window and size.
	page2 := Query{TimeMin: time.Now(), TimeMax: time.Now().Add(time.Hour), MaxResults: 50, PageToken: p1.NextPageToken}
	p2, err := svc.ListEvents(context.Background(), "t", "acc", page2)
	if err != nil {
		t.Fatal(err)
	}
	got := src.queries[1]
	if got.PageToken != "skiptoken:abc" || !got.TimeMin.Equal(page1.TimeMin) || !got.TimeMax.Equal(page1.TimeMax) || got.MaxResults != 10 {
		t.Fatalf("page 2 query = %+v, want page 1 window with the provider token", got)
	}
	if p2.NextPageToken != "" {
		t.Fatalf("last page must have no next token, got %q", p2.NextPageToken)
	}
}

func TestServiceRejectsBadPageTokens(t *testing.T) {
	googleTok := encodePageToken(oauth.ProviderGoogle, "g-token", Query{
		TimeMin: time.Now(), TimeMax: time.Now().Add(time.Hour), MaxResults: 5,
	})
	for name, token := range map[string]string{
		"other provider's token": googleTok,
		"not base64":             "%%%",
		"not json":               "bm90LWpzb24",
		"too long":               strings.Repeat("A", maxPageTokenLen+1),
		"empty provider token":   encodePageToken(oauth.ProviderMicrosoft, "", Query{}),
	} {
		t.Run(name, func(t *testing.T) {
			tokens := &fakeTokens{access: oauth.AccessToken{Value: "a", Provider: oauth.ProviderMicrosoft}}
			src := &fakeSource{}
			svc := NewService(tokens, map[oauth.Provider]Source{oauth.ProviderMicrosoft: src})

			q := Query{TimeMin: time.Now(), TimeMax: time.Now().Add(time.Hour), MaxResults: 5, PageToken: token}
			if token == "" {
				q.PageToken = "x" // an empty wrapper encodes to "", use garbage instead
			}
			_, err := svc.ListEvents(context.Background(), "t", "acc", q)
			if !errors.Is(err, ErrInvalidPageToken) {
				t.Fatalf("err = %v, want ErrInvalidPageToken", err)
			}
			if src.n != 0 {
				t.Fatal("the provider must not be called with a bad page token")
			}
		})
	}
}

func TestServiceSkipsEmptyPages(t *testing.T) {
	tokens := &fakeTokens{access: oauth.AccessToken{Value: "a", Provider: oauth.ProviderMicrosoft}}
	ev := Event{ID: "e1", Provider: "microsoft"}
	src := &fakeSource{pages: []Page{
		{NextPageToken: "skiptoken:1"},                      // all cancelled
		{NextPageToken: "skiptoken:2"},                      // all cancelled
		{Events: []Event{ev}, NextPageToken: "skiptoken:3"}, // first real page
	}}
	svc := NewService(tokens, map[oauth.Provider]Source{oauth.ProviderMicrosoft: src})

	page, err := svc.ListEvents(context.Background(), "t", "acc", Query{
		TimeMin: time.Now(), TimeMax: time.Now().Add(time.Hour), MaxResults: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || src.n != 3 {
		t.Fatalf("events = %d after %d provider calls, want 1 after 3", len(page.Events), src.n)
	}
	if src.queries[1].PageToken != "skiptoken:1" || src.queries[2].PageToken != "skiptoken:2" {
		t.Fatalf("hops must follow the provider tokens: %+v", src.queries)
	}
	c, err := decodePageToken(page.NextPageToken)
	if err != nil || c.T != "skiptoken:3" {
		t.Fatalf("next token = %+v %v, want the last page's token", c, err)
	}
}

func TestServiceBoundsEmptyPageHops(t *testing.T) {
	tokens := &fakeTokens{access: oauth.AccessToken{Value: "a", Provider: oauth.ProviderMicrosoft}}
	var pages []Page
	for i := range 20 {
		pages = append(pages, Page{NextPageToken: fmt.Sprintf("skiptoken:%d", i)})
	}
	src := &fakeSource{pages: pages}
	svc := NewService(tokens, map[oauth.Provider]Source{oauth.ProviderMicrosoft: src})

	page, err := svc.ListEvents(context.Background(), "t", "acc", Query{
		TimeMin: time.Now(), TimeMax: time.Now().Add(time.Hour), MaxResults: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if src.n != 1+maxEmptyPageHops {
		t.Fatalf("provider calls = %d, want %d", src.n, 1+maxEmptyPageHops)
	}
	// Still empty after the bound: the client gets a token to continue.
	if len(page.Events) != 0 || page.NextPageToken == "" {
		t.Fatalf("page = %+v, want empty with a next token", page)
	}
}
