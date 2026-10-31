package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/httpapi/middleware"
)

// Deps is everything the router needs, built once in main (no globals).
// A struct instead of positional parameters keeps NewRouter stable as
// features are added, and lets tests set only what they use.
type Deps struct {
	Log      *slog.Logger
	DB       *pgxpool.Pool
	Tenants  middleware.TenantLookup
	Env      apikey.Env
	Connect  Connector
	Callback Callbacker
	Calendar CalendarReader
	// CalendarWrite adds the event write routes; nil leaves them unregistered.
	// Idempotency is optional: without it, Idempotency-Key is ignored.
	CalendarWrite CalendarWriter
	Idempotency   IdempotencyStore
	// Accounts lists the tenant's connected accounts; Disconnect removes one.
	Accounts   AccountLister
	Disconnect Disconnector
	// Webhooks receives provider push notifications. Nil leaves push off;
	// those paths then fall through to API-key auth like any other /v1 route.
	Webhooks Webhooks
	// ClientIPHeader names the header a trusted proxy puts the client address
	// in. Empty uses the socket address.
	ClientIPHeader string
	// RateLimit budgets requests per tenant and per IP. Nil limits nothing.
	RateLimit *middleware.RateLimiter
}

// Webhooks is the public push surface. *notify.Service implements it.
type Webhooks interface {
	Google(http.ResponseWriter, *http.Request)
	Microsoft(http.ResponseWriter, *http.Request)
	MicrosoftLifecycle(http.ResponseWriter, *http.Request)
}

// routeTable is a ServeMux that remembers its "METHOD /path" patterns, so a
// test can compare them with the OpenAPI spec.
type routeTable struct {
	*http.ServeMux
	patterns *[]string
}

func (t routeTable) Handle(pattern string, h http.Handler) {
	*t.patterns = append(*t.patterns, pattern)
	t.ServeMux.Handle(pattern, h)
}

func (t routeTable) HandleFunc(pattern string, h http.HandlerFunc) {
	*t.patterns = append(*t.patterns, pattern)
	t.ServeMux.HandleFunc(pattern, h)
}

// NewRouter builds the API's handler.
func NewRouter(d Deps) http.Handler {
	h, _ := newRouter(d)
	return h
}

func newRouter(d Deps) (http.Handler, []string) {
	log, db, rl := d.Log, d.DB, d.RateLimit
	var patterns []string
	mux := routeTable{http.NewServeMux(), &patterns}

	// Public: probes from Docker/Kubernetes carry no API key.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			log.Warn("readyz failed", "err", err)
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	// Public OAuth callback: the browser returns from the provider without an
	// API key. Go 1.22+ picks this more-specific pattern over the /v1/ catch-all.
	mux.Handle("GET /v1/oauth/{provider}/callback", rl.PerIP(handleCallback(log, d.Callback)))

	// Public provider webhooks. Same routing rule as the callback: no API key.
	// The per-subscription secret is the credential. Registered only when push
	// is configured, so a deployment without WEBHOOK_BASE_URL does not advertise them.
	// notify limits them per IP itself, so a flood of bad keys cannot spend the
	// budget that provider notifications need.
	if d.Webhooks != nil {
		mux.HandleFunc("POST /v1/webhooks/google", d.Webhooks.Google)
		mux.HandleFunc("POST /v1/webhooks/microsoft/lifecycle", d.Webhooks.MicrosoftLifecycle)
		mux.HandleFunc("POST /v1/webhooks/microsoft", d.Webhooks.Microsoft)
	}

	// Private: everything under /v1/ requires an API key. Auth runs before
	// routing, so unknown /v1 paths also answer 401 without a key and do
	// not reveal which routes exist. The rate limit runs after auth because
	// it needs the tenant; probes above are never limited.
	v1 := routeTable{http.NewServeMux(), &patterns}
	v1.HandleFunc("GET /v1/me", handleMe)
	v1.HandleFunc("GET /v1/connect/{provider}", handleConnect(log, d.Connect))
	v1.HandleFunc("GET /v1/calendars/{id}/events", handleListEvents(log, d.Calendar))
	if d.CalendarWrite != nil {
		v1.HandleFunc("POST /v1/calendars/{id}/events", handleCreateEvent(log, d.CalendarWrite, d.Idempotency))
		v1.HandleFunc("PATCH /v1/calendars/{id}/events/{event_id}", handleUpdateEvent(log, d.CalendarWrite, d.Idempotency))
		v1.HandleFunc("DELETE /v1/calendars/{id}/events/{event_id}", handleDeleteEvent(log, d.CalendarWrite, d.Idempotency))
	}
	v1.HandleFunc("GET /v1/accounts", handleListAccounts(log, d.Accounts))
	v1.HandleFunc("DELETE /v1/accounts/{id}", handleDisconnectAccount(log, d.Disconnect))
	// Straight to the ServeMux: this catch-all is not a route of its own.
	mux.ServeMux.Handle("/v1/", middleware.RequireAPIKey(log, d.Tenants, d.Env, rl)(rl.PerTenant(v1)))

	// Outer runs first. middleware.RequestID must wrap Recover/middleware.Logger so they see the id in context.
	h := middleware.RequestID(middleware.Recover(log)(middleware.Logger(log)(mux)))
	return middleware.ClientIPFrom(d.ClientIPHeader)(h), patterns
}
