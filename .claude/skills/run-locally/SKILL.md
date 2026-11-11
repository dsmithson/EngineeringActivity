---
name: run-locally
description: Start Engineering Activity locally with sample data, run its Go and Playwright tests, and check a change in a real browser. Use when asked to run, test, debug, or screenshot the app, or to confirm a UI/API/migration change works.
---

# Run and test Engineering Activity locally

Requires Docker (Linux containers), Go (version in `go.mod`), and Node for Playwright.

## Fastest loop: no database

```bash
go build -o /tmp/dash ./cmd/dashboard
DEMO_MODE=true LISTEN_ADDR=127.0.0.1:8099 /tmp/dash      # in-memory sample data
```

Good for UI work on `/` and for `/claude.html` (which never needs a database). Static files are
embedded, so **rebuild after every change** to `internal/web/static`.

## Realistic loop: PostgreSQL + Liquibase + sample data

```bash
[ -f .env ] || cp .env.example .env    # set POSTGRES_PASSWORD (letters/digits) if new
docker compose up --build -d --wait web
```

Open http://127.0.0.1:8080 (pill shows **DEV FIXTURE DATA**). Liquibase applies the schema and
`@dev-seed` fixtures, re-anchored to the current hour on every start. After changing a
changeset, rerun migrations without deleting data:

```bash
docker compose stop web && docker compose up --no-deps --force-recreate --exit-code-from liquibase liquibase && docker compose up -d web
```

Don't use `docker compose down -v` unless you mean to wipe the database.

Sample Claude spend report: `node scripts/sample-spend-report.mjs /tmp/sample.csv`, then drop the
file on `/claude.html`. Never use a real export in tests or screenshots.

## Tests

```bash
go vet ./... && go test ./...
PLAYWRIGHT_BASE_URL=http://127.0.0.1:8080 npx playwright test        # against a running stack
./scripts/test-compose.ps1 -BrowserTests        # full isolated acceptance (PowerShell)
```

- Playwright runs desktop and mobile Chromium; first time: `npm ci && npx playwright install chromium`.
- Some `dashboard.spec.js` tests skip in demo mode (they need the database); that is expected.
- `TEST_DATABASE_URL` (a disposable, Liquibase-initialized database) enables PostgreSQL
  integration tests in `go test`; they truncate tables.

## Checking a change visually

Write a short Playwright script (see `scripts/readme-screenshots.mjs` for the pattern), screenshot
desktop (1440 wide) and mobile (390 wide), and look at the images: check labels, overflow
(`document.documentElement.scrollWidth <= innerWidth`) and console errors. The CSP blocks injected
`<style>` tags; set `element.style` instead.

## Gotchas

- Work-area colors and categorical palettes are validated for the dark surface; reuse them.
- The collector refuses to write into a database containing fixtures (`--allow-fixture-db` overrides; don't).
- Windows Git Bash: prefix `docker run -v` commands with `MSYS_NO_PATHCONV=1` and pass `cygpath -w` paths.
