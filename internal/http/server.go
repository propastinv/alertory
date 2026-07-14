package http

import (
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/propastinv/alertory/internal/workflows"
)

// NewServer wires up both the Alertmanager webhook API and the web UI on
// a single mux, so everything runs from the one port this service already
// listens on - no separate auth/process for the UI (not needed yet, per
// the "no auth for now" ask).
func NewServer(pool *pgxpool.Pool, rules *workflows.RuleStore) http.Handler {
	mux := http.NewServeMux()

	token := os.Getenv("BEARER_TOKEN")
	mux.Handle("/api/v1/alerts", AlertsHandler(pool, rules, token))
	mux.Handle("/providers/oauth2/slack", SlackOAuthCallback(pool))

	tmpl, err := loadTemplates()
	if err != nil {
		log.Fatalf("failed to load web UI templates: %v", err)
	}

	mux.Handle("GET /{$}", dashboardHandler(pool, tmpl.dashboard))

	mux.Handle("GET /rules", rulesListHandler(pool, tmpl.rulesList))
	mux.Handle("GET /rules/new", newRuleFormHandler(tmpl.ruleForm))
	mux.Handle("GET /rules/{id}/edit", editRuleFormHandler(pool, tmpl.ruleForm))
	mux.Handle("POST /rules", saveRuleHandler(pool))
	mux.Handle("POST /rules/{id}", saveRuleHandler(pool))
	mux.Handle("POST /rules/{id}/delete", deleteRuleHandler(pool))

	mux.Handle("GET /settings", settingsHandler(pool, tmpl.settings))

	return mux
}
