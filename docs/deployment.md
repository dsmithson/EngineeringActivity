# Deploying to Kubernetes

The Helm chart in [`helm/engineering-activity`](../helm/engineering-activity) installs:

| Component | Kind | Notes |
| --- | --- | --- |
| Web/API | Deployment + Service | Serves the dashboard and `/api/v1/*`; HTTP Basic auth from a Secret |
| Metrics | Service (`<release>-metrics`, port 9090) | Prometheus scrape target; never exposed through the Ingress |
| Collector | CronJob, `5 * * * *` in `Etc/UTC`, `concurrencyPolicy: Forbid` | Run-to-completion; exits nonzero when a source fails |
| Git mirror | PVC (40Gi, kept on uninstall) | Bare mirror of each collected repository |
| Migrations | Job `<release>-migrate-r<revision>` | Liquibase `update`, context filter `live` |
| Backfill | Optional Job | For example `--months 24`; also performs the first full mirror fetch |
| PostgreSQL 17.9 | Optional StatefulSet + PVC | Disable to use an existing database |
| Ingress | Optional | Host-based; TLS when a TLS Secret exists |
| ServiceMonitor | Optional | For the Prometheus Operator |

Start your values file from [`deploy/values.example.yaml`](../deploy/values.example.yaml) and keep
it in your own deployment repository. This repository deliberately holds no environment's values.

## How migrations are ordered

Migrations are an ordinary Job rather than a Helm `pre-install` hook. A hook runs before any
release resources exist, so on a first install it could not wait for the bundled PostgreSQL. Instead:

1. The migration Job's init container waits until PostgreSQL accepts TCP connections, then runs
   `liquibase update`. Its name includes the Helm revision, so each install or upgrade applies the
   changelog baked into that revision's `migrations` image.
2. The web Deployment, collector CronJob and backfill Job each have a `wait-for-schema` init
   container (the same image) that runs `liquibase status` until the database is up to date.
   Workloads never start against an older schema.
3. Liquibase's `DATABASECHANGELOGLOCK` serializes concurrent updates. If a migration fails, the
   Job fails, workloads stay in `Init`, and the previous web Pods keep serving.

The context filter is `live`. Development sample data is tagged `@dev-seed` and never runs in a
cluster. Do not set `migrations.contextFilter` to `dev-seed`.

### With Argo CD

Argo CD renders charts with `helm template`, where the release revision is always 1, so the
migration Job's name never changes. The Job therefore carries Argo `Sync` hook annotations with
`BeforeHookCreation`: Argo deletes and re-runs it on every sync, and Helm ignores the annotations.
Liquibase `update` is idempotent, so the extra runs are harmless.

Point the Application at `helm/engineering-activity` in this repository (a pinned revision or
`main`) and at your values file with a second, `ref: values` source. To adopt a release you first
installed by hand, set `helm.releaseName` to the existing release name, and keep `fullnameOverride`
and `nameOverride` matching what is running: Deployment and StatefulSet selectors are immutable.

## 1. Build and push images

From the repository root, tag every image with the commit. Never use `latest`:

```bash
tag=$(git rev-parse --short=12 HEAD)
reg=registry.example.com/engineering
for target in web collector migrations; do
  docker build --platform linux/amd64 --target $target -t "$reg/engineering-activity-$target:$tag" .
  docker push "$reg/engineering-activity-$target:$tag"
done
```

Set `images.registry` and the three `images.*.tag` values. The chart refuses to render without
explicit tags. Images are amd64; on a mixed-architecture cluster add a
`kubernetes.io/arch: amd64` node selector to `web`, `collector` and `postgresql`.

## 2. Namespace and Secrets

Secrets are created outside Helm and never committed.

```bash
kubectl create namespace activity
# The database password is interpolated into DATABASE_URL, so keep it URL-safe (hex is ideal).
kubectl -n activity create secret generic activity-secrets \
  --from-literal=database-password="$(openssl rand -hex 16)" \
  --from-literal=azure-devops-pat='<read-only PAT>' \
  --from-literal=dashboard-username='<user>' \
  --from-literal=dashboard-password='<password>'
```

Add a `docker-registry` Secret too if your registry is private, and list it in `images.pullSecrets`.

### Azure DevOps PAT

Create it at `https://dev.azure.com/<your-org>/_usersSettings/tokens`:

- **Organization:** only the one you collect (not "All accessible organizations").
- **Scopes:** Custom defined → **Code: Read** and **Work Items: Read**. Nothing else: Code Read
  covers repository metadata, pull requests and `git fetch` of the mirror; Work Items Read covers
  WIQL, revision history and workflow states.
- **Expiration:** the longest your policy allows; note the date. When it expires the collector
  fails with `error_class=auth` and `activity_sync_last_success_timestamp_seconds` stops advancing.

The collector reads exactly what the PAT's owner can read. Rotate the PAT with
`kubectl create secret … --dry-run=client -o yaml | kubectl apply -f -`; the next run picks it up.

For a throwaway install only, `secrets.create=true` renders a Secret from `secrets.values.*`
passed with `--set`. Never put real values in a file.

## 3. Install or upgrade

```bash
helm upgrade --install activity helm/engineering-activity -n activity -f my-values.yaml
kubectl -n activity get pods,jobs,cronjobs
kubectl -n activity logs job/activity-migrate-r1
```

Validate without touching a cluster:

```bash
helm lint helm/engineering-activity -f my-values.yaml
helm template activity helm/engineering-activity -n activity -f my-values.yaml
```

No local Helm? Run the pinned image, for example
`docker run --rm -v "$PWD:/repo" -v "$HOME/.kube:/root/.kube:ro" alpine/helm:3.16.2 upgrade …`.

Upgrades use the same command with new image tags. The new migration Job runs first and new web
Pods wait for it.

## 4. Initial history: backfill

The scheduled collector bootstraps the previous seven days automatically. For long-range
reporting, run the backfill Job. It shares the ReadWriteOnce mirror volume with the CronJob, so
suspend the CronJob while it runs:

```bash
helm upgrade activity helm/engineering-activity -n activity -f my-values.yaml \
  --set collector.suspend=true --set backfill.enabled=true --set backfill.months=24 --set backfill.id=1
kubectl -n activity logs -f job/activity-backfill-1
# When the Job completes, re-run the plain upgrade to resume the CronJob:
helm upgrade activity helm/engineering-activity -n activity -f my-values.yaml
```

The first backfill also clones the full mirror (a large monorepo can take hours). The Job allows
14 hours, and the collector stops 10 minutes earlier to record its outcome. To run it again, bump
`backfill.id`. A one-off scheduled-style run is
`kubectl create job --from=cronjob/activity-collector activity-manual-<n>`.

## 5. Seeding a cluster database from a local backfill

If you already ran a backfill locally (`compose.live.yaml`, see
[local development](local-development.md)), copying it avoids re-reading history from Azure DevOps.

```bash
# 1. Dump the local live database (data only; Liquibase creates the schema).
docker compose --env-file .env.live -f compose.yaml -f compose.live.yaml exec -T postgres \
  pg_dump -U activity -d activity --data-only --exclude-table=databasechangelog --exclude-table=databasechangeloglock -Fc > activity-live.dump
# 2. Install with the CronJob suspended so migrations create the schema but nothing collects yet.
helm upgrade --install activity helm/engineering-activity -n activity -f my-values.yaml --set collector.suspend=true
# 3. Restore into the bundled PostgreSQL. Pre-seeded rows such as the 'unassigned'/'unknown'
#    identities already exist; clear them and restore in one transaction.
kubectl -n activity exec -i activity-postgres-0 -- \
  sh -c 'psql -U activity -d activity -c "DELETE FROM identities"; pg_restore -U activity -d activity --data-only --disable-triggers --single-transaction' < activity-live.dump
# 4. Resume hourly collection from the restored watermarks.
helm upgrade activity helm/engineering-activity -n activity -f my-values.yaml
```

Both databases must be at the same changelog version: run the same image tag locally before dumping.

## 6. Checking sync health

- **Collector logs** are JSON lines: `kubectl -n activity logs job/<job>`. Each source logs
  `outcome`, `window_start`/`window_end`, `fetched`/`inserted`/`updated`/`unchanged`,
  `unknown_stats`, `retries`/`throttles` and a bounded `error_class` (`auth`, `throttled`, `git`,
  `database`, `config`, …). No tokens or payloads are logged.
- **Status API:** `kubectl -n activity port-forward svc/activity-web 8080:80`, then
  `GET /api/v1/status` (Basic auth): per-source watermark, coverage start, last success and outcome.
- **Metrics** (cluster-internal): `kubectl -n activity port-forward svc/activity-metrics 9090`,
  then `GET /metrics`. Collector values come from summaries persisted in PostgreSQL, so they
  survive the batch Pod exiting.

Useful alerts:

```promql
activity_exporter_up == 0                                         # web cannot read PostgreSQL
time() - activity_sync_last_success_timestamp_seconds > 3 * 3600  # a source stopped syncing
activity_sync_last_outcome{outcome="failed"} == 1
increase(activity_source_throttles_total[1h]) > 20
```

## 7. Prometheus scraping

With the Prometheus Operator (for example kube-prometheus-stack), set
`monitoring.serviceMonitor.enabled=true`; add `monitoring.serviceMonitor.labels` if your
Prometheus selects ServiceMonitors by label. The metrics Service also carries `prometheus.io/*`
annotations for annotation-based setups.

The metrics endpoint has no authentication. It exposes only operational aggregates; labels are
limited to source, repository name, outcome, route and status class, never people, PRs, SHAs or
run ids. It is reachable only inside the cluster.

## 8. Volumes, retention and backups

- **Prefer block storage for PostgreSQL.** Databases on NFS are prone to corruption.
- **Check your storage class's reclaim policy.** With `Delete`, deleting a PVC destroys its volume.
  The chart leaves both PVCs in place on uninstall; never delete them in a routine reinstall.
- **PostgreSQL PVC** (`data-<release>-postgres-0`) is kept by the StatefulSet on uninstall.
- **Git mirror PVC** has `helm.sh/resource-policy: keep`. It is a cache: losing it costs a re-clone.
- **A PVC is not a backup.** Schedule a logical backup, for example a nightly:

  ```bash
  kubectl -n activity exec activity-postgres-0 -- pg_dump -U activity -d activity -Fc > activity-$(date +%Y%m%d).dump
  ```

  Restore into an empty chart-created database with `pg_restore --clean --if-exists -d activity`,
  then let the collector catch up. Facts can be re-collected from Azure DevOps, except history it
  no longer exposes (deleted branches and work items).

## 9. Using an existing PostgreSQL server

```yaml
postgresql:
  enabled: false
database:
  host: postgres.example.internal
  port: 5432
  name: activity
  user: activity
  sslmode: require
```

Create the database and an owner role first. Liquibase creates all tables, plus a
`dashboard_read` NOLOGIN role for read-only report users. The Secret's `database-password` is
that user's password.

## Values reference (most used)

| Key | Default | Purpose |
| --- | --- | --- |
| `images.registry`, `images.*.repository/tag`, `images.pullSecrets` | none / required tags | Images built from this repo |
| `secrets.existingSecret`, `secrets.keys.*` | `activity-secrets` | Secret name and key names |
| `azureDevOps.organization/project/repositories` | required | Collection scope |
| `azureDevOps.workItemTypes/completionStates` | `[Task]` / QA build, ready to test, closed | What counts as a completed task |
| `web.branding.*` | Engineering / Activity / Your repositories | Header and footer text and link |
| `postgresql.enabled`, `postgresql.storage.*` | `true`, 10Gi | Bundled database |
| `database.*` | `activity` / 5432 / `disable` | Connection settings |
| `collector.schedule/timeZone/suspend` | `5 * * * *` / `Etc/UTC` / false | CronJob |
| `collector.overlap/bootstrap/reconcileEvery/reconcileWindow` | 2h / 168h / 24h / 168h | Window planning |
| `collector.mirror.*` | 40Gi RWO, kept | Git mirror volume |
| `backfill.enabled/months/id` | false / 24 / "1" | Historical Job |
| `ingress.*` | disabled | Host, class, TLS Secret |
| `web.auth.enabled` | true | Basic auth (disable only behind an authenticating ingress) |
| `monitoring.serviceMonitor.enabled`, `monitoring.annotations` | false / true | Scrape configuration |
| `migrations.contextFilter` | `live` | Never `dev-seed` |
| `fullnameOverride`, `nameOverride` | empty | Resource names and the `app.kubernetes.io/name` label |
