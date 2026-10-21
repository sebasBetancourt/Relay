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
	"github.com/sebasBetancourt/relay/internal/janitor"
	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
	"github.com/sebasBetancourt/relay/internal/vault"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("relay", buildinfo.String())
		return
	}
	_ = godotenv.Load()

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

	deps := httpapi.Deps{
		Log:      log,
		DB:       pool,
		Tenants:  store.NewTenants(pool),
		Env:      cfg.Env,
		Connect:  oauthSvc,
		Callback: oauthSvc,
		Calendar: calendarSvc,

		Accounts:   accounts,
		Disconnect: oauthSvc,
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           httpapi.NewRouter(deps),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Janitor shares the pool: cancel it before pool.Close on shutdown.
	janitorCtx, janitorCancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		janitor.Run(janitorCtx, janitor.Config{
			Log:      log,
			Purger:   oauthStates,
			Interval: 10 * time.Minute,
			Batch:    1000,
		})
	}()

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
	janitorCancel()
	wg.Wait()
	pool.Close()
	log.Info("bye")
}
