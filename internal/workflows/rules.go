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

		// A notification-only rule (e.g. forwarded emails pushed through
		// this same webhook) isn't a real alert with a lifecycle - there's
		// no "resolved" for an email that already arrived. Enrichment runs
		// on every new event rather than gating on status=="firing", and
		// the member's status is forced to "resolved" immediately so it's
		// treated as a one-and-done event: sent once, never expected to
		// change, and cleaned up right after.
		shouldEnrich := isNew && (alert.Status == "firing" || rule.NotificationOnly)
		if len(rule.Enrichments) > 0 && shouldEnrich {
			enrichAlert(&alert, rule.Enrichments)
		}

		status := alert.Status
		if rule.NotificationOnly {
			status = "resolved"
		}

		target := ""
		if rule.TargetLabel != "" {
			target = alert.Labels[rule.TargetLabel]
		}

		// The rule's DisplayTitle may be a template over the alert's own
		// data ("{{ .Labels.match }}", "{{ .Annotations.title }}"), so it's
		// rendered here - after enrichment, per alert - and stored on the
		// member. A static title renders to itself unchanged.
		displayTitle := renderDisplayTitle(rule.DisplayTitle, alert)

		member := db.GroupMember{
			Fingerprint:   alert.Fingerprint,
			Alertname:     alert.Alertname,
			Status:        status,
			Target:        target,
			DisplayTitle:  displayTitle,
			DisplayFields: resolveDisplayFields(rule.ExtraFields, alert.Annotations, alert.Labels),
			StartsAt:      alert.StartsAt,
			EndsAt:        alert.EndsAt,
			UpdatedAt:     time.Now(),
		}

		info := db.GroupInfo{
			GroupKey:         computeGroupKey(rule, alert.Labels),
			RuleName:         rule.Name,
			Channel:          rule.Channel,
			Team:             rule.Team,
			DisplayTitle:     displayTitle,
			NotificationOnly: rule.NotificationOnly,
		}

		if err := db.UpsertGroupMember(ctx, pool, info, member, debounceWindow, maxGroupWindow); err != nil {
			log.Printf("failed to upsert alert group member (rule=%s fingerprint=%s): %v", rule.Name, alert.Fingerprint, err)
		}
	}
}

// resolveDisplayFields picks out only the keys a rule explicitly mapped to
// a Slack field, and truncates each value. Each key is looked up in the
// alert's annotations first, then its labels - sources spread this data
// across both (an email bridge typically puts the address in a label and
// the subject in an annotation), and which side a given key lives on is an
// implementation detail of the source that a rule author shouldn't have to
// know. This is deliberately an allow-list rather than "show everything":
// alert sources sometimes stuff large raw payloads (a full email body, a
// stack trace) into an annotation, and rendering that wholesale as its own
// field is exactly what broke past cards - it blew past Slack's size
// limits and silently pushed the fields the rule actually wanted off the
// message.
func resolveDisplayFields(fields []db.RuleField, annotations, labels map[string]string) []db.MemberField {
	if len(fields) == 0 {
		return nil
	}

	var out []db.MemberField
	for _, f := range fields {
		v := annotations[f.AnnotationKey]
		if v == "" {
			v = labels[f.AnnotationKey]
		}
		if v == "" {
			continue
		}
		out = append(out, db.MemberField{Title: f.Title, Value: truncateValue(v, maxFieldValueLen)})
	}
	return out
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

// renderDisplayTitle turns a rule's display-title setting into the final
// per-alert title. It supports Go templates over the alert's data
// ("{{ .Labels.match }}", "{{ .Annotations.title }}"), which is what makes
// a dynamic header possible for sources like forwarded emails where every
// event shares one alertname but carries its real subject in a
// label/annotation. Plain static strings pass through unchanged; a broken
// template or one that renders to nothing falls back to the raw string, so
// a bad edit in the rule form degrades to an ugly title, never a lost
// message.
func renderDisplayTitle(tmpl string, alert db.AlertUpsert) string {
	if tmpl == "" || !strings.Contains(tmpl, "{{") {
		return tmpl
	}
	rendered, err := renderAlertTemplate(tmpl, alert.Labels, alert.Annotations)
	if err != nil {
		log.Printf("display title template %q: %v", tmpl, err)
		return tmpl
	}
	rendered = strings.TrimSpace(rendered)
	if rendered == "" || rendered == "<no value>" {
		return tmpl
	}
	return truncateValue(rendered, maxFieldValueLen)
}

type labelTemplateData struct {
	Labels      map[string]string
	Annotations map[string]string
}

// renderLabelTemplate supports the same "{{ .Labels.NAME }}" templates the
// old YAML rules used for enrichment URLs/params.
func renderLabelTemplate(tmpl string, labels map[string]string) (string, error) {
	return renderAlertTemplate(tmpl, labels, nil)
}

// renderAlertTemplate renders "{{ .Labels.NAME }}" / "{{ .Annotations.NAME }}"
// templates against a single alert's data.
func renderAlertTemplate(tmpl string, labels, annotations map[string]string) (string, error) {
	t, err := template.New("t").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, labelTemplateData{Labels: labels, Annotations: annotations}); err != nil {
		return "", err
	}
	return buf.String(), nil
}
