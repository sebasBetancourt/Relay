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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound is returned when a lookup matches no row.
	ErrNotFound = errors.New("not found")
	// ErrRevoked is returned when an API key exists but was revoked.
	ErrRevoked = errors.New("api key revoked")
)

const (
	maxTenantNameLen = 200 // Matches the tenants_name_len CHECK (counts characters, not bytes).

	// tenantCacheTTL bounds how long another instance may keep serving a key
	// revoked elsewhere. RevokeKey clears the local entry immediately.
	tenantCacheTTL     = 30 * time.Second
	maxTenantCacheSize = 10_000

	pgForeignKeyViolation = "23503"
	pgInvalidTextRepr     = "22P02" // e.g. a tenant id that is not a uuid.
)

// Tenant is a Relay customer. It never carries the API key or its hash.
// KeyPrefix is the display prefix of the key used for the lookup (or the
// first key, on Create).
//
// RateLimitPerMinute overrides the global limit; nil means use the global one.
type Tenant struct {
	ID                 string
	Name               string
	KeyPrefix          string
	CreatedAt          time.Time
	RateLimitPerMinute *int
}

type cacheEntry struct {
	tenant    Tenant
	expiresAt time.Time
}

type Tenants struct {
	db *pgxpool.Pool

	mu    sync.RWMutex
	cache map[string]cacheEntry // key: api key hash; only active keys are cached.
}

func NewTenants(db *pgxpool.Pool) *Tenants {
	return &Tenants{db: db, cache: make(map[string]cacheEntry)}
}

// Create inserts a tenant and its first (already hashed) API key in one
// transaction, so a tenant never exists without a key.
func (s *Tenants) Create(ctx context.Context, name, apiKeyHash, keyPrefix string) (Tenant, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n == 0 || n > maxTenantNameLen {
		return Tenant{}, fmt.Errorf("create tenant: name must be 1-%d characters", maxTenantNameLen)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Tenant{}, fmt.Errorf("create tenant: begin: %w", err)
	}
	// No-op after a successful Commit.
	defer func() { _ = tx.Rollback(ctx) }()

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
		VALUES ($1, $2, $3)`,
		t.ID, apiKeyHash, keyPrefix,
	)
	if err != nil {
		return Tenant{}, fmt.Errorf("create tenant: insert key: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Tenant{}, fmt.Errorf("create tenant: commit: %w", err)
	}
	t.KeyPrefix = keyPrefix
	return t, nil
}

// AddKey attaches another (already hashed) API key to an existing tenant,
// e.g. to rotate keys without downtime. Unknown tenant → ErrNotFound.
func (s *Tenants) AddKey(ctx context.Context, tenantID, apiKeyHash, keyPrefix string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO api_keys (tenant_id, key_hash, key_prefix)
		VALUES ($1, $2, $3)`,
		tenantID, apiKeyHash, keyPrefix,
	)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == pgForeignKeyViolation || pgErr.Code == pgInvalidTextRepr) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("add api key: %w", err)
	}
	return nil
}

// RevokeKey marks a key as revoked. Idempotent: revoking twice keeps the
// first revoked_at. Unknown key → ErrNotFound.
func (s *Tenants) RevokeKey(ctx context.Context, apiKeyHash string) error {
	// Drop the cache entry first, so this instance stops trusting the key
	// even if the UPDATE fails halfway.
	s.cacheDelete(apiKeyHash)

	tag, err := s.db.Exec(ctx, `
		UPDATE api_keys
		SET revoked_at = COALESCE(revoked_at, now())
		WHERE key_hash = $1`,
		apiKeyHash,
	)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ByAPIKeyHash finds the tenant owning a key: ErrNotFound if the key does
// not exist, ErrRevoked if it was revoked. Active hits are cached for
// tenantCacheTTL to keep the DB off the hot path of every request.
func (s *Tenants) ByAPIKeyHash(ctx context.Context, apiKeyHash string) (Tenant, error) {
	if t, ok := s.cacheGet(apiKeyHash); ok {
		return t, nil
	}

	var (
		t         Tenant
		revokedAt *time.Time
	)
	err := s.db.QueryRow(ctx, `
		SELECT t.id::text, t.name, k.key_prefix, t.created_at, t.rate_limit_per_minute, k.revoked_at
		FROM api_keys k
		JOIN tenants t ON t.id = k.tenant_id
		WHERE k.key_hash = $1`,
		apiKeyHash,
	).Scan(&t.ID, &t.Name, &t.KeyPrefix, &t.CreatedAt, &t.RateLimitPerMinute, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("tenant by api key hash: %w", err)
	}
	if revokedAt != nil {
		return Tenant{}, ErrRevoked
	}

	s.cachePut(apiKeyHash, t)
	return t, nil
}

func (s *Tenants) cacheGet(hash string) (Tenant, bool) {
	s.mu.RLock()
	e, ok := s.cache[hash]
	s.mu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		return Tenant{}, false
	}
	return e.tenant, true
}

func (s *Tenants) cachePut(hash string, t Tenant) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	// Keep memory bounded: sweep expired entries, and if still full start over.
	if len(s.cache) >= maxTenantCacheSize {
		for h, e := range s.cache {
			if now.After(e.expiresAt) {
				delete(s.cache, h)
			}
		}
		if len(s.cache) >= maxTenantCacheSize {
			clear(s.cache)
		}
	}
	s.cache[hash] = cacheEntry{tenant: t, expiresAt: now.Add(tenantCacheTTL)}
}

func (s *Tenants) cacheDelete(hash string) {
	s.mu.Lock()
	delete(s.cache, hash)
	s.mu.Unlock()
}
