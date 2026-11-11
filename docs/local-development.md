# Local development: Compose, Liquibase, fixtures, and live data

Two local stacks share `compose.yaml`:

| Stack | Command | Liquibase context | Data |
| --- | --- | --- | --- |
| Development (default) | `docker compose up --build -d` | `dev-seed` | Refreshable fixtures in PostgreSQL, labeled **DEV FIXTURE DATA** |
| Local live | `docker compose --env-file .env.live -f compose.yaml -f compose.live.yaml up -d --build web` | `live` | Real Azure DevOps facts written by the collector |

Both run PostgreSQL, a one-shot Liquibase migration, and the Go web/API server. PostgreSQL must be healthy before Liquibase runs; Liquibase must exit 0 before web starts. The web app reads PostgreSQL (`DATABASE_URL`); it no longer uses in-memory demo data in Compose. `DEMO_MODE=true` remains only for the database-free container preview ([frontend-container.md](frontend-container.md)).

## First startup (development fixtures)

Run from the repository root with Docker Desktop's Linux engine running. If `.env` does not exist, copy `.env.example` to `.env` and set `POSTGRES_PASSWORD`. Do not replace an existing `.env` or commit it.

| Setting | Default / purpose |
| --- | --- |
| `POSTGRES_DB` / `POSTGRES_USER` | `activity`; simple letters/digits/underscores |
| `POSTGRES_PASSWORD` | Required; letters and digits only, because it is embedded in the web `DATABASE_URL` |
| `WEB_PORT` | `8080`; dashboard and API, host loopback only |
| `METRICS_PORT` | `9090`; Prometheus `/metrics`, host loopback only |
| `LIQUIBASE_CONTEXTS` | `dev-seed`; any other value (e.g. `live`) skips fixtures |

```powershell
docker compose up --build -d
docker compose ps -a
docker compose logs liquibase
```

Open <http://127.0.0.1:8080>. Expected: `postgres` and `web` running/healthy, `liquibase` exited 0. The web service sets `ALLOW_UNAUTHENTICATED=true` only because both ports are published on host loopback; anywhere shared, set `DASHBOARD_USERNAME`/`DASHBOARD_PASSWORD` and terminate HTTPS at ingress. PostgreSQL is not published in this stack; use `docker compose exec` for queries.

Useful endpoints: `/api/v1/activity` (`?user=`, `group=`, `repo=`, `bucket=hour|day|week|month`, `from=`, `to=`, `tz=`), `/api/v1/identities`, `/api/v1/status`, `/api/v1/groups`, `/api/v1/repositories`, `/healthz`, `/readyz`, and `http://127.0.0.1:9090/metrics`.

## Development seed data

`db/changelog/changes/004-dev-seed.sql` contains two changesets tagged `context:@dev-seed`. The `@` makes the context **explicit-only**: Liquibase 4.33 runs them only when the context filter names `dev-seed`. With no filter, or with `live`, they are skipped (verified by `scripts/test-compose.ps1` on fresh databases).

- `004-dev-seed-function` creates `dev_seed_refresh()`.
- `004-dev-seed-refresh` is `runAlways`: every `liquibase update` with `dev-seed` deletes **only fixture-owned rows** and regenerates them relative to the current UTC hour, so the rolling seven-day dashboard never goes empty. Re-running produces identical row counts.

Fixture ownership: repository `00000000-0000-4000-8000-00000000f1c5` (`FixtureRepo`, `is_fixture`), work items in organization `fixture` / project `FIXTURE`, identities `fixture:*` (kind `fixture`), aliases with source `fixture`, checkpoints scoped to those, collection run `00000000-0000-4000-8000-0000000f1c50`, and `app_metadata('dataset','fixture')`. Groups `fpga` and `software` are created if missing but never deleted. Live rows are never modified.

Contents: four people (two in `fpga`, two in `software`) plus an unmapped Git author (`Eve Contractor (Git)`) and unassigned tasks; hourly activity for the last 168 hours with nights, weekends and empty hours; merges, binary (`partial_binary`) and `unavailable` statistics; Claude/Copilot/Codex co-author trailers; per-file rows across `hdl`, `fpga_project`, `software`, `docs`, `generated_vendor`, `binary_asset`; completed PRs (one with unavailable statistics); a task reassigned and reopened (`399999`: created by Ada, completed by Ben, reopened and completed again by Ada); and ~24 months of daily history in which the FPGA group's commit volume and AI-marker share rise, for exercising long-range reports offline.

To refresh fixtures on a running stack, rerun the migration (see below). The collector refuses to write into a database containing fixtures (`--allow-fixture-db` overrides; don't).

## Local live data

`compose.live.yaml` sets project name `activity-live` (separate volume), context `live`, and publishes PostgreSQL on `127.0.0.1:${POSTGRES_HOST_PORT:-55433}` so a natively run collector can write to it. Keep `.env.live` (ignored) with its own password and ports, e.g. `WEB_PORT=8081`, `METRICS_PORT=9091`, `LIQUIBASE_CONTEXTS=live`.

```powershell
docker compose --env-file .env.live -f compose.yaml -f compose.live.yaml up -d --build web
```

Run the collector natively (its Git mirror can be large; keep it on a roomy disk) with `DATABASE_URL` pointing at the published port. A containerized collector is also available under the `collector` profile (`docker compose --profile collector run --rm collector`), using the `mirrors` volume and `AZURE_DEVOPS_PAT`.

## Stop and restart without deleting data

```powershell
docker compose down
docker compose up -d
```

Named volumes persist. Do not use `down -v` for routine work: it deletes database data. Changing `POSTGRES_*` after initialization does not alter existing roles/databases.

## Add and apply a migration

Keep applied changesets immutable. Add a new numbered SQL file in `db/changelog/changes/` with `--liquibase formatted sql` and a unique `--changeset dashboard:...` header, and add an `<include>` to `db/changelog/db.changelog-master.xml`. Replaceable objects (views/functions) use `runOnChange:true`. Compose mounts `db/changelog` read-only; the Dockerfile `migrations` target bakes the same directory into `/liquibase/changelog` for Kubernetes.

Editing a mounted file does not rerun an exited init container. After changing migrations (or to refresh fixtures):

```powershell
docker compose stop web
docker compose up -d --wait postgres
docker compose up --no-deps --force-recreate --abort-on-container-exit --exit-code-from liquibase liquibase
# Continue only if the preceding command exits with code 0:
docker compose up -d web
```

`docker compose restart web` does not rerun migrations. A failed migration leaves web unstarted on first startup; fix forward with a new changeset rather than editing applied ones or clearing checksums.

## Inspect the database

```powershell
docker compose exec postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT key, value FROM app_metadata;"'
docker compose exec postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT id, filename, exectype FROM databasechangelog ORDER BY orderexecuted;"'
docker compose exec postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT * FROM rpt_collection_status;"'
```

The idle lock is `databasechangeloglock.locked = false`. `docker compose config --quiet` validates without printing interpolated credentials.

## Automated acceptance

```powershell
./scripts/test-compose.ps1 -BrowserTests
```

Uses a generated project, environment, temporary changelog copy, and disposable volume. It validates: fresh migrate + seed (changeset counts derived from the changelog), restart persistence, reseed idempotency, **no seed with an empty filter or `live`**, an additional changeset, a failing migration blocking web, the database-backed API (contributor filter and a 24-month monthly `fpga` group query), `/metrics` (`activity_exporter_up 1` and fixture watermarks), the collector container refusing the fixture database, and the Playwright suite against the seeded stack. Only the test-owned stack/volume is removed.
