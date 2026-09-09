# Contributing

Thanks for considering a contribution to alertory. It's a small, focused project - keep that in mind for the size of change you propose; a small, well-explained PR is much easier to review and merge than a large one.

## Local setup

Requirements: Go 1.25+, Docker (for Postgres).

```bash
git clone https://github.com/propastinv/alertory.git
cd alertory

docker-compose up -d   # starts Postgres on localhost:5432

export DATABASE_URL="postgres://alertory:alertory@localhost:5432/alertory?sslmode=disable"
go run ./cmd/app
```

The service starts on `:8080`. The `/api/v1/alerts` webhook works immediately (no `BEARER_TOKEN` set means no auth check). The web UI needs OIDC configured to be reachable at all - see [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md#web-ui-auth) - so for UI work, either stand up a local Keycloak or work against the handlers directly.

You can send a test alert straight to the webhook without a real Alertmanager:

```bash
curl -X POST localhost:8080/api/v1/alerts \
  -H 'Content-Type: application/json' \
  -d '{
    "status": "firing",
    "alerts": [{
      "status": "firing",
      "fingerprint": "test-1",
      "labels": {"alertname": "TestAlert", "job": "demo"},
      "annotations": {"summary": "just a test"}
    }]
  }'
```

## Making changes

- `go build ./...` and `go vet ./...` should pass before you open a PR.
- `gofmt -l .` should print nothing - run `gofmt -w` on anything it flags.
- Match the existing comment style: this codebase leans on doc comments that explain *why* a piece of logic exists, not just what it does. New non-trivial logic should get the same treatment.
- Schema changes go in `internal/db/migrate.go` as a new, additive migration - see the existing entries for the pattern. Never edit a migration that's already shipped.
- If you touch rule matching, grouping, or Slack rendering, skim [`docs/RULES.md`](docs/RULES.md) and [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) first and update them if behavior changes.

## Opening a PR

- Keep it focused - one logical change per PR.
- Describe the *problem* being solved, not just the change, especially for anything touching dedup/batching/delivery timing - that logic has non-obvious edge cases (see the comments in `internal/workflows/batching.go` and `internal/workflows/flush.go`) and reviewers need the reasoning to sanity-check it.
- Note any manual testing you did (e.g. "sent N bursts of test alerts through a local Alertmanager and confirmed one combined message").

## Releasing

Releases are cut by pushing a tag, not by merging anything special:

```bash
git tag v0.0.23
git push origin v0.0.23
```

That single push (must point at a commit already on `main`) triggers two workflows:

- [`build.yml`](.github/workflows/build.yml) builds and pushes `ghcr.io/propastinv/alertory:0.0.23` (and re-tags `:latest`).
- [`helm-release.yml`](.github/workflows/helm-release.yml) bumps [`charts/alertory/Chart.yaml`](charts/alertory/Chart.yaml)'s `appVersion` to `0.0.23` and its own `version` (a patch bump, independent of the app's version number - it has to keep increasing on its own regardless of what the app is tagged, or `helm repo update`/Renovate/anyone pinned to "latest" chart version won't see anything new to pick up), commits that bump to `main`, and publishes the chart to the [Helm repo](charts/alertory/README.md).

Both are gated behind [`ci.yml`](.github/workflows/ci.yml) passing on `main` first (build/vet/gofmt/helm-lint) - make sure the commit you're tagging is one where that's green.

## Reporting bugs / requesting features

Open a GitHub issue. For a bug, include what you sent Alertmanager (or the raw webhook payload), what you expected in Slack, and what you got instead - the mismatch is usually the whole story with this kind of pipeline.
