# Browser regression tests

Playwright tests the running Go application in desktop (1440 × 1000) and mobile (390 × 844) Chromium viewports. Node and Playwright are development tools only; they are not included in the Go production image.

## Run against the seeded Compose stack (primary)

```powershell
npm ci
npm run test:e2e:install
./scripts/test-compose.ps1 -BrowserTests
```

This runs the suite against the PostgreSQL-backed web service with Liquibase `dev-seed` fixtures (dataset `fixture`, pill **DEV FIXTURE DATA**).

## Run against the in-memory demo container

```powershell
./scripts/test-frontend-container.ps1 -BrowserTests
```

Builds the web image, runs it with `DEMO_MODE=true` on a random loopback port, runs HTTP and browser checks, verifies graceful stop, and removes its container. Tests that need a collection watermark are skipped in demo mode.

## Run against an existing stack

```powershell
$env:PLAYWRIGHT_BASE_URL = 'http://127.0.0.1:8080'
npm run test:e2e
npm run test:e2e:report
```

The target must serve demo or fixture data: the first test asserts `demo` or `dataset === 'fixture'` and fails against live data. The suite never contacts Azure DevOps.

## Coverage and artifacts

`claude.spec.js` covers the upload-only Claude usage page (`/claude.html`) with a synthetic spend-report CSV: page navigation, numbered-account roll-up, date range from the file name, rejection of non-report files, the People picker, the expandable per-model table with worked costs and the rates table, and multi-line tooltips (6 scenarios per viewport).

Eleven scenarios per viewport (22 tests), against `/api/v1/activity` (including range switching 7d/30d/12m/24m and group filtering):

- Real endpoint rendering: eight graphs, 168 hourly buckets, dataset label, AI-marker note on the commits card, no page/console errors, no horizontal overflow.
- Contributor filter: graph and summary totals match the filtered response and reset correctly.
- Timezone switching: labels change to UTC and back while totals stay constant.
- All eight CSV exports: header, filename, 168 UTC rows, and values match the response.
- Loading state, initial failure and recovery, failed refresh retaining charts, empty response.
- Uncollected hours (fixture/live only): buckets at or after `collectedThrough` are marked "not yet collected", the footer shows "Collected through", all three sources are present and fresh.

Expected totals come from the response, not hard-coded counts. Go integration tests cover aggregation correctness. `playwright-report/` holds the HTML report with desktop/mobile screenshots; `test-results/` keeps failure traces. Review screenshots before visual sign-off.

## Validation status

2026-10-15 (Phase 4/5): `./scripts/test-compose.ps1 -BrowserTests` passed all stages and **18/18** Playwright tests against the seeded stack (21.6 s); `./scripts/test-frontend-container.ps1 -BrowserTests` passed 16 tests with 2 watermark tests skipped in demo mode. Desktop and mobile screenshots were reviewed: layout unchanged, no clipping. Earlier Phase 1 history: 16/16 passed on 2026-10-08.

Configuration follows the official [Playwright test configuration](https://playwright.dev/docs/test-configuration) and [project configuration](https://playwright.dev/docs/test-projects) guidance.

2026-10-15 (ranges and groups): `./scripts/test-compose.ps1 -BrowserTests` passed all stages and **22/22** Playwright tests (27.2 s). Weekly/daily axes and the 24-month group view were reviewed in desktop and mobile screenshots against the local live-data stack.
