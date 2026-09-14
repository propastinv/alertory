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

// ActiveAlertFilter narrows ListActiveAlerts. The zero value ("", "", "")
// means "no filter" - every field is optional and additive (AND).
type ActiveAlertFilter struct {
	Status    string
	Alertname string // exact match, e.g. from the dashboard's alert-name selector
	// Search matches loosely against alertname, labels and annotations
	// (as text) so free-form terms like a site/host name that only shows
	// up in a label value still find the alert, not just an alertname
	// substring match.
	Search string
}

// ListActiveAlerts powers the web UI dashboard. Returns the page of rows
// plus the total row count matching the filter (pre-pagination), via a
// COUNT(*) OVER() window so it's one round trip instead of two.
func ListActiveAlerts(ctx context.Context, pool *pgxpool.Pool, f ActiveAlertFilter, limit, offset int) ([]ActiveAlertRow, int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	query := `
		SELECT fingerprint, alertname, status, labels, starts_at, last_seen,
		       count(*) OVER()
		FROM active_alerts
		WHERE ($1 = '' OR status = $1)
		  AND ($2 = '' OR alertname = $2)
		  AND ($3 = '' OR alertname ILIKE '%' || $3 || '%'
		            OR labels::text ILIKE '%' || $3 || '%'
		            OR annotations::text ILIKE '%' || $3 || '%')
		ORDER BY last_seen DESC
		LIMIT $4 OFFSET $5
	`

	rows, err := pool.Query(ctx, query, f.Status, f.Alertname, f.Search, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []ActiveAlertRow
	var total int
	for rows.Next() {
		var r ActiveAlertRow
		var labelsJSON *string
		if err := rows.Scan(&r.Fingerprint, &r.Alertname, &r.Status, &labelsJSON, &r.StartsAt, &r.LastSeen, &total); err != nil {
			return nil, 0, err
		}
		if labelsJSON != nil {
			_ = json.Unmarshal([]byte(*labelsJSON), &r.Labels)
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// ListDistinctAlertnames backs the dashboard's alert-name filter selector.
func ListDistinctAlertnames(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `SELECT DISTINCT alertname FROM active_alerts ORDER BY alertname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// DailyAlertCount is one point of the dashboard's alert history graph.
type DailyAlertCount struct {
	Day   time.Time
	Count int64
}

// CountFiringEventsByDay returns one row per day for the last `days` days
// (including today), with the count of alert_events that fired that day -
// zero-filled via generate_series so a quiet day still renders as a bar
// instead of a gap.
func CountFiringEventsByDay(ctx context.Context, pool *pgxpool.Pool, days int) ([]DailyAlertCount, error) {
	if days <= 0 {
		days = 7
	}

	rows, err := pool.Query(ctx, `
		SELECT d::date, COALESCE(count(e.id), 0)
		FROM generate_series(CURRENT_DATE - ($1::int - 1), CURRENT_DATE, interval '1 day') AS d
		LEFT JOIN alert_events e
		  ON e.received_at::date = d::date AND e.status = 'firing'
		GROUP BY d
		ORDER BY d
	`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DailyAlertCount
	for rows.Next() {
		var c DailyAlertCount
		if err := rows.Scan(&c.Day, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
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
