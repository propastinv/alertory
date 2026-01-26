package http

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
	"os"
	"fmt"
    "net/url"

	"github.com/propastinv/alertory/internal/db"
	"github.com/propastinv/alertory/internal/models"

	"github.com/jackc/pgx/v5/pgxpool"
)

func AlertsHandler(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload models.WebhookPayload

		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid json", 400)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		for _, alert := range payload.Alerts {
			alertname := alert.Labels["alertname"]

			err := db.UpsertAlert(ctx, pool, db.AlertUpsert{
				Fingerprint: alert.Fingerprint,
				Alertname:   alertname,
				Status:      alert.Status,
				StartsAt:    alert.StartsAt,
				EndsAt:      alert.EndsAt,
				Labels:      alert.Labels,
				Annotations: alert.Annotations,
				Payload:     alert,
			})

			if err != nil {
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

		if err := db.UpsertProvider(pool, "slack", "access_token", result.AccessToken); err != nil {
			http.Error(w, "failed to save token", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "Slack connected successfully")
	})
}
