package main

import (
	"context"
	"log"
	"net/http"
	"os"

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

	handler := httpapi.NewServer(pool)

	addr := ":8080"
	log.Println("Listening on", addr)
	log.Fatal(http.ListenAndServe(addr, handler))
}
