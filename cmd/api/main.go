package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/sebasBetancourt/relay/internal/config"
	"github.com/sebasBetancourt/relay/internal/connect"
	relayhttp "github.com/sebasBetancourt/relay/internal/http"
	"github.com/sebasBetancourt/relay/internal/store"
	"github.com/sebasBetancourt/relay/internal/vault"
)

func main() {
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log := config.NewLogger(cfg.LogLevel)

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
	defer pool.Close()

	oauthHTTP := &http.Client{Timeout: 10 * time.Second}
	connectSvc := connect.NewService(connect.Deps{
		States:   store.NewOAuthStates(pool),
		Accounts: store.NewAccounts(pool),
		Vault:    vlt,
		Providers: map[connect.Provider]connect.ProviderSpec{
			connect.ProviderGoogle: connect.GoogleSpec(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRedirectURL),
		},
		HTTPClient: oauthHTTP,
	})

	deps := relayhttp.Deps{
		Log:      log,
		DB:       pool,
		Tenants:  store.NewTenants(pool),
		Env:      cfg.Env,
		Connect:  connectSvc,
		Callback: connectSvc,
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           relayhttp.NewRouter(deps),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Info("starting relay api", "port", cfg.Port, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("forced shutdown", "err", err)
	}
	log.Info("bye")
}
