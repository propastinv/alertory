# alertory

**Alertmanager fires. alertory dedupes, groups, and turns the mess into Slack messages that don't spam your team.**

[![Build and Push](https://github.com/propastinv/alertory/actions/workflows/build.yml/badge.svg)](https://github.com/propastinv/alertory/actions/workflows/build.yml)
[![Go Report](https://img.shields.io/badge/go-1.25-00ADD8?logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

alertory sits between [Prometheus Alertmanager](https://prometheus.io/docs/alerting/latest/alertmanager/) and Slack. It receives Alertmanager's webhook, decides which alerts belong together, waits just long enough to catch a burst, and posts (and later *edits*, never re-posts) one tidy Slack card per incident instead of a wall of duplicate pings. A small built-in web UI lets you manage routing rules without touching YAML or redeploying anything.

It was built to fix a very specific, very common pain: a flapping check or a mass outage turning your `#alerts` channel into hundreds of near-identical messages that everyone mutes within a week.

## Why it's worth using

- **No more alert spam.** A burst of 200 identical alerts becomes one message ("200 hosts firing, 12 resolved") instead of 200. A single alert still gets its own dedicated card - nothing is combined until it actually needs to be.
- **Messages update in place.** When an alert resolves, alertory edits the original Slack message instead of posting a new "RESOLVED" one. Your channel stays a live status board, not a scrolling transcript.
- **Debounced, not delayed forever.** Alerts wait a short window (default 8s, capped at 45s) to see if friends show up before flushing - long enough to catch a burst, short enough that a single critical alert still reaches Slack in seconds.
- **Routing rules live in a real UI**, backed by Postgres - no YAML redeploys to change where an alert goes. Match on any label, route by team, group related alerts together, pick which annotations show up as fields.
- **Fan out to more than one place.** A rule's Slack destination can be a comma-separated list of channel/user IDs - notify a team channel and page a specific person from the same rule, with each destination's message tracked and edited independently.
- **Enrich before you notify.** A rule can call out to an internal HTTP endpoint (e.g. "how many users does this affect?") before rendering the Slack card, so the first message already has the context an on-call engineer needs.
- **Not just for alerts.** `notification_only` rules turn any Alertmanager-shaped webhook (a forwarded email, a one-off notice) into a plain "sent once, no lifecycle" Slack post - no fake "resolved" state required.
- **Ingestion never blocks on Slack.** The webhook handler only writes to Postgres and returns; a separate flush worker talks to Slack out of band. If Slack is slow or down, Alertmanager still gets a fast `200 OK` and doesn't pile on retries.
- **Boring, auditable storage.** Everything - active alerts, rules, in-flight message state - lives in Postgres. No hidden state in memory, no message tracking lost on a restart or a rolling deploy.
- **Single small binary, single container.** One Go binary, one Postgres database. `docker-compose up` and you have a database; point the binary at it and you're running.

## How it works

```
Alertmanager --webhook--> alertory  --dedupe + debounce-->  alert_groups (Postgres)
                              |                                    |
                        matches rule                        flush worker (every 3s)
                        (web UI / DB)                              |
                                                              renders + posts/updates
                                                                    |
                                                                  Slack
```

1. **Ingest** - Alertmanager POSTs to `/api/v1/alerts`. Each alert is matched against your enabled rules and upserted into a debounced group; the handler never talks to Slack directly, so a Slack outage can't slow down or fail alert ingestion.
2. **Dedupe & batch** - alerts sharing a rule and the same grouping labels (default: alertname) land in the same group. A burst above the mass-alert threshold collapses into one combined message; anything smaller gets one message per alert.
3. **Flush** - a background worker claims due groups every few seconds, renders the Slack message, and either posts a new one or edits the existing one in place depending on whether this group has already been notified.
4. **Manage** - the `/ui/rules` web UI (behind SSO) is where you create, edit, and disable routing rules - who matches what, which Slack channel(s), which team, which annotations to surface, whether to batch by anything besides alertname.

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the full data-flow and schema walkthrough.

## Quick start

```bash
git clone https://github.com/propastinv/alertory.git
cd alertory

# spin up Postgres
docker-compose up -d

export DATABASE_URL="postgres://alertory:alertory@localhost:5432/alertory?sslmode=disable"
go run ./cmd/app
```

The service listens on `:8080` by default, serving both the Alertmanager webhook (`/api/v1/alerts`) and the web UI under `/ui/` (`/ui/`, `/ui/rules`, `/ui/settings`). Point Alertmanager's `webhook_configs` at `http://<host>:8080/api/v1/alerts`, then open `/ui/rules` to create your first routing rule - or drop a legacy YAML rule file into `workflows/` before first boot to have it auto-imported.

The web UI (everything under `/ui/`) requires SSO (Keycloak or any OIDC provider) to be configured - see [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md#web-ui-auth) for why, and how to set it up. Without it, the UI serves `503` and only the webhook endpoint works.

### A minimal Alertmanager route

```yaml
receivers:
  - name: alertory
    webhook_configs:
      - url: http://alertory:8080/api/v1/alerts
        send_resolved: true
```

### Running with Docker

Prebuilt images are published to `ghcr.io/propastinv/alertory` on every tagged release (see [`CONTRIBUTING.md`](CONTRIBUTING.md#releasing)):

```bash
docker run -p 8080:8080 \
  -e DATABASE_URL="postgres://alertory:alertory@postgres:5432/alertory?sslmode=disable" \
  ghcr.io/propastinv/alertory:latest
```

## Feature tour

| Feature | What it does |
|---|---|
| Dedup & debounce | Groups alerts by rule + labels, waits a short window before sending, so a flapping check doesn't spam a message per flap |
| Mass-alert batching | A burst above a configurable threshold collapses into one combined message instead of one-per-alert |
| Live-updating messages | Slack messages are edited in place as status changes, instead of piling up new ones |
| Multi-channel routing | One rule can notify several Slack channels/users at once, each with its own independently-tracked message |
| Enrichments | Rules can call out to an HTTP endpoint to attach extra context (e.g. affected user count) before the first send |
| Custom fields | Map any alert annotation/label onto a named field shown on the Slack card |
| Team & target labels | Surface a fixed "Team" and a per-alert "Target" (host, user, etc.) on every message from a rule |
| Grouping by label | Group by any combination of labels, not just alertname, to control what counts as "the same incident" |
| Notification-only rules | Treat a webhook as a one-shot notice (e.g. a forwarded email) with no firing/resolved lifecycle |
| Web UI rule editor | Create, edit, and toggle rules from `/ui/rules` - stored in Postgres, no redeploy needed |
| Legacy YAML import | Existing `workflows/*.yaml` rule files are imported once on first boot into an empty rule set |
| SSO-gated UI, token-gated webhook | The web UI requires OIDC/Keycloak login; the Alertmanager webhook uses its own bearer token |
| Automatic retention | Resolved alert history and stale internal state are cleaned up on a schedule |

## Documentation

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) - how a webhook call turns into a Slack message, and the Postgres schema behind it
- [`docs/RULES.md`](docs/RULES.md) - everything a routing rule can do: matching, grouping, multi-channel, enrichments, notification-only
- [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) - every environment variable, Slack OAuth setup, and SSO setup
- [`charts/alertory`](charts/alertory) - the Helm chart, including how to split the public webhook and the SSO-gated `/ui` admin UI across separate Ingresses
- [`CONTRIBUTING.md`](CONTRIBUTING.md) - local dev setup and how to send a PR

## Running on Kubernetes

```bash
helm repo add alertory https://propastinv.github.io/alertory
helm repo update
helm install alertory alertory/alertory -n alertory --create-namespace -f my-values.yaml
```

See [`charts/alertory/README.md`](charts/alertory/README.md) for required values and how the chart splits the webhook and admin UI into separate Ingresses.

## Status

alertory is a small, focused, actively-used internal tool that's been open-sourced as-is. It intentionally does one thing (Alertmanager → Slack, done well) rather than trying to be a general notification router. Issues and PRs are welcome.

## License

[MIT](LICENSE)
