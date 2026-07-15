package workflows

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/propastinv/alertory/internal/db"
	"github.com/propastinv/alertory/internal/slack"
)

// maxMemberLines caps how many individual alerts get listed in a combined
// message; beyond this we just say how many more there are.
const maxMemberLines = 20

// maxFieldValueLen caps how long any single extra-field value can be
// before it's shown in Slack. Even for annotations a rule explicitly opted
// into, an unexpectedly large value shouldn't be able to break the card.
const maxFieldValueLen = 400

func truncateValue(v string, max int) string {
	r := []rune(v)
	if len(r) <= max {
		return v
	}
	return string(r[:max]) + "…"
}

// GroupStyle is the rule-level rendering config snapshotted onto a group
// (see db.GroupInfo) - the bits that shape a message but aren't
// per-member data.
type GroupStyle struct {
	Team         string
	DisplayTitle string
	// NotificationOnly means these members aren't real alerts with a
	// firing/resolved lifecycle (e.g. forwarded emails) - so the message
	// never shows a "RESOLVED" transition, a resolved color, or
	// Starts/Resolved At timestamps, since none of that means anything
	// for a one-shot notification.
	NotificationOnly bool
}

// RenderBucketMessage renders the Slack content for one message bucket. A
// single alert (the default, "one alert = one message" case) gets the
// detailed per-alert layout; more than one alert in a bucket only happens
// when a burst tripped the mass threshold, and gets the condensed summary
// layout instead.
func RenderBucketMessage(style GroupStyle, members []db.GroupMember) (string, []slack.Attachment) {
	if len(members) == 1 {
		return renderIndividual(style, members[0])
	}
	return renderBatch(style, members)
}

func renderIndividual(style GroupStyle, m db.GroupMember) (string, []slack.Attachment) {
	title := m.Alertname
	if style.DisplayTitle != "" {
		title = style.DisplayTitle
	}

	color := "#e01e5a"
	if !style.NotificationOnly && m.Status == "resolved" {
		color = "#2eb67d"
		title = "RESOLVED: " + title
	}

	var fields []slack.Field
	if style.Team != "" {
		fields = append(fields, slack.Field{Title: "Team", Value: style.Team, Short: true})
	}
	if m.Target != "" {
		fields = append(fields, slack.Field{Title: "Target", Value: m.Target, Short: true})
	}
	if !style.NotificationOnly {
		fields = append(fields, slack.Field{Title: "Starts At", Value: m.StartsAt.Format(time.RFC3339), Short: true})
		if m.Status == "resolved" && m.EndsAt != nil {
			fields = append(fields, slack.Field{Title: "Resolved At", Value: m.EndsAt.Format(time.RFC3339), Short: true})
		}
	}
	for _, f := range m.DisplayFields {
		fields = append(fields, slack.Field{Title: f.Title, Value: f.Value})
	}

	return "", []slack.Attachment{{Color: color, Title: title, Fields: fields}}
}

func renderBatch(style GroupStyle, members []db.GroupMember) (string, []slack.Attachment) {
	sort.Slice(members, func(i, j int) bool { return members[i].StartsAt.Before(members[j].StartsAt) })

	firing, resolved := 0, 0
	var earliestStart, latestEnd time.Time
	alertnames := map[string]bool{}
	targets := map[string]bool{}

	for _, m := range members {
		alertnames[m.Alertname] = true
		if m.Target != "" {
			targets[m.Target] = true
		}
		if earliestStart.IsZero() || m.StartsAt.Before(earliestStart) {
			earliestStart = m.StartsAt
		}
		if m.Status == "resolved" {
			resolved++
			if m.EndsAt != nil && m.EndsAt.After(latestEnd) {
				latestEnd = *m.EndsAt
			}
		} else {
			firing++
		}
	}

	title := joinSorted(alertnames)
	if style.DisplayTitle != "" {
		title = style.DisplayTitle
	}

	color := "#e01e5a"
	allResolved := firing == 0 && len(members) > 0

	if style.NotificationOnly {
		title = fmt.Sprintf("%s (%d)", title, len(members))
	} else {
		if allResolved {
			color = "#2eb67d"
			title = "RESOLVED: " + title
		}
		title = fmt.Sprintf("%s (%d firing, %d resolved)", title, firing, resolved)
	}

	var fields []slack.Field
	if style.Team != "" {
		fields = append(fields, slack.Field{Title: "Team", Value: style.Team, Short: true})
	}
	switch len(targets) {
	case 0:
		// no target label configured for this rule - nothing to show
	case 1:
		fields = append(fields, slack.Field{Title: "Target", Value: joinSorted(targets), Short: true})
	default:
		fields = append(fields, slack.Field{Title: "Targets", Value: fmt.Sprintf("%d affected", len(targets)), Short: true})
	}
	if !style.NotificationOnly {
		if !earliestStart.IsZero() {
			fields = append(fields, slack.Field{Title: "Starts At", Value: earliestStart.Format(time.RFC3339), Short: true})
		}
		if allResolved && !latestEnd.IsZero() {
			fields = append(fields, slack.Field{Title: "Resolved At", Value: latestEnd.Format(time.RFC3339), Short: true})
		}
	}

	if lines := memberLines(members, style.NotificationOnly); len(lines) > 0 {
		fields = append(fields, slack.Field{
			Title: fmt.Sprintf("Alerts (%d)", len(members)),
			Value: strings.Join(lines, "\n"),
		})
	}

	return "", []slack.Attachment{{Color: color, Title: title, Fields: fields}}
}

func memberLines(members []db.GroupMember, notificationOnly bool) []string {
	var lines []string
	for i, m := range members {
		if i >= maxMemberLines {
			lines = append(lines, fmt.Sprintf("...and %d more", len(members)-maxMemberLines))
			break
		}
		label := m.Target
		if label == "" {
			label = m.Alertname
		}
		if !notificationOnly && m.Status == "resolved" {
			label += " (resolved)"
		}
		lines = append(lines, label)
	}
	return lines
}

func joinSorted(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
