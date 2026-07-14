package db

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Connect(dsn string) *pgxpool.Pool {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		log.Fatalf("parse dsn: %v", err)
	}

	// Configurable so this can be tuned for load without a code change;
	// defaults are a modest bump over the old hardcoded 15/2 to give the
	// flush worker and web UI headroom alongside webhook ingestion.
	cfg.MaxConns = envInt32("DB_MAX_CONNS", 25)
	cfg.MinConns = envInt32("DB_MIN_CONNS", 4)
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}

	return pool
}

func envInt32(key string, def int32) int32 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("invalid %s=%q, using default %d: %v", key, v, def, err)
		return def
	}
	return int32(n)
}
