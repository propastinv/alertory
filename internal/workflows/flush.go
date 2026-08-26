package workflows

import (
	"context"
	"log"
	"strings"
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

// splitChannels turns a rule's Channel setting into the list of distinct
// Slack channel/user IDs it should notify. A rule normally names a single
// channel, but it can also be a comma-separated list ("C0111,C0222") so an
// alert fans out to more than one channel - e.g. a team channel plus a
// specific person's DM - as independent, independently-editable messages.
func splitChannels(raw string) []string {
	parts := strings.Split(raw, ",")
	seen := make(map[string]bool, len(parts))
	channels := make([]string, 0, len(parts))
	for _, p := range parts {
		ch := strings.TrimSpace(p)
		if ch == "" || seen[ch] {
			continue
		}
		seen[ch] = true
		channels = append(channels, ch)
	}
	return channels
}

// processGroup sends or updates exactly the Slack messages needed to
// reflect a group's current state. By default every alert gets its own
// message per destination channel; a set of alerts that first became
// "unsent" together in numbers greater than massThreshold is collapsed
// into one combined message (per channel) instead (see buildBuckets).
// Messages that already exist are only touched if something in them
// actually changed since they were last sent, so a quiet group produces
// zero Slack calls.
func processGroup(ctx context.Context, pool *pgxpool.Pool, token string, g db.AlertGroup) {
	channels := splitChannels(g.Channel)
	buckets := buildBuckets(g.Members, massThreshold)
	style := GroupStyle{Team: g.Team, NotificationOnly: g.NotificationOnly}

	var sendErr error
	for _, b := range buckets {
		if !bucketChanged(b) {
			continue
		}

		text, attachments := RenderBucketMessage(style, b.members)

		existingByChannel := make(map[string]string, len(b.targets))
		for _, t := range b.targets {
			existingByChannel[t.Channel] = t.TS
		}

		newTargets := make([]db.NotifiedTarget, 0, len(channels))
		var bucketErr error
		for _, ch := range channels {
			if ts, ok := existingByChannel[ch]; ok {
				if err := slack.Update(token, ch, ts, text, attachments); err != nil {
					log.Printf("flush worker: failed to update group %s (bucket of %d, ts=%s) on %s: %v", g.GroupKey, len(b.members), ts, ch, err)
					bucketErr = err
				}
				newTargets = append(newTargets, db.NotifiedTarget{Channel: ch, TS: ts})
				continue
			}

			res, err := slack.Post(token, ch, text, attachments)
			if err != nil {
				log.Printf("flush worker: failed to send group %s (bucket of %d) to %s: %v", g.GroupKey, len(b.members), ch, err)
				bucketErr = err
				continue
			}
			newTargets = append(newTargets, db.NotifiedTarget{Channel: res.Channel, TS: res.TS})
		}

		if bucketErr != nil {
			sendErr = bucketErr
		}

		for _, m := range b.members {
			updated := g.Members[m.Fingerprint]
			updated.NotifiedTargets = newTargets
			// Only mark the member caught-up if every channel succeeded;
			// otherwise bucketChanged keeps retrying this bucket next
			// flush, re-sending only to whichever channels are still
			// missing a target (the ones that already succeeded are
			// updated in place, which is a no-op change in content).
			if bucketErr == nil {
				updated.NotifiedStatus = updated.Status
			}
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
		if len(m.NotifiedTargets) == 0 || m.NotifiedStatus != m.Status {
			return false
		}
	}
	return true
}
