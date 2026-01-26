package db

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

func AutoMigrate(ctx context.Context, db *pgxpool.Pool) {
	stmts := []string{
		`
CREATE TABLE IF NOT EXISTS active_alerts (
  id           BIGSERIAL PRIMARY KEY,

  fingerprint  TEXT NOT NULL UNIQUE,
  alertname    TEXT NOT NULL,
  status       TEXT NOT NULL,

  starts_at    TIMESTAMPTZ,
  ends_at      TIMESTAMPTZ,

  labels       JSONB,
  annotations  JSONB,

  meta         JSONB,

  first_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
`,
		`
CREATE INDEX IF NOT EXISTS idx_active_alerts_status 
ON active_alerts(status);
`,
		`
CREATE INDEX IF NOT EXISTS idx_active_alerts_alertname 
ON active_alerts(alertname);
`,
		`
CREATE TABLE IF NOT EXISTS alert_events (
  id           BIGSERIAL PRIMARY KEY,
  fingerprint  TEXT NOT NULL,
  status       TEXT NOT NULL,
  received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  payload      JSONB NOT NULL
);
`,
		`
CREATE INDEX IF NOT EXISTS idx_alert_events_fingerprint 
ON alert_events(fingerprint);
`,
	}

	for _, stmt := range stmts {
		_, err := db.Exec(ctx, stmt)
		if err != nil {
			log.Fatalf("auto migrate failed: %v\nSQL: %s", err, stmt)
		}
	}

	log.Println("DB auto-migrate complete")
}
