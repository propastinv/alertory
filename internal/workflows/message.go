package workflows

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/propastinv/alertory/internal/db"
	"github.com/propastinv/alertory/internal/slack"
)

// maxMemberLines caps how many individual alerts get listed in a group
// message; beyond this we just say how many more there are, so a group of
// hundreds of members can't blow past Slack's message size limits.
const maxMemberLines = 20

// RenderGroupMessage builds the one fixed Slack layout used for every
// alert group: a header with firing/resolved counts, a colored attachment
// with Team/Target/Starts/Resolved fields, and a capped list of member
// alerts. It always rebuilds the full message from the group's current
// state, so flushing a group after one of its members resolves can never
// clobber the rest of the message (the failure mode of the old
// per-alert chat.update calls).
func RenderGroupMessage(g db.AlertGroup) (string, []slack.Attachment) {
	members := sortedMembers(g.Members)

	firing, resolved := 0, 0
	var earliestStart time.Time
	var latestEnd time.Time
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

	allResolved := firing == 0 && len(members) > 0

	title := joinSorted(alertnames)
	icon := ":rotating_light:"
	color := "#e01e5a"
	if allResolved {
		icon = ":white_check_mark:"
		color = "#2eb67d"
		title = "RESOLVED: " + title
	}
	if len(members) > 1 {
		title = fmt.Sprintf("%s (%d firing, %d resolved)", title, firing, resolved)
	}

	var fields []slack.Field
	if g.Team != "" {
		fields = append(fields, slack.Field{Title: "Team", Value: g.Team, Short: true})
	}
	switch len(targets) {
	case 0:
		// no target label configured for this rule - nothing to show
	case 1:
		fields = append(fields, slack.Field{Title: "Target", Value: joinSorted(targets), Short: true})
	default:
		fields = append(fields, slack.Field{Title: "Targets", Value: fmt.Sprintf("%d affected", len(targets)), Short: true})
	}
	if !earliestStart.IsZero() {
		fields = append(fields, slack.Field{Title: "Starts At", Value: earliestStart.Format(time.RFC3339), Short: true})
	}
	if allResolved && !latestEnd.IsZero() {
		fields = append(fields, slack.Field{Title: "Resolved At", Value: latestEnd.Format(time.RFC3339), Short: true})
	}

	if lines := memberLines(members); len(lines) > 0 {
		fields = append(fields, slack.Field{
			Title: fmt.Sprintf("Alerts (%d)", len(members)),
			Value: strings.Join(lines, "\n"),
		})
	}

	attachment := slack.Attachment{
		Color:  color,
		Title:  icon + " " + title,
		Fields: fields,
	}

	return "", []slack.Attachment{attachment}
}

func memberLines(members []db.GroupMember) []string {
	var lines []string
	for i, m := range members {
		if i >= maxMemberLines {
			lines = append(lines, fmt.Sprintf("...and %d more", len(members)-maxMemberLines))
			break
		}
		statusIcon := "\U0001F525" // fire
		if m.Status == "resolved" {
			statusIcon = "✅" // check
		}
		label := m.Target
		if label == "" {
			label = m.Alertname
		}
		lines = append(lines, statusIcon+" "+label)
	}
	return lines
}

func sortedMembers(m map[string]db.GroupMember) []db.GroupMember {
	out := make([]db.GroupMember, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt.Before(out[j].StartsAt) })
	return out
}

func joinSorted(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
