# alertory

Ingests Alertmanager webhooks, groups mass alerts, and notifies Slack. Ships a small web UI (dashboard, rule editor, Slack connection status) on the same port as the webhook.

```
export DATABASE_URL="postgres://alertory:alertory@localhost:5432/alertory?sslmode=disable"
```

## Environment variables

- `DATABASE_URL` (required)
- `PORT` - HTTP port (default `8080`); serves both `/api/v1/alerts` and the web UI (`/`, `/rules`, `/settings`)
- `BEARER_TOKEN` - if set, required as `Authorization: Bearer <token>` on the webhook endpoint
- `SLACK_CLIENT_ID`, `SLACK_CLIENT_SECRET`, `APP_URL` - enables the "Connect Slack" OAuth flow under `/settings`
- `ALERT_RETENTION` - how long to keep resolved alert history (default `168h` / 7 days)
- `CLEANUP_INTERVAL` - how often the retention cleanup runs (default `1h`)
- `DB_MAX_CONNS`, `DB_MIN_CONNS` - Postgres pool sizing (defaults `25`/`4`)

## Workflow rules

Rules used to live in `workflows/*.yaml`. They're now edited from `/rules` in the web UI and stored in Postgres; the YAML files are only read once, on first boot with an empty `workflow_rules` table, to seed the initial set (match labels, channel, target label - `team` and `group_by` are new and start blank).
