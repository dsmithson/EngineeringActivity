/* Header and footer branding from /api/v1/config (BRAND_* settings on the server). The HTML
   carries neutral defaults, so a failed request leaves a usable page. */
fetch('/api/v1/config').then(r => r.ok ? r.json() : null).then(b => {
  if (!b) return;
  const set = (key, text) => { for (const el of document.querySelectorAll(`[data-brand="${key}"]`)) el.textContent = text; };
  set('organization', b.organization); set('organization-upper', b.organization.toUpperCase());
  set('team-upper', b.team.toUpperCase()); set('scope', b.scope); set('mark', b.organization.charAt(0).toUpperCase());
  for (const a of document.querySelectorAll('[data-brand="link"]')) {
    if (b.linkUrl) { a.href = b.linkUrl; a.textContent = b.linkLabel + ' ↗'; a.hidden = false; }
  }
  document.title = `${b.organization} · ${document.title}`;
}).catch(() => {});
