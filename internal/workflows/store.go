package workflows

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/propastinv/alertory/internal/db"
)

// RuleStore caches enabled workflow rules in memory so the ingestion hot
// path never blocks on a DB query per request. It refreshes on an
// interval, so rule edits made from the web UI take effect within one
// refresh cycle without requiring a restart.
type RuleStore struct {
	pool *pgxpool.Pool

	mu    sync.RWMutex
	rules []db.WorkflowRule
}

func NewRuleStore(pool *pgxpool.Pool) *RuleStore {
	return &RuleStore{pool: pool}
}

// Rules returns the current cached rule set. Safe for concurrent use.
func (s *RuleStore) Rules() []db.WorkflowRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rules
}

func (s *RuleStore) Refresh(ctx context.Context) error {
	rules, err := db.ListEnabledWorkflowRules(ctx, s.pool)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.rules = rules
	s.mu.Unlock()
	return nil
}

// Run refreshes immediately, then on every tick of interval, until ctx is
// cancelled. Intended to run in its own goroutine.
func (s *RuleStore) Run(ctx context.Context, interval time.Duration) {
	if err := s.Refresh(ctx); err != nil {
		log.Printf("rule store: initial refresh failed: %v", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Refresh(ctx); err != nil {
				log.Printf("rule store: refresh failed: %v", err)
			}
		}
	}
}
