# CLAUDE.md

Guidance for AI agents (Claude Code and others) working in this repository.

## What this is

A self-hosted dashboard for Azure DevOps teams: a Go **collector** pulls pull requests, commits
(via a bare Git mirror) and work-item revisions into **PostgreSQL**; a Go **web** server embeds
a no-build-step HTML/JS UI and serves read APIs. A second page (`/claude.html`) parses a Claude
Team "spend report" CSV entirely in the browser. **Liquibase** owns the schema. A **Helm chart**
deploys it. See [README.md](README.md) for the tour and [docs/](docs) for depth.

## Map

| Path | What |
| --- | --- |
| `cmd/dashboard`, `internal/web` | Web/API server; static UI in `internal/web/static` (embedded with `go:embed`) |
| `cmd/collector`, `internal/collector`, `internal/ado`, `internal/gitmirror`, `internal/facts` | Collection pipeline |
| `internal/store` | PostgreSQL access (pgx); SQL aggregation for chart buckets |
| `db/changelog` | Liquibase changelog: `001`–`005`; `004-dev-seed.sql` is sample data, context `@dev-seed` only |
| `helm/engineering-activity` | Chart; `deploy/values.example.yaml` is a template, never real values |
| `tests/e2e` | Playwright suites (`dashboard.spec.js`, `claude.spec.js`) |
| `scripts/` | Acceptance scripts, a read-only report query helper, screenshot tooling |

## Skills in this repo

- **`run-locally`**: start the sample-data stack, run Go and browser tests, see a change working.
- **`deploy`**: build images and roll out a release with Helm or Argo CD.
- **`readme-screenshots`**: regenerate `docs/images/*` from sample data only.
- **`activity-dataviz`**: answer questions and build reports from the collected data.

## Rules that matter

- **Never commit real data or environment details.** No real names, emails, hostnames, internal
  repositories, registry names, PATs, `.env*` files (except `.env.example`) or spend reports.
  Screenshots come only from `@dev-seed` fixtures and `scripts/sample-spend-report.mjs`.
  Environment-specific Helm values live in the operator's own GitOps repository.
- **Liquibase changesets are immutable once applied.** Add a new numbered changeset instead of
  editing one. Views and functions use `runOnChange:true`. Never add `runAlways` (the
  `wait-for-schema` init containers wait for `liquibase status` to report up to date, which a
  `runAlways` changeset never does). If an applied changeset must change, add a `--validCheckSum`
  for the old checksum and say why. In formatted SQL, a comment line beginning with
  `-- changeset` is parsed as a directive; don't write one.
- **The UI has a strict Content-Security-Policy** (`script-src 'self'; style-src 'self'`): no
  inline scripts or `<style>`, no CDNs. Keep assets in `internal/web/static`. Rebuild the Go
  binary or image to see static changes, because they are embedded.
- **Counting semantics are a contract** ([architecture](docs/architecture.md),
  [reporting](docs/reporting.md)): unknown statistics stay NULL, never zero; merges are excluded
  from commit counts; PR and commit metrics are never added together; AI markers are a lower bound.
- **Charts:** colors come from the validated categorical palette already in the code; keep
  per-series colors fixed by entity, not rank.
- **Commits:** small and focused; run `go vet ./... && go test ./...` and the relevant
  Playwright suite before committing. CI (`.github/workflows/ci.yml`) runs everything on each PR.
- **Versions:** semantic versioning; `VERSION`, `Chart.yaml` and the `vX.Y.Z` tag must agree.
  Images are built only by `.github/workflows/release.yml`; see `docs/releasing.md`.
