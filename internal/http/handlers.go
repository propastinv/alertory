package http

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

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
