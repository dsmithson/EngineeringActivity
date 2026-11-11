# Frontend container

The web image serves the embedded HTML, JavaScript, and CSS from Go. With `DEMO_MODE=true` it is a database-free, visibly labeled in-memory preview. In Compose and Kubernetes the same image reads PostgreSQL through `/api/v1/*` (see [local-development.md](local-development.md)).

## Build and run the preview

From the repository root with Docker running in Linux-container mode:

```sh
docker build --target web -t ado-dashboard-web:dev .
docker run --rm --name ado-dashboard-preview -p 127.0.0.1:8080:8080 -e DEMO_MODE=true -e ALLOW_UNAUTHENTICATED_DEMO=true ado-dashboard-web:dev
```

Open <http://127.0.0.1:8080>. The explicit demo opt-in permits listening on the container network interface; publish only on host loopback for this unauthenticated local preview. Without the opt-in, unauthenticated demo binds must use a loopback address. Live mode still requires database configuration and both dashboard username/password settings.

The runtime runs as UID/GID 10001. Assets are embedded; Node, a frontend build step, and external CDNs are not needed. Git is not installed in the web image; the separate `collector` target carries Git, and the `migrations` target packages Liquibase with `db/changelog`. The default command remains `dashboard serve`; `web` is the default final stage.

## Configuration and checks

- `LISTEN_ADDR` defaults to `0.0.0.0:8080` inside the image. For a different port, set `LISTEN_ADDR=0.0.0.0:9090` and publish `127.0.0.1:9090:9090`.
- `DEMO_MODE=true` selects in-memory sample data. Compose instead uses Liquibase `dev-seed` fixtures in PostgreSQL.
- `ALLOW_UNAUTHENTICATED_DEMO=true` is required for an unauthenticated fixture preview on a non-loopback bind; it never bypasses live-mode authentication.
- `/healthz` is process liveness; `/readyz` also checks PostgreSQL in live mode. Both are available without browser authentication.
- The built-in Docker healthcheck executes `dashboard healthcheck`, follows the configured port, bypasses HTTP proxies, and needs no curl or credentials.
- SIGTERM/SIGINT stops accepting connections and allows active requests up to ten seconds to finish. Use a stop timeout longer than this grace period.

```sh
docker exec ado-dashboard-preview dashboard healthcheck
docker inspect --format '{{.State.Health.Status}}' ado-dashboard-preview
docker stop --time 15 ado-dashboard-preview
```

Check the page at desktop and narrow widths, change contributor and timezone, inspect the 168 hourly buckets and fixture label, and export CSV. These browser checks complement Go tests; passing tests alone does not prove container or visual acceptance.

These interactions now have a [Playwright suite](browser-tests.md). Install its development dependencies with `npm ci` and `npm run test:e2e:install`, then run `./scripts/test-frontend-container.ps1 -BrowserTests`. Review the attached screenshots in the HTML report for visual sign-off. The user-run container suite passed all 16 cases; the lead also reviewed the desktop/mobile screenshots. The restricted agent environment still cannot launch Playwright itself.

## Version sources and reproducibility

As checked on 2026-10-08, the [official Go image manifest](https://raw.githubusercontent.com/docker-library/official-images/master/library/golang) lists `1.27.1-bookworm`, and the [official Debian image manifest](https://raw.githubusercontent.com/docker-library/official-images/master/library/debian) lists `bookworm-20261005-slim`. The Dockerfile pins those published tags. The Go release feed already lists a newer release, but its corresponding Docker tag was not listed in the checked image manifest.

The build uses the checked-in Go module checksums, read-only module resolution, a static Go binary, and trimmed paths. This is version-pinned rather than byte-for-byte reproducible: tags can be republished and Debian package installation uses current package indexes. Immutable image digests and a package snapshot can be adopted if needed later.

## Validation recorded for Phase 1

Focused command and frontend tests passed with `go test ./cmd/dashboard ./internal/web`. They cover demo bind protection, health status/custom-port handling, and waiting for an active request during shutdown. The local Go tool reports a nonfatal telemetry-file permission warning in this workspace environment.

On 2026-10-08 the user ran the container smoke script successfully outside the sandbox: embedded UI/assets, 168 hourly buckets, contributor filter (360 → 144), UID 10001, Docker health, and graceful stop all passed. Subsequently, all 16 desktop/mobile Playwright cases passed in 23.1 seconds, including timezone/CSV checks, and the lead reviewed both screenshots successfully.

Run `pwsh -File scripts/test-frontend-container.ps1` for the repository's repeatable container smoke check (image build, published assets/API, healthcheck, and cleanup). Docker must be running and accessible to the current process. The agent's sandbox cannot access the engine pipe; the successful run above was performed by the user. The script removes its temporary container, so use the build/run preview commands above for browser checks.
