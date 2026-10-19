// Command relayctl is the admin CLI for Relay.
//
//	relayctl tenant create --name "Acme"
//
// It only needs DATABASE_URL and RELAY_ENV (not the Google/encryption vars
// the API requires). The new API key is the only thing written to stdout,
// so it can be piped or redirected safely; everything else goes to stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/config"
	"github.com/sebasBetancourt/relay/internal/store"
)

// Exit codes: 2 = bad usage (same as the flag package), 1 = runtime failure.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const usage = `Usage:
  relayctl tenant create --name <name>

Environment:
  DATABASE_URL  Postgres connection string
  RELAY_ENV     live | test (decides rly_live_ or rly_test_ keys)
`

func main() {
	_ = godotenv.Load()
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

// run holds all the logic so tests can call it with fake args, env and
// output instead of spawning a process.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) < 2 || args[0] != "tenant" || args[1] != "create" {
		say(stderr, usage)
		return exitUsage
	}
	return tenantCreate(ctx, args[2:], stdout, stderr, getenv)
}

func tenantCreate(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("relayctl tenant create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "tenant name (1-200 characters)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if strings.TrimSpace(*name) == "" || fs.NArg() > 0 {
		say(stderr, usage)
		return exitUsage
	}

	// Validate the env before touching the DB, reporting every problem at once.
	var errs []error
	dbURL := getenv("DATABASE_URL")
	if dbURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	env, err := config.ParseEnv(getenv("RELAY_ENV"))
	if err != nil {
		errs = append(errs, fmt.Errorf("RELAY_ENV: %w", err))
	}
	if len(errs) > 0 {
		say(stderr, "invalid config: %v\n", errors.Join(errs...))
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	pool, err := store.NewPool(ctx, dbURL)
	if err != nil {
		say(stderr, "db connect failed: %v\n", err)
		return exitError
	}
	defer pool.Close()

	key := apikey.Generate(env)
	tenant, err := store.NewTenants(pool).Create(ctx, *name, key.Hash, key.KeyPrefix)
	if err != nil {
		say(stderr, "%v\n", err)
		return exitError
	}

	say(stderr, "Tenant created\n  id:     %s\n  name:   %s\n  prefix: %s\n  env:    %s\n\n",
		tenant.ID, tenant.Name, tenant.KeyPrefix, env)
	say(stderr, "API key below. Save it now: it will NOT be shown again (only its hash is stored).\n")

	// The key exists only in memory now. If writing it fails (closed pipe,
	// full disk) it is lost for good, so fail loudly instead of exiting 0.
	if _, err := fmt.Fprintln(stdout, key.Plaintext); err != nil {
		say(stderr, "could not write the API key (%v); tenant %s has a key nobody knows: delete it and run again\n",
			err, tenant.ID)
		return exitError
	}
	return exitOK
}

// say writes an informational message. Failing to write to stderr leaves
// nowhere to report it, so the error is deliberately ignored.
func say(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}
