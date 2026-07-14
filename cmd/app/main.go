package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/propastinv/alertory/internal/db"
	httpapi "github.com/propastinv/alertory/internal/http"
	"github.com/propastinv/alertory/internal/workflows"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool := db.Connect(dsn)
	defer pool.Close()

	db.AutoMigrate(ctx, pool)

	if legacyRules, err := workflows.LoadLegacyYAMLRules("workflows"); err != nil {
		log.Printf("no legacy workflows/*.yaml to seed: %v", err)
	} else if err := db.SeedWorkflowRulesFromYAML(ctx, pool, legacyRules); err != nil {
		log.Printf("failed to seed workflow rules from YAML: %v", err)
	}

	ruleStore := workflows.NewRuleStore(pool)
	if err := ruleStore.Refresh(ctx); err != nil {
		log.Printf("initial rule load failed, starting with an empty rule set: %v", err)
	}
	go ruleStore.Run(ctx, 10*time.Second)

	go workflows.RunFlushWorker(ctx, pool)

	go runCleanupLoop(ctx, pool)

	handler := httpapi.NewServer(pool, ruleStore)

	addr := ":" + envOrDefault("PORT", "8080")
	server := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	go func() {
		log.Println("Listening on", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

// runCleanupLoop trims old alert data on a fixed interval. It runs
// immediately on startup (the previous version waited a full interval
// before the first pass, so a fresh deploy would accumulate unbounded
// rows until then) and deletes in small batches so a big backlog can't
// hold a long-running lock on a busy table.
func runCleanupLoop(ctx context.Context, pool *pgxpool.Pool) {
	retention := envDuration("ALERT_RETENTION", 7*24*time.Hour)
	interval := envDuration("CLEANUP_INTERVAL", 1*time.Hour)

	runCleanup := func() {
		active, events, err := db.DeleteOldAlerts(ctx, pool, retention)
		if err != nil {
			log.Printf("cleanup error: %v", err)
			return
		}

		closedGroups, err := db.CleanupResolvedGroups(ctx, pool, 1*time.Hour)
		if err != nil {
			log.Printf("cleanup error (alert_groups): %v", err)
		}

		if active > 0 || events > 0 || closedGroups > 0 {
			log.Printf("cleanup: removed %d active_alerts, %d alert_events, %d alert_groups", active, events, closedGroups)
			// Table is insert/delete heavy under load; nudge autovacuum
			// along right after a big trim instead of waiting on its
			// normal schedule.
			if err := db.VacuumAnalyze(ctx, pool); err != nil {
				log.Printf("cleanup: vacuum failed: %v", err)
			}
		}
	}

	runCleanup()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runCleanup()
		}
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("invalid %s=%q, using default %s: %v", key, v, def, err)
		return def
	}
	return d
}
