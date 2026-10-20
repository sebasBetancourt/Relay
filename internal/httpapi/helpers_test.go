package httpapi

import (
	"context"
	"io"
	"log/slog"

	"github.com/sebasBetancourt/relay/internal/store"
)

// fakeLookup stands in for the store: known hashes return a tenant,
// revoked hashes ErrRevoked, err (if set) fails every call.
type fakeLookup struct {
	tenants map[string]store.Tenant
	revoked map[string]bool
	err     error
	calls   int
}

func (f *fakeLookup) ByAPIKeyHash(_ context.Context, hash string) (store.Tenant, error) {
	f.calls++
	if f.err != nil {
		return store.Tenant{}, f.err
	}
	if f.revoked[hash] {
		return store.Tenant{}, store.ErrRevoked
	}
	t, ok := f.tenants[hash]
	if !ok {
		return store.Tenant{}, store.ErrNotFound
	}
	return t, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
