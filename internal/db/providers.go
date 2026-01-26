package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func UpsertProviderSetting(pool *pgxpool.Pool, provider, key, value string) error {
	_, err := pool.Exec(context.Background(), `
	INSERT INTO providers (provider, key, value, updated_at)
	VALUES ($1, $2, $3, now())
	ON CONFLICT (provider, key)
	DO UPDATE SET value = EXCLUDED.value, updated_at = now()
	`, provider, key, value)
	return err
}

func GetProviderSetting(pool *pgxpool.Pool, provider, key string) string {
	var value string
	err := pool.QueryRow(context.Background(),
		`SELECT value FROM providers WHERE provider=$1 AND key=$2`,
		provider, key).Scan(&value)
	if err != nil {
		return ""
	}
	return value
}
