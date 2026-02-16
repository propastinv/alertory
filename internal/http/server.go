package http

import (
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/propastinv/alertory/internal/workflows"
)

func NewServer(db *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()

	token := os.Getenv("BEARER_TOKEN")

	rules, err := workflows.LoadWorkflowRules("workflows")
	if err != nil {
		log.Fatalf("failed to load workflow rules: %v", err)
	}
	mux.Handle("/api/v1/alerts", AlertsHandler(db, rules, token))

	mux.Handle("/providers/oauth2/slack", SlackOAuthCallback(db))

	return mux
}
