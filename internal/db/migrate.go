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
		// into "members" instead of triggering an immediate Slack send. By
		// default each member ends up as its own Slack message; a member
		// only shares a message with others when a burst of them showed up
		// unsent at the same time (see the workflows package's bucketing
		// logic) - each member tracks its own notified_channel/notified_ts
		// inside its JSON so per-alert and batched messages are both just
		// "the set of members whose notified_ts matches".
		// A group row is deleted once every member has resolved and every
		// member's last notification reflects that, so this table's size
		// tracks "currently unresolved alert groups", not overall history.
		`
CREATE TABLE IF NOT EXISTS alert_groups (
  group_key        TEXT PRIMARY KEY,
  rule_name        TEXT NOT NULL,
  channel          TEXT NOT NULL,
  team             TEXT,

  members          JSONB NOT NULL DEFAULT '{}',

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
		// Upgrade path for anything already running the earlier version of
		// this table, which tracked one Slack message per whole group.
		`
ALTER TABLE alert_groups DROP COLUMN IF EXISTS slack_channel_id;
`,
		`
ALTER TABLE alert_groups DROP COLUMN IF EXISTS slack_ts;
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
		// extra_fields lets a rule pick specific annotations to surface as
		// their own Slack fields (e.g. "Email" <- annotation "email"),
		// instead of the per-alert card either omitting annotations or
		// dumping all of them wholesale - the latter is exactly what broke
		// alerts whose annotations include a large raw payload (like a full
		// email body): it got rendered as its own field and blew past
		// Slack's field/message size limits, pushing everything else out.
		`
ALTER TABLE workflow_rules ADD COLUMN IF NOT EXISTS extra_fields JSONB NOT NULL DEFAULT '[]';
`,
		// web_sessions backs the web UI's login state (see internal/auth).
		// Sessions are opaque server-side records referenced by a random
		// cookie value, rather than a self-contained signed token, so a
		// logout (or an admin wiping this table) actually revokes access
		// immediately instead of waiting out a token's lifetime.
		`
CREATE TABLE IF NOT EXISTS web_sessions (
  id          TEXT PRIMARY KEY,
  subject     TEXT NOT NULL,
  email       TEXT NOT NULL DEFAULT '',
  name        TEXT NOT NULL DEFAULT '',
  csrf_token  TEXT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at  TIMESTAMPTZ NOT NULL
);
`,
		`
CREATE INDEX IF NOT EXISTS idx_web_sessions_expires
ON web_sessions(expires_at);
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
