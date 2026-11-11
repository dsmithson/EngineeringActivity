import { test, expect } from '@playwright/test';

// Synthetic spend report (same columns as the claude.ai "Export spend report" CSV); no real usage data.
const header = 'user_email,account_uuid,product,model,total_requests,total_prompt_tokens,total_completion_tokens,total_net_spend_usd,total_gross_spend_usd,user_id,total_uncached_input_tokens,total_cache_read_tokens,total_cache_write_5m_tokens,total_cache_write_1h_tokens,total_web_search_count,slack_channel_id,teams_channel_id';
const rows = [
  'pat.example@example.com,a1,Claude Code,claude-opus-5-5,10,1000000,2000,0.0,0.0,u1,100000,800000,100000,0,0,,',
  'pat.exampleC2@example.com,a2,Claude Code,claude-haiku-4-5-20251001,5,500000,1000,0.0,0.0,u2,500000,0,0,0,0,,',
  'sam.sample@example.com,a3,Chat,claude-sonnet-5-5,3,200000,500,0.0,0.0,u3,20000,180000,0,0,0,,',
];
const csv = [header, ...rows].join('\n') + '\n';
const fileName = 'spend-report-00000000-2026-09-01-to-2026-09-30.csv';

async function upload(page) {
  await page.goto('/claude.html');
  await page.setInputFiles('#file', { name: fileName, mimeType: 'text/csv', buffer: Buffer.from(csv) });
  await expect(page.locator('#people-chart svg')).toBeVisible();
}

test('navigation links the activity and Claude usage pages', async ({ page }) => {
  await page.goto('/claude.html');
  await expect(page.getByRole('navigation', { name: 'Pages' }).getByRole('link', { name: 'Claude usage' })).toHaveAttribute('aria-current', 'page');
  await expect(page.locator('#report')).toBeHidden();
  await page.getByRole('link', { name: 'Development activity' }).click();
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Development, around the clock.');
  await page.getByRole('link', { name: 'Claude usage' }).click();
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Claude, by the token.');
});

test('uploaded report rolls numbered accounts into one person and reads the range from the file name', async ({ page }) => {
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await upload(page);
  await expect(page.locator('#eyebrow')).toHaveText('SEP 1 – SEP 30, 2026 · 30 DAYS');
  await expect(page.locator('#summary strong').first()).toHaveText('2');
  await expect(page.locator('#people-table tbody tr.person')).toHaveCount(2);
  await expect(page.locator('#people-table tbody tr.person').first().locator('td').nth(0)).toHaveText('Pat Example');
  await expect(page.locator('#people-table tbody tr.person').first().locator('td').nth(1)).toHaveText('2');
  await expect(page.locator('#people-chart .legend span')).toHaveText(['Opus 5.5 · 1M', 'Sonnet 5.5 · 200.5K', 'Haiku 4.5 · 501K']);
  await page.locator('#merge').uncheck();
  await expect(page.locator('#people-table tbody tr.person')).toHaveCount(3);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  expect(errors).toEqual([]);
});

test('a file without spend-report columns is rejected with a message', async ({ page }) => {
  await page.goto('/claude.html');
  await page.setInputFiles('#file', { name: 'other.csv', mimeType: 'text/csv', buffer: Buffer.from('a,b\n1,2\n') });
  await expect(page.getByRole('status')).toContainText("doesn't look like a Claude spend report");
  await expect(page.locator('#report')).toBeHidden();
});

test('people picker hides individuals from every chart and the table, and All restores them', async ({ page }) => {
  await upload(page);
  await page.evaluate(() => localStorage.removeItem('claude.hiddenPeople'));
  await expect(page.locator('#people-summary')).toHaveText('All people (2)');
  await page.locator('#people-picker summary').click();
  await page.locator('#people-list').getByLabel('Sam Sample').uncheck();
  await expect(page.locator('#people-summary')).toHaveText('1 of 2 people');
  await expect(page.locator('#people-table tbody tr.person')).toHaveCount(1);
  await expect(page.locator('#summary strong').first()).toHaveText('1');
  await expect(page.locator('#people-chart .legend span')).toHaveText(['Opus 5.5 · 1M', 'Haiku 4.5 · 501K']);
  await page.getByRole('button', { name: 'All', exact: true }).click();
  await expect(page.locator('#people-table tbody tr.person')).toHaveCount(2);
  await page.keyboard.press('Escape');
  await expect(page.locator('#people-picker')).not.toHaveAttribute('open', '');
});

test('table expands each person into per-model rows with the cost worked out, and lists the rates used', async ({ page }) => {
  await upload(page);
  const pat = page.locator('#people-table tbody tr.person').first();
  const models = page.locator('#people-table tbody tr.model:visible');
  await expect(models).toHaveCount(0);
  await pat.getByRole('button', { name: 'Pat Example' }).click();
  await expect(pat.getByRole('button', { name: 'Pat Example' })).toHaveAttribute('aria-expanded', 'true');
  // Opus 5.5: 100K uncached × $4 + 100K 5-min writes × $5 + 800K reads × $0.20 = $1.06 input; 2K output × $20 = $0.04.
  await expect(models).toHaveCount(2);
  await expect(models.nth(0).locator('td')).toHaveText(['Opus 5.5', '', '10', '1M', '2K', '80.0%', '$1.06', '$0.04', '$1.10', '66.7%']);
  await expect(models.nth(1).locator('td').nth(9)).toHaveText('33.3%');
  await expect(models.nth(0).locator('td').nth(8)).toHaveAttribute('title', /Cache read: 800,000 × \$0\.20 = \$0\.16/);
  await expect(page.locator('#rates-table tbody tr')).toHaveCount(3);
  await expect(page.locator('#rates-table tbody tr').first().locator('td')).toHaveText(['Opus 5.5', '$4', '$5', '$8', '$0.20', '$20', 'claude-opus-5-5']);
  await expect(page.locator('#rates-note')).toBeHidden();
  // Sonnet 5.5 cache reads are 0.05x input on the pricing page ($0.10), not the usual 0.1x.
  await expect(page.locator('#rates-table tbody tr').nth(1).locator('td')).toHaveText(['Sonnet 5.5', '$2', '$2.50', '$4', '$0.10', '$10', 'claude-sonnet-5-5']);
  await expect(page.getByRole('link', { name: /Anthropic API pricing/ })).toHaveAttribute('href', 'https://platform.claude.com/docs/en/about-claude/pricing');
  await page.getByRole('button', { name: 'Expand all' }).click();
  await expect(models).toHaveCount(3);
  await page.getByRole('button', { name: 'Collapse all' }).click();
  await expect(models).toHaveCount(0);
  const [download] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: 'Download CSV' }).click()]);
  const lines = (await (await download.createReadStream()).toArray()).join('').trim().split(/\r\n/);
  expect(lines[0]).toContain('model,requests');
  expect(lines).toHaveLength(4);
  expect(lines[1]).toBe('"Pat Example","pat.example@example.com pat.examplec2@example.com",claude-opus-5-5,10,1000000,2000,100000,800000,100000,0,1.06,0.04,1.10');
});

test('cache chart legend shows labels only and tooltips are one line per part', async ({ page }) => {
  await upload(page);
  await expect(page.locator('#cache-chart .legend span')).toHaveText(['Uncached input', 'Cache write · 5 min', 'Cache write · 1 h', 'Cache read']);
  const title = await page.locator('#people-chart svg title').first().textContent();
  expect(title.split('\n')[0]).toBe('Pat Example: 1,503,000');
  expect(title.split('\n').length).toBeGreaterThan(1);
});
