/* Claude usage: parses a claude.ai "Export spend report" CSV in the browser. Nothing is uploaded. */
const $ = id => document.getElementById(id);
const ns = 'http://www.w3.org/2000/svg';
const num = n => new Intl.NumberFormat().format(Math.round(n));
const compact = n => Intl.NumberFormat('en', {notation: 'compact', maximumFractionDigits: 1}).format(n);
const usd = n => '$' + (n >= 100 ? num(n) : n.toFixed(2));
function node(tag, attrs = {}, text) { const el = document.createElementNS(ns, tag); for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v); if (text !== undefined) el.textContent = text; return el; }

const REQUIRED = ['user_email', 'product', 'model', 'total_requests', 'total_prompt_tokens', 'total_completion_tokens', 'total_uncached_input_tokens', 'total_cache_read_tokens', 'total_cache_write_5m_tokens', 'total_cache_write_1h_tokens'];
// Categorical order validated (dataviz validator, dark surface #111e30): adjacent pairs pass CVD and normal-vision checks.
const PALETTE = ['#3987e5', '#d95926', '#199e70', '#c98500', '#d55181', '#008300', '#9085e9', '#e66767'];
const OTHER = '#6b7a90';
// List prices, USD per million tokens (Anthropic API, 2026-10). in/out; cache writes are 1.25× (5 min) and 2× (1 h) input.
// The export reports $0 spend for seat-plan usage, so cost here is an API-equivalent estimate.
// Source: https://platform.claude.com/docs/en/about-claude/pricing (checked 2026-10-28). Cache reads are 0.1× input,
// except 0.025× on Fable 5.1 and 0.05× on Opus 5.5 / Sonnet 5.5. Haiku 5.5 costs 5× more for prompts over 100K tokens;
// the export has no per-request prompt sizes, so its lower tier is used (noted on the page).
const PRICES = {
  'claude-fable-5-1': {in: 10, out: 50, read: 0.25},
  'claude-opus-5-5': {in: 4, out: 20, read: 0.20},
  'claude-opus-5': {in: 5, out: 25, read: 0.50},
  'claude-opus-4-8': {in: 5, out: 25, read: 0.50},
  'claude-sonnet-5-5': {in: 2, out: 10, read: 0.10},
  'claude-sonnet-5': {in: 2, out: 10, read: 0.20},
  'claude-haiku-5-5': {in: 0.10, out: 0.50, read: 0.01, tiered: true},
  'claude-haiku-4-5': {in: 1, out: 5, read: 0.10},
};
// Per-million rate for each token type (TOKEN_TYPES keys).
const rates = p => ({uncached: p.in, write5m: p.in * 1.25, write1h: p.in * 2, read: p.read, output: p.out});
// Whole dollars bare ($4); otherwise every significant digit so tokens × rate checks exactly ($0.125).
const rate = n => '$' + (Number.isInteger(n) ? n : new Intl.NumberFormat('en-US', {minimumFractionDigits: 2, maximumFractionDigits: 4}).format(n));
// Models keep a fixed color whatever is filtered or ranked; unknown models fold into "Other models".
const MODELS = Object.keys(PRICES);
const PRODUCTS = ['Claude Code', 'Cowork', 'Chat', 'Claude Design', 'Claude in Xcode'];
const TOKEN_TYPES = [
  {key: 'uncached', label: 'Uncached input'}, {key: 'write5m', label: 'Cache write · 5 min'}, {key: 'write1h', label: 'Cache write · 1 h'},
  {key: 'read', label: 'Cache read'}, {key: 'output', label: 'Output'},
];
const MEASURES = {
  tokens: {label: 'Total tokens', short: 'tokens', fmt: compact, full: num},
  input: {label: 'Input tokens', short: 'input tokens', fmt: compact, full: num},
  output: {label: 'Output tokens', short: 'output tokens', fmt: compact, full: num},
  requests: {label: 'Requests', short: 'requests', fmt: compact, full: num},
  cost: {label: 'Est. API cost', short: 'est. API cost', fmt: n => '$' + compact(n), full: usd},
};

let report = null; // {rows, from, to, fileName}
// Which tokens the chart shows when Measure is Tokens: output (default), input, or total (both).
let tokenView = 'output';
// People unchecked in the People picker, by person key (or account email when accounts aren't combined).
// Remembered in this browser so a screenshot set-up survives reloading a newer report.
const hidden = new Set((() => { try { return JSON.parse(localStorage.getItem('claude.hiddenPeople')) || []; } catch { return []; } })());
const saveHidden = () => { try { localStorage.setItem('claude.hiddenPeople', JSON.stringify([...hidden])); } catch {} };

const baseModel = id => id.replace(/-\d{8}$/, '');
function modelLabel(id) {
  const m = /^claude-([a-z]+)-(\d+)(?:-(\d+))?/.exec(baseModel(id));
  return m ? `${m[1][0].toUpperCase()}${m[1].slice(1)} ${m[2]}${m[3] ? '.' + m[3] : ''}` : id;
}
// Numbered alternates (jane.doeC2@…, …C3@…) belong to the same person.
const personKey = email => email.toLowerCase().replace(/c\d+@/, '@');
function personName(key) {
  const local = key.split('@')[0];
  return local.split(/[._-]+/).filter(Boolean).map(w => w[0].toUpperCase() + w.slice(1)).join(' ') || key;
}

function parseCSV(text) {
  const rows = []; let row = [], field = '', quoted = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (quoted) { if (c === '"') { if (text[i + 1] === '"') { field += '"'; i++; } else quoted = false; } else field += c; }
    else if (c === '"') quoted = true;
    else if (c === ',') { row.push(field); field = ''; }
    else if (c === '\n' || c === '\r') { if (c === '\r' && text[i + 1] === '\n') i++; row.push(field); field = ''; if (row.some(v => v !== '')) rows.push(row); row = []; }
    else field += c;
  }
  row.push(field); if (row.some(v => v !== '')) rows.push(row);
  return rows;
}

function load(text, fileName) {
  const [header, ...body] = parseCSV(text.replace(/^﻿/, ''));
  const missing = REQUIRED.filter(h => !(header || []).includes(h));
  if (missing.length) throw Error(`This doesn't look like a Claude spend report (missing ${missing.slice(0, 3).join(', ')}${missing.length > 3 ? '…' : ''}).`);
  const col = Object.fromEntries(header.map((h, i) => [h, i])), n = (r, h) => Number(r[col[h]]) || 0;
  const rows = body.map(r => ({
    email: r[col.user_email], product: r[col.product] || 'Unknown', model: baseModel(r[col.model] || 'unknown'),
    requests: n(r, 'total_requests'), input: n(r, 'total_prompt_tokens'), output: n(r, 'total_completion_tokens'),
    uncached: n(r, 'total_uncached_input_tokens'), read: n(r, 'total_cache_read_tokens'), write5m: n(r, 'total_cache_write_5m_tokens'), write1h: n(r, 'total_cache_write_1h_tokens'),
    web: col.total_web_search_count !== undefined ? n(r, 'total_web_search_count') : 0,
    netSpend: col.total_net_spend_usd !== undefined ? n(r, 'total_net_spend_usd') : 0,
  })).filter(r => r.email);
  if (!rows.length) throw Error('The report has a header but no usage rows.');
  const products = [...new Set(rows.map(r => r.product))].sort((a, b) => (PRODUCTS.indexOf(a) + 1 || 99) - (PRODUCTS.indexOf(b) + 1 || 99)), select = $('product'), kept = select.value;
  select.replaceChildren(new Option('All products', ''), ...products.map(x => new Option(x, x))); select.value = products.includes(kept) ? kept : '';
  for (const r of rows) r.cost = costOf(r);
  const m = /(\d{4}-\d{2}-\d{2})-to-(\d{4}-\d{2}-\d{2})/.exec(fileName || '');
  report = {rows, fileName, from: m?.[1], to: m?.[2]};
}

// Cost by token type, so the cost measure can also be stacked by token type. Null for unpriced models.
function costParts(r) {
  const p = PRICES[r.model]; if (!p) return null;
  const per = rates(p);
  return Object.fromEntries(TOKEN_TYPES.map(t => [t.key, r[t.key] * per[t.key] / 1e6]));
}
const costOf = r => { const c = costParts(r); return c ? Object.values(c).reduce((a, b) => a + b, 0) : 0; };
function measureOf(r, measure) {
  switch (measure) {
    case 'input': return r.input;
    case 'output': return r.output;
    case 'requests': return r.requests;
    case 'cost': return r.cost;
    default: return r.input + r.output;
  }
}

function filtered() {
  const product = $('product').value;
  return report.rows.filter(r => !product || r.product === product);
}
function people(rows) {
  const merge = $('merge').checked, map = new Map();
  for (const r of rows) {
    const key = merge ? personKey(r.email) : r.email.toLowerCase();
    if (!map.has(key)) map.set(key, {key, name: merge ? personName(key) : r.email, accounts: new Set(), rows: []});
    const p = map.get(key); p.accounts.add(r.email.toLowerCase()); p.rows.push(r);
  }
  return [...map.values()];
}

// Series for the main chart, keyed by the stack choice; each returns per-row contributions.
function stackSeries(rows) {
  const stack = $('stack').value, measure = currentMeasure();
  if (stack === 'tokens') {
    return TOKEN_TYPES.filter(t => measure !== 'output' || t.key === 'output').filter(t => measure !== 'input' || t.key !== 'output')
      .map((t, i) => ({label: t.label, color: PALETTE[i], value: r => measure === 'cost' ? (costParts(r)?.[t.key] || 0) : r[t.key]}));
  }
  const order = stack === 'product' ? PRODUCTS : MODELS, field = stack === 'product' ? 'product' : 'model';
  const present = new Set(rows.map(r => r[field])), known = order.filter(k => present.has(k)), unknown = [...present].filter(k => !order.includes(k));
  const series = known.map(k => ({label: field === 'model' ? modelLabel(k) : k, color: PALETTE[order.indexOf(k)], value: r => r[field] === k ? measureOf(r, measure) : 0}));
  if (unknown.length) series.push({label: field === 'model' ? 'Other models' : 'Other products', color: OTHER, value: r => unknown.includes(r[field]) ? measureOf(r, measure) : 0});
  return series;
}

function barChart(holder, items, series, fmt, full, legendValue) {
  holder.replaceChildren();
  const width = Math.max(holder.clientWidth, 260), horizontal = width < 640;
  const totals = items.map(it => series.map(s => it.rows.reduce((n, r) => n + s.value(r), 0)));
  const sums = totals.map(t => t.reduce((a, b) => a + b, 0)), max = Math.max(1e-9, ...sums);
  const describe = i => [`${items[i].name}: ${full(sums[i])}`, ...series.map((s, k) => totals[i][k] ? `${s.label}: ${full(totals[i][k])}` : '').filter(Boolean)].join('\n');
  let svg;
  if (horizontal) {
    const row = 26, labelW = 104, left = labelW + 6, right = 10, top = 6, bottom = 20, height = top + bottom + row * items.length, pw = width - left - right;
    svg = node('svg', {viewBox: `0 0 ${width} ${height}`, class: 'plot bars', role: 'img', 'aria-label': $('people-title').textContent});
    svg.style.height = height + 'px';
    for (let n = 0; n <= 3; n++) { const x = left + pw * n / 3; svg.append(node('line', {x1: x, y1: top, x2: x, y2: height - bottom, class: 'grid'}), node('text', {x, y: height - 6, 'text-anchor': n ? (n === 3 ? 'end' : 'middle') : 'start'}, fmt(max * n / 3))); }
    items.forEach((it, i) => {
      const y = top + i * row; let acc = 0;
      svg.append(node('text', {x: labelW, y: y + row / 2 + 3, 'text-anchor': 'end', class: 'name'}, it.name.length > 17 ? it.name.slice(0, 16) + '…' : it.name));
      series.forEach((s, k) => { const w = totals[i][k] / max * pw; if (w <= 0) return; svg.append(node('rect', {x: left + acc, y: y + 5, width: w, height: row - 10, fill: s.color, stroke: '#111e30', 'stroke-width': w > 3 ? 1 : 0})); acc += w; });
      const hit = node('rect', {x: 0, y, width, height: row, fill: 'transparent'}); hit.append(node('title', {}, describe(i))); svg.append(hit);
    });
  } else {
    const height = 300, left = 46, right = 8, top = 12, bottom = 54, pw = width - left - right, ph = height - top - bottom, step = pw / items.length, bw = Math.min(56, step * 0.68);
    svg = node('svg', {viewBox: `0 0 ${width} ${height}`, class: 'plot bars', role: 'img', 'aria-label': $('people-title').textContent});
    svg.style.height = height + 'px';
    for (let n = 0; n <= 4; n++) { const y = top + ph - ph * n / 4; svg.append(node('line', {x1: left, y1: y, x2: width - right, y2: y, class: 'grid'}), node('text', {x: left - 6, y: y + 3, 'text-anchor': 'end'}, fmt(max * n / 4))); }
    items.forEach((it, i) => {
      const cx = left + step * (i + .5), x = cx - bw / 2; let acc = 0;
      series.forEach((s, k) => { const h = totals[i][k] / max * ph; if (h <= 0) return; svg.append(node('rect', {x, y: top + ph - acc - h, width: bw, height: h, fill: s.color, stroke: '#111e30', 'stroke-width': h > 3 ? 1 : 0})); acc += h; });
      const words = it.name.split(' '), first = words[0], rest = words.slice(1).join(' ');
      svg.append(node('text', {x: cx, y: top + ph + 15, 'text-anchor': 'middle', class: 'name'}, first), node('text', {x: cx, y: top + ph + 28, 'text-anchor': 'middle', class: 'name'}, rest.length > 14 ? rest.slice(0, 13) + '…' : rest));
      if (it.accounts > 1) svg.append(node('text', {x: cx, y: top + ph + 41, 'text-anchor': 'middle'}, `${it.accounts} accounts`));
      const hit = node('rect', {x: left + step * i, y: top, width: step, height: ph + bottom, fill: 'transparent'}); hit.append(node('title', {}, describe(i))); svg.append(hit);
    });
  }
  holder.append(svg);
  const legend = document.createElement('div'); legend.className = 'legend';
  series.forEach((s, k) => { const span = document.createElement('span'), sw = document.createElement('i'); sw.style.background = s.color; span.append(sw, legendValue === false ? s.label : `${s.label} · ${fmt(totals.reduce((n, t) => n + t[k], 0))}`); legend.append(span); });
  holder.append(legend);
}

function rangeText() {
  if (!report.from || !report.to) return {eyebrow: 'UPLOADED SPEND REPORT · DATE RANGE NOT IN FILE NAME', title: report.fileName || 'Uploaded report', days: null};
  const f = new Date(report.from + 'T00:00:00Z'), t = new Date(report.to + 'T00:00:00Z'), days = Math.round((t - f) / 864e5) + 1;
  const d = (x, o) => new Intl.DateTimeFormat('en-US', {timeZone: 'UTC', month: 'short', day: 'numeric', ...o}).format(x);
  const title = `${d(f, f.getUTCFullYear() !== t.getUTCFullYear() ? {year: 'numeric'} : {})} – ${d(t, {year: 'numeric'})}`;
  return {eyebrow: `${title.toUpperCase()} · ${days} DAYS`, title, days};
}

// The measure the chart and row order use: Tokens resolves to the token toggle's choice.
const currentMeasure = () => $('measure').value !== 'tokens' ? $('measure').value : tokenView === 'total' ? 'tokens' : tokenView;

function render() {
  if (!report) return;
  let rows = filtered(); const measure = currentMeasure(), M = MEASURES[measure];
  const r = rangeText();
  $('eyebrow').textContent = r.eyebrow; $('range-title').textContent = r.title; $('range-detail').textContent = `${report.fileName || 'Uploaded file'} · ${num(report.rows.length)} rows`;
  $('mode').textContent = 'LOCAL FILE · NOT STORED'; $('mode').className = 'pill demo';
  for (const id of ['report', 'toolbar']) $(id).hidden = false;
  $('drop').classList.add('compact');
  // Token-type stacks cannot split requests.
  $('stack').querySelector('[value=tokens]').disabled = measure === 'requests';
  if (measure === 'requests' && $('stack').value === 'tokens') $('stack').value = 'model';

  const everyone = people(rows), shown = everyone.filter(p => !hidden.has(p.key));
  fillPicker(everyone, shown.length);
  rows = shown.flatMap(p => p.rows);
  const ppl = shown.map(p => ({...p, accounts: p.accounts.size, total: p.rows.reduce((n, x) => n + measureOf(x, measure), 0)})).sort((a, b) => b.total - a.total || a.name.localeCompare(b.name));
  const sum = f => rows.reduce((n, x) => n + f(x), 0), input = sum(x => x.input), unpriced = new Set(rows.filter(x => !PRICES[x.model]).map(x => modelLabel(x.model)));
  const accounts = new Set(rows.map(x => x.email.toLowerCase())).size;
  $('summary').replaceChildren();
  for (const [label, value, note] of [
    ['People', num(ppl.length), ($('merge').checked ? `${num(accounts)} accounts, numbered alternates combined` : `${num(accounts)} accounts`) + (shown.length < everyone.length ? ` · ${everyone.length - shown.length} hidden` : '')],
    ['Total tokens', compact(input + sum(x => x.output)), `${compact(sum(x => x.output))} output · ${num(sum(x => x.requests))} requests`],
    ['Cache hit rate', input ? (sum(x => x.read) / input * 100).toFixed(1) + '%' : '—', 'Share of input tokens read from the prompt cache'],
    ['Est. API cost', usd(sum(x => x.cost)), unpriced.size ? `List price · excludes ${[...unpriced].join(', ')}` : 'At list API prices · not your invoice'],
  ]) { const card = document.createElement('div'); card.className = 'stat'; for (const [tag, text] of [['span', label], ['strong', value], ['small', note]]) { const el = document.createElement(tag); el.textContent = text; card.append(el); } $('summary').append(card); }

  const stackWord = {model: 'model', product: 'product', tokens: 'token type'}[$('stack').value];
  $('people-title').textContent = `${M.label} by person`;
  $('token-view').hidden = $('measure').value !== 'tokens';
  for (const b of $('token-view').querySelectorAll('button')) b.setAttribute('aria-pressed', b.dataset.view === tokenView);
  $('table-order').textContent = M.label.toLowerCase();
  $('people-sub').textContent = `Stacked by ${stackWord} · ${$('product').value || 'all products'} · ${r.days ? `${r.days}-day report` : 'whole report'}`;
  barChart($('people-chart'), ppl, stackSeries(rows), M.fmt, M.full);

  // Cache efficiency: where each person's input tokens came from (share of input).
  const inputSeries = TOKEN_TYPES.slice(0, 4).map((t, i) => ({label: t.label, color: PALETTE[i], value: x => x[t.key]}));
  const pct = n => (n * 100).toFixed(n >= .995 || n < .01 ? 1 : 0) + '%';
  barChart($('cache-chart'), ppl.map(p => { const tot = p.rows.reduce((n, x) => n + x.input, 0) || 1; return {...p, rows: p.rows.map(x => ({uncached: x.uncached / tot, write5m: x.write5m / tot, write1h: x.write1h / tot, read: x.read / tot}))}; }), inputSeries, pct, pct, false);

  renderTable(ppl);
  renderRates(rows);
  $('status').textContent = `Loaded ${report.fileName || 'report'} — parsed in this browser only; nothing was sent to the server. Reload the page to clear it.`; $('status').className = '';
}

// Table: one row per person (total cost), expandable into one row per model with the cost worked out.
const expanded = new Set();
const TOKEN_KEYS = ['requests', 'input', 'output', 'uncached', 'read', 'write5m', 'write1h'];
function modelRows(p) {
  const by = new Map();
  for (const x of p.rows) { const m = by.get(x.model) || Object.fromEntries([['model', x.model], ...TOKEN_KEYS.map(k => [k, 0])]); for (const k of TOKEN_KEYS) m[k] += x[k]; by.set(x.model, m); }
  return [...by.values()].map(m => { const c = costParts(m); return {...m, priced: !!c, inCost: c ? c.uncached + c.write5m + c.write1h + c.read : 0, outCost: c ? c.output : 0}; })
    .sort((a, b) => (b.input + b.output) - (a.input + a.output));
}
// Hover text: tokens × rate for each part of a model's cost.
function costWork(m) {
  const p = PRICES[m.model]; if (!p) return `${modelLabel(m.model)} has no known API price; excluded from the estimate.`;
  const per = rates(p), c = costParts(m);
  return [`${modelLabel(m.model)} at list price (per million tokens):`, ...TOKEN_TYPES.filter(t => m[t.key]).map(t => `${t.label}: ${num(m[t.key])} × ${rate(per[t.key])} = ${usd(c[t.key])}`), `Total: ${usd(m.inCost + m.outCost)}`].join('\n');
}
function cell(tr, text, cls, title) { const td = document.createElement('td'); td.textContent = text; if (cls) td.className = cls; if (title) td.title = title; tr.append(td); return td; }
function renderTable(ppl) {
  const tbody = $('people-table').querySelector('tbody'); tbody.replaceChildren();
  const hit = (read, input) => input ? (read / input * 100).toFixed(1) + '%' : '—';
  for (const p of ppl) {
    const models = modelRows(p), s = f => models.reduce((n, m) => n + f(m), 0), tokens = s(m => m.input + m.output), open = expanded.has(p.key);
    const tr = document.createElement('tr'), first = document.createElement('td'), toggle = document.createElement('button');
    tr.className = 'person'; toggle.type = 'button'; toggle.className = 'row-toggle'; toggle.setAttribute('aria-expanded', open); toggle.textContent = p.name;
    toggle.addEventListener('click', () => { expanded.has(p.key) ? expanded.delete(p.key) : expanded.add(p.key); render(); });
    first.append(toggle); tr.append(first);
    const accts = [...new Set(p.rows.map(x => x.email))]; if (accts.length > 1) first.title = accts.join('\n');
    cell(tr, num(p.accounts), 'n'); cell(tr, num(s(m => m.requests)), 'n'); cell(tr, compact(s(m => m.input)), 'n'); cell(tr, compact(s(m => m.output)), 'n'); cell(tr, compact(tokens), 'n'); cell(tr, hit(s(m => m.read), s(m => m.input)), 'n');
    cell(tr, usd(s(m => m.inCost)), 'n'); cell(tr, usd(s(m => m.outCost)), 'n'); cell(tr, usd(s(m => m.inCost + m.outCost)), 'n total'); cell(tr, `${models.length} model${models.length === 1 ? '' : 's'}`, 'n muted');
    tbody.append(tr);
    for (const m of models) {
      const sub = document.createElement('tr'), work = costWork(m); sub.className = 'model'; sub.hidden = !open;
      cell(sub, modelLabel(m.model)); cell(sub, ''); cell(sub, num(m.requests), 'n'); cell(sub, compact(m.input), 'n', num(m.input)); cell(sub, compact(m.output), 'n', num(m.output)); cell(sub, compact(m.input + m.output), 'n', num(m.input + m.output)); cell(sub, hit(m.read, m.input), 'n');
      cell(sub, m.priced ? usd(m.inCost) : '—', 'n', work); cell(sub, m.priced ? usd(m.outCost) : '—', 'n', work); cell(sub, m.priced ? usd(m.inCost + m.outCost) : 'not priced', 'n', work);
      cell(sub, tokens ? ((m.input + m.output) / tokens * 100).toFixed(1) + '%' : '—', 'n');
      tbody.append(sub);
    }
  }
  $('expand-all').textContent = ppl.length && ppl.every(p => expanded.has(p.key)) ? 'Collapse all' : 'Expand all';
}
// Rates table: every rate used for the models in view, so the estimate can be checked by hand.
function renderRates(rows) {
  const tbody = $('rates-table').querySelector('tbody'); tbody.replaceChildren();
  const present = [...new Set(rows.map(x => x.model))].sort((a, b) => (MODELS.indexOf(a) + 1 || 99) - (MODELS.indexOf(b) + 1 || 99));
  let tiered = false;
  for (const model of present) {
    const tr = document.createElement('tr'), p = PRICES[model]; cell(tr, modelLabel(model) + (p?.tiered ? '*' : ''));
    if (!p) { for (const t of TOKEN_TYPES) cell(tr, '—', 'n'); cell(tr, 'No known price · excluded', 'muted'); tbody.append(tr); continue; }
    const per = rates(p);
    for (const t of TOKEN_TYPES) cell(tr, rate(per[t.key]), 'n');
    cell(tr, model, 'muted'); tiered ||= !!p.tiered; tbody.append(tr);
  }
  $('rates-note').hidden = !tiered;
}

// People picker: one checkbox per person in the current product filter, alphabetical.
function fillPicker(everyone, shownCount) {
  $('people-summary').textContent = shownCount === everyone.length ? `All people (${everyone.length})` : `${shownCount} of ${everyone.length} people`;
  const list = $('people-list'); list.replaceChildren();
  for (const p of [...everyone].sort((a, b) => a.name.localeCompare(b.name))) {
    const label = document.createElement('label'), input = document.createElement('input');
    input.type = 'checkbox'; input.value = p.key; input.checked = !hidden.has(p.key);
    input.addEventListener('change', () => { input.checked ? hidden.delete(p.key) : hidden.add(p.key); saveHidden(); render(); });
    label.append(input, p.name); if (p.accounts.size > 1) label.title = [...p.accounts].join('\n');
    list.append(label);
  }
}
function setAll(show) {
  for (const p of people(filtered())) show ? hidden.delete(p.key) : hidden.add(p.key);
  saveHidden(); render();
}

function download() {
  // One line per person and model, cost split by input/output so it can be re-checked in a spreadsheet.
  const ppl = people(filtered()).filter(p => !hidden.has(p.key)), cols = ['person', 'accounts', 'model', 'requests', 'input_tokens', 'output_tokens', 'uncached_input_tokens', 'cache_read_tokens', 'cache_write_5m_tokens', 'cache_write_1h_tokens', 'input_cost_usd', 'output_cost_usd', 'est_api_cost_usd'];
  const lines = ppl.flatMap(p => modelRows(p).map(m => [`"${p.name}"`, `"${[...p.accounts].join(' ')}"`, m.model, m.requests, m.input, m.output, m.uncached, m.read, m.write5m, m.write1h, ...(m.priced ? [m.inCost.toFixed(2), m.outCost.toFixed(2), (m.inCost + m.outCost).toFixed(2)] : ['', '', ''])].join(',')));
  const url = URL.createObjectURL(new Blob([[cols.join(','), ...lines].join('\r\n')], {type: 'text/csv'})), a = document.createElement('a');
  a.href = url; a.download = `claude-usage-by-person${report.from ? `-${report.from}-to-${report.to}` : ''}.csv`; a.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
}

async function open(file) {
  if (!file) return;
  try { load(await file.text(), file.name); render(); }
  catch (e) { $('status').textContent = e.message + (report ? ' — still showing the previous report.' : ''); $('status').className = 'error'; }
}
$('file').addEventListener('change', e => open(e.target.files[0]));
const drop = $('drop');
drop.addEventListener('dragover', e => { e.preventDefault(); drop.classList.add('over'); });
drop.addEventListener('dragleave', () => drop.classList.remove('over'));
drop.addEventListener('drop', e => { e.preventDefault(); drop.classList.remove('over'); open(e.dataTransfer.files[0]); });
for (const id of ['measure', 'stack', 'product', 'merge']) $(id).addEventListener('change', render);
for (const b of $('token-view').querySelectorAll('button')) b.addEventListener('click', () => { tokenView = b.dataset.view; render(); });
$('download').addEventListener('click', download);
$('expand-all').addEventListener('click', () => {
  const ppl = people(filtered()).filter(p => !hidden.has(p.key)), all = ppl.every(p => expanded.has(p.key));
  for (const p of ppl) all ? expanded.delete(p.key) : expanded.add(p.key);
  render();
});
$('people-all').addEventListener('click', () => setAll(true));
$('people-none').addEventListener('click', () => setAll(false));
// Close the picker on an outside click or Escape, like a native select.
document.addEventListener('click', e => { if (!$('people-picker').contains(e.target)) $('people-picker').open = false; });
document.addEventListener('keydown', e => { if (e.key === 'Escape' && $('people-picker').open) { $('people-picker').open = false; $('people-picker').querySelector('summary').focus(); } });
let resizeTimer; window.addEventListener('resize', () => { clearTimeout(resizeTimer); resizeTimer = setTimeout(render, 150); });
