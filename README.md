# alertory

Ingests Alertmanager webhooks, groups mass alerts, and notifies Slack. Ships a small web UI (dashboard, rule editor, Slack connection status) on the same port as the webhook.

```
export DATABASE_URL="postgres://alertory:alertory@localhost:5432/alertory?sslmode=disable"
```

## Environment variables

- `DATABASE_URL` (required)
- `PORT` - HTTP port (default `8080`); serves both `/api/v1/alerts` and the web UI (`/`, `/rules`, `/settings`)
- `BEARER_TOKEN` - if set, required as `Authorization: Bearer <token>` on the webhook endpoint (unaffected by SSO below)
- `APP_URL` - this app's own public base URL (e.g. `https://alertory.example.com`), used to build both the Slack OAuth redirect and the OIDC redirect URI
- `SLACK_CLIENT_ID`, `SLACK_CLIENT_SECRET` - enables the "Connect Slack" OAuth flow under `/settings`
- `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` - Keycloak realm SSO for the web UI. All three (plus `APP_URL`) must be set for the UI to work at all - see "Web UI auth" below.
- `ALERT_RETENTION` - how long to keep resolved alert history (default `168h` / 7 days)
- `CLEANUP_INTERVAL` - how often the retention cleanup runs (default `1h`)
- `DB_MAX_CONNS`, `DB_MIN_CONNS` - Postgres pool sizing (defaults `25`/`4`)
- `MASS_ALERT_THRESHOLD` - how many alerts becoming unsent together at once triggers a combined Slack message instead of one-per-alert (default `5`)
- `ALERT_DEBOUNCE`, `ALERT_MAX_WINDOW` - burst-detection timing (defaults `8s` / `45s`)

## Web UI auth

The web UI (`/`, `/rules`, `/settings`, and the Slack OAuth callback) is public-facing and requires SSO login via Keycloak (or any spec-compliant OIDC provider) - `OIDC_ISSUER_URL` should be the realm URL, e.g. `https://keycloak.example.com/realms/alertory`. Register `${APP_URL}/auth/callback` as a valid redirect URI on the Keycloak client.

If `OIDC_ISSUER_URL`/`OIDC_CLIENT_ID`/`OIDC_CLIENT_SECRET` aren't all set, the web UI fails closed: every UI route returns 503 instead of running without auth. The `/api/v1/alerts` webhook is never affected either way - it keeps its own `BEARER_TOKEN` check, since Alertmanager can't do a browser login.

This only checks that a login succeeded against your Keycloak realm - it doesn't currently check group/role membership. If you need to restrict access to specific Keycloak groups, that would go in `internal/auth/handlers.go`'s callback, after `idToken.Claims(&claims)`.

## First-time setup after pulling this

This adds `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2` as dependencies. Run `go mod tidy` once (needs network access) before building, so `go.sum` picks up their checksums.

## Workflow rules

Rules used to live in `workflows/*.yaml`. They're now edited from `/rules` in the web UI and stored in Postgres; the YAML files are only read once, on first boot with an empty `workflow_rules` table, to seed the initial set (match labels, channel, target label - `team` and `group_by` are new and start blank).
