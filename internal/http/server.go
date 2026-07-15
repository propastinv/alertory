package http

import (
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/propastinv/alertory/internal/auth"
	"github.com/propastinv/alertory/internal/workflows"
)

// NewServer wires up both the Alertmanager webhook API and the web UI on
// a single mux. The webhook (/api/v1/alerts) keeps its own bearer-token
// check and is never gated by authSvc - Alertmanager can't do a browser
// login. Every web UI route, including the Slack OAuth callback (it's an
// admin action), requires an authenticated session when authSvc is set.
//
// If authSvc is nil (OIDC isn't configured), the web UI fails closed: it
// serves 503 instead of silently running without auth, since this UI is
// meant to be reachable from the public internet.
func NewServer(pool *pgxpool.Pool, rules *workflows.RuleStore, authSvc *auth.Service) http.Handler {
	mux := http.NewServeMux()

	token := os.Getenv("BEARER_TOKEN")
	mux.Handle("/api/v1/alerts", AlertsHandler(pool, rules, token))

	if authSvc == nil {
		log.Println("WARNING: OIDC_ISSUER_URL/OIDC_CLIENT_ID/OIDC_CLIENT_SECRET not fully set - web UI is disabled (503) until SSO is configured")
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "web UI is disabled: SSO is not configured", http.StatusServiceUnavailable)
		}))
		return mux
	}

	mux.Handle("/auth/login", authSvc.LoginHandler())
	mux.Handle("/auth/callback", authSvc.CallbackHandler(pool))
	mux.Handle("/auth/logout", authSvc.LogoutHandler(pool))

	mux.Handle("/providers/oauth2/slack", auth.RequireAuth(pool, SlackOAuthCallback(pool)))

	tmpl, err := loadTemplates()
	if err != nil {
		log.Fatalf("failed to load web UI templates: %v", err)
	}

	mux.Handle("GET /{$}", auth.RequireAuth(pool, dashboardHandler(pool, tmpl.dashboard)))

	mux.Handle("GET /rules", auth.RequireAuth(pool, rulesListHandler(pool, tmpl.rulesList)))
	mux.Handle("GET /rules/new", auth.RequireAuth(pool, newRuleFormHandler(tmpl.ruleForm)))
	mux.Handle("GET /rules/{id}/edit", auth.RequireAuth(pool, editRuleFormHandler(pool, tmpl.ruleForm)))
	mux.Handle("POST /rules", auth.RequireAuth(pool, saveRuleHandler(pool)))
	mux.Handle("POST /rules/{id}", auth.RequireAuth(pool, saveRuleHandler(pool)))
	mux.Handle("POST /rules/{id}/delete", auth.RequireAuth(pool, deleteRuleHandler(pool)))

	mux.Handle("GET /settings", auth.RequireAuth(pool, settingsHandler(pool, tmpl.settings)))

	return mux
}
