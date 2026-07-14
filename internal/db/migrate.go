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
		// Cleanup queries filter on last_seen; without this index DeleteOldAlerts
		// does a full table scan on every run.
		`
CREATE INDEX IF NOT EXISTS idx_active_alerts_last_seen
ON active_alerts(last_seen);
`,
		// "meta" used to hold per-alert Slack bookkeeping (channel/ts) for
		// the old one-message-per-alert send path. That state now lives in
		// alert_groups instead, so this column is dead weight - every
		// upsert was marshalling and writing an extra JSONB blob for
		// nothing. Drop it to shrink the table and cut write cost.
		`
ALTER TABLE active_alerts DROP COLUMN IF EXISTS meta;
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
		// Cleanup queries filter on received_at; same reasoning as above.
		`
CREATE INDEX IF NOT EXISTS idx_alert_events_received_at
ON alert_events(received_at);
`,
		`
CREATE TABLE IF NOT EXISTS providers (
    id          BIGSERIAL PRIMARY KEY,
    provider    TEXT NOT NULL,
    key         TEXT NOT NULL,
    value       TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
`,
		`
CREATE UNIQUE INDEX IF NOT EXISTS idx_providers_provider_key
ON providers(provider, key);
`,
		// alert_groups is the durable dedup/debounce queue: one row per
		// (rule, channel, group-by key). Incoming alerts upsert themselves
		// into "members" instead of triggering an immediate Slack send; a
		// background worker flushes rows once they're due, rebuilding the
		// whole Slack message from current member state every time so a
		// resolve never clobbers other alerts in the same message.
		// A group row is deleted once every member has resolved and that
		// final state has been flushed to Slack, so this table's size
		// tracks "currently unresolved alert groups", not overall history.
		`
CREATE TABLE IF NOT EXISTS alert_groups (
  group_key        TEXT PRIMARY KEY,
  rule_name        TEXT NOT NULL,
  channel          TEXT NOT NULL,
  team             TEXT,

  members          JSONB NOT NULL DEFAULT '{}',

  slack_channel_id TEXT,
  slack_ts         TEXT,

  dirty            BOOLEAN NOT NULL DEFAULT true,
  attempts         INT NOT NULL DEFAULT 0,
  last_error       TEXT,

  first_event_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  flush_after      TIMESTAMPTZ NOT NULL DEFAULT now(),
  max_flush_by     TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_flushed_at  TIMESTAMPTZ,
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
`,
		// The flush worker polls exactly this shape: unflushed, due rows.
		`
CREATE INDEX IF NOT EXISTS idx_alert_groups_due
ON alert_groups(flush_after) WHERE dirty;
`,
		// workflow_rules replaces the free-form YAML rules as the editable
		// source of truth. On first boot (empty table) it's seeded from the
		// workflows/*.yaml files so existing deployments keep working; from
		// then on it's managed from the web UI.
		`
CREATE TABLE IF NOT EXISTS workflow_rules (
  id            BIGSERIAL PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,

  match_labels  JSONB NOT NULL DEFAULT '{}',
  channel       TEXT NOT NULL,
  team          TEXT NOT NULL DEFAULT '',
  target_label  TEXT NOT NULL DEFAULT '',
  group_by      JSONB NOT NULL DEFAULT '[]',
  enrichments   JSONB NOT NULL DEFAULT '[]',

  enabled       BOOLEAN NOT NULL DEFAULT true,

  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
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
