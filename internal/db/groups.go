package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MemberField is a resolved (title, value) pair ready to render as a
// Slack field, computed once at ingestion time from a rule's ExtraFields
// mapping (see workflows.ProcessAlert). Resolving it up front, rather
// than re-reading the rule's config at flush time, means rendering never
// depends on the rule still existing/unchanged later, and keeps the value
// length-capped at the point it's captured.
type MemberField struct {
	Title string `json:"title"`
	Value string `json:"value"`
}

// GroupMember is the state of a single alert inside an alert_groups row.
// NotifiedChannel/NotifiedTS identify which Slack message currently
// represents this alert - by default that's a message of its own; it only
// ends up sharing one with other members when they all became "unsent" at
// the same time in numbers above the mass-alert threshold. NotifiedStatus
// is the alert's Status as of the last successful send/update, used to
// tell whether its message is still up to date.
type GroupMember struct {
	Fingerprint string    `json:"fingerprint"`
	Alertname   string    `json:"alertname"`
	Status      string    `json:"status"` // "firing" or "resolved"
	Target      string    `json:"target,omitempty"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      *time.Time `json:"ends_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`

	// DisplayTitle is the rule's display-title setting rendered against
	// this specific alert's labels/annotations at ingestion time (see
	// workflows.renderDisplayTitle). Per-member rather than group-level
	// because a title template like "{{ .Labels.match }}" can produce a
	// different value for every alert in the same group. Empty means
	// "no override" - rendering falls back to the group's static title,
	// then to the alertname.
	DisplayTitle string `json:"display_title,omitempty"`

	// DisplayFields are pre-resolved (title, value) pairs from the rule's
	// ExtraFields mapping - not the alert's raw annotations. Only
	// explicitly mapped, length-capped values ever end up here, so a huge
	// annotation (like a full raw email body) never gets stored or
	// rendered just because it happened to be present.
	DisplayFields []MemberField `json:"display_fields,omitempty"`

	NotifiedChannel string `json:"notified_channel,omitempty"`
	NotifiedTS      string `json:"notified_ts,omitempty"`
	NotifiedStatus  string `json:"notified_status,omitempty"`
}

// AlertGroup is a durable queue of alerts sharing a rule/channel/group-by
// key. It's the unit the flush worker claims and processes; how many
// actual Slack messages that turns into is decided per-flush from the
// members' notified state (see workflows.buildBuckets).
type AlertGroup struct {
	GroupKey         string
	RuleName         string
	Channel          string
	Team             string
	DisplayTitle     string
	NotificationOnly bool
	Members          map[string]GroupMember
	Attempts         int
}

// GroupInfo is the rule-level data stamped onto a group. It's written at
// creation and refreshed from the rule on every later upsert (see
// UpsertGroupMember): groups with a stable group key (e.g. all forwarded
// emails sharing one alertname) can live across a rule edit, and the whole
// point of editing a rule is that its next message looks like the new
// config - a stale creation-time snapshot kept groups rendering the old
// layout indefinitely.
type GroupInfo struct {
	GroupKey         string
	RuleName         string
	Channel          string
	Team             string
	DisplayTitle     string
	NotificationOnly bool
}

func (g AlertGroup) AllResolved() bool {
	if len(g.Members) == 0 {
		return true
	}
	for _, m := range g.Members {
		if m.Status != "resolved" {
			return false
		}
	}
	return true
}

// UpsertGroupMember records an alert's current state inside its group and
// (re)arms the debounce timer: flush_after moves out by `debounce` on every
// new event, but never past first_event_at + maxWindow, so a continuous
// storm still gets flushed periodically instead of being delayed forever.
//
// The member's JSON is merged into the existing one with jsonb_set + ||
// rather than replaced outright: the incoming GroupMember never carries
// NotifiedChannel/NotifiedTS/NotifiedStatus (those are only ever set by
// the flush worker), and thanks to `omitempty` those keys are simply
// absent from its JSON - so merging preserves whatever notification
// bookkeeping already existed instead of wiping it out every time an
// alert's status changes.
func UpsertGroupMember(ctx context.Context, pool *pgxpool.Pool, info GroupInfo, member GroupMember, debounce, maxWindow time.Duration) error {
	memberJSON, err := json.Marshal(member)
	if err != nil {
		return err
	}

	debounceItv := fmt.Sprintf("%d seconds", int(debounce.Seconds()))
	maxItv := fmt.Sprintf("%d seconds", int(maxWindow.Seconds()))

	_, err = pool.Exec(ctx, `
		INSERT INTO alert_groups (
		  group_key, rule_name, channel, team, display_title, notification_only, members,
		  dirty, first_event_at, flush_after, max_flush_by, updated_at
		)
		VALUES (
		  $1, $2, $3, $4, $5, $6, jsonb_build_object($7::text, $8::jsonb),
		  true, now(), now() + $9::interval, now() + $10::interval, now()
		)
		ON CONFLICT (group_key) DO UPDATE SET
		  members = jsonb_set(
		    alert_groups.members,
		    ARRAY[$7::text],
		    COALESCE(alert_groups.members -> $7::text, '{}'::jsonb) || $8::jsonb,
		    true
		  ),
		  team              = EXCLUDED.team,
		  display_title     = EXCLUDED.display_title,
		  notification_only = EXCLUDED.notification_only,
		  dirty       = true,
		  attempts    = 0,
		  last_error  = NULL,
		  flush_after = LEAST(now() + $9::interval, alert_groups.max_flush_by),
		  updated_at  = now()
	`, info.GroupKey, info.RuleName, info.Channel, info.Team, info.DisplayTitle, info.NotificationOnly,
		member.Fingerprint, string(memberJSON), debounceItv, maxItv)

	return err
}

// ClaimDueGroups atomically claims up to `limit` groups that are due for a
// flush, using FOR UPDATE SKIP LOCKED so multiple app instances can run the
// flush worker concurrently without double-sending the same group. Claimed
// rows are marked not-dirty with flush_after pushed out by `lease`; if the
// caller crashes before finalizing, the row becomes due again once the
// lease expires instead of being stuck forever.
func ClaimDueGroups(ctx context.Context, pool *pgxpool.Pool, limit int, lease time.Duration) ([]AlertGroup, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT group_key, rule_name, channel, team, display_title, notification_only, members, attempts
		FROM alert_groups
		WHERE dirty AND flush_after <= now()
		ORDER BY flush_after
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, limit)
	if err != nil {
		return nil, err
	}

	var groups []AlertGroup
	var keys []string
	for rows.Next() {
		var g AlertGroup
		var membersJSON []byte

		if err := rows.Scan(&g.GroupKey, &g.RuleName, &g.Channel, &g.Team, &g.DisplayTitle, &g.NotificationOnly, &membersJSON, &g.Attempts); err != nil {
			rows.Close()
			return nil, err
		}
		g.Members = map[string]GroupMember{}
		_ = json.Unmarshal(membersJSON, &g.Members)
		groups = append(groups, g)
		keys = append(keys, g.GroupKey)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(keys) > 0 {
		_, err = tx.Exec(ctx, `
			UPDATE alert_groups
			SET dirty = false, flush_after = now() + $2::interval
			WHERE group_key = ANY($1)
		`, keys, fmt.Sprintf("%d seconds", int(lease.Seconds())))
		if err != nil {
			return nil, err
		}
	}

	return groups, tx.Commit(ctx)
}

// SaveGroupProgress persists each member's updated notification state
// after a successful flush pass. If every member is resolved and its
// message already reflects that, the group's job is done and the row is
// removed; otherwise it's saved clean (dirty=false) until something
// changes again.
func SaveGroupProgress(ctx context.Context, pool *pgxpool.Pool, groupKey string, members map[string]GroupMember, done bool) error {
	if done {
		_, err := pool.Exec(ctx, `DELETE FROM alert_groups WHERE group_key = $1`, groupKey)
		return err
	}

	membersJSON, err := json.Marshal(members)
	if err != nil {
		return err
	}

	_, err = pool.Exec(ctx, `
		UPDATE alert_groups
		SET members = $2, dirty = false, attempts = 0, last_error = NULL, last_flushed_at = now(), updated_at = now()
		WHERE group_key = $1
	`, groupKey, string(membersJSON))
	return err
}

// SaveGroupProgressFailed persists whatever succeeded before a failure
// (some buckets may have gone out fine) and re-arms the group for retry
// with capped exponential backoff, so a persistently failing send (e.g.
// bad token) doesn't hammer the Slack API or the flush worker every tick.
func SaveGroupProgressFailed(ctx context.Context, pool *pgxpool.Pool, groupKey string, members map[string]GroupMember, attempts int, sendErr error) error {
	membersJSON, err := json.Marshal(members)
	if err != nil {
		return err
	}

	backoff := time.Duration(attempts+1) * 10 * time.Second
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}

	_, err = pool.Exec(ctx, `
		UPDATE alert_groups
		SET members = $2, dirty = true, attempts = attempts + 1, last_error = $3, flush_after = now() + $4::interval, updated_at = now()
		WHERE group_key = $1
	`, groupKey, string(membersJSON), sendErr.Error(), fmt.Sprintf("%d seconds", int(backoff.Seconds())))
	return err
}

// CleanupResolvedGroups is a safety net for groups whose final flush never
// ran (e.g. the process was killed mid-flush): any group where every
// member is resolved and hasn't changed in a while gets removed so this
// table can't grow unbounded.
func CleanupResolvedGroups(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (int64, error) {
	res, err := pool.Exec(ctx, `
		DELETE FROM alert_groups
		WHERE updated_at < $1
		  AND NOT EXISTS (
		    SELECT 1 FROM jsonb_each(members) m
		    WHERE m.value ->> 'status' <> 'resolved'
		  )
	`, time.Now().Add(-olderThan))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

// CountOpenAlertGroups is used by the web UI dashboard.
func CountOpenAlertGroups(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM alert_groups`).Scan(&n)
	return n, err
}
