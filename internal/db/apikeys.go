package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// APIKey is a named webhook credential. The raw key is never stored -
// only KeyHash (its SHA-256 hex digest) is, so a database leak doesn't
// hand out working credentials.
type APIKey struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

func CreateAPIKey(ctx context.Context, pool *pgxpool.Pool, id, name, keyHash string) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO api_keys (id, name, key_hash)
		VALUES ($1, $2, $3)
	`, id, name, keyHash)
	return err
}

func ListAPIKeys(ctx context.Context, pool *pgxpool.Pool) ([]APIKey, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, name, created_at FROM api_keys ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.CreatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// APIKeyExists reports whether keyHash matches a stored key, so the
// webhook can accept any issued API key alongside (or instead of) the
// single static BEARER_TOKEN.
func APIKeyExists(ctx context.Context, pool *pgxpool.Pool, keyHash string) (bool, error) {
	var id string
	err := pool.QueryRow(ctx, `SELECT id FROM api_keys WHERE key_hash = $1`, keyHash).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CountAPIKeys is used to decide whether the webhook should require auth
// even when BEARER_TOKEN is unset: once at least one key has been issued,
// the endpoint stops being open-by-default.
func CountAPIKeys(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM api_keys`).Scan(&n)
	return n, err
}

func DeleteAPIKey(ctx context.Context, pool *pgxpool.Pool, id string) error {
	_, err := pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, id)
	return err
}
