package db

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
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
}

// BatchGetActiveAlerts fetches the last known starts_at for many
// fingerprints in one round trip, used to decide whether an incoming
// alert is new/changed. This replaces two sequential queries per alert
// (GetActiveAlertMeta + IsNewAlert), which turned a single webhook call
// carrying hundreds of alerts into hundreds of blocking DB round trips.
func BatchGetActiveAlerts(ctx context.Context, pool *pgxpool.Pool, fingerprints []string) (map[string]time.Time, error) {
	result := make(map[string]time.Time, len(fingerprints))
	if len(fingerprints) == 0 {
		return result, nil
	}

	rows, err := pool.Query(ctx, `
		SELECT fingerprint, starts_at
		FROM active_alerts
		WHERE fingerprint = ANY($1)
	`, fingerprints)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var fp string
		var startsAt time.Time
		if err := rows.Scan(&fp, &startsAt); err != nil {
			return nil, err
		}
		result[fp] = startsAt
	}
	return result, rows.Err()
}

// BatchUpsertAlerts pipelines every alert_events insert and active_alerts
// upsert for a webhook call into a single round trip via pgx.Batch,
// replacing what used to be 2*N sequential Exec calls.
func BatchUpsertAlerts(ctx context.Context, pool *pgxpool.Pool, upserts []AlertUpsert) error {
	if len(upserts) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, a := range upserts {
		labelsJSON, _ := json.Marshal(a.Labels)
		annotationsJSON, _ := json.Marshal(a.Annotations)
		payloadJSON, _ := json.Marshal(a.Payload)

		batch.Queue(`
			INSERT INTO alert_events (fingerprint, status, payload)
			VALUES ($1, $2, $3)
		`, a.Fingerprint, a.Status, string(payloadJSON))

		batch.Queue(`
			INSERT INTO active_alerts (
			  fingerprint, alertname, status, starts_at, ends_at,
			  labels, annotations,
			  first_seen, last_seen, updated_at
			)
			VALUES (
			  $1, $2, $3, $4, $5,
			  $6, $7,
			  now(), now(), now()
			)
			ON CONFLICT (fingerprint)
			DO UPDATE SET
			  status = EXCLUDED.status,
			  starts_at = EXCLUDED.starts_at,
			  ends_at = EXCLUDED.ends_at,
			  last_seen = now(),
			  updated_at = now()
		`, a.Fingerprint, a.Alertname, a.Status, a.StartsAt, a.EndsAt,
			string(labelsJSON), string(annotationsJSON))
	}

	br := pool.SendBatch(ctx, batch)
	defer br.Close()

	for i := 0; i < batch.Len(); i++ {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

// ActiveAlertRow is a row of the dashboard's active alerts table.
type ActiveAlertRow struct {
	Fingerprint string
	Alertname   string
	Status      string
	Labels      map[string]string
	StartsAt    time.Time
	LastSeen    time.Time
}

// ListActiveAlerts powers the web UI dashboard. statusFilter == "" means
// no filter.
func ListActiveAlerts(ctx context.Context, pool *pgxpool.Pool, statusFilter string, limit int) ([]ActiveAlertRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}

	query := `
		SELECT fingerprint, alertname, status, labels, starts_at, last_seen
		FROM active_alerts
		WHERE ($1 = '' OR status = $1)
		ORDER BY last_seen DESC
		LIMIT $2
	`

	rows, err := pool.Query(ctx, query, statusFilter, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ActiveAlertRow
	for rows.Next() {
		var r ActiveAlertRow
		var labelsJSON *string
		if err := rows.Scan(&r.Fingerprint, &r.Alertname, &r.Status, &labelsJSON, &r.StartsAt, &r.LastSeen); err != nil {
			return nil, err
		}
		if labelsJSON != nil {
			_ = json.Unmarshal([]byte(*labelsJSON), &r.Labels)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteOldAlerts trims both alert tables, deleting in small batches so a
// large backlog can't hold a single long-running transaction/lock on a
// busy table.
func DeleteOldAlerts(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (int64, int64, error) {
	cutoff := time.Now().Add(-olderThan)

	activeDeleted, err := deleteInBatches(ctx, pool, `
		DELETE FROM active_alerts
		WHERE ctid IN (
		  SELECT ctid FROM active_alerts WHERE last_seen < $1 LIMIT 5000
		)
	`, cutoff)
	if err != nil {
		return activeDeleted, 0, err
	}

	eventsDeleted, err := deleteInBatches(ctx, pool, `
		DELETE FROM alert_events
		WHERE ctid IN (
		  SELECT ctid FROM alert_events WHERE received_at < $1 LIMIT 5000
		)
	`, cutoff)
	if err != nil {
		return activeDeleted, eventsDeleted, err
	}

	return activeDeleted, eventsDeleted, nil
}

// VacuumAnalyze is run after a cleanup pass removes a meaningful number of
// rows. This service is insert/delete-heavy (every alert writes an event
// row and every cleanup pass deletes a batch), which is exactly the
// workload that makes tables bloat between scheduled autovacuum runs.
func VacuumAnalyze(ctx context.Context, pool *pgxpool.Pool) error {
	for _, table := range []string{"active_alerts", "alert_events", "alert_groups", "web_sessions"} {
		if _, err := pool.Exec(ctx, "VACUUM (ANALYZE) "+table); err != nil {
			return err
		}
	}
	return nil
}

func deleteInBatches(ctx context.Context, pool *pgxpool.Pool, query string, cutoff time.Time) (int64, error) {
	var total int64
	for {
		res, err := pool.Exec(ctx, query, cutoff)
		if err != nil {
			return total, err
		}
		n := res.RowsAffected()
		total += n
		if n == 0 {
			return total, nil
		}
	}
}
