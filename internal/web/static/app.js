/* No external scripts, fonts, or chart services: all assets are served by Go. */
const $ = id => document.getElementById(id);
const ns = 'http://www.w3.org/2000/svg';
let latest, groups = [], activeController, requestNumber = 0;
const num = n => new Intl.NumberFormat().format(n);
// Ranges offered at the top of the page; the server aligns buckets in the display timezone.
const RANGES = {
  '7d': {eyebrow: 'THE LAST SEVEN DAYS · ALL 24 HOURS', title: 'Past 7 days', detail: 'Hourly · faded hours not yet collected'},
  '30d': {eyebrow: 'THE LAST 30 DAYS · DAY BY DAY', title: 'Past 30 days', detail: 'Daily · current day is partial'},
  '12m': {eyebrow: 'THE LAST 12 MONTHS · WEEK BY WEEK', title: 'Past 12 months', detail: 'Weekly (Mon–Sun) · current week is partial'},
  '24m': {eyebrow: 'THE LAST 24 MONTHS · WEEK BY WEEK', title: 'Past 24 months', detail: 'Weekly (Mon–Sun) · faded weeks precede collection'},
};
const UNIT = {hour: {word: 'hour', adverb: 'Hourly'}, day: {word: 'day', adverb: 'Daily'}, week: {word: 'week', adverb: 'Weekly'}, month: {word: 'month', adverb: 'Monthly'}};
const unit = () => UNIT[latest?.bucket] || UNIT.hour;
function node(tag, attrs = {}, text) { const el = document.createElementNS(ns, tag); for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v); if (text !== undefined) el.textContent = text; return el; }
function date(at, options = {}) { return new Intl.DateTimeFormat('en-US', {timeZone: $('timezone').value, month: 'short', day: 'numeric', ...options}).format(new Date(at)); }
const localPart = (at, part) => new Intl.DateTimeFormat('en-GB', {timeZone: $('timezone').value, hourCycle: 'h23', [part]: part === 'weekday' ? 'short' : part === 'month' ? 'numeric' : '2-digit'}).format(new Date(at));
// Bucket label for tooltips and readouts.
function bucketLabel(at) {
  switch (latest.bucket) {
    case 'day': return date(at, {weekday: 'short', year: 'numeric'});
    case 'week': return 'Week of ' + date(at, {year: 'numeric'});
    case 'month': return new Intl.DateTimeFormat('en-US', {timeZone: $('timezone').value, month: 'long', year: 'numeric'}).format(new Date(at));
    default: return date(at, {hour: '2-digit', minute: '2-digit', hourCycle: 'h23'});
  }
}
// Axis tick for bucket i: gridline plus an optional label (midnights, Mondays, or month starts).
// monthEvery labels every Nth month on a calendar grid so January (with its year) is always shown.
function tick(b, i, monthEvery = 1) {
  const at = b[i].at;
  switch (latest.bucket) {
    case 'day': return localPart(at, 'weekday') === 'Mon' ? date(at) : null;
    case 'week': case 'month': {
      if (i > 0 && localPart(at, 'month') === localPart(b[i - 1].at, 'month')) return null;
      if (i === 0 && latest.bucket === 'week') return null;
      const month = Number(localPart(at, 'month')), jan = month === 1;
      if ((month - 1) % monthEvery !== 0) return '';
      return new Intl.DateTimeFormat('en-US', {timeZone: $('timezone').value, month: 'short', ...(jan ? {year: 'numeric'} : {})}).format(new Date(at));
    }
    default: return localPart(at, 'hour') === '00' ? date(at) : null;
  }
}
const bucketEnd = (b, i) => new Date(i + 1 < b.length ? b[i + 1].at : latest.end);
// Buckets before collection coverage or after the latest collection are not confirmed quiet.
function coverage(b, i) {
  if (latest.collectedThrough && new Date(b[i].at) >= new Date(latest.collectedThrough)) return 'not yet collected';
  if (latest.collectedFrom && new Date(b[i].at) < new Date(latest.collectedFrom)) return bucketEnd(b, i) <= new Date(latest.collectedFrom) ? 'before collection coverage' : 'partly before collection coverage';
  return '';
}
const uncovered = (b, i) => coverage(b, i) !== '' && coverage(b, i) !== 'partly before collection coverage';
const isPartial = (i, b) => i === b.length - 1 && (latest.partialLastBucket ?? latest.partialLastHour) !== false;
function plot(id, series, unknownKey, opts = {}) {
  const holder = $(id); holder.replaceChildren(); const b = latest.buckets, u = unit();
  if (!series.length) { const empty = document.createElement('div'); empty.className = 'empty'; empty.textContent = 'Select at least one area above.'; holder.append(empty); return; }
  const width = Math.max(holder.clientWidth, 260), height = 180, left = 37, right = 8, top = 12, bottom = 27;
  const pw = width - left - right, ph = height - top - bottom, step = pw / b.length;
  const max = Math.max(1, ...b.map(row => series.reduce((n, s) => n + row[s.key], 0)));
  const svg = node('svg', {viewBox: `0 0 ${width} ${height}`, class: 'plot', role: 'img', 'aria-label': `${series.map(s => s.label).join(' and ')} per ${u.word}, ${(RANGES[latest.range] || RANGES['7d']).title.toLowerCase()}`});
  for (let n = 0; n <= 3; n++) { const y = top + ph - ph * n / 3; svg.append(node('line', {x1:left,y1:y,x2:width-right,y2:y,class:'grid'})); svg.append(node('text', {x:left-6,y:y+3,'text-anchor':'end'}, Intl.NumberFormat('en',{notation:'compact',maximumFractionDigits:1}).format(max*n/3))); }
  let lastLabelX = -Infinity;
  const monthPx = step * (latest.bucket === 'week' ? 52 / 12 : 1), monthEvery = [1, 2, 3, 6, 12].find(k => k * monthPx >= 46) || 12;
  const readout = document.createElement('div'); readout.className = 'hour-readout'; readout.textContent = `Hover over a ${u.word} for details.`;
  b.forEach((row, i) => {
    const x = left + i * step, label = tick(b, i, monthEvery);
    if (label !== null) { svg.append(node('line',{x1:x,y1:top,x2:x,y2:top+ph,class:'grid'})); if (label && x - lastLabelX >= (latest.bucket === 'hour' ? 44 : 40) && x + 30 < width) { svg.append(node('text',{x:x+2,y:height-7},label)); lastLabelX = x; } }
    const note = coverage(b, i), faded = uncovered(b, i);
    let accumulated = 0;
    for (const s of series) { const value = row[s.key], barHeight = value / max * ph; const attrs = {x,y:top+ph-accumulated-barHeight,width:Math.max(.7,step-(step>4?1:.5)),height:barHeight,fill:s.color,opacity:faded?.3:isPartial(i,b)?.6:1}; if (opts.gaps && barHeight > 2) Object.assign(attrs, {stroke: '#111e30', 'stroke-width': 1}); svg.append(node('rect',attrs)); accumulated += barHeight; }
    if (faded) svg.append(node('rect',{x,y:top,width:Math.max(.7,step-.5),height:ph,class:'uncovered'}));
    const hit = node('rect',{x,y:top,width:step,height:ph,fill:'transparent'});
    const text = `${bucketLabel(row.at)}: ${series.map(s=>`${s.label} ${num(row[s.key])}`).join(' · ')}${unknownKey&&row[unknownKey]?' · incomplete statistics':''}${isPartial(i,b)?` · partial ${u.word}`:''}${note?` · ${note}`:''}`;
    hit.append(node('title',{},text)); hit.addEventListener('pointerenter',()=>readout.textContent=text); svg.append(hit);
  });
  holder.append(svg);
  const legend = document.createElement('div'); legend.className = 'legend';
  for (const s of series) { const span = document.createElement('span'), swatch = document.createElement('i'); swatch.className = s.class || ''; if (!s.class) swatch.style.background = s.color; span.append(swatch, `${s.label} · ${num(b.reduce((sum,row)=>sum+row[s.key],0))}`); legend.append(span); }
  holder.append(legend, readout);
  const unknown = unknownKey ? b.reduce((n,row)=>n+row[unknownKey],0) : 0;
  if (unknown) { const warning = document.createElement('div'); warning.className = 'chart-note'; warning.textContent = `Incomplete statistics for ${num(unknown)} events; displayed sums include known values only.`; holder.append(warning); }
  // Download provides the complete data without making every bar a keyboard stop.
  const download = document.createElement('button'); download.type = 'button'; download.textContent = `Download ${u.adverb.toLowerCase()} CSV`;
  download.addEventListener('click', () => {
    const first = latest.bucket === 'hour' ? 'hour_utc' : `${u.word}_start_utc`;
    const csv = [first+','+series.map(s=>s.key).join(',')+(unknownKey?',unknown_events':''), ...b.map(row=>row.at+','+series.map(s=>row[s.key]).join(',')+(unknownKey?','+row[unknownKey]:''))].join('\r\n');
    const url = URL.createObjectURL(new Blob([csv],{type:'text/csv'})); const a = document.createElement('a'); a.href = url; a.download = id + (latest.range && latest.range !== '7d' ? '-' + latest.range : '') + '.csv'; a.click(); setTimeout(()=>URL.revokeObjectURL(url),1000);
  });
  holder.append(download);
}
const mint=(key,label)=>({key,label,color:'#6be1be',class:'mint'}), blue=(key,label)=>({key,label,color:'#719dff',class:'blue'});
function render(){
 if(!latest)return;
 const r=RANGES[latest.range]||RANGES['7d'],u=unit();
 $('eyebrow').textContent=r.eyebrow;$('range-title').textContent=r.title;$('range-detail').textContent=latest.bucket==='hour'?RANGES['7d'].detail:r.detail;
 for(const el of document.querySelectorAll('.unit-word'))el.textContent=u.word;
 const fixture=!latest.demo&&latest.dataset==='fixture';$('mode').textContent=latest.demo?'DEMO · SAMPLE DATA':fixture?'DEV FIXTURE DATA':'LIVE SOURCE';$('mode').className='pill'+(latest.demo||fixture?' demo':'');
 const total=key=>latest.buckets.reduce((n,b)=>n+(b[key]||0),0);$('summary').replaceChildren();
 for(const [label,key,note] of [['PRs completed','prCompleted','By PR creator'],['Unique commits','commits',`${num(total('merges'))} merge commits excluded · ${num(total('aiMarkedCommits'))} with AI co-author markers (lower bound)`],['Tasks created','taskCreated','By historical assignee'],['Completion events','taskCompleted','Reopened tasks can complete again']]){const card=document.createElement('div');card.className='stat';for(const [tag,text] of [['span',label],['strong',num(total(key))],['small',note]]){const el=document.createElement(tag);el.textContent=text;card.append(el)}$('summary').append(card)}
 plot('pr-count',[blue('prCreated','Created'),mint('prCompleted','Completed')]);plot('pr-lines',[mint('prAdded','Added'),blue('prDeleted','Deleted')],'prUnknown');plot('pr-files',[mint('prFiles','Files')],'prUnknown');plot('pr-commits',[mint('prCommits','Commits')],'prUnknown');plot('commits',[mint('commits','Commits')]);plot('commit-lines',[mint('added','Added'),blue('deleted','Deleted')],'commitUnknown');plot('commit-files',[mint('files','Files')],'commitUnknown');plot('tasks',[blue('taskCreated','Created'),mint('taskCompleted','Completed')]);renderAreas();
 const clock=at=>date(at,{hour:'2-digit',minute:'2-digit',hourCycle:'h23'});$('updated').textContent=`View refreshed ${clock(new Date())}`+(latest.demo?'':latest.collectedThrough?` · Collected through ${clock(latest.collectedThrough)}`:' · Not yet collected')+(latest.collectedFrom&&!latest.demo?` · History from ${date(latest.collectedFrom,{year:'numeric'})}`:'');
}
// Work areas: commits classified by changed paths (rules in work_area_rules). Checkboxes pick
// visible areas; hidden areas leave the stack and the axis rescales to what is shown.
const AREA_PRESETS = {all: () => true, software: a => a.kind === 'software', firmware: a => a.kind === 'firmware' || a.kind === 'fpga', novendor: a => a.kind !== 'other'};
let visibleAreas = null;
function areaSelection(){
 const areas=latest.workAreas||[];
 if(!visibleAreas){let saved=null;try{saved=new URLSearchParams(location.search).get('areas')}catch{}visibleAreas=new Set(saved?saved.split(',').filter(a=>areas.some(x=>x.area===a)):areas.filter(AREA_PRESETS.novendor).map(a=>a.area))}
 return areas.filter(a=>visibleAreas.has(a.area));
}
function renderAreas(){
 const section=$('areas-section'),areas=latest.workAreas||[];section.hidden=!areas.length;if(!areas.length)return;
 const shown=areaSelection(),box=$('area-filters');box.replaceChildren();
 for(const a of areas){const label=document.createElement('label'),input=document.createElement('input'),swatch=document.createElement('i');input.type='checkbox';input.value=a.area;input.checked=visibleAreas.has(a.area);swatch.style.background=a.color;label.className='area-toggle';label.append(input,swatch,a.label);input.addEventListener('change',()=>{input.checked?visibleAreas.add(a.area):visibleAreas.delete(a.area);saveState();renderAreas()});box.append(label)}
 for(const b of latest.buckets)for(const a of areas){const v=(b.areas||{})[a.area];b['areaLines_'+a.area]=v?v.lines:0;b['areaCommits_'+a.area]=v?v.commits:0}
 plot('area-lines',shown.map(a=>({key:'areaLines_'+a.area,label:a.label,color:a.color})),null,{gaps:true});
 plot('area-commits',shown.map(a=>({key:'areaCommits_'+a.area,label:a.label,color:a.color})),null,{gaps:true});
}
for(const btn of document.querySelectorAll('[data-area-preset]'))btn.addEventListener('click',()=>{if(!latest)return;visibleAreas=new Set((latest.workAreas||[]).filter(AREA_PRESETS[btn.dataset.areaPreset]).map(a=>a.area));saveState();renderAreas()});
// Rebuild the contributor selector: Everyone, then groups, then people active in the range.
function fillWho(identities){
 const select=$('user'),selected=select.value;select.replaceChildren(new Option('Everyone',''));
 if(groups.length){const og=document.createElement('optgroup');og.label='Groups';for(const g of groups)og.append(new Option(`${g.description||g.name} (${g.members})`,'group:'+g.name));select.append(og)}
 const people=document.createElement('optgroup');people.label='People';for(const u of identities)people.append(new Option(u.name,u.id));select.append(people);
 if(selected&&![...select.options].some(o=>o.value===selected))people.append(new Option('Selected contributor (no activity in range)',selected));
 select.value=selected;
}
function saveState(){
 try{const p=new URLSearchParams(location.search);p.set('range',$('range').value);$('user').value?p.set('who',$('user').value):p.delete('who');if(visibleAreas)p.set('areas',[...visibleAreas].join(','));history.replaceState(null,'','?'+p.toString())}catch{}
}
// Collection status: a one-line headline, then one line per source (stale or failed ones flagged).
function showStatus(data){
 const box=$('status');box.replaceChildren();box.className='';
 const line=(text,cls)=>{const el=document.createElement('div');el.textContent=text;if(cls)el.className=cls;box.append(el)};
 if(data.demo){line('Preview only — deterministic sample activity, not Azure DevOps data.'+($('range').value!=='7d'?' Longer ranges need the database.':''));return}
 const names={commits:'Commits',pull_requests:'Pull requests',tasks:'Tasks'},sources=data.sources||[],clock=at=>date(at,{hour:'2-digit',minute:'2-digit',hourCycle:'h23'});
 const missing=Object.keys(names).filter(n=>!sources.some(s=>s.source===n));
 const bad=s=>s.outcome==='failed'||!s.lastSuccess||Date.now()-new Date(s.lastSuccess)>2*60*60*1000;
 const issue=missing.length>0||sources.some(bad);
 if(data.dataset==='fixture')line('Development fixture data from PostgreSQL — deterministic sample activity, not Azure DevOps data.');
 line(issue?'Collection needs attention — data may be incomplete or stale.':'Collection healthy — all sources synced within the last 2 hours.',issue?'status-head error':'status-head');
 for(const s of sources){
  const what=`${names[s.source]||s.source} (${s.scopeName||s.scope})`;
  const state=s.outcome==='failed'?`last attempt failed (${s.errorClass||'error'})${s.lastSuccess?` · last success ${clock(s.lastSuccess)}`:''}`:s.lastSuccess?`synced ${clock(s.lastSuccess)}`:'awaiting first sync';
  line(`${what}: ${state}`,bad(s)?'error':'');
 }
 for(const n of missing)line(`${names[n]}: awaiting collection`,'error');
}
async function refresh(){
 const number=++requestNumber;activeController?.abort();activeController=new AbortController();$('refresh').disabled=true;saveState();
 const who=$('user').value,q=new URLSearchParams({range:$('range').value,tz:$('timezone').value,user:who.startsWith('group:')?'':who});if(who.startsWith('group:'))q.set('group',who.slice(6));
 try{
  const groupsRequest=fetch('/api/v1/groups',{signal:activeController.signal}).then(r=>r.ok?r.json():{groups:[]}).catch(e=>{if(e.name==='AbortError')throw e;return {groups:[]}});
  const res=await fetch('/api/v1/activity?'+q.toString(),{signal:activeController.signal});if(!res.ok)throw Error(`Activity unavailable (HTTP ${res.status})`);const data=await res.json();groups=(await groupsRequest).groups||[];if(number!==requestNumber)return;latest=data;
  fillWho(data.identities||[]);
  showStatus(data);render();
 }catch(e){if(e.name!=='AbortError'){$('status').textContent=e.message+(latest?' — showing the previous successful view.':'');$('status').className='error'}}finally{if(number===requestNumber)$('refresh').disabled=false}
}
(function restore(){try{const p=new URLSearchParams(location.search);if(RANGES[p.get('range')])$('range').value=p.get('range');const who=p.get('who');if(who){$('user').append(new Option(who,who));$('user').value=who}}catch{}})();
$('range').addEventListener('change',refresh);$('user').addEventListener('change',refresh);
// Hourly buckets are UTC instants, so only labels change; day/week buckets are aligned in the zone and need new data.
$('timezone').addEventListener('change',()=>latest&&latest.bucket!=='hour'&&!latest.demo?refresh():render());
// Filters stay pinned while charts scroll (desktop/tablet); compact once pinned for screenshots.
try{new IntersectionObserver(([e])=>document.querySelector('.toolbar').classList.toggle('stuck',!e.isIntersecting&&e.boundingClientRect.top<0)).observe($('toolbar-sentinel'))}catch{}
$('refresh').addEventListener('click',refresh);let resizeTimer;window.addEventListener('resize',()=>{clearTimeout(resizeTimer);resizeTimer=setTimeout(render,150)});setInterval(refresh,60000);refresh();
