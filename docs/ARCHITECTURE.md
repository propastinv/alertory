# Architecture

This is a walkthrough of how one Alertmanager webhook call turns into a Slack message (and how that message later gets edited instead of duplicated), plus the Postgres schema behind it.

## The pipeline

```
                POST /api/v1/alerts
                        |
                        v
              +-------------------+
              |  AlertsHandler    |  bearer-token check, decode payload
              +-------------------+
                        |
                for each alert:
                        v
              +-------------------+
              | workflows.        |  match against enabled rules,
              | ProcessAlert      |  run enrichments, resolve display
              +-------------------+  fields/title, upsert into its group
                        |
                        v
              +-------------------+
              |  alert_groups     |  durable dedup/debounce queue
              |  (Postgres)       |  one row per (rule, group-by key)
              +-------------------+
                        ^
                        | claims due rows every 3s
                        |
              +-------------------+
              | RunFlushWorker /  |  builds "buckets" (one Slack message's
              | processGroup      |  worth of alerts), renders content,
              +-------------------+  posts new or updates existing messages
                        |
                        v
                     Slack API
```

The webhook handler (`internal/http/handlers.go`) never calls Slack. It only matches, enriches, and writes to Postgres, then returns `200 OK`. This is deliberate: Alertmanager's retry behavior means a slow or failing downstream call on the ingestion path causes exactly the pile-up alertory exists to prevent. All Slack I/O happens later, out of band, in the flush worker.

## Dedup & debounce

Every alert that matches a rule is upserted as a *member* of an `alert_groups` row, keyed by `(rule name, resolved group-by labels)` - by default just `alertname`, but a rule can group by any combination of labels. Upserting a member re-arms a debounce timer: `flush_after` moves out by `ALERT_DEBOUNCE` (default 8s) on every new event, but never past `first_event_at + ALERT_MAX_WINDOW` (default 45s). That's what lets a burst of alerts arriving over a couple of seconds collapse into one flush, while a continuous storm still gets flushed periodically instead of being delayed forever.

## Flushing into buckets

`RunFlushWorker` ticks every 3 seconds and claims due groups with `FOR UPDATE SKIP LOCKED`, so multiple instances of the service can run the flush loop concurrently without double-sending. For each claimed group, `buildBuckets` (`internal/workflows/batching.go`) decides how its members map onto Slack messages:

- Members that already have a message (or messages - see below) stay grouped with whoever shares that exact same message.
- Members that have never been sent are batched together only if there are more of them than `MASS_ALERT_THRESHOLD` (default 5); otherwise each gets its own bucket, so a single alert always gets its own message.

A bucket is only re-rendered and re-sent if something about it actually changed (a member's status moved) since it was last sent - a quiet group produces zero Slack API calls.

## Multi-channel delivery

A rule's `channel` field can be a comma-separated list of Slack channel/user IDs. Each bucket is sent to every channel in the list independently: the first send does a `chat.postMessage` per channel, and each member remembers one `(channel, ts)` pair per destination (`db.NotifiedTarget`). Later flushes for the same bucket call `chat.update` per channel using its own remembered `ts`, so every destination's message is edited in place on its own thread of history - a channel and a person's DM both stay in sync, independently, from a single rule.

## Rendering

`RenderBucketMessage` (`internal/workflows/message.go`) picks between two layouts:

- **Individual** - a single alert gets a detailed card: title, color by status, Team/Target fields, Starts/Resolved At, and any rule-configured extra fields.
- **Batch** - more than one alert in a bucket gets a condensed summary: counts of firing/resolved, the set of distinct targets, and up to 20 individual alert lines (with a "...and N more" tail beyond that).

`notification_only` rules skip anything lifecycle-related (no resolved color, no timestamps) since there's no real firing/resolved transition for a one-shot notice.

## Storage

Everything lives in Postgres (`internal/db/migrate.go` runs additive migrations on boot):

| Table | Purpose |
|---|---|
| `active_alerts` | Current state of every alert alertory has seen, for the dashboard |
| `alert_events` | History of state transitions, for the dashboard's per-alert timeline |
| `alert_groups` | The durable dedup/debounce/delivery-tracking queue described above |
| `workflow_rules` | Routing rules, edited from `/rules` |
| `providers` | Provider-level settings (currently: the Slack OAuth access token) |
| `web_sessions` | Web UI login sessions (OIDC) |

There's no in-memory message tracking anywhere: which Slack message represents which alert, on which channel, is persisted as part of the group's JSON member state. A restart or a rolling deploy mid-burst just picks up where it left off once the claim lease (`claimLease`, 30s) expires.

## Web UI vs. webhook auth

The two HTTP surfaces have separate, independent auth: the webhook (`/api/v1/alerts`) checks a static `BEARER_TOKEN` (Alertmanager can't do a browser login), while every other route requires an authenticated OIDC session. If OIDC isn't configured, the web UI fails closed (`503`) rather than running open - see [`CONFIGURATION.md`](CONFIGURATION.md#web-ui-auth).
