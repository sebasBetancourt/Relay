package main

import (
	"net/http"
	"os"

	"github.com/joho/godotenv"
	"github.com/sebasBetancourt/relay/internal/config"
	relayhttp "github.com/sebasBetancourt/relay/internal/http"
)

func main() {
	_ = godotenv.Load() 

	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	log := config.NewLogger(cfg.LogLevel)

	mux := relayhttp.NewRouter()
	// RequestID outermost so Logger/Recover see the id in context.
	handler := relayhttp.RequestID(relayhttp.Recover(log)(relayhttp.Logger(log)(mux)))

	log.Info("starting relay api", "port", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, handler); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}