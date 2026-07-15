package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WebSession is a logged-in web UI session, created after a successful
// OIDC login (see internal/auth).
type WebSession struct {
	ID        string
	Subject   string
	Email     string
	Name      string
	CSRFToken string
	ExpiresAt time.Time
}

func CreateWebSession(ctx context.Context, pool *pgxpool.Pool, id, subject, email, name, csrfToken string, expiresAt time.Time) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO web_sessions (id, subject, email, name, csrf_token, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, subject, email, name, csrfToken, expiresAt)
	return err
}

// GetWebSession returns nil (no error) if the session doesn't exist or has
// expired, so callers can treat both the same way: not logged in.
func GetWebSession(ctx context.Context, pool *pgxpool.Pool, id string) (*WebSession, error) {
	var s WebSession
	err := pool.QueryRow(ctx, `
		SELECT id, subject, email, name, csrf_token, expires_at
		FROM web_sessions
		WHERE id = $1 AND expires_at > now()
	`, id).Scan(&s.ID, &s.Subject, &s.Email, &s.Name, &s.CSRFToken, &s.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func DeleteWebSession(ctx context.Context, pool *pgxpool.Pool, id string) error {
	_, err := pool.Exec(ctx, `DELETE FROM web_sessions WHERE id = $1`, id)
	return err
}

// CleanupExpiredSessions is called from the periodic DB cleanup pass.
func CleanupExpiredSessions(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	res, err := pool.Exec(ctx, `DELETE FROM web_sessions WHERE expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}
