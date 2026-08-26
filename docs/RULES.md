# Routing rules

A rule decides three things: **which alerts match it**, **where they go in Slack**, and **how they're grouped and rendered** once there. Rules are managed from `/rules` in the web UI and stored in Postgres - no redeploy needed to add or change one.

## Fields

| Field | Required | Description |
|---|---|---|
| Name | yes | Unique rule name. Also used, together with the channel and group-by values, to compute which alerts land in the same Slack message. |
| Match labels | no | `key=value` pairs, one per line. An alert must match **every** listed label exactly to be picked up by this rule. No match labels means the rule matches everything. |
| Slack channel ID(s) | yes | A Slack channel or user ID (`C0123456`, `U0123456`). Can be a **comma-separated list** to notify more than one channel/person - see [Multiple destinations](#multiple-destinations) below. |
| Team | no | A fixed label shown as a "Team" field on every message from this rule. |
| Target label | no | Which alert label to surface as "Target" (e.g. a hostname or account). Left blank, no Target field is shown. |
| Group by | no | Comma-separated label names. Alerts sharing this rule and the same values for these labels are treated as the same incident and collapse into one Slack message. Defaults to `alertname` if left blank. |
| Extra fields | no | `Title = annotation_key` pairs, one per line. Each maps an alert annotation (or, if not found there, a label) onto a named field on the Slack card. This is an allow-list on purpose - only fields you explicitly map are ever shown, so a large raw payload dumped into some annotation can't blow up the message. |
| Enrichments | no | JSON array of HTTP calls to make before rendering the first message for a new alert - see [Enrichments](#enrichments). |
| Display title | no | Replaces the alertname as the message header. Can be a template like `{{ .Labels.match }}` or `{{ .Annotations.title }}`, rendered per alert - useful for sources that aren't really "alerts" and don't send a meaningful alertname (e.g. a forwarded-email bridge). |
| Notification only | no | Marks the rule as a one-shot notice rather than a stateful alert - see [Notification-only rules](#notification-only-rules). |
| Enabled | - | Disabled rules are skipped entirely; alerts that would have matched are simply not routed anywhere. |

## Grouping and batching

Alerts are grouped by `(rule, group-by label values)`. Within one group, alerts are batched into Slack messages like this:

- A burst of more than `MASS_ALERT_THRESHOLD` (default `5`, see [`CONFIGURATION.md`](CONFIGURATION.md)) alerts becoming "unsent" at the same time collapses into **one combined message**.
- Anything smaller gets **one message per alert**.
- Once a message exists, it's edited in place as its member(s)' status changes - never re-posted.

This means the default "one alert, one message" behavior is preserved for the common case, and batching only kicks in for a genuine mass event.

## Multiple destinations

The Slack channel field accepts a comma-separated list, e.g.:

```
C012ALERTS, U045ONCALL
```

Each destination in the list gets its own Slack message, sent and later edited independently - so a rule can post a persistent card in a team channel while also DMing whoever's on call, without needing two separate rules. If one destination temporarily fails to send (e.g. a bad ID, a revoked DM), the others aren't affected; alertory retries only the failed destination on the next flush.

## Enrichments

An enrichment calls out to an HTTP endpoint before the first message for a *new* alert is rendered, and stores the response into an annotation so it can be surfaced with [Extra fields](#fields):

```json
[
  {
    "name": "affected_users",
    "url": "http://internal-service/users",
    "method": "GET",
    "params": [{ "name": "target", "value": "{{ .Labels.login }}" }],
    "response_field": "users",
    "store_in": "affected_users"
  }
]
```

Enrichments only run once, on the alert's first firing event (or, for a `notification_only` rule, its one event) - not on every update - so a flapping alert doesn't hammer the enrichment endpoint.

## Notification-only rules

Some things that flow through the same webhook aren't really "alerts" with a lifecycle - a forwarded email, a one-off system notice. A `notification_only` rule treats every event as immediately resolved: no "RESOLVED" transition, no resolved color, no Starts/Resolved At timestamps - just a single message that says what happened, once.

## Migrating from the old YAML rules

Rules used to live in `workflows/*.yaml`, with the Slack message layout hand-templated per rule. That format is only read once, on first boot with an empty rules table in Postgres, to seed the initial rule set - match labels, the Slack channel, and a best-effort `Target` label are carried over; `Team` and `Group by` are new fields that come through blank and are meant to be filled in from the web UI. The Slack layout itself is no longer freely templatable per rule; it's fixed in code (see [`ARCHITECTURE.md`](ARCHITECTURE.md#rendering)) so every rule's messages stay visually consistent.

If you're bringing up a fresh instance, drop your legacy `.yaml` files into a `workflows/` directory next to the binary before first boot; after that, all editing happens in `/rules`.
