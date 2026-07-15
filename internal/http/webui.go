package http

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/propastinv/alertory/internal/auth"
	"github.com/propastinv/alertory/internal/db"
)

// currentUser returns a display label for the nav bar. Every web UI route
// is behind auth.RequireAuth, so a missing session here just means "not
// worth failing the page over" - it renders blank rather than erroring.
func currentUser(r *http.Request) string {
	sess, ok := auth.FromContext(r.Context())
	if !ok {
		return ""
	}
	if sess.Email != "" {
		return sess.Email
	}
	return sess.Name
}

func csrfToken(r *http.Request) string {
	sess, ok := auth.FromContext(r.Context())
	if !ok {
		return ""
	}
	return sess.CSRFToken
}

func renderPage(w http.ResponseWriter, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
		log.Printf("template render failed: %v", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

type dashboardData struct {
	Active       string
	User         string
	Alerts       []db.ActiveAlertRow
	StatusFilter string
	OpenGroups   int
}

func dashboardHandler(pool *pgxpool.Pool, tmpl *template.Template) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		status := r.URL.Query().Get("status")

		alerts, err := db.ListActiveAlerts(ctx, pool, status, 200)
		if err != nil {
			log.Printf("failed to list active alerts: %v", err)
			http.Error(w, "failed to load alerts", http.StatusInternalServerError)
			return
		}

		openGroups, err := db.CountOpenAlertGroups(ctx, pool)
		if err != nil {
			log.Printf("failed to count open alert groups: %v", err)
		}

		renderPage(w, tmpl, dashboardData{
			Active:       "dashboard",
			User:         currentUser(r),
			Alerts:       alerts,
			StatusFilter: status,
			OpenGroups:   openGroups,
		})
	})
}

func rulesListHandler(pool *pgxpool.Pool, tmpl *template.Template) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rules, err := db.ListWorkflowRules(r.Context(), pool)
		if err != nil {
			log.Printf("failed to list workflow rules: %v", err)
			http.Error(w, "failed to load rules", http.StatusInternalServerError)
			return
		}
		data := struct {
			Active    string
			User      string
			CSRFToken string
			Rules     []db.WorkflowRule
		}{Active: "rules", User: currentUser(r), CSRFToken: csrfToken(r), Rules: rules}
		renderPage(w, tmpl, data)
	})
}

type ruleFormData struct {
	Active      string
	User        string
	CSRFToken   string
	FormAction  string
	Submit      string
	ID          int64
	Name        string
	Channel     string
	Team        string
	TargetLabel string
	MatchLabels string
	GroupBy     string
	ExtraFields string
	Enrichments string
	Enabled     bool
}

func newRuleFormHandler(tmpl *template.Template) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		renderPage(w, tmpl, ruleFormData{
			Active:     "rules",
			User:       currentUser(r),
			CSRFToken:  csrfToken(r),
			FormAction: "/rules",
			Submit:     "Create rule",
			Enabled:    true,
		})
	})
}

func editRuleFormHandler(pool *pgxpool.Pool, tmpl *template.Template) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		rule, err := db.GetWorkflowRule(r.Context(), pool, id)
		if err != nil {
			log.Printf("failed to load rule %d: %v", id, err)
			http.Error(w, "failed to load rule", http.StatusInternalServerError)
			return
		}
		if rule == nil {
			http.NotFound(w, r)
			return
		}

		renderPage(w, tmpl, ruleFormData{
			Active:      "rules",
			User:        currentUser(r),
			CSRFToken:   csrfToken(r),
			FormAction:  fmt.Sprintf("/rules/%d", id),
			Submit:      "Save changes",
			ID:          id,
			Name:        rule.Name,
			Channel:     rule.Channel,
			Team:        rule.Team,
			TargetLabel: rule.TargetLabel,
			MatchLabels: formatLabels(rule.MatchLabels),
			GroupBy:     strings.Join(rule.GroupBy, ", "),
			ExtraFields: formatFieldList(rule.ExtraFields),
			Enrichments: formatEnrichments(rule.Enrichments),
			Enabled:     rule.Enabled,
		})
	})
}

func saveRuleHandler(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if !auth.CheckCSRF(r) {
			http.Error(w, "invalid or missing csrf token", http.StatusForbidden)
			return
		}

		var id int64
		if idStr := r.PathValue("id"); idStr != "" {
			id, _ = strconv.ParseInt(idStr, 10, 64)
		}

		var enrichments []db.Enrichment
		if raw := strings.TrimSpace(r.FormValue("enrichments")); raw != "" {
			if err := json.Unmarshal([]byte(raw), &enrichments); err != nil {
				http.Error(w, "invalid enrichments JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
		}

		rule := db.WorkflowRule{
			ID:          id,
			Name:        strings.TrimSpace(r.FormValue("name")),
			Channel:     strings.TrimSpace(r.FormValue("channel")),
			Team:        strings.TrimSpace(r.FormValue("team")),
			TargetLabel: strings.TrimSpace(r.FormValue("target_label")),
			MatchLabels: parseLabels(r.FormValue("match_labels")),
			GroupBy:     parseCSV(r.FormValue("group_by")),
			ExtraFields: parseFieldList(r.FormValue("extra_fields")),
			Enrichments: enrichments,
			Enabled:     r.FormValue("enabled") == "on",
		}

		if rule.Name == "" || rule.Channel == "" {
			http.Error(w, "name and channel are required", http.StatusBadRequest)
			return
		}

		if err := db.UpsertWorkflowRule(r.Context(), pool, rule); err != nil {
			log.Printf("failed to save rule %q: %v", rule.Name, err)
			http.Error(w, "failed to save rule", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, "/rules", http.StatusFound)
	})
}

func deleteRuleHandler(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if !auth.CheckCSRF(r) {
			http.Error(w, "invalid or missing csrf token", http.StatusForbidden)
			return
		}

		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if err := db.DeleteWorkflowRule(r.Context(), pool, id); err != nil {
			log.Printf("failed to delete rule %d: %v", id, err)
			http.Error(w, "failed to delete rule", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/rules", http.StatusFound)
	})
}

func settingsHandler(pool *pgxpool.Pool, tmpl *template.Template) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := db.GetProviderSetting(pool, "slack", "access_token")
		teamName := db.GetProviderSetting(pool, "slack", "team_name")

		clientID := os.Getenv("SLACK_CLIENT_ID")
		appURL := os.Getenv("APP_URL")

		var authorizeURL string
		if clientID != "" && appURL != "" {
			redirect := appURL + "/providers/oauth2/slack"
			authorizeURL = "https://slack.com/oauth/v2/authorize?client_id=" + url.QueryEscape(clientID) +
				"&scope=chat:write&redirect_uri=" + url.QueryEscape(redirect)
		}

		data := struct {
			Active          string
			User            string
			Connected       bool
			TeamName        string
			AuthorizeURL    string
			Retention       string
			CleanupInterval string
		}{
			Active:          "settings",
			User:            currentUser(r),
			Connected:       token != "",
			TeamName:        teamName,
			AuthorizeURL:    authorizeURL,
			Retention:       envOrDefault("ALERT_RETENTION", "168h"),
			CleanupInterval: envOrDefault("CLEANUP_INTERVAL", "1h"),
		}
		renderPage(w, tmpl, data)
	})
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseLabels(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return out
}

func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+labels[k])
	}
	return strings.Join(lines, "\n")
}

// parseFieldList parses "Title=annotation_key" lines (order preserved,
// unlike a map) into rule ExtraFields.
func parseFieldList(raw string) []db.RuleField {
	var out []db.RuleField
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		title := strings.TrimSpace(parts[0])
		key := strings.TrimSpace(parts[1])
		if title == "" || key == "" {
			continue
		}
		out = append(out, db.RuleField{Title: title, AnnotationKey: key})
	}
	return out
}

func formatFieldList(fields []db.RuleField) string {
	lines := make([]string, 0, len(fields))
	for _, f := range fields {
		lines = append(lines, f.Title+"="+f.AnnotationKey)
	}
	return strings.Join(lines, "\n")
}

func parseCSV(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func formatEnrichments(e []db.Enrichment) string {
	if len(e) == 0 {
		return ""
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}
