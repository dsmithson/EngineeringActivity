# Releasing

Versions follow [semantic versioning](https://semver.org): `MAJOR.MINOR.PATCH`.

- **PATCH** (`0.4.1`): bug fixes only; no setting, chart value or API changes.
- **MINOR** (`0.5.0`): new features, new optional settings or chart values. Existing values keep working.
- **MAJOR** (`1.0.0`, later `2.0.0`): a breaking change, such as a renamed or removed setting,
  chart value or API field, or a migration that needs manual steps. Before 1.0, minor versions may
  also contain breaking changes; call them out in the release notes.

The version lives in three places that must agree; CI checks them:

| File | Field |
| --- | --- |
| `VERSION` | `0.4.0` |
| `helm/engineering-activity/Chart.yaml` | `version: 0.4.0` and `appVersion: "0.4.0"` |
| The Git tag | `v0.4.0` |

## Cut a release

1. Bump `VERSION` and both fields in `Chart.yaml` in a pull request; merge it once CI is green.
2. Tag the merge commit on `main` and push the tag:

   ```bash
   git switch main && git pull
   git tag -a v0.4.0 -m "v0.4.0"
   git push origin v0.4.0
   ```

3. The **Release images** workflow checks that the tag matches `VERSION` and `Chart.yaml`, builds the
   three images for `linux/amd64`, pushes them to GitHub Container Registry, and creates a GitHub
   release with generated notes:

   - `ghcr.io/dsmithson/engineering-activity-web:0.4.0`
   - `ghcr.io/dsmithson/engineering-activity-collector:0.4.0`
   - `ghcr.io/dsmithson/engineering-activity-migrations:0.4.0`

   Each also gets `:0.4` and `:latest`. The running app shows its version in the page footer and
   in `GET /api/v1/config`.

4. Deploy by setting the three `images.*.tag` values to `0.4.0` (and pinning the chart's Git
   revision to `v0.4.0` if Argo CD reads the chart from this repository).

## Images from main

Every push to `main` also publishes `:main` and `:sha-<12-char commit>` images, useful for trying a
change before tagging. Don't deploy `:main` or `:latest` anywhere you care about: they move.

## Pull requests

The **CI** workflow runs on every pull request and push to `main`:

| Job | What it checks |
| --- | --- |
| Go unit tests | `go vet`, `go test`, JavaScript syntax |
| Helm chart | lint and render with the example values; refuses to render without collection scope; chart version matches `VERSION` |
| PostgreSQL integration tests | applies the Liquibase changelog to a fresh PostgreSQL 17 and runs the collector's end-to-end test against it |
| Compose acceptance + Playwright | `scripts/test-compose.ps1 -BrowserTests`: migrations, sample data, restart persistence, seed isolation, a failing migration blocking startup, the database-backed API, metrics, and the Playwright suite on desktop and mobile Chromium |

The Playwright report is uploaded as an artifact when the acceptance job fails.
