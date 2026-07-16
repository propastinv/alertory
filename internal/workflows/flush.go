package workflows

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/propastinv/alertory/internal/db"
	"github.com/propastinv/alertory/internal/slack"
)

const (
	flushInterval = 3 * time.Second
	flushBatch    = 25
	// claimLease bounds how long a claimed-but-not-yet-finalized group is
	// left alone. If the process dies mid-send, the group becomes due
	// again once the lease expires instead of being stuck forever.
	claimLease = 30 * time.Second
)

// RunFlushWorker periodically claims due alert groups and sends/updates
// whatever Slack messages they need. Safe to run from multiple instances
// of this service at once (see db.ClaimDueGroups). Blocks until ctx is
// cancelled.
func RunFlushWorker(ctx context.Context, pool *pgxpool.Pool) {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			flushDueGroups(ctx, pool)
		}
	}
}

func flushDueGroups(ctx context.Context, pool *pgxpool.Pool) {
	groups, err := db.ClaimDueGroups(ctx, pool, flushBatch, claimLease)
	if err != nil {
		log.Printf("flush worker: claim failed: %v", err)
		return
	}
	if len(groups) == 0 {
		return
	}

	token := db.GetProviderSetting(pool, "slack", "access_token")

	for _, g := range groups {
		processGroup(ctx, pool, token, g)
	}
}

// processGroup sends or updates exactly the Slack messages needed to
// reflect a group's current state. By default every alert gets its own
// message; a set of alerts that first became "unsent" together in numbers
// greater than massThreshold is collapsed into one combined message
// instead (see buildBuckets). Messages that already exist are only
// touched if something in them actually changed since they were last
// sent, so a quiet group produces zero Slack calls.
func processGroup(ctx context.Context, pool *pgxpool.Pool, token string, g db.AlertGroup) {
	buckets := buildBuckets(g.Members, massThreshold)
	style := GroupStyle{Team: g.Team, NotificationOnly: g.NotificationOnly}

	var sendErr error
	for _, b := range buckets {
		if !bucketChanged(b) {
			continue
		}

		text, attachments := RenderBucketMessage(style, b.members)

		if b.ts == "" {
			res, err := slack.Post(token, g.Channel, text, attachments)
			if err != nil {
				log.Printf("flush worker: failed to send group %s (bucket of %d) to %s: %v", g.GroupKey, len(b.members), g.Channel, err)
				sendErr = err
				continue
			}
			b.channelID, b.ts = res.Channel, res.TS
		} else if err := slack.Update(token, b.channelID, b.ts, text, attachments); err != nil {
			log.Printf("flush worker: failed to update group %s (bucket ts=%s) on %s: %v", g.GroupKey, b.ts, g.Channel, err)
			sendErr = err
			continue
		}

		for _, m := range b.members {
			updated := g.Members[m.Fingerprint]
			updated.NotifiedChannel = b.channelID
			updated.NotifiedTS = b.ts
			updated.NotifiedStatus = updated.Status
			g.Members[m.Fingerprint] = updated
		}
	}

	if sendErr != nil {
		if err := db.SaveGroupProgressFailed(ctx, pool, g.GroupKey, g.Members, g.Attempts, sendErr); err != nil {
			log.Printf("flush worker: failed to save failed progress for group %s: %v", g.GroupKey, err)
		}
		return
	}

	done := g.AllResolved() && allNotifiedCurrent(g.Members)
	if err := db.SaveGroupProgress(ctx, pool, g.GroupKey, g.Members, done); err != nil {
		log.Printf("flush worker: failed to save progress for group %s: %v", g.GroupKey, err)
	}
}

func allNotifiedCurrent(members map[string]db.GroupMember) bool {
	for _, m := range members {
		if m.NotifiedTS == "" || m.NotifiedStatus != m.Status {
			return false
		}
	}
	return true
}
