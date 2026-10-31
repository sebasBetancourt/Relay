package contract

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// recorder stands in for *testing.T: a test that cannot fail proves nothing,
// so these tests assert that the validator complains.
type recorder struct{ problems []string }

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

const validTenant = `{"id":"3fa85f64-5717-4562-b3fc-2c963f66afa6","name":"Acme","key_prefix":"rly_test_AbCdEfGh"}`

type response struct {
	status      int
	contentType string
	body        string
	noRequestID bool
}

func exercise(t *testing.T, method, target, reqBody string, res response) []string {
	t.Helper()
	spec, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	rep := &recorder{}
	handler := spec.Wrap(rep, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !res.noRequestID {
			w.Header().Set("X-Request-ID", "req-1")
		}
		if res.contentType != "" {
			w.Header().Set("Content-Type", res.contentType)
		}
		w.WriteHeader(res.status)
		_, _ = io.WriteString(w, res.body)
	}))
	req := httptest.NewRequest(method, target, strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != res.status || rec.Body.String() != res.body {
		t.Fatalf("the client must get the response untouched: %d %q", rec.Code, rec.Body.String())
	}
	return rep.problems
}

const account = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

func TestValidResponsesPass(t *testing.T) {
	tests := []struct {
		name, method, target string
		res                  response
	}{
		{"json", http.MethodGet, "/v1/me", response{status: 200, contentType: "application/json", body: validTenant}},
		{"text plain", http.MethodGet, "/healthz", response{status: 200, contentType: "text/plain; charset=utf-8", body: "ok"}},
		{"no content", http.MethodDelete, "/v1/accounts/" + account, response{status: 204}},
		{"redirect with net/http's note", http.MethodGet, "/v1/connect/google", response{status: 302, contentType: "text/html", body: "<a>Found</a>"}},
		{"auth hides unknown paths", http.MethodGet, "/v1/nope", response{status: 401, contentType: "application/json", body: `{"error":"unauthorized"}`}},
		{"wrong method", http.MethodPost, "/healthz", response{status: 405, contentType: "text/plain", body: "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exercise(t, tt.method, tt.target, "", tt.res); len(got) != 0 {
				t.Fatalf("problems: %v", got)
			}
		})
	}
}

func TestViolationsAreCaught(t *testing.T) {
	tests := []struct {
		name, method, target, reqBody string
		res                           response
		want                          string
	}{
		{
			name: "undocumented field", method: http.MethodGet, target: "/v1/me",
			res:  response{status: 200, contentType: "application/json", body: strings.Replace(validTenant, "{", `{"secret":1,`, 1)},
			want: "secret",
		},
		{
			name: "undocumented field in a nested object", method: http.MethodGet, target: "/v1/calendars/" + account + "/events",
			res: response{status: 200, contentType: "application/json", body: `{"events":[{"id":"e","title":"t","start":"2026-11-02","end":"2026-11-03","all_day":true,"provider":"google",
				"organizer":{"email":"a@b.co","phone":"1"}}]}`},
			want: "phone",
		},
		{
			name: "missing field", method: http.MethodGet, target: "/v1/me",
			res:  response{status: 200, contentType: "application/json", body: `{"id":"x"}`},
			want: "missing",
		},
		{
			name: "wrong type", method: http.MethodGet, target: "/v1/me",
			res:  response{status: 200, contentType: "application/json", body: `{"id":"x","name":1,"key_prefix":"y"}`},
			want: "name",
		},
		{
			name: "undocumented status", method: http.MethodGet, target: "/v1/me",
			res:  response{status: 418, contentType: "application/json", body: `{}`},
			want: "status",
		},
		{
			name: "undocumented content type", method: http.MethodGet, target: "/v1/me",
			res:  response{status: 200, contentType: "text/html", body: validTenant},
			want: "Content-Type",
		},
		{
			name: "body on an empty response", method: http.MethodDelete, target: "/v1/accounts/" + account,
			res:  response{status: 204, contentType: "text/plain", body: "gone"},
			want: "no body",
		},
		{
			name: "missing request id", method: http.MethodGet, target: "/v1/me",
			res:  response{status: 200, contentType: "application/json", body: validTenant, noRequestID: true},
			want: "X-Request-ID",
		},
		{
			name: "served route that is not documented", method: http.MethodGet, target: "/v1/secret",
			res:  response{status: 200, contentType: "application/json", body: `{}`},
			want: "not in the spec",
		},
		{
			name: "undocumented query parameter", method: http.MethodGet, target: "/v1/accounts?max_results=5&sort=name",
			res:  response{status: 200, contentType: "application/json", body: `{"accounts":[]}`},
			want: "undocumented query parameter sort",
		},
		{
			name: "accepted request outside the spec", method: http.MethodGet, target: "/v1/accounts?max_results=abc",
			res:  response{status: 200, contentType: "application/json", body: `{"accounts":[]}`},
			want: "request",
		},
		{
			name: "accepted body outside the spec", method: http.MethodPost, target: "/v1/calendars/" + account + "/events", reqBody: `{"title":"no times"}`,
			res:  response{status: 201, contentType: "application/json", body: `{"id":"e","title":"t","start":"2026-11-02","end":"2026-11-03","all_day":true,"provider":"google"}`},
			want: "request",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := exercise(t, tt.method, tt.target, tt.reqBody, tt.res)
			if len(got) == 0 {
				t.Fatal("the validator accepted a broken exchange")
			}
			if joined := strings.Join(got, "\n"); !strings.Contains(joined, tt.want) {
				t.Fatalf("problems %q do not mention %q", joined, tt.want)
			}
		})
	}
}

// A request the server refused may be wrong on purpose: tests send bad input.
func TestRefusedRequestsAreNotJudged(t *testing.T) {
	got := exercise(t, http.MethodGet, "/v1/accounts?max_results=abc", "",
		response{status: 400, contentType: "application/json", body: `{"error":"invalid_query","detail":"max_results"}`})
	if len(got) != 0 {
		t.Fatalf("problems: %v", got)
	}
}

func TestRoutesListsTheSpec(t *testing.T) {
	spec, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if routes := spec.Routes(); !slices.Contains(routes, "POST /v1/calendars/{id}/events") {
		t.Fatalf("routes = %v", routes)
	}
}
