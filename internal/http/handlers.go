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

func AlertsHandler(pool *pgxpool.Pool, rules []workflows.WorkflowRule, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		expected := "Bearer " + token
		if authHeader != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var payload models.WebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid json", 400)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		var upserts []db.AlertUpsert
		var allPendings []workflows.SlackPending

		for _, alert := range payload.Alerts {
			alertname := alert.Labels["alertname"]

			existingMeta, err := db.GetActiveAlertMeta(ctx, pool, alert.Fingerprint)
			if err != nil {
				log.Printf("failed to load alert meta: %v", err)
			}

			isNew, err := db.IsNewAlert(ctx, pool, alert.Fingerprint, *alert.StartsAt)
			if err != nil {
				log.Printf("error checking if alert is new: %v", err)
			}

			upsert := db.AlertUpsert{
				Fingerprint: alert.Fingerprint,
				Alertname:   alertname,
				Status:      alert.Status,
				StartsAt:    *alert.StartsAt,
				EndsAt:      alert.EndsAt,
				Labels:      alert.Labels,
				Annotations: alert.Annotations,
				Payload:     alert,
				Meta:        existingMeta,
			}

			var pendings []workflows.SlackPending
			upsert, pendings = workflows.ProcessAlert(ctx, upsert, rules, pool, isNew)
			allPendings = append(allPendings, pendings...)
			upserts = append(upserts, upsert)
		}

		// Send all firing alerts as one Slack message per channel.
		workflows.SendBatchedSlack(pool, allPendings)

		for _, upsert := range upserts {
			if err := db.UpsertAlert(ctx, pool, upsert); err != nil {
				log.Printf("upsert alert failed: %v", err)
				http.Error(w, "db error", 500)
				return
			}
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
			Error       string `json:"error"`
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

		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "Slack connected successfully")
	})
}
