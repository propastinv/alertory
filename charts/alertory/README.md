# alertory Helm chart

Deploys [alertory](https://github.com/propastinv/alertory) - an Alertmanager-to-Slack dedup/routing service - to Kubernetes.

## Install

From the hosted chart repo (published via GitHub Pages on every tagged release - see `.github/workflows/helm-release.yml`):

```bash
helm repo add alertory https://propastinv.github.io/alertory
helm repo update
helm install alertory alertory/alertory -n alertory --create-namespace -f my-values.yaml
```

Or straight from a checked-out clone of this repo:

```bash
helm dependency update charts/alertory   # only needed if postgresql.enabled=true
helm install alertory charts/alertory -n alertory --create-namespace -f my-values.yaml
```

## Required values

At minimum, set the Postgres connection string and (if you want the admin UI reachable, which requires SSO) the OIDC/Slack settings:

```yaml
config:
  APP_URL: "https://alertory.example.com"

secret:
  data:
    DATABASE_URL: "postgres://alertory:alertory@my-postgres:5432/alertory?sslmode=disable"
    BEARER_TOKEN: "change-me"
    OIDC_ISSUER_URL: "https://keycloak.example.com/realms/alertory"
    OIDC_CLIENT_ID: "alertory"
    OIDC_CLIENT_SECRET: "xxxxx"
    SLACK_CLIENT_ID: "1234567890.1234567890"
    SLACK_CLIENT_SECRET: "xxxxx"
```

See the main [`docs/CONFIGURATION.md`](../../docs/CONFIGURATION.md) for what every one of these does.

For anything beyond a quick start, prefer `secret.existingSecret` (pointing at a Secret you manage via sealed-secrets/external-secrets/etc.) over `secret.data`, since values passed through `secret.data` land in Helm's release history in plaintext.

## Splitting the webhook and admin UI across two Ingresses

alertory serves two logically separate surfaces on the same port: the public Alertmanager webhook (`/api/v1/alerts`, its own bearer-token auth) and the SSO-gated admin UI (everything under `/ui/`). This chart creates them as **two independent `Ingress` resources** so you can put different hosts/annotations on each - e.g. a public DNS name with no extra auth for the webhook, and an internal-only ingress class / IP allowlist / auth-gateway annotation for `/ui`:

```yaml
ingress:
  webhook:
    enabled: true
    className: nginx
    host: alertory.example.com
    path: /api/v1/alerts

  ui:
    enabled: true
    className: nginx-internal
    host: alertory-admin.example.com
    path: /ui
    annotations:
      nginx.ingress.kubernetes.io/whitelist-source-range: "10.0.0.0/8"
```

Whichever host resolves to `/ui` in a browser must match `config.APP_URL` - it's used to build the OIDC and Slack OAuth redirect URLs.

## Bundled Postgres

Set `postgresql.enabled: true` to deploy [bitnami/postgresql](https://github.com/bitnami/charts/tree/main/bitnami/postgresql) as a dependency and have `DATABASE_URL` wired up automatically - convenient for a demo/eval, but it has no backup/HA story. For anything real, point `secret.data.DATABASE_URL` (or `secret.existingSecret`) at a managed Postgres instance instead and leave `postgresql.enabled: false`.

## Values

See [`values.yaml`](values.yaml) - every field has an inline comment.
