// Captures the README screenshots from a running stack seeded with dev-seed fixtures, plus a
// fictional Claude spend report. Never point this at a stack with real data.
//
//   node scripts/readme-screenshots.mjs <baseUrl> <spend-report.csv> [outDir]
//
// See docs/screenshots.md for the full recipe (sample stack, branding, cleanup).
import { chromium } from '@playwright/test';

const [base = 'http://127.0.0.1:8095', csv, out = 'docs/images'] = process.argv.slice(2);
if (!csv) throw new Error('usage: node scripts/readme-screenshots.mjs <baseUrl> <spend-report.csv> [outDir]');

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2, timezoneId: 'America/Phoenix', locale: 'en-US' });
const errors = [];
page.on('pageerror', e => errors.push(e.message));

const shot = async (name, locator, opts = {}) => {
  await locator.scrollIntoViewIfNeeded();
  await page.waitForTimeout(300);
  await locator.screenshot({ path: `${out}/${name}.png`, ...opts });
  console.log(`saved ${out}/${name}.png`);
};

// Element shots: unpin the filter bar so it doesn't overlap, and park the mouse.
const unpin = () => page.evaluate(() => { for (const t of document.querySelectorAll('.toolbar')) t.style.position = 'static'; });
const top = async height => {
  await page.mouse.move(0, 0);
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.waitForTimeout(300);
  return { x: 0, y: 0, width: 1440, height };
};

// Development activity page: refuse to shoot anything but fixture or demo data.
const activity = page.waitForResponse(r => new URL(r.url()).pathname === '/api/v1/activity');
await page.goto(`${base}/?range=30d`);
const data = await (await activity).json();
if (data.dataset !== 'fixture' && !data.demo) throw new Error('Refusing to capture: the stack is not serving fixture or demo data');
await page.waitForSelector('#pr-count svg');
await page.waitForSelector('#area-lines svg');
await unpin();
await page.screenshot({ path: `${out}/activity-hero.png`, fullPage: true, clip: await top(1290) });
console.log(`saved ${out}/activity-hero.png`);
await shot('activity-work-areas', page.locator('#areas-section'));

// Claude usage page with the fictional spend report.
await page.goto(`${base}/claude.html`);
await page.evaluate(() => { try { localStorage.clear(); } catch {} });
await page.setInputFiles('#file', csv);
await page.waitForSelector('#people-chart svg');
await unpin();
await page.screenshot({ path: `${out}/claude-hero.png`, fullPage: true, clip: await top(1045) });
console.log(`saved ${out}/claude-hero.png`);
await page.getByRole('button', { name: 'Maya Chen' }).click();
await page.mouse.move(0, 0);
await shot('claude-cost-table', page.locator('#people-table').locator('xpath=ancestor::section'));
await shot('claude-rates', page.locator('#rates-table').locator('xpath=ancestor::section'));

await browser.close();
if (errors.length) throw new Error('Page errors: ' + errors.join('; '));
