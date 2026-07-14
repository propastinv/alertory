package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
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
		if token != "" {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
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

func SlackOAuthCallback(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		redirectURI := fmt.Sprintf("%s/providers/oauth2/slack", appURL)

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

		http.Redirect(w, r, "/settings", http.StatusFound)
	})
}
