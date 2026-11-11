// Writes a fictional claude.ai "Export spend report" CSV for screenshots and demos.
// Usage: node scripts/sample-spend-report.mjs [out.csv]. Every name and number is made up.
import { writeFileSync } from 'node:fs';

const people = [
  // [email, scale, model mix: share of tokens per model]
  ['maya.chen@acme.example', 9.0, {'claude-opus-5-5': .55, 'claude-sonnet-5-5': .25, 'claude-haiku-5-5': .05, 'claude-fable-5-1': .15}],
  ['maya.chenC2@acme.example', 4.0, {'claude-opus-5-5': .7, 'claude-sonnet-5-5': .3}],
  ['luis.ortega@acme.example', 7.5, {'claude-opus-5-5': .4, 'claude-opus-5': .35, 'claude-sonnet-5': .25}],
  ['priya.nair@acme.example', 6.0, {'claude-sonnet-5-5': .5, 'claude-opus-5-5': .4, 'claude-haiku-4-5': .1}],
  ['sam.okafor@acme.example', 4.2, {'claude-opus-4-8': .45, 'claude-sonnet-5': .45, 'claude-haiku-5-5': .1}],
  ['jonas.berg@acme.example', 2.8, {'claude-sonnet-5-5': .7, 'claude-haiku-5-5': .3}],
  ['ana.silva@acme.example', 1.6, {'claude-opus-5-5': .6, 'claude-sonnet-5-5': .4}],
  ['tom.reyes@acme.example', .7, {'claude-sonnet-5': .8, 'claude-haiku-4-5': .2}],
  ['kim.park@acme.example', .25, {'claude-opus-5-5': 1}],
];
const products = [['Claude Code', .82], ['Cowork', .1], ['Chat', .08]];
const header = 'user_email,account_uuid,product,model,total_requests,total_prompt_tokens,total_completion_tokens,total_net_spend_usd,total_gross_spend_usd,user_id,total_uncached_input_tokens,total_cache_read_tokens,total_cache_write_5m_tokens,total_cache_write_1h_tokens,total_web_search_count,slack_channel_id,teams_channel_id';
const rows = [header];
let seed = 7;
const rand = () => (seed = (seed * 16807) % 2147483647) / 2147483647;
people.forEach(([email, scale, mix], p) => {
  for (const [product, share] of products) {
    if (product !== 'Claude Code' && p % 3 === 2) continue;
    for (const [model, m] of Object.entries(mix)) {
      const input = Math.round(scale * 1e9 * share * m * (0.85 + rand() * 0.3));
      if (input < 1e5) continue;
      // Shares of input: cache reads, then the rest split between writes and uncached (sums to 1).
      const hit = 0.93 + rand() * 0.05, w1h = (1 - hit) * (0.45 + rand() * 0.2), w5m = (1 - hit) * 0.3;
      const read = Math.round(input * hit), write1h = Math.round(input * w1h), write5m = Math.round(input * w5m);
      const uncached = input - read - write1h - write5m;
      const output = Math.round(input * (0.0025 + rand() * 0.002));
      const requests = Math.round(input / (240000 + rand() * 80000));
      rows.push([email, `00000000-0000-4000-8000-${String(p).padStart(12, '0')}`, product, model, requests, input, output, '0.0', '0.0',
        `user_sample${p}`, uncached, read, write5m, write1h, Math.round(rand() * 6), '', ''].join(','));
    }
  }
});
const out = process.argv[2] || 'spend-report-sample-2026-08-01-to-2026-10-31.csv';
writeFileSync(out, rows.join('\n') + '\n');
console.log(`wrote ${rows.length - 1} rows to ${out}`);
