// Package contract checks real HTTP traffic against api/openapi.yaml, so the
// spec cannot drift from the code. Tests wrap their router with Wrap, and every
// response they trigger is validated: a documented status, Content-Type and
// body, and an X-Request-ID.
//
// The spec stays the contract and the code stays the behavior: nothing is
// generated from it, so the architecture does not depend on a tool.
package contract

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
)

// Reporter is the part of *testing.T a Wrap needs. The package's own tests pass
// a recorder, to prove that a broken response really is reported.
type Reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// Spec is a loaded, closed api/openapi.yaml.
type Spec struct {
	doc    *openapi3.T
	router routers.Router
}

var load = sync.OnceValues(func() (*Spec, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("contract: cannot locate the repository")
	}
	return Load(filepath.Join(filepath.Dir(file), "..", "..", "..", "api", "openapi.yaml"))
})

// Default returns the repository's spec. Loading it once keeps the suites fast.
func Default() (*Spec, error) { return load() }

// Load reads a spec from path.
func Load(path string) (*Spec, error) {
	doc, err := openapi3.NewLoader().LoadFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("load spec: %w", err)
	}
	// Match any host: tests have none, and servers lists only the local stack.
	doc.Servers = nil
	if err := doc.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("invalid spec: %w", err)
	}
	router, err := legacy.NewRouter(doc)
	if err != nil {
		return nil, fmt.Errorf("spec router: %w", err)
	}
	closeSchemas(doc)
	return &Spec{doc: doc, router: router}, nil
}

// closeSchemas forbids undocumented fields, in memory only. Without it JSON
// Schema accepts any extra property and a new field could ship undocumented.
// The published spec stays open, so a client may keep ignoring new fields.
func closeSchemas(doc *openapi3.T) {
	seen := map[*openapi3.Schema]bool{}
	for _, ref := range doc.Components.Schemas {
		closeSchema(ref, seen)
	}
}

func closeSchema(ref *openapi3.SchemaRef, seen map[*openapi3.Schema]bool) {
	if ref == nil || ref.Value == nil || seen[ref.Value] {
		return
	}
	s := ref.Value
	seen[s] = true
	if len(s.Properties) > 0 && s.AdditionalProperties.Schema == nil {
		no := false
		s.AdditionalProperties.Has = &no
	}
	for _, p := range s.Properties {
		closeSchema(p, seen)
	}
	closeSchema(s.Items, seen)
}

// Routes lists every documented "METHOD /path".
func (s *Spec) Routes() []string {
	var out []string
	for path, item := range s.doc.Paths.Map() {
		for method := range item.Operations() {
			out = append(out, method+" "+path)
		}
	}
	slices.Sort(out)
	return out
}

// Wrap validates everything that passes through next and reports each mismatch
// on t. The client still gets the response, so the test's own asserts keep working.
func (s *Spec) Wrap(t Reporter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)

		// Validate on a copy: r was consumed, and its context may be done.
		check := r.Clone(context.Background())
		check.Body = io.NopCloser(bytes.NewReader(body))
		for _, problem := range s.check(check, body, rec) {
			t.Helper()
			t.Errorf("contract: %s %s → %d: %s", r.Method, r.URL.Path, rec.Code, problem)
		}

		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

// check returns what is wrong with one exchange.
func (s *Spec) check(r *http.Request, reqBody []byte, res *httptest.ResponseRecorder) []string {
	route, params, err := s.router.FindRoute(r)
	if err != nil {
		// Auth runs before routing and hides which paths exist, and the mux
		// refuses a wrong method: those answers are by design. Anything else
		// means a route served without being documented.
		if res.Code == http.StatusUnauthorized || res.Code == http.StatusNotFound || res.Code == http.StatusMethodNotAllowed {
			return nil
		}
		return []string{"route is not in the spec: " + err.Error()}
	}

	var problems []string
	if res.Header().Get("X-Request-ID") == "" {
		problems = append(problems, "missing X-Request-ID")
	}
	opts := &openapi3filter.Options{
		AuthenticationFunc:    openapi3filter.NoopAuthenticationFunc,
		IncludeResponseStatus: true,
		MultiError:            true,
	}
	input := &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route, Options: opts}

	problems = append(problems, undocumentedParams(route, r)...)
	// A request the server refused may be wrong on purpose. One it accepted
	// must match the spec, or the docs promise something the code does not.
	if res.Code < http.StatusBadRequest {
		if err := openapi3filter.ValidateRequest(context.Background(), input); err != nil {
			problems = append(problems, "request: "+summarize(err))
		}
		r.Body = io.NopCloser(bytes.NewReader(reqBody))
	}

	err = openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: input,
		Status:                 res.Code,
		Header:                 res.Header(),
		Body:                   io.NopCloser(bytes.NewReader(res.Body.Bytes())),
		Options:                opts,
	})
	if err != nil {
		problems = append(problems, "response: "+summarize(err))
	}
	if extra := undocumentedBody(route, res); extra != "" {
		problems = append(problems, extra)
	}
	return problems
}

// undocumentedParams catches what ValidateRequest does not: it only checks
// parameters the spec lists, so a query parameter it never heard of passes.
func undocumentedParams(route *routers.Route, r *http.Request) []string {
	known := map[string]bool{}
	for _, list := range []openapi3.Parameters{route.PathItem.Parameters, route.Operation.Parameters} {
		for _, p := range list {
			if p.Value != nil && p.Value.In == openapi3.ParameterInQuery {
				known[p.Value.Name] = true
			}
		}
	}
	var out []string
	for name := range r.URL.Query() {
		if !known[name] {
			out = append(out, "undocumented query parameter "+name)
		}
	}
	slices.Sort(out)
	return out
}

// undocumentedBody catches a body on a response the spec says is empty, which
// ValidateResponse skips. A redirect's small HTML note comes from net/http.
func undocumentedBody(route *routers.Route, res *httptest.ResponseRecorder) string {
	if res.Body.Len() == 0 || (res.Code >= 300 && res.Code < 400) {
		return ""
	}
	doc := route.Operation.Responses.Status(res.Code)
	if doc == nil || doc.Value == nil || len(doc.Value.Content) > 0 {
		return "" // an unknown status is already reported
	}
	return "the spec documents no body, but got " + res.Header().Get("Content-Type")
}

// summarize keeps the reason and drops the schema dump.
func summarize(err error) string {
	msg, _, _ := strings.Cut(err.Error(), "\nSchema:")
	return strings.TrimSpace(msg)
}
