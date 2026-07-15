package db

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnrichmentParam is a single request parameter for an HTTPEnrichment.
type EnrichmentParam struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Enrichment describes an HTTP call made to enrich a firing alert before
// it's rendered into a Slack message (e.g. looking up affected user count).
type Enrichment struct {
	Name          string            `json:"name"`
	URL           string            `json:"url"`
	Method        string            `json:"method,omitempty"`
	Params        []EnrichmentParam `json:"params,omitempty"`
	ResponseField string            `json:"response_field,omitempty"`
	StoreIn       string            `json:"store_in,omitempty"`
}

// RuleField maps one alert annotation onto a named Slack field, e.g.
// Title "Email" <- annotation key "email". Only annotations explicitly
// listed here are shown on the per-alert card; anything else (like a raw
// email body dumped into an annotation) stays out of Slack entirely.
type RuleField struct {
	Title         string `json:"title"`
	AnnotationKey string `json:"annotation_key"`
}

// WorkflowRule is the DB-backed replacement for the old free-form YAML
// rules. The Slack layout itself is now fixed in code (see workflows
// package); a rule only supplies the structured bits: which alerts it
// matches, where they go, whose team owns them, which label identifies the
// affected target, which annotations to surface as extra fields, how to
// collapse mass firings into one message, and any enrichment calls to run
// first.
type WorkflowRule struct {
	ID          int64
	Name        string
	MatchLabels map[string]string
	Channel     string
	Team        string
	TargetLabel string
	GroupBy     []string
	ExtraFields []RuleField
	Enrichments []Enrichment
	Enabled     bool

	// DisplayTitle, if set, replaces the alertname label as the Slack
	// message header. Meant for sources that aren't really "alerts" (e.g.
	// forwarded emails) and don't send a meaningful alertname.
	DisplayTitle string
	// NotificationOnly marks a rule as a one-shot notification rather than
	// a stateful alert: incoming status is ignored, there's no "resolved"
	// transition, and the message never flips color or shows a resolved
	// time.
	NotificationOnly bool
}

// Matches reports whether the given alert labels satisfy every label
// constraint on this rule.
func (r WorkflowRule) Matches(labels map[string]string) bool {
	for k, v := range r.MatchLabels {
		if val, ok := labels[k]; !ok || val != v {
			return false
		}
	}
	return true
}

const ruleColumns = `id, name, match_labels, channel, team, target_label, group_by, extra_fields, enrichments, enabled, display_title, notification_only`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanWorkflowRule(row rowScanner) (WorkflowRule, error) {
	var r WorkflowRule
	var matchLabelsJSON, groupByJSON, extraFieldsJSON, enrichmentsJSON []byte

	err := row.Scan(&r.ID, &r.Name, &matchLabelsJSON, &r.Channel, &r.Team, &r.TargetLabel, &groupByJSON, &extraFieldsJSON, &enrichmentsJSON, &r.Enabled, &r.DisplayTitle, &r.NotificationOnly)
	if err != nil {
		return r, err
	}

	_ = json.Unmarshal(matchLabelsJSON, &r.MatchLabels)
	_ = json.Unmarshal(groupByJSON, &r.GroupBy)
	_ = json.Unmarshal(extraFieldsJSON, &r.ExtraFields)
	_ = json.Unmarshal(enrichmentsJSON, &r.Enrichments)

	return r, nil
}

func ListWorkflowRules(ctx context.Context, pool *pgxpool.Pool) ([]WorkflowRule, error) {
	rows, err := pool.Query(ctx, `SELECT `+ruleColumns+` FROM workflow_rules ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []WorkflowRule
	for rows.Next() {
		r, err := scanWorkflowRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// ListEnabledWorkflowRules returns only rules that should currently be
// evaluated against incoming alerts.
func ListEnabledWorkflowRules(ctx context.Context, pool *pgxpool.Pool) ([]WorkflowRule, error) {
	rows, err := pool.Query(ctx, `SELECT `+ruleColumns+` FROM workflow_rules WHERE enabled ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []WorkflowRule
	for rows.Next() {
		r, err := scanWorkflowRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func GetWorkflowRule(ctx context.Context, pool *pgxpool.Pool, id int64) (*WorkflowRule, error) {
	row := pool.QueryRow(ctx, `SELECT `+ruleColumns+` FROM workflow_rules WHERE id=$1`, id)
	r, err := scanWorkflowRule(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

func CountWorkflowRules(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_rules`).Scan(&n)
	return n, err
}

// UpsertWorkflowRule creates a new rule (ID == 0) or updates an existing one.
func UpsertWorkflowRule(ctx context.Context, pool *pgxpool.Pool, r WorkflowRule) error {
	if r.MatchLabels == nil {
		r.MatchLabels = map[string]string{}
	}
	if r.GroupBy == nil {
		r.GroupBy = []string{}
	}
	if r.ExtraFields == nil {
		r.ExtraFields = []RuleField{}
	}
	if r.Enrichments == nil {
		r.Enrichments = []Enrichment{}
	}

	matchLabelsJSON, _ := json.Marshal(r.MatchLabels)
	groupByJSON, _ := json.Marshal(r.GroupBy)
	extraFieldsJSON, _ := json.Marshal(r.ExtraFields)
	enrichmentsJSON, _ := json.Marshal(r.Enrichments)

	if r.ID == 0 {
		_, err := pool.Exec(ctx, `
			INSERT INTO workflow_rules (
			  name, match_labels, channel, team, target_label, group_by,
			  extra_fields, enrichments, enabled, display_title, notification_only, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now())
			ON CONFLICT (name) DO UPDATE SET
			  match_labels      = EXCLUDED.match_labels,
			  channel           = EXCLUDED.channel,
			  team              = EXCLUDED.team,
			  target_label      = EXCLUDED.target_label,
			  group_by          = EXCLUDED.group_by,
			  extra_fields      = EXCLUDED.extra_fields,
			  enrichments       = EXCLUDED.enrichments,
			  enabled           = EXCLUDED.enabled,
			  display_title     = EXCLUDED.display_title,
			  notification_only = EXCLUDED.notification_only,
			  updated_at        = now()
		`, r.Name, string(matchLabelsJSON), r.Channel, r.Team, r.TargetLabel, string(groupByJSON),
			string(extraFieldsJSON), string(enrichmentsJSON), r.Enabled, r.DisplayTitle, r.NotificationOnly)
		return err
	}

	_, err := pool.Exec(ctx, `
		UPDATE workflow_rules SET
		  name = $2, match_labels = $3, channel = $4, team = $5,
		  target_label = $6, group_by = $7, extra_fields = $8, enrichments = $9, enabled = $10,
		  display_title = $11, notification_only = $12,
		  updated_at = now()
		WHERE id = $1
	`, r.ID, r.Name, string(matchLabelsJSON), r.Channel, r.Team, r.TargetLabel, string(groupByJSON),
		string(extraFieldsJSON), string(enrichmentsJSON), r.Enabled, r.DisplayTitle, r.NotificationOnly)
	return err
}

func DeleteWorkflowRule(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	_, err := pool.Exec(ctx, `DELETE FROM workflow_rules WHERE id = $1`, id)
	return err
}

// SeedWorkflowRulesFromYAML imports the legacy workflows/*.yaml rules into
// the DB, but only the very first time (table empty) so it never clobbers
// edits made from the web UI on later restarts.
func SeedWorkflowRulesFromYAML(ctx context.Context, pool *pgxpool.Pool, rules []WorkflowRule) error {
	if len(rules) == 0 {
		return nil
	}

	n, err := CountWorkflowRules(ctx, pool)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	for _, r := range rules {
		if err := UpsertWorkflowRule(ctx, pool, r); err != nil {
			return err
		}
	}
	log.Printf("seeded %d workflow rule(s) from workflows/*.yaml", len(rules))
	return nil
}
