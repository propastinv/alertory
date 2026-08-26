# Configuration

alertory is configured entirely through environment variables - no config file.

## Core

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | *(required)* | Postgres connection string, e.g. `postgres://alertory:alertory@localhost:5432/alertory?sslmode=disable`. |
| `PORT` | `8080` | HTTP port. Serves both `/api/v1/alerts` (the Alertmanager webhook) and the web UI (`/`, `/rules`, `/settings`). |
| `BEARER_TOKEN` | *(none)* | If set, required as `Authorization: Bearer <token>` on the webhook endpoint. Independent of the web UI's SSO - set this so anyone who can reach the webhook route can't post arbitrary alerts. |
| `APP_URL` | *(required for SSO/Slack OAuth)* | This app's own public base URL (e.g. `https://alertory.example.com`). Used to build both the Slack OAuth redirect and the OIDC redirect URI. |

## Dedup & batching

| Variable | Default | Description |
|---|---|---|
| `ALERT_DEBOUNCE` | `8s` | How long a group waits after its last event before flushing, to catch more alerts arriving in the same burst. Re-armed by every new event. |
| `ALERT_MAX_WINDOW` | `45s` | The debounce timer never pushes flushing out past this long after the group's *first* event - keeps a continuous storm from being delayed forever. |
| `MASS_ALERT_THRESHOLD` | `5` | If more than this many alerts become "unsent" together in one flush, they're combined into a single Slack message instead of one-per-alert. |

## Retention & cleanup

| Variable | Default | Description |
|---|---|---|
| `ALERT_RETENTION` | `168h` (7 days) | How long resolved alert history (`active_alerts` / `alert_events`) is kept before being deleted. |
| `CLEANUP_INTERVAL` | `1h` | How often the retention cleanup pass runs. Runs once immediately on startup too, so a fresh deploy doesn't accumulate unbounded rows until the first tick. |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `25` / `4` | Postgres connection pool sizing. |

## Slack

Slack delivery is authenticated via a workspace-level OAuth token, connected once from the `/settings` page in the web UI (not per-rule):

| Variable | Description |
|---|---|
| `SLACK_CLIENT_ID`, `SLACK_CLIENT_SECRET` | Your Slack app's credentials. Enables the "Connect Slack" button under `/settings`, which walks through OAuth and stores the resulting access token in Postgres. |

Your Slack app needs `chat:write` scope (to post and update messages) and its OAuth redirect URL set to `${APP_URL}/providers/oauth2/slack`.

## Web UI auth

The web UI (`/`, `/rules`, `/settings`, and the Slack OAuth callback) is public-facing and requires SSO login via an OIDC-compliant provider (Keycloak is what it's built against, but any spec-compliant provider should work):

| Variable | Description |
|---|---|
| `OIDC_ISSUER_URL` | The realm/issuer URL, e.g. `https://keycloak.example.com/realms/alertory`. |
| `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` | Your OIDC client's credentials. |

Register `${APP_URL}/auth/callback` as a valid redirect URI on the OIDC client.

**All three of `OIDC_ISSUER_URL` / `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET`, plus `APP_URL`, must be set for the UI to work at all.** If any are missing, the web UI fails closed: every UI route returns `503` rather than running without auth. The `/api/v1/alerts` webhook is never affected either way - it only ever checks its own `BEARER_TOKEN`, since Alertmanager can't do a browser login.

If the variables *are* set but OIDC discovery fails at startup (e.g. the provider is unreachable), the process exits rather than silently disabling the UI - since SSO was explicitly requested, a hard failure that gets noticed (and retried by whatever supervises the process) beats a quiet fallback to "disabled."

SSO here only checks that a login succeeded against your provider - it doesn't check group/role membership. To restrict access to specific groups, that logic goes in `internal/auth/handlers.go`'s callback, after `idToken.Claims(&claims)`.

## Example `.env`

```bash
DATABASE_URL=postgres://alertory:alertory@localhost:5432/alertory?sslmode=disable
PORT=8080
BEARER_TOKEN=change-me
APP_URL=https://alertory.example.com

SLACK_CLIENT_ID=1234567890.1234567890
SLACK_CLIENT_SECRET=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx

OIDC_ISSUER_URL=https://keycloak.example.com/realms/alertory
OIDC_CLIENT_ID=alertory
OIDC_CLIENT_SECRET=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx

ALERT_RETENTION=168h
CLEANUP_INTERVAL=1h
ALERT_DEBOUNCE=8s
ALERT_MAX_WINDOW=45s
MASS_ALERT_THRESHOLD=5
```
