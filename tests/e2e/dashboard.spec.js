import { test, expect } from '@playwright/test';

const activityRoute = '**/api/v1/activity?*';
const activityPath = '/api/v1/activity';
const charts = [
  ['pr-count', [['prCreated', 'Created'], ['prCompleted', 'Completed']]],
  ['pr-lines', [['prAdded', 'Added'], ['prDeleted', 'Deleted']], 'prUnknown'],
  ['pr-files', [['prFiles', 'Files']], 'prUnknown'],
  ['pr-commits', [['prCommits', 'Commits']], 'prUnknown'],
  ['commits', [['commits', 'Commits']]],
  ['commit-lines', [['added', 'Added'], ['deleted', 'Deleted']], 'commitUnknown'],
  ['commit-files', [['files', 'Files']], 'commitUnknown'],
  ['tasks', [['taskCreated', 'Created'], ['taskCompleted', 'Completed']]],
];
const summaryKeys = ['prCompleted', 'commits', 'taskCreated', 'taskCompleted'];
const total = (data, key) => data.buckets.reduce((sum, bucket) => sum + bucket[key], 0);
const formatted = value => new Intl.NumberFormat('en-US').format(value);

async function expectData(page, data) {
  expect(data.buckets).toHaveLength(168);
  await expect(page.locator('#summary strong')).toHaveText(summaryKeys.map(key => formatted(total(data, key))));
  // Work-area charts (2) appear when the API provides work areas (database-backed only).
  await expect(page.locator('svg.plot')).toHaveCount(charts.length + (data.workAreas?.length ? 2 : 0));
  for (const [id, series] of charts) {
    await expect(page.locator(`#${id} .legend span`)).toHaveText(series.map(([key, label]) => `${label} · ${formatted(total(data, key))}`));
    await expect(page.locator(`#${id} svg title`)).toHaveCount(168);
  }
}

async function openDashboard(page) {
  const responsePromise = page.waitForResponse(response => new URL(response.url()).pathname === activityPath);
  await page.goto('/');
  const response = await responsePromise;
  expect(response.ok()).toBeTruthy();
  const data = await response.json();
  await expectData(page, data);
  return data;
}

async function fixture(request) {
  const response = await request.get('/api/v1/activity?user=');
  expect(response.ok()).toBeTruthy();
  return response.json();
}

test('real dashboard renders all hourly charts, sample labels and a responsive layout', async ({ page }, testInfo) => {
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
  const data = await openDashboard(page);
  // The suite runs against labeled sample data only: the in-memory demo container or the
  // Liquibase dev-seed fixtures in PostgreSQL. Never against live Azure DevOps data.
  expect(data.demo || data.dataset === 'fixture', 'Run this suite against demo or dev-seed fixture data').toBe(true);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Development, around the clock.');
  await expect(page.locator('#mode')).toHaveText(data.demo ? 'DEMO · SAMPLE DATA' : 'DEV FIXTURE DATA');
  await expect(page.locator('#summary .stat').nth(1).locator('small')).toContainText('AI co-author markers (lower bound)');
  await expect(page.getByRole('status')).toContainText('deterministic sample activity');
  await expect(page.getByRole('button', { name: 'Download hourly CSV' })).toHaveCount(8 + (data.workAreas?.length ? 2 : 0));
  await expect(page.getByRole('button', { name: 'Refresh' })).toBeEnabled();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  for (const [id] of charts) {
    const box = await page.locator(`#${id} svg`).boundingBox();
    expect(box.width).toBeGreaterThan(0);
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(page.viewportSize().width + 1);
  }
  await testInfo.attach('dashboard', { body: await page.screenshot({ fullPage: true }), contentType: 'image/png' });
  expect(errors).toEqual([]);
});

test('contributor selection filters every chart using the real endpoint', async ({ page }) => {
  const all = await openDashboard(page);
  expect(all.identities.length).toBeGreaterThan(0);
  const identity = all.identities[0];
  const responsePromise = page.waitForResponse(response => {
    const url = new URL(response.url());
    return url.pathname === activityPath && url.searchParams.get('user') === identity.id;
  });
  await page.getByLabel('Contributor').selectOption(identity.id);
  const response = await responsePromise;
  expect(response.ok()).toBeTruthy();
  const filtered = await response.json();
  await expectData(page, filtered);
  await expect(page.getByLabel('Contributor')).toHaveValue(identity.id);
  expect(summaryKeys.some(key => total(filtered, key) < total(all, key))).toBe(true);
  const resetPromise = page.waitForResponse(response => {
    const url = new URL(response.url());
    return url.pathname === activityPath && url.searchParams.get('user') === '';
  });
  await page.getByLabel('Contributor').selectOption('');
  await expectData(page, await (await resetPromise).json());
});

test('timezone changes hour labels without changing data totals', async ({ page }) => {
  const data = await openDashboard(page);
  const label = page.locator('#pr-count svg title').first();
  const phoenix = await label.textContent();
  await page.getByLabel('Display timezone').selectOption('UTC');
  await expect(label).not.toHaveText(phoenix);
  const utcHour = new Intl.DateTimeFormat('en-US', { timeZone: 'UTC', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).format(new Date(data.buckets[0].at));
  await expect(label).toContainText(utcHour);
  await expectData(page, data);
  await page.getByLabel('Display timezone').selectOption('America/Phoenix');
  await expect(label).toHaveText(phoenix);
  await expectData(page, data);
});

test('every CSV download contains its actual 168 hourly values in UTC', async ({ page }) => {
  const data = await openDashboard(page);
  for (const [id, series, unknown] of charts) {
    const downloadPromise = page.waitForEvent('download');
    await page.locator(`#${id}`).getByRole('button', { name: 'Download hourly CSV' }).click();
    const download = await downloadPromise;
    expect(download.suggestedFilename()).toBe(`${id}.csv`);
    const stream = await download.createReadStream();
    const chunks = [];
    for await (const chunk of stream) chunks.push(chunk);
    const rows = Buffer.concat(chunks).toString('utf8').trim().split(/\r?\n/);
    const keys = series.map(([key]) => key);
    expect(rows).toHaveLength(169);
    expect(rows[0]).toBe(['hour_utc', ...keys, ...(unknown ? ['unknown_events'] : [])].join(','));
    expect(rows.slice(1)).toEqual(data.buckets.map(bucket => [bucket.at, ...keys.map(key => bucket[key]), ...(unknown ? [bucket[unknown]] : [])].join(',')));
  }
});

test('loading state disables refresh until the response arrives', async ({ page, request }) => {
  const data = await fixture(request);
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  await page.route(activityRoute, async route => {
    await gate;
    await route.fulfill({ json: data });
  });
  try {
    await page.goto('/');
    await expect(page.getByRole('status')).toHaveText('Loading activity…');
    await expect(page.locator('#mode')).toHaveText('Connecting');
    await expect(page.getByRole('button', { name: 'Refresh' })).toBeDisabled();
    await expect(page.locator('svg.plot')).toHaveCount(0);
  } finally {
    release();
  }
  await expectData(page, data);
  await expect(page.getByRole('button', { name: 'Refresh' })).toBeEnabled();
});

test('initial request failure is visible and refresh recovers', async ({ page, request }) => {
  const data = await fixture(request);
  await page.route(activityRoute, route => route.fulfill({ status: 503, body: 'Unavailable' }));
  await page.goto('/');
  await expect(page.getByRole('status')).toHaveText('Activity unavailable (HTTP 503)');
  await expect(page.getByRole('status')).toHaveClass('error');
  await expect(page.locator('svg.plot')).toHaveCount(0);
  await page.unroute(activityRoute);
  await page.route(activityRoute, route => route.fulfill({ json: data }));
  await page.getByRole('button', { name: 'Refresh' }).click();
  await expectData(page, data);
  await expect(page.getByRole('status')).not.toHaveClass('error');
});

test('failed refresh retains the previous successful charts', async ({ page }) => {
  const data = await openDashboard(page);
  await page.route(activityRoute, route => route.fulfill({ status: 503, body: 'Unavailable' }));
  await page.getByRole('button', { name: 'Refresh' }).click();
  await expect(page.getByRole('status')).toHaveText('Activity unavailable (HTTP 503) — showing the previous successful view.');
  await expectData(page, data);
  await expect(page.getByRole('button', { name: 'Refresh' })).toBeEnabled();
});

test('no activity renders zero totals and preserves all hourly buckets', async ({ page, request }) => {
  const data = await fixture(request);
  data.identities = [];
  data.buckets = data.buckets.map(bucket => Object.fromEntries(Object.entries(bucket).map(([key, value]) => [key, key === 'at' ? value : 0])));
  await page.route(activityRoute, route => route.fulfill({ json: data }));
  await page.goto('/');
  await expectData(page, data);
  await expect(page.locator('#summary strong')).toHaveText(['0', '0', '0', '0']);
  await expect(page.getByLabel('Contributor').locator('optgroup[label="People"] option')).toHaveCount(0);
  await expect(page.getByLabel('Contributor').locator('option').first()).toHaveText('Everyone');
  await expect(page.getByRole('status')).not.toHaveClass('error');
});

test('hours after the latest collection are marked not yet collected, not quiet', async ({ page }) => {
  const data = await openDashboard(page);
  test.skip(data.demo, 'The in-memory demo has no collection watermark');
  expect(data.collectedThrough, 'fixture checkpoints set a watermark').toBeTruthy();
  const cutoff = new Date(data.collectedThrough);
  const pending = data.buckets.map((bucket, i) => [bucket, i]).filter(([bucket]) => new Date(bucket.at) >= cutoff);
  expect(pending.length).toBeGreaterThan(0);
  const titles = page.locator('#commits svg title');
  for (const [, i] of pending) await expect(titles.nth(i)).toContainText('not yet collected');
  await expect(titles.nth(pending[0][1] - 1)).not.toContainText('not yet collected');
  await expect(page.locator('#updated')).toContainText('Collected through');
  const sources = data.sources.map(s => s.source).sort();
  expect(sources).toEqual(['commits', 'pull_requests', 'tasks']);
  await expect(page.getByRole('status')).not.toHaveClass('error');
});

test('range selector switches between hourly, daily and weekly views', async ({ page }) => {
  const initial = await openDashboard(page);
  test.skip(initial.demo, 'Longer ranges need the database-backed API');
  for (const [range, bucket, count, csv] of [['30d', 'day', 30, 'Download daily CSV'], ['12m', 'week', 52, 'Download weekly CSV'], ['24m', 'week', 104, 'Download weekly CSV']]) {
    const responsePromise = page.waitForResponse(r => { const u = new URL(r.url()); return u.pathname === activityPath && u.searchParams.get('range') === range; });
    await page.getByLabel('Range').selectOption(range);
    const data = await (await responsePromise).json();
    expect(data.bucket).toBe(bucket);
    expect(data.range).toBe(range);
    expect(data.buckets).toHaveLength(count);
    await expect(page.locator('#commits svg title')).toHaveCount(count);
    await expect(page.locator('#summary strong').nth(1)).toHaveText(formatted(total(data, 'commits')));
    await expect(page.getByRole('button', { name: csv })).toHaveCount(10);
    await expect(page.locator('#range-title')).not.toHaveText('Past 7 days');
    if (bucket === 'week') await expect(page.locator('#commits svg title').first()).toContainText('Week of');
    expect(page.url()).toContain('range=' + range);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  }
  // Weeks before history collection began are marked, not presented as quiet weeks.
  const weekly = await (await page.request.get('/api/v1/activity?range=24m&tz=UTC&user=')).json();
  weekly.collectedFrom = weekly.buckets[10].at;
  await page.route(activityRoute, route => route.fulfill({ json: weekly }));
  await page.getByRole('button', { name: 'Refresh' }).click();
  const titles = page.locator('#commits svg title');
  await expect(titles.nth(0)).toContainText('before collection coverage');
  await expect(titles.nth(9)).toContainText('before collection coverage');
  await expect(titles.nth(10)).not.toContainText('collection coverage');
  await page.unroute(activityRoute);
  const back = page.waitForResponse(r => new URL(r.url()).searchParams.get('range') === '7d');
  await page.getByLabel('Range').selectOption('7d');
  await expectData(page, await (await back).json());
});

test('groups appear in the contributor selector and filter every chart', async ({ page }) => {
  const all = await openDashboard(page);
  test.skip(all.demo, 'Groups are stored in PostgreSQL');
  const options = page.getByLabel('Contributor').locator('optgroup[label="Groups"] option');
  await expect(options).not.toHaveCount(0);
  const value = await options.first().getAttribute('value');
  expect(value).toMatch(/^group:/);
  const responsePromise = page.waitForResponse(r => { const u = new URL(r.url()); return u.pathname === activityPath && u.searchParams.get('group') === value.slice(6); });
  await page.getByLabel('Contributor').selectOption(value);
  const filtered = await (await responsePromise).json();
  await expectData(page, filtered);
  expect(summaryKeys.some(key => total(filtered, key) < total(all, key))).toBe(true);
  // State survives a reload through the URL.
  await page.reload();
  await expect(page.getByLabel('Contributor')).toHaveValue(value);
});

test('work-area checkboxes and presets choose the stacked areas', async ({ page }) => {
  const data = await openDashboard(page);
  test.skip(!data.workAreas?.length, 'Work areas need the database-backed API');
  // Fixture commits must be tagged (dev-seed refresh runs the tagger), not just present.
  expect(data.buckets.some(b => Object.keys(b.areas || {}).length > 0), 'work-area tags present').toBe(true);
  const toggles = page.locator('#area-filters input[type=checkbox]');
  await expect(toggles).toHaveCount(data.workAreas.length);
  // Default hides the vendored/generated bucket.
  const other = data.workAreas.filter(a => a.kind === 'other').map(a => a.area);
  for (const a of data.workAreas) await expect(page.locator(`#area-filters input[value="${a.area}"]`)).toBeChecked({ checked: !other.includes(a.area) });
  const legend = page.locator('#area-lines .legend span');
  await expect(legend).toHaveCount(data.workAreas.length - other.length);
  // Totals per area match the API (sum of bucket lines).
  const first = data.workAreas[0];
  const lines = data.buckets.reduce((n, b) => n + ((b.areas || {})[first.area]?.lines || 0), 0);
  await expect(legend.first()).toHaveText(`${first.label} · ${formatted(lines)}`);
  await page.getByRole('button', { name: 'Software only' }).click();
  const software = data.workAreas.filter(a => a.kind === 'software');
  await expect(legend).toHaveCount(software.length);
  await expect(page.locator('#area-commits .legend span')).toHaveCount(software.length);
  expect(page.url()).toContain('areas=');
  // Unchecking one area removes it from both stacks.
  await page.locator(`#area-filters input[value="${software[0].area}"]`).uncheck();
  await expect(legend).toHaveCount(software.length - 1);
  // Primary-area commits add up to total non-merge commits for the visible-all case.
  await page.getByRole('button', { name: 'All', exact: true }).click();
  const primary = data.buckets.reduce((n, b) => n + Object.values(b.areas || {}).reduce((m, v) => m + v.commits, 0), 0);
  expect(primary).toBeLessThanOrEqual(total(data, 'commits'));
  await expect(page.locator('#area-commits svg title')).toHaveCount(168);
});

test('filters stay pinned while charts scroll on wide screens only', async ({ page }) => {
  await openDashboard(page);
  await page.locator('#tasks').scrollIntoViewIfNeeded();
  await page.mouse.wheel(0, 400);
  const toolbar = page.locator('.toolbar');
  const wide = page.viewportSize().width > 600;
  if (wide) {
    await expect(toolbar).toHaveClass(/stuck/);
    const box = await toolbar.boundingBox();
    expect(Math.round(box.y)).toBe(0);
    await expect(page.getByLabel('Range')).toBeInViewport();
    await expect(page.getByLabel('Contributor')).toBeInViewport();
  } else {
    await expect(page.getByLabel('Range')).not.toBeInViewport();
  }
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
});
