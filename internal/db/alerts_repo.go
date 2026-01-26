package db

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type AlertUpsert struct {
	Fingerprint string
	Alertname   string
	Status      string
	StartsAt    time.Time
	EndsAt      *time.Time
	Labels      map[string]string
	Annotations map[string]string
	Payload     any
	Meta        map[string]any
}

func UpsertAlert(ctx context.Context, db *pgxpool.Pool, a AlertUpsert) error {
	labelsJSON, _ := json.Marshal(a.Labels)
	annotationsJSON, _ := json.Marshal(a.Annotations)
	payloadJSON, _ := json.Marshal(a.Payload)
	metaJSON, _ := json.Marshal(a.Meta)

	_, err := db.Exec(ctx, `
INSERT INTO alert_events (fingerprint, status, payload)
VALUES ($1, $2, $3)
`, a.Fingerprint, a.Status, payloadJSON)
	if err != nil {
		return err
	}

	_, err = db.Exec(ctx, `
INSERT INTO active_alerts (
  fingerprint, alertname, status, starts_at, ends_at,
  labels, annotations, meta,
  first_seen, last_seen, updated_at
)
VALUES (
  $1, $2, $3, $4, $5,
  $6, $7, $8,
  now(), now(), now()
)
ON CONFLICT (fingerprint)
DO UPDATE SET
  status = EXCLUDED.status,
  ends_at = EXCLUDED.ends_at,
  meta = EXCLUDED.meta,
  last_seen = now(),
  updated_at = now()
`, a.Fingerprint, a.Alertname, a.Status, a.StartsAt, a.EndsAt,
	string(labelsJSON), string(annotationsJSON), string(metaJSON))

	return err
}
