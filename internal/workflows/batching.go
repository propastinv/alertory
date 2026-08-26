package workflows

import (
	"sort"
	"strings"

	"github.com/propastinv/alertory/internal/db"
)

// bucket is one Slack message's worth of alerts - or rather, one message
// per destination channel, since a rule can list several comma-separated
// Slack channel IDs. targets is empty until the bucket has been sent at
// least once; after that it holds one (channel, ts) pair per channel so
// each can be edited in place independently.
type bucket struct {
	targets []db.NotifiedTarget
	members []db.GroupMember
}

// targetKey collapses a member's NotifiedTargets into a single string so
// members that were sent together (same set of channel/ts pairs) can be
// grouped back into the same bucket on a later flush.
func targetKey(targets []db.NotifiedTarget) string {
	if len(targets) == 0 {
		return ""
	}
	parts := make([]string, len(targets))
	for i, t := range targets {
		parts[i] = t.Channel + "=" + t.TS
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// buildBuckets partitions a group's members into the Slack messages that
// represent them.
//
// Members that already have Slack message(s) (NotifiedTargets set) stay
// grouped with whichever other members share that exact same set of
// targets - that's what a combined message from a past burst looks like
// on later flushes.
//
// Members that have never been sent are only batched together if there
// are more of them than threshold; otherwise each gets its own bucket.
// This is the core of "one alert, one message by default, merge only on a
// real burst": a single alert arriving alone always gets its own message,
// while 200 alerts that showed up together in the same debounce window
// collapse into one.
func buildBuckets(members map[string]db.GroupMember, threshold int) []*bucket {
	existingByKey := map[string]*bucket{}
	var unsent []db.GroupMember

	for _, m := range members {
		if len(m.NotifiedTargets) == 0 {
			unsent = append(unsent, m)
			continue
		}
		key := targetKey(m.NotifiedTargets)
		b, ok := existingByKey[key]
		if !ok {
			b = &bucket{targets: m.NotifiedTargets}
			existingByKey[key] = b
		}
		b.members = append(b.members, m)
	}

	buckets := make([]*bucket, 0, len(existingByKey)+1)
	for _, b := range existingByKey {
		buckets = append(buckets, b)
	}

	switch {
	case len(unsent) == 0:
		// nothing new
	case len(unsent) > threshold:
		buckets = append(buckets, &bucket{members: unsent})
	default:
		for _, m := range unsent {
			buckets = append(buckets, &bucket{members: []db.GroupMember{m}})
		}
	}

	return buckets
}

// bucketChanged reports whether a bucket needs to be (re)sent: brand new
// buckets always do; existing ones do only if some member's status has
// moved on since the message was last sent/updated.
func bucketChanged(b *bucket) bool {
	if len(b.targets) == 0 {
		return true
	}
	for _, m := range b.members {
		if m.NotifiedStatus != m.Status {
			return true
		}
	}
	return false
}
