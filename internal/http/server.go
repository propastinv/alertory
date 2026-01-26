package http

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

func NewServer(db *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("/api/v1/alerts", AlertsHandler(db))
	mux.Handle("/providers/oauth2/slack", SlackOAuthCallback(db))

	return mux
}
