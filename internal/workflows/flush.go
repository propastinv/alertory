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
// their Slack message. It's safe to run from multiple instances of this
// service at once (see db.ClaimDueGroups). Blocks until ctx is cancelled.
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
		text, attachments := RenderGroupMessage(g)
		fullyResolved := g.AllResolved()

		var channelID, ts string
		var sendErr error

		if g.SlackTS == "" {
			res, err := slack.Post(token, g.Channel, text, attachments)
			sendErr = err
			if err == nil {
				channelID, ts = res.Channel, res.TS
			}
		} else {
			channelID, ts = g.SlackChannelID, g.SlackTS
			sendErr = slack.Update(token, g.SlackChannelID, g.SlackTS, text, attachments)
		}

		if sendErr != nil {
			log.Printf("flush worker: failed to send group %s to %s: %v", g.GroupKey, g.Channel, sendErr)
			if err := db.FinalizeGroupFailed(ctx, pool, g.GroupKey, g.Attempts, sendErr); err != nil {
				log.Printf("flush worker: failed to mark group %s failed: %v", g.GroupKey, err)
			}
			continue
		}

		if err := db.FinalizeGroupSent(ctx, pool, g.GroupKey, channelID, ts, fullyResolved); err != nil {
			log.Printf("flush worker: failed to finalize group %s: %v", g.GroupKey, err)
		}
	}
}
