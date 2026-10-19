package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

// ErrRevoked is returned when the API key exists but was revoked.
var ErrRevoked = errors.New("api key revoked")

const (
	maxTenantNameLen = 200
	apiKeyCacheTTL   = 5 * time.Second
)

// Tenant is a Relay customer. It never carries API key material.
type Tenant struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

type Tenants struct {
	db *pgxpool.Pool

	mu    sync.Mutex
	cache map[string]tenantCacheEntry // key_hash → tenant
	ttl   time.Duration
}

type tenantCacheEntry struct {
	tenant  Tenant
	expires time.Time
}

func NewTenants(db *pgxpool.Pool) *Tenants {
	return &Tenants{
		db:    db,
		cache: make(map[string]tenantCacheEntry),
		ttl:   apiKeyCacheTTL,
	}
}

// Create inserts a tenant and its first API key in one transaction.
func (s *Tenants) Create(ctx context.Context, name, apiKeyHash, keyPrefix string) (Tenant, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxTenantNameLen {
		return Tenant{}, fmt.Errorf("create tenant: name must be 1-%d characters", maxTenantNameLen)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Tenant{}, fmt.Errorf("create tenant: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var t Tenant
	err = tx.QueryRow(ctx, `
		INSERT INTO tenants (name)
		VALUES ($1)
		RETURNING id::text, name, created_at`,
		name,
	).Scan(&t.ID, &t.Name, &t.CreatedAt)
	if err != nil {
		return Tenant{}, fmt.Errorf("create tenant: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO api_keys (tenant_id, key_hash, key_prefix)
		VALUES ($1::uuid, $2, $3)`,
		t.ID, apiKeyHash, keyPrefix,
	)
	if err != nil {
		return Tenant{}, fmt.Errorf("create tenant api key: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Tenant{}, fmt.Errorf("create tenant: commit: %w", err)
	}
	return t, nil
}

// AddKey attaches another API key to an existing tenant (rotation / multi-key).
func (s *Tenants) AddKey(ctx context.Context, tenantID, apiKeyHash, keyPrefix string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO api_keys (tenant_id, key_hash, key_prefix)
		VALUES ($1::uuid, $2, $3)`,
		tenantID, apiKeyHash, keyPrefix,
	)
	if err != nil {
		return fmt.Errorf("add api key: %w", err)
	}
	return nil
}

// ByAPIKeyHash finds the tenant for an active (non-revoked) key.
// Hot hashes are cached in memory for a few seconds to avoid hitting Postgres
// on every authenticated request.
func (s *Tenants) ByAPIKeyHash(ctx context.Context, apiKeyHash string) (Tenant, error) {
	if t, ok := s.cacheGet(apiKeyHash); ok {
		return t, nil
	}

	var t Tenant
	var revokedAt *time.Time
	err := s.db.QueryRow(ctx, `
		SELECT t.id::text, t.name, t.created_at, k.revoked_at
		FROM api_keys k
		JOIN tenants t ON t.id = k.tenant_id
		WHERE k.key_hash = $1`,
		apiKeyHash,
	).Scan(&t.ID, &t.Name, &t.CreatedAt, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("tenant by api key hash: %w", err)
	}
	if revokedAt != nil {
		return Tenant{}, ErrRevoked
	}

	// Best-effort; do not fail the request if the touch fails.
	_, _ = s.db.Exec(ctx, `
		UPDATE api_keys SET last_used_at = now() WHERE key_hash = $1 AND revoked_at IS NULL`,
		apiKeyHash,
	)

	s.cachePut(apiKeyHash, t)
	return t, nil
}

// RevokeKey marks a key unusable and drops it from the cache.
func (s *Tenants) RevokeKey(ctx context.Context, apiKeyHash string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE api_keys
		SET revoked_at = now()
		WHERE key_hash = $1 AND revoked_at IS NULL`,
		apiKeyHash,
	)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	s.cacheDelete(apiKeyHash)
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Tenants) cacheGet(hash string) (Tenant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[hash]
	if !ok || time.Now().After(e.expires) {
		if ok {
			delete(s.cache, hash)
		}
		return Tenant{}, false
	}
	return e.tenant, true
}

func (s *Tenants) cachePut(hash string, t Tenant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[hash] = tenantCacheEntry{tenant: t, expires: time.Now().Add(s.ttl)}
}

func (s *Tenants) cacheDelete(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, hash)
}
