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
  starts_at = EXCLUDED.starts_at,
  ends_at = EXCLUDED.ends_at,
  meta = EXCLUDED.meta,
  last_seen = now(),
  updated_at = now()
`, a.Fingerprint, a.Alertname, a.Status, a.StartsAt, a.EndsAt,
	string(labelsJSON), string(annotationsJSON), string(metaJSON))

	return err
}

func GetActiveAlertMeta(ctx context.Context, db *pgxpool.Pool, fingerprint string) (map[string]any, error) {
	var metaJSON *string

	err := db.QueryRow(ctx, `
		SELECT meta
		FROM active_alerts
		WHERE fingerprint = $1
	`, fingerprint).Scan(&metaJSON)

	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, err
	}

	if metaJSON == nil || *metaJSON == "" {
		return nil, nil
	}

	var meta map[string]any
	if err := json.Unmarshal([]byte(*metaJSON), &meta); err != nil {
		return nil, err
	}

	return meta, nil
}
func IsNewAlert(ctx context.Context, db *pgxpool.Pool, fingerprint string, startsAt time.Time) (bool, error) {
	var dbStartsAt *time.Time

	err := db.QueryRow(ctx, `
		SELECT starts_at
		FROM active_alerts
		WHERE fingerprint = $1
	`, fingerprint).Scan(&dbStartsAt)

	if err != nil {
		if err.Error() == "no rows in result set" {
			return true, nil
		}
		return false, err
	}

	if dbStartsAt == nil {
		return true, nil
	}

	return !dbStartsAt.Equal(startsAt), nil
}
func GetAlert(ctx context.Context, db *pgxpool.Pool, fingerprint string) (*AlertUpsert, error) {
	var alertname, status string
	var startsAt time.Time
	var endsAt *time.Time
	var labelsJSON, annotationsJSON *string

	err := db.QueryRow(ctx, `
		SELECT alertname, status, starts_at, ends_at, labels, annotations
		FROM active_alerts
		WHERE fingerprint = $1
	`, fingerprint).Scan(&alertname, &status, &startsAt, &endsAt, &labelsJSON, &annotationsJSON)

	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, err
	}

	var labels map[string]string
	var annotations map[string]string

	if labelsJSON != nil {
		json.Unmarshal([]byte(*labelsJSON), &labels)
	}
	if annotationsJSON != nil {
		json.Unmarshal([]byte(*annotationsJSON), &annotations)
	}

	return &AlertUpsert{
		Fingerprint: fingerprint,
		Alertname:   alertname,
		Status:      status,
		StartsAt:    startsAt,
		EndsAt:      endsAt,
		Labels:      labels,
		Annotations: annotations,
	}, nil
}

func DeleteOldAlerts(ctx context.Context, db *pgxpool.Pool, olderThan time.Duration) (int64, int64, error) {
	cutoff := time.Now().Add(-olderThan)

	res1, err := db.Exec(ctx, `DELETE FROM active_alerts WHERE last_seen < $1`, cutoff)
	if err != nil {
		return 0, 0, err
	}

	res2, err := db.Exec(ctx, `DELETE FROM alert_events WHERE received_at < $1`, cutoff)
	if err != nil {
		return res1.RowsAffected(), 0, err
	}

	return res1.RowsAffected(), res2.RowsAffected(), nil
}

func GetAnnotations(ctx context.Context, db *pgxpool.Pool, fingerprint string) (map[string]string, error) {
	var annotationsJSON *string

	err := db.QueryRow(ctx, `
		SELECT annotations
		FROM active_alerts
		WHERE fingerprint = $1
	`, fingerprint).Scan(&annotationsJSON)

	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, err
	}

	if annotationsJSON == nil {
		return nil, nil
	}

	var annotations map[string]string
	if err := json.Unmarshal([]byte(*annotationsJSON), &annotations); err != nil {
		return nil, err
	}

	return annotations, nil
}
