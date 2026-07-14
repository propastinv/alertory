package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GroupMember is the state of a single alert inside an alert_groups row.
type GroupMember struct {
	Fingerprint string            `json:"fingerprint"`
	Alertname   string            `json:"alertname"`
	Status      string            `json:"status"` // "firing" or "resolved"
	Target      string            `json:"target,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	StartsAt    time.Time         `json:"starts_at"`
	EndsAt      *time.Time        `json:"ends_at,omitempty"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// AlertGroup is a durable, debounced batch of alerts that map to a single
// Slack message. Alerts join a group by upserting themselves as a member;
// a background worker (see workflows.RunFlushWorker) periodically claims
// due groups and renders/sends the message for the group as a whole.
type AlertGroup struct {
	GroupKey       string
	RuleName       string
	Channel        string
	Team           string
	Members        map[string]GroupMember
	SlackChannelID string
	SlackTS        string
	Attempts       int
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
func UpsertGroupMember(ctx context.Context, pool *pgxpool.Pool, groupKey, ruleName, channel, team string, member GroupMember, debounce, maxWindow time.Duration) error {
	memberJSON, err := json.Marshal(member)
	if err != nil {
		return err
	}

	debounceItv := fmt.Sprintf("%d seconds", int(debounce.Seconds()))
	maxItv := fmt.Sprintf("%d seconds", int(maxWindow.Seconds()))

	_, err = pool.Exec(ctx, `
		INSERT INTO alert_groups (
		  group_key, rule_name, channel, team, members,
		  dirty, first_event_at, flush_after, max_flush_by, updated_at
		)
		VALUES (
		  $1, $2, $3, $4, jsonb_build_object($5::text, $6::jsonb),
		  true, now(), now() + $7::interval, now() + $8::interval, now()
		)
		ON CONFLICT (group_key) DO UPDATE SET
		  members     = alert_groups.members || jsonb_build_object($5::text, $6::jsonb),
		  dirty       = true,
		  attempts    = 0,
		  last_error  = NULL,
		  flush_after = LEAST(now() + $7::interval, alert_groups.max_flush_by),
		  updated_at  = now()
	`, groupKey, ruleName, channel, team, member.Fingerprint, string(memberJSON), debounceItv, maxItv)

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
		SELECT group_key, rule_name, channel, team, members, slack_channel_id, slack_ts, attempts
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
		var slackChannelID, slackTS *string

		if err := rows.Scan(&g.GroupKey, &g.RuleName, &g.Channel, &g.Team, &membersJSON, &slackChannelID, &slackTS, &g.Attempts); err != nil {
			rows.Close()
			return nil, err
		}
		g.Members = map[string]GroupMember{}
		_ = json.Unmarshal(membersJSON, &g.Members)
		if slackChannelID != nil {
			g.SlackChannelID = *slackChannelID
		}
		if slackTS != nil {
			g.SlackTS = *slackTS
		}
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

// FinalizeGroupSent records where the (re)rendered message ended up. If
// every member has resolved, the group's job is done and the row is
// removed; otherwise it's left clean until new members arrive.
func FinalizeGroupSent(ctx context.Context, pool *pgxpool.Pool, groupKey, slackChannelID, slackTS string, fullyResolved bool) error {
	if fullyResolved {
		_, err := pool.Exec(ctx, `DELETE FROM alert_groups WHERE group_key = $1`, groupKey)
		return err
	}

	_, err := pool.Exec(ctx, `
		UPDATE alert_groups
		SET slack_channel_id = $2, slack_ts = $3, last_flushed_at = now(), attempts = 0, last_error = NULL
		WHERE group_key = $1
	`, groupKey, slackChannelID, slackTS)
	return err
}

// FinalizeGroupFailed re-arms a group for retry with capped exponential
// backoff so a persistently failing send (e.g. bad token) doesn't hammer
// the Slack API or the flush worker every tick.
func FinalizeGroupFailed(ctx context.Context, pool *pgxpool.Pool, groupKey string, attempts int, sendErr error) error {
	backoff := time.Duration(attempts+1) * 10 * time.Second
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}

	_, err := pool.Exec(ctx, `
		UPDATE alert_groups
		SET dirty = true, attempts = attempts + 1, last_error = $2, flush_after = now() + $3::interval
		WHERE group_key = $1
	`, groupKey, sendErr.Error(), fmt.Sprintf("%d seconds", int(backoff.Seconds())))
	return err
}

// CleanupResolvedGroups is a safety net for groups whose final flush never
// ran (e.g. the process was killed between claiming and finalizing): any
// group where every member is resolved and hasn't changed in a while gets
// removed so this table can't grow unbounded.
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
