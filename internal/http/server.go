package http

import (
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/propastinv/alertory/internal/auth"
	"github.com/propastinv/alertory/internal/workflows"
)

// healthzHandler is an unauthenticated liveness/readiness endpoint. It
// exists because /api/v1/alerts - the only other unauthenticated-by-
// default route - starts requiring a bearer token as soon as
// BEARER_TOKEN is set (which CONFIGURATION.md recommends for any real
// deployment), which would otherwise make a naive Kubernetes probe
// pointed at it fail with 401 forever. Pings the pool so a pod that
// can't reach Postgres gets reported unready instead of accepting
// traffic it can't actually serve.
func healthzHandler(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, "db unreachable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

// NewServer wires up both the Alertmanager webhook API and the web UI on
// a single mux. The webhook (/api/v1/alerts) keeps its own bearer-token
// check and is never gated by authSvc - Alertmanager can't do a browser
// login. Every web UI route, including the Slack OAuth callback (it's an
// admin action), requires an authenticated session when authSvc is set.
//
// The entire admin surface lives under /ui/ (dashboard, rules, settings,
// login/logout, Slack OAuth) so a deployment can put it behind a
// different Ingress/host/network policy than the public-facing webhook
// path - e.g. an internal-only Ingress for /ui vs. a public one for
// /api/v1/alerts.
//
// If authSvc is nil, the web UI either fails closed (503, the default when
// OIDC just isn't configured) or - only when DISABLE_AUTH=true - runs fully
// unauthenticated via auth.DevBypass. The latter is for local development
// only: never set DISABLE_AUTH on a deployment reachable by anyone else.
func NewServer(pool *pgxpool.Pool, rules *workflows.RuleStore, authSvc *auth.Service) http.Handler {
	mux := http.NewServeMux()

	token := os.Getenv("BEARER_TOKEN")
	mux.Handle("/api/v1/alerts", AlertsHandler(pool, rules, token))
	mux.Handle("/healthz", healthzHandler(pool))

	authDisabled := os.Getenv("DISABLE_AUTH") == "true"

	if authSvc == nil && !authDisabled {
		log.Println("WARNING: OIDC_ISSUER_URL/OIDC_CLIENT_ID/OIDC_CLIENT_SECRET not fully set - web UI is disabled (503) until SSO is configured")
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "web UI is disabled: SSO is not configured", http.StatusServiceUnavailable)
		}))
		return securityHeaders(mux)
	}

	if authDisabled {
		log.Println("WARNING: DISABLE_AUTH=true - web UI is serving every /ui/ route without authentication")
	}

	// wrap gates a UI handler behind a real session when SSO is configured,
	// or behind the fixed dev session when it's intentionally disabled -
	// everything downstream (currentUser, CheckCSRF, ...) works unchanged
	// either way since both paths populate the same session context.
	wrap := func(h http.Handler) http.Handler {
		if authSvc == nil {
			return auth.DevBypass(h)
		}
		return auth.RequireAuth(pool, h)
	}

	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/ui/", http.StatusFound)
	}))

	if authSvc != nil {
		mux.Handle("/ui/auth/login", authSvc.LoginHandler())
		mux.Handle("/ui/auth/callback", authSvc.CallbackHandler(pool))
		mux.Handle("/ui/auth/logout", authSvc.LogoutHandler(pool))
	} else {
		// There's no real session to end, but base.html always renders a
		// "Log out" link - give it somewhere to go instead of a 404.
		mux.Handle("/ui/auth/logout", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/ui/", http.StatusFound)
		}))
	}

	mux.Handle("/ui/providers/oauth2/slack/authorize", wrap(SlackAuthorizeHandler()))
	mux.Handle("/ui/providers/oauth2/slack", wrap(SlackOAuthCallback(pool)))

	tmpl, err := loadTemplates()
	if err != nil {
		log.Fatalf("failed to load web UI templates: %v", err)
	}

	mux.Handle("GET /ui/{$}", wrap(dashboardHandler(pool, tmpl.dashboard)))

	mux.Handle("GET /ui/rules", wrap(rulesListHandler(pool, tmpl.rulesList)))
	mux.Handle("GET /ui/rules/new", wrap(newRuleFormHandler(tmpl.ruleForm)))
	mux.Handle("GET /ui/rules/{id}/edit", wrap(editRuleFormHandler(pool, tmpl.ruleForm)))
	mux.Handle("POST /ui/rules", wrap(saveRuleHandler(pool)))
	mux.Handle("POST /ui/rules/{id}", wrap(saveRuleHandler(pool)))
	mux.Handle("POST /ui/rules/{id}/delete", wrap(deleteRuleHandler(pool)))

	mux.Handle("GET /ui/settings", wrap(settingsHandler(pool, tmpl.settings)))

	mux.Handle("GET /ui/api-keys", wrap(apiKeysListHandler(pool, tmpl.apiKeys)))
	mux.Handle("POST /ui/api-keys", wrap(createAPIKeyHandler(pool, tmpl.apiKeys)))
	mux.Handle("POST /ui/api-keys/{id}/delete", wrap(deleteAPIKeyHandler(pool)))

	return securityHeaders(mux)
}

// securityHeaders adds baseline hardening headers to every response. The
// web UI renders third-party content only from cdn.tailwindcss.com (see
// templates/base.html), so the CSP allows that origin for scripts/styles
// and otherwise stays locked to 'self'.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline' https://cdn.tailwindcss.com; "+
				"style-src 'self' 'unsafe-inline' https://cdn.tailwindcss.com; "+
				"img-src 'self' data:; "+
				"frame-ancestors 'none'; "+
				"base-uri 'self'")
		next.ServeHTTP(w, r)
	})
}
