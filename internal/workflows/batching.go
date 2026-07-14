package workflows

import "github.com/propastinv/alertory/internal/db"

// bucket is one Slack message's worth of alerts: either a single alert
// (the default case) or, when several alerts became unsent at the same
// time in numbers above massThreshold, all of them together.
type bucket struct {
	ts        string // "" means this bucket hasn't been sent yet
	channelID string
	members   []db.GroupMember
}

// buildBuckets partitions a group's members into the Slack messages that
// represent them.
//
// Members that already have a Slack message (NotifiedTS set) stay grouped
// with whichever other members share that same ts - that's what a
// combined message from a past burst looks like on later flushes.
//
// Members that have never been sent are only batched together if there
// are more of them than threshold; otherwise each gets its own bucket.
// This is the core of "one alert, one message by default, merge only on a
// real burst": a single alert arriving alone always gets its own message,
// while 200 alerts that showed up together in the same debounce window
// collapse into one.
func buildBuckets(members map[string]db.GroupMember, threshold int) []*bucket {
	existingByTS := map[string]*bucket{}
	var unsent []db.GroupMember

	for _, m := range members {
		if m.NotifiedTS == "" {
			unsent = append(unsent, m)
			continue
		}
		b, ok := existingByTS[m.NotifiedTS]
		if !ok {
			b = &bucket{ts: m.NotifiedTS, channelID: m.NotifiedChannel}
			existingByTS[m.NotifiedTS] = b
		}
		b.members = append(b.members, m)
	}

	buckets := make([]*bucket, 0, len(existingByTS)+1)
	for _, b := range existingByTS {
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
	if b.ts == "" {
		return true
	}
	for _, m := range b.members {
		if m.NotifiedStatus != m.Status {
			return true
		}
	}
	return false
}
