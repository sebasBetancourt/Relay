package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrIdempotencyMismatch: the key was already used with another request.
	ErrIdempotencyMismatch = errors.New("idempotency key reused with another request")
	// ErrIdempotencyInProgress: the first request with this key is still running.
	ErrIdempotencyInProgress = errors.New("idempotency key in progress")
)

// StoredResponse is a finished request's answer, replayed on a retry.
type StoredResponse struct {
	Status int
	Body   []byte // nil for an empty response
}

// Idempotency remembers finished writes per tenant and key.
type Idempotency struct {
	db *pgxpool.Pool
}

func NewIdempotency(db *pgxpool.Pool) *Idempotency {
	return &Idempotency{db: db}
}

// Begin claims key for this request. It returns nil when the caller should run
// the request, and a response when it already ran. The insert decides between
// concurrent requests, so exactly one of them gets to run.
//
// A claim older than a minute is taken over: every request ends well before
// that, so it belongs to a process that died. Taking it over can repeat a write
// the dead process finished, which is better than blocking the key for a day.
func (s *Idempotency) Begin(ctx context.Context, tenantID, key, requestHash string) (*StoredResponse, error) {
	var claimed bool
	err := s.db.QueryRow(ctx, `
		INSERT INTO idempotency_keys (tenant_id, key, request_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, key) DO UPDATE SET locked_at = now()
		WHERE idempotency_keys.status IS NULL
		  AND idempotency_keys.request_hash = EXCLUDED.request_hash
		  AND idempotency_keys.locked_at < now() - interval '1 minute'
		RETURNING true`,
		tenantID, key, requestHash,
	).Scan(&claimed)
	if err == nil {
		return nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("idempotency begin: %w", err)
	}

	var (
		hash   string
		status *int
		body   []byte
	)
	err = s.db.QueryRow(ctx, `
		SELECT request_hash, status, response
		FROM idempotency_keys
		WHERE tenant_id = $1 AND key = $2`,
		tenantID, key,
	).Scan(&hash, &status, &body)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Released between the two statements: the caller can retry.
		return nil, ErrIdempotencyInProgress
	case err != nil:
		return nil, fmt.Errorf("idempotency read: %w", err)
	case hash != requestHash:
		return nil, ErrIdempotencyMismatch
	case status == nil:
		return nil, ErrIdempotencyInProgress
	}
	return &StoredResponse{Status: *status, Body: body}, nil
}

// Complete stores the answer of a claimed request.
func (s *Idempotency) Complete(ctx context.Context, tenantID, key string, res StoredResponse) error {
	var body any
	if res.Body != nil {
		body = json.RawMessage(res.Body)
	}
	_, err := s.db.Exec(ctx, `
		UPDATE idempotency_keys SET status = $3, response = $4
		WHERE tenant_id = $1 AND key = $2`,
		tenantID, key, res.Status, body,
	)
	if err != nil {
		return fmt.Errorf("idempotency complete: %w", err)
	}
	return nil
}

// Release forgets a claim whose request failed, so the client can retry it.
func (s *Idempotency) Release(ctx context.Context, tenantID, key string) error {
	_, err := s.db.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE tenant_id = $1 AND key = $2 AND status IS NULL`,
		tenantID, key,
	)
	if err != nil {
		return fmt.Errorf("idempotency release: %w", err)
	}
	return nil
}

// DeleteExpired removes up to limit rows older than a day, with SKIP LOCKED
// like the oauth states purge.
func (s *Idempotency) DeleteExpired(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	tag, err := s.db.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE (tenant_id, key) IN (
			SELECT tenant_id, key FROM idempotency_keys
			WHERE created_at < now() - interval '24 hours'
			ORDER BY created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)`, limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired idempotency keys: %w", err)
	}
	return tag.RowsAffected(), nil
}
