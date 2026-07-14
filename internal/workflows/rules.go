package workflows

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/propastinv/alertory/internal/db"
)

// Debounce/window used to collapse bursts of alerts into a single Slack
// message: the group's flush is pushed out by debounceWindow on every new
// event, but never past first_event_at + maxGroupWindow, so a continuous
// storm still gets flushed periodically instead of waiting forever.
const (
	debounceWindow = 8 * time.Second
	maxGroupWindow = 45 * time.Second
)

// ProcessAlert matches an incoming alert against every enabled rule and,
// for each match, upserts the alert as a member of its debounced group.
// It does not talk to Slack directly - that happens later, out of the
// HTTP request path, in RunFlushWorker - so ingestion latency no longer
// depends on Slack's API being fast or even reachable.
func ProcessAlert(ctx context.Context, pool *pgxpool.Pool, rules []db.WorkflowRule, alert db.AlertUpsert, isNew bool) {
	for _, rule := range rules {
		if !rule.Matches(alert.Labels) {
			continue
		}

		if len(rule.Enrichments) > 0 && isNew && alert.Status == "firing" {
			enrichAlert(&alert, rule.Enrichments)
		}

		target := ""
		if rule.TargetLabel != "" {
			target = alert.Labels[rule.TargetLabel]
		}

		member := db.GroupMember{
			Fingerprint: alert.Fingerprint,
			Alertname:   alert.Alertname,
			Status:      alert.Status,
			Target:      target,
			Annotations: alert.Annotations,
			StartsAt:    alert.StartsAt,
			EndsAt:      alert.EndsAt,
			UpdatedAt:   time.Now(),
		}

		groupKey := computeGroupKey(rule, alert.Labels)

		if err := db.UpsertGroupMember(ctx, pool, groupKey, rule.Name, rule.Channel, rule.Team, member, debounceWindow, maxGroupWindow); err != nil {
			log.Printf("failed to upsert alert group member (rule=%s fingerprint=%s): %v", rule.Name, alert.Fingerprint, err)
		}
	}
}

// computeGroupKey decides which alerts get collapsed into the same Slack
// message. Alerts sharing a rule, channel, and the same values for the
// rule's GroupBy labels land in the same group. If GroupBy isn't set, we
// default to grouping by alertname - this is what turns "200 hosts firing
// the same alert at once" into one Slack message instead of 200.
func computeGroupKey(rule db.WorkflowRule, labels map[string]string) string {
	groupBy := rule.GroupBy
	if len(groupBy) == 0 {
		groupBy = []string{"alertname"}
	}

	keys := make([]string, len(groupBy))
	copy(keys, groupBy)
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}

	h := sha1.New()
	h.Write([]byte(rule.Name + "|" + rule.Channel + "|" + strings.Join(parts, ",")))
	return rule.Name + ":" + hex.EncodeToString(h.Sum(nil))[:16]
}

// enrichAlert runs each configured HTTP enrichment and stores its result
// into the alert's annotations under Enrichment.StoreIn (or Name). Errors
// are logged and skipped - a broken enrichment endpoint shouldn't block
// the alert from being grouped and sent.
func enrichAlert(alert *db.AlertUpsert, enrichments []db.Enrichment) {
	for _, e := range enrichments {
		renderedURL, err := renderLabelTemplate(e.URL, alert.Labels)
		if err != nil {
			log.Printf("enrichment %s: bad URL template: %v", e.Name, err)
			continue
		}

		params := make(map[string]string, len(e.Params))
		for _, p := range e.Params {
			v, err := renderLabelTemplate(p.Value, alert.Labels)
			if err != nil {
				log.Printf("enrichment %s: bad param template %s: %v", e.Name, p.Name, err)
				continue
			}
			params[p.Name] = v
		}

		method := e.Method
		if method == "" {
			method = "GET"
		}

		if method == "GET" && len(params) > 0 {
			keys := make([]string, 0, len(params))
			for k := range params {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var q []string
			for _, k := range keys {
				q = append(q, k+"="+params[k])
			}
			renderedURL += "?" + strings.Join(q, "&")
		}

		req, err := http.NewRequest(method, renderedURL, nil)
		if err != nil {
			log.Printf("enrichment %s: bad request: %v", e.Name, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("enrichment %s: request failed: %v", e.Name, err)
			continue
		}

		var result map[string]any
		decodeErr := json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if decodeErr != nil {
			log.Printf("enrichment %s: bad response: %v", e.Name, decodeErr)
			continue
		}

		storeIn := e.StoreIn
		if storeIn == "" {
			storeIn = e.Name
		}

		var value any = result
		if e.ResponseField != "" {
			value = result[e.ResponseField]
		}

		if alert.Annotations == nil {
			alert.Annotations = make(map[string]string)
		}
		alert.Annotations[storeIn] = fmt.Sprintf("%v", value)
	}
}

type labelTemplateData struct {
	Labels map[string]string
}

// renderLabelTemplate supports the same "{{ .Labels.NAME }}" templates the
// old YAML rules used for enrichment URLs/params.
func renderLabelTemplate(tmpl string, labels map[string]string) (string, error) {
	t, err := template.New("t").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, labelTemplateData{Labels: labels}); err != nil {
		return "", err
	}
	return buf.String(), nil
}
