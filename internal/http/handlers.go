package http

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/propastinv/alertory/internal/db"
	"github.com/propastinv/alertory/internal/models"
	"github.com/propastinv/alertory/internal/workflows"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AlertsHandler ingests an Alertmanager webhook call. It intentionally
// does no Slack I/O on this path: alerts are matched against rules and
// upserted into their debounced group (see workflows.ProcessAlert), and
// the group flush worker sends to Slack later, out of band. That keeps
// this handler's latency independent of Slack's API and lets Alertmanager
// get a fast, reliable 200 even during a mass-alert burst - which matters
// because a slow/failing webhook response is exactly what causes
// Alertmanager to retry and pile on more load.
func AlertsHandler(pool *pgxpool.Pool, rules *workflows.RuleStore, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !checkWebhookAuth(r, pool, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var payload models.WebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		if len(payload.Alerts) == 0 {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()

		fingerprints := make([]string, 0, len(payload.Alerts))
		for _, a := range payload.Alerts {
			fingerprints = append(fingerprints, a.Fingerprint)
		}

		knownStartsAt, err := db.BatchGetActiveAlerts(ctx, pool, fingerprints)
		if err != nil {
			log.Printf("failed to batch load active alerts: %v", err)
		}

		activeRules := rules.Rules()
		upserts := make([]db.AlertUpsert, 0, len(payload.Alerts))

		for _, alert := range payload.Alerts {
			startsAt := time.Now()
			if alert.StartsAt != nil {
				startsAt = *alert.StartsAt
			}

			prevStartsAt, known := knownStartsAt[alert.Fingerprint]
			isNew := !known || !prevStartsAt.Equal(startsAt)

			upsert := db.AlertUpsert{
				Fingerprint: alert.Fingerprint,
				Alertname:   alert.Labels["alertname"],
				Status:      alert.Status,
				StartsAt:    startsAt,
				EndsAt:      alert.EndsAt,
				Labels:      alert.Labels,
				Annotations: alert.Annotations,
				Payload:     alert,
			}

			workflows.ProcessAlert(ctx, pool, activeRules, upsert, isNew)
			upserts = append(upserts, upsert)
		}

		if err := db.BatchUpsertAlerts(ctx, pool, upserts); err != nil {
			log.Printf("batch upsert failed: %v", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})
}

// slackOAuthStateCookie is a short-lived, single-use cookie tying a
// "Connect Slack" click to its OAuth callback. Slack's authorize URL is
// built without it in older code paths; without a state check, an
// attacker could start the OAuth flow with *their own* Slack workspace,
// then trick an already-logged-in admin into opening the resulting
// callback URL (classic login-CSRF) - the admin's session would then
// silently store the attacker's Slack access token as "the" workspace
// integration, redirecting all future alerts to the attacker.
const slackOAuthStateCookie = "alertory_slack_oauth_state"

func slackSecureCookies() bool {
	return strings.HasPrefix(os.Getenv("APP_URL"), "https://")
}

// SlackAuthorizeHandler redirects to Slack's OAuth authorize screen,
// stashing a random state value in a cookie that SlackOAuthCallback
// verifies before exchanging any code.
func SlackAuthorizeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientID := os.Getenv("SLACK_CLIENT_ID")
		appURL := os.Getenv("APP_URL")
		if clientID == "" || appURL == "" {
			http.Error(w, "Slack OAuth is not configured", http.StatusInternalServerError)
			return
		}

		state, err := randomToken(32)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     slackOAuthStateCookie,
			Value:    state,
			Path:     "/",
			MaxAge:   600,
			HttpOnly: true,
			Secure:   slackSecureCookies(),
			SameSite: http.SameSiteLaxMode,
		})

		redirect := appURL + "/ui/providers/oauth2/slack"
		authorizeURL := "https://slack.com/oauth/v2/authorize?client_id=" + url.QueryEscape(clientID) +
			"&scope=chat:write&state=" + url.QueryEscape(state) +
			"&redirect_uri=" + url.QueryEscape(redirect)
		http.Redirect(w, r, authorizeURL, http.StatusFound)
	})
}

func SlackOAuthCallback(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stateCookie, err := r.Cookie(slackOAuthStateCookie)
		http.SetCookie(w, &http.Cookie{
			Name: slackOAuthStateCookie, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, Secure: slackSecureCookies(), SameSite: http.SameSiteLaxMode,
		})
		if err != nil || stateCookie.Value == "" || r.URL.Query().Get("state") != stateCookie.Value {
			http.Error(w, "invalid or missing oauth state", http.StatusBadRequest)
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}

		clientID := os.Getenv("SLACK_CLIENT_ID")
		clientSecret := os.Getenv("SLACK_CLIENT_SECRET")
		if clientID == "" || clientSecret == "" {
			http.Error(w, "Slack client_id or client_secret is not set", http.StatusInternalServerError)
			return
		}
		appURL := os.Getenv("APP_URL")
		if appURL == "" {
			log.Println("APP_URL is not set")
			http.Error(w, "APP_URL is not set", http.StatusInternalServerError)
			return
		}

		redirectURI := fmt.Sprintf("%s/ui/providers/oauth2/slack", appURL)

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.PostForm("https://slack.com/api/oauth.v2.access", url.Values{
			"client_id":     {clientID},
			"client_secret": {clientSecret},
			"code":          {code},
			"redirect_uri":  {redirectURI},
		})
		if err != nil {
			http.Error(w, "failed to exchange token", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		var result struct {
			OK          bool   `json:"ok"`
			AccessToken string `json:"access_token"`
			Team        struct {
				Name string `json:"name"`
			} `json:"team"`
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			http.Error(w, "invalid response from Slack", http.StatusInternalServerError)
			return
		}

		if !result.OK {
			http.Error(w, "Slack error: "+result.Error, http.StatusBadRequest)
			return
		}

		if err := db.UpsertProviderSetting(pool, "slack", "access_token", result.AccessToken); err != nil {
			http.Error(w, "failed to save token", http.StatusInternalServerError)
			return
		}
		if result.Team.Name != "" {
			_ = db.UpsertProviderSetting(pool, "slack", "team_name", result.Team.Name)
		}

		http.Redirect(w, r, "/ui/settings", http.StatusFound)
	})
}

func randomToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashAPIKey hashes a raw API key for storage/lookup. SHA-256 (not
// bcrypt/scrypt) is enough here because the input is already a
// high-entropy random token, not a user-chosen password - there's
// nothing for an attacker to brute-force via a fast hash.
func hashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// checkWebhookAuth authorizes an /api/v1/alerts request against the
// static BEARER_TOKEN (if set) or any issued API key. Once at least one
// API key exists, the endpoint requires auth even if BEARER_TOKEN was
// never set, since generating a key is an explicit signal that the
// deployment wants this endpoint locked down.
func checkWebhookAuth(r *http.Request, pool *pgxpool.Pool, token string) bool {
	const prefix = "Bearer "
	supplied := r.Header.Get("Authorization")
	suppliedKey := strings.TrimPrefix(supplied, prefix)

	if token != "" {
		expected := prefix + token
		// Constant-time compare: a plain != leaks how many leading bytes
		// of the token guessed correctly via response timing.
		if len(supplied) == len(expected) && subtle.ConstantTimeCompare([]byte(supplied), []byte(expected)) == 1 {
			return true
		}
	}

	if !strings.HasPrefix(supplied, prefix) || suppliedKey == "" {
		return token == "" && !anyAPIKeysExist(r.Context(), pool)
	}

	ok, err := db.APIKeyExists(r.Context(), pool, hashAPIKey(suppliedKey))
	if err != nil {
		log.Printf("failed to check API key: %v", err)
		return false
	}
	if ok {
		return true
	}

	return token == "" && !anyAPIKeysExist(r.Context(), pool)
}

func anyAPIKeysExist(ctx context.Context, pool *pgxpool.Pool) bool {
	n, err := db.CountAPIKeys(ctx, pool)
	if err != nil {
		log.Printf("failed to count API keys: %v", err)
		return true // fail closed
	}
	return n > 0
}
