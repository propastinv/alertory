package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/propastinv/alertory/internal/db"
	httpapi "github.com/propastinv/alertory/internal/http"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}

	pool := db.Connect(dsn)
	defer pool.Close()

	db.AutoMigrate(context.Background(), pool)

	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			active, events, err := db.DeleteOldAlerts(context.Background(), pool, 7*24*time.Hour)
			if err != nil {
				log.Printf("cleanup error: %v", err)
			} else {
				log.Printf("cleanup: removed %d active_alerts, %d alert_events", active, events)
			}
		}
	}()

	handler := httpapi.NewServer(pool)

	addr := ":8080"
	log.Println("Listening on", addr)
	log.Fatal(http.ListenAndServe(addr, handler))
}
