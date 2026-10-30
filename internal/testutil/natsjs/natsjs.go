// Package natsjs starts an in-process JetStream server for tests.
// Production talks to NATS_URL; it does not embed a server.
package natsjs

import (
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// Start runs JetStream on 127.0.0.1 and returns a client URL.
func Start(t *testing.T) string {
	t.Helper()
	return StartServer(t).ClientURL()
}

// StartServer runs JetStream on 127.0.0.1. Tests that need to kill the
// broker (publish-while-down) shut this server down themselves; cleanup
// still runs Shutdown, which is safe to call twice.
func StartServer(t *testing.T) *server.Server {
	t.Helper()
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		NoLog:     true,
		NoSigs:    true,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	t.Cleanup(func() {
		s.Shutdown()
		s.WaitForShutdown()
	})
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	return s
}
