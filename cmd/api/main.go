package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	// Embed the IANA time zone database: the distroless image has none, and
	// Microsoft events are normalized with time.LoadLocation.
	_ "time/tzdata"

	"github.com/joho/godotenv"

	"github.com/sebasBetancourt/relay/internal/buildinfo"
	"github.com/sebasBetancourt/relay/internal/calendar"
	"github.com/sebasBetancourt/relay/internal/config"
	"github.com/sebasBetancourt/relay/internal/httpapi"
	"github.com/sebasBetancourt/relay/internal/httpapi/middleware"
	"github.com/sebasBetancourt/relay/internal/janitor"
	"github.com/sebasBetancourt/relay/internal/notify"
	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/queue/natsq"
	"github.com/sebasBetancourt/relay/internal/ratelimit"
	"github.com/sebasBetancourt/relay/internal/refresher"
	"github.com/sebasBetancourt/relay/internal/store"
	syncsvc "github.com/sebasBetancourt/relay/internal/sync"
	"github.com/sebasBetancourt/relay/internal/vault"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("relay", buildinfo.String())
		return
	}
	_ = godotenv.Load()

	// An image has no shell, so a deploy migrates through this binary. It needs
	// only the database: a release must not depend on the rest of the config.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		version, err := store.Migrate(os.Getenv("DATABASE_URL"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("schema at version", version)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log := config.NewLogger(cfg.LogLevel)
	for _, w := range cfg.Warnings(time.Now()) {
		log.Warn("config warning", "detail", w)
	}

	vlt, err := vault.New(cfg.EncryptionKey)
	if err != nil {
		log.Error("vault init failed", "err", err)
		os.Exit(1)
	}

	pool, err := store.NewPool(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Error("db connect failed", "err", err)
		os.Exit(1)
	}

	apiHTTP := &http.Client{Timeout: 10 * time.Second}

	// Only configured providers are wired; the others answer 400
	// unsupported_provider.
	providers := map[oauth.Provider]oauth.ProviderSpec{}
	sources := map[oauth.Provider]calendar.Source{}
	if g := cfg.Google; g != nil {
		providers[oauth.ProviderGoogle] = oauth.GoogleSpec(g.ClientID, g.ClientSecret, g.RedirectURL)
		sources[oauth.ProviderGoogle] = calendar.NewGoogleSource(apiHTTP)
	}
	if m := cfg.Microsoft; m != nil {
		providers[oauth.ProviderMicrosoft] = oauth.MicrosoftSpec(m.ClientID, m.ClientSecret, m.RedirectURL, m.Tenant, oauth.MicrosoftGraphURL)
		sources[oauth.ProviderMicrosoft] = calendar.NewMicrosoftSource(apiHTTP)
	}

	oauthStates := store.NewOAuthStates(pool)
	accounts := store.NewAccounts(pool)
	oauthSvc := oauth.NewService(oauth.Deps{
		States:     oauthStates,
		Accounts:   accounts,
		Vault:      vlt,
		Providers:  providers,
		HTTPClient: apiHTTP,
		Log:        log,
	})
	calendarSvc := calendar.NewService(oauthSvc, sources)

	var (
		natsClient *natsq.Client
		notifySvc  *notify.Service
		syncSvc    *syncsvc.Service
	)
	if cfg.NATSURL != "" {
		natsClient, err = natsq.Connect(context.Background(), cfg.NATSURL, natsq.WithLogger(log))
		if err != nil {
			log.Error("nats connect failed", "err", err)
			os.Exit(1)
		}
		log.Info("jetstream enabled")
	}
	if cfg.WebhookBaseURL == "" {
		log.Info("calendar push disabled", "reason", "WEBHOOK_BASE_URL is unset; staying on polling")
	} else {
		notifyHTTP := &http.Client{Timeout: 20 * time.Second}
		pushProviders := map[string]notify.Provider{}
		if cfg.Google != nil {
			pushProviders["google"] = notify.NewGoogle(notifyHTTP)
		}
		if cfg.Microsoft != nil {
			pushProviders["microsoft"] = notify.NewMicrosoft(notifyHTTP)
		}
		notifySvc = notify.New(notify.Deps{
			Log: log, Accounts: accounts, Tokens: oauthSvc,
			Subs: store.NewSubscriptions(pool), Providers: pushProviders,
			Publish: natsClient, BaseURL: cfg.WebhookBaseURL,
		})
		oauthSvc.UsePush(notifySvc)
		log.Info("calendar push enabled", "base_url", cfg.WebhookBaseURL)
	}
	if natsClient == nil {
		log.Info("delta sync disabled", "reason", "NATS_URL is unset; events stay on demand")
	} else {
		sources := map[string]syncsvc.DeltaSource{}
		if cfg.Google != nil {
			sources["google"] = syncsvc.NewGoogle(apiHTTP)
		}
		if cfg.Microsoft != nil {
			sources["microsoft"] = syncsvc.NewMicrosoft(apiHTTP)
		}
		syncSvc = syncsvc.New(syncsvc.Deps{
			Log: log, Accounts: accounts, Tokens: oauthSvc,
			Store: store.NewSyncStore(pool), Sources: sources, Publish: natsClient,
		})
		oauthSvc.UseSync(syncSvc)
		log.Info("delta sync enabled")
	}

	var (
		limiter ratelimit.Limiter = ratelimit.NewLocal()
		redis   *ratelimit.Redis
	)
	if cfg.RedisURL == "" {
		log.Warn("rate limit is per replica", "reason", "REDIS_URL is unset; with N replicas the limit is N times higher")
	} else {
		redis, err = ratelimit.OpenRedis(cfg.RedisURL)
		if err != nil {
			log.Error("redis config failed", "err", err)
			os.Exit(1)
		}
		limiter = ratelimit.NewFailover(log, redis, limiter)
		log.Info("rate limit shared through redis")
	}

	idempotency := store.NewIdempotency(pool)

	deps := httpapi.Deps{
		Log:            log,
		DB:             pool,
		Tenants:        store.NewTenants(pool),
		Env:            cfg.Env,
		ClientIPHeader: cfg.ClientIPHeader,
		Connect:        oauthSvc,
		Callback:       oauthSvc,
		Calendar:       calendarSvc,

		CalendarWrite: calendarSvc,
		Idempotency:   idempotency,

		Accounts:   accounts,
		Disconnect: oauthSvc,
		RateLimit: middleware.NewRateLimiter(log, limiter,
			ratelimit.Limit{PerMinute: cfg.RateLimitPerMinute, Burst: cfg.RateLimitBurst},
			ratelimit.Limit{PerMinute: cfg.RateLimitIPPerMinute, Burst: cfg.RateLimitIPBurst},
		),
	}
	if notifySvc != nil {
		deps.Webhooks = notifySvc
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           httpapi.NewRouter(deps),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Background jobs share the pool: cancel them and wait before pool.Close,
	// or their last query fails with "pool closed".
	bgCtx, bgCancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(3)
	if notifySvc != nil {
		wg.Add(2)
	}
	if syncSvc != nil {
		wg.Add(2)
	}
	go func() {
		defer wg.Done()
		janitor.Run(bgCtx, janitor.Config{
			Log:      log,
			Purger:   oauthStates,
			Interval: 10 * time.Minute,
			Batch:    1000,
		})
	}()
	go func() {
		defer wg.Done()
		janitor.Run(bgCtx, janitor.Config{
			Log:      log,
			Name:     "idempotency keys",
			Purger:   idempotency,
			Interval: 10 * time.Minute,
			Batch:    1000,
		})
	}()
	go func() {
		defer wg.Done()
		refresher.Run(bgCtx, refresher.Config{
			Log:         log,
			Accounts:    accounts,
			Refresh:     oauthSvc,
			Interval:    time.Minute,
			Ahead:       cfg.RefreshAhead,
			Concurrency: cfg.RefreshConcurrency,
		})
	}()
	if notifySvc != nil {
		go func() {
			defer wg.Done()
			notify.Run(bgCtx, notify.Config{
				Log: log, Claims: store.NewSubscriptions(pool), Maintain: notifySvc,
			})
		}()
		go func() {
			defer wg.Done()
			if err := natsClient.Consume(bgCtx, notify.SubjectLifecycle, notifySvc.HandleJob); err != nil && bgCtx.Err() == nil {
				log.Error("subscription lifecycle consumer stopped", "err", err)
			}
		}()
	}
	if syncSvc != nil {
		go func() {
			defer wg.Done()
			syncsvc.Run(bgCtx, syncsvc.ScheduleConfig{
				Log: log, Claims: store.NewSyncStore(pool), Publish: natsClient,
			})
		}()
		go func() {
			defer wg.Done()
			if err := natsClient.Consume(bgCtx, notify.SubjectSync, syncSvc.HandleJob); err != nil && bgCtx.Err() == nil {
				log.Error("calendar sync consumer stopped", "err", err)
			}
		}()
	}

	go func() {
		log.Info("starting relay api", "version", buildinfo.Version, "port", cfg.Port, "env", cfg.Env, "providers", cfg.Providers())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("forced shutdown", "err", err)
	}
	bgCancel()
	wg.Wait()
	if natsClient != nil {
		if err := natsClient.Drain(); err != nil {
			log.Error("nats drain failed", "err", err)
			natsClient.Close()
		}
	}
	if redis != nil {
		if err := redis.Close(); err != nil {
			log.Warn("redis close failed", "err", err)
		}
	}
	pool.Close()
	log.Info("bye")
}
