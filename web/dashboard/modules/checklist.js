/* ==========================================================================
   Setup checklist — the dashboard's answer to "what is still missing?".

   A new user's first-run experience was: create an admin account, land on the
   Health page, see a list of systemd units, and get no hint that TMDB needs a
   key or that Prowlarr needs at least one indexer. This section states each
   requirement, its live state, how to fix it, and where to get what is needed.
   ========================================================================== */

import {esc, apiTry, safeUrl} from '../../shared/api.js';
import {icon} from '../../shared/icons.js';
import {loadConfigured, issues, HELP_LINKS} from '../../shared/status.js';
import {showSection} from './nav.js';

const STATE_META = {
  ok:      {glyph: 'check', cls: 'ok',      label: 'Ready'},
  warn:    {glyph: 'alert', cls: 'warn',    label: 'Needs attention'},
  bad:     {glyph: 'x',     cls: 'bad',     label: 'Not configured'},
  unknown: {glyph: 'help',  cls: 'unknown', label: 'Unknown'},
};

let prowlarrUrl = '';
let jellyfinUrl = '';

export function initChecklist() {
  document.getElementById('section-setup').addEventListener('click', e => {
    const go = e.target.closest('[data-goto]');
    if (go) { showSection(go.dataset.goto); return; }
    if (e.target.closest('[data-checklist-refresh]')) renderChecklist();
  });
}

export async function renderChecklist() {
  const host = document.getElementById('checklist-body');
  host.innerHTML = skeleton();

  // Connection URLs come from the admin settings endpoint so the checklist can
  // offer a real "open Prowlarr" link rather than a generic doc page.
  const settings = await apiTry('/api/settings');
  if (settings.ok && settings.data && settings.data.connections) {
    const c = settings.data.connections;
    prowlarrUrl = (c.prowlarr && c.prowlarr.url) || '';
    jellyfinUrl = (c.jellyfin && c.jellyfin.url) || '';
  }

  const cfg = await loadConfigured({force: true});
  if (!jellyfinUrl && cfg.jellyfin_url) jellyfinUrl = cfg.jellyfin_url;

  if (!cfg.known) {
    host.innerHTML = `<div class="callout err">${icon('alert')}<div class="callout-body">
      <div class="callout-title">Could not read the configuration state</div>
      <p>GET /api/configured did not answer. The orchestrator may be mid-restart.</p>
      <div class="callout-actions">
        <button class="btn sm" type="button" data-checklist-refresh="1">${icon('refresh')} Retry</button>
      </div></div></div>`;
    return;
  }

  const rows = issues(cfg);
  const remaining = rows.filter(r => r.state === 'bad').length;
  const warnings = rows.filter(r => r.state === 'warn').length;

  const summary = remaining
    ? `<div class="callout err">${icon('alert')}<div class="callout-body">
        <div class="callout-title">${remaining} thing${remaining === 1 ? '' : 's'} still to do</div>
        <p>The app will not work properly until these are resolved.</p></div></div>`
    : warnings
      ? `<div class="callout warn">${icon('info')}<div class="callout-body">
          <div class="callout-title">Working, with ${warnings} warning${warnings === 1 ? '' : 's'}</div>
          <p>Core setup is complete. The items below are optional or currently degraded.</p></div></div>`
      : `<div class="callout good">${icon('check')}<div class="callout-body">
          <div class="callout-title">Setup complete</div>
          <p>Everything JellyFreedom needs is configured. Go and request something.</p></div></div>`;

  host.innerHTML = summary + `<ol class="check-list">${rows.map(rowHTML).join('')}</ol>` + footerHTML();
}

function rowHTML(r) {
  const meta = STATE_META[r.state] || STATE_META.unknown;
  const actions = [];
  if (r.fix && r.fix.hash) {
    actions.push(`<button class="btn sm" type="button" data-goto="${esc(r.fix.hash.replace(/^#/, ''))}">
      ${icon('settings')} ${esc(r.fix.label || 'Fix this')}</button>`);
  }
  if (r.key === 'prowlarr' && prowlarrUrl) {
    actions.push(`<a class="btn sm ghost" href="${safeUrl(prowlarrUrl)}" target="_blank" rel="noopener">
      ${icon('external')} Open Prowlarr</a>`);
  }
  if (r.key === 'jellyfin' && jellyfinUrl) {
    actions.push(`<a class="btn sm ghost" href="${safeUrl(jellyfinUrl)}" target="_blank" rel="noopener">
      ${icon('external')} Open Jellyfin</a>`);
  }
  if (r.fix && r.fix.external) {
    actions.push(`<a class="btn sm ghost" href="${safeUrl(r.fix.external.url)}" target="_blank" rel="noopener">
      ${icon('help')} ${esc(r.fix.external.label)}</a>`);
  }
  return `<li class="check-row check-${meta.cls}">
    <span class="check-icon">${icon(meta.glyph)}</span>
    <div class="check-body">
      <div class="check-head">
        <span class="check-label">${esc(r.label)}</span>
        <span class="badge ${meta.cls === 'bad' ? 'failed' : meta.cls === 'warn' ? 'warn' : meta.cls === 'ok' ? 'ok' : 'inactive'}">${esc(meta.label)}</span>
      </div>
      <p class="check-detail">${esc(r.detail)}</p>
      ${actions.length ? `<div class="check-actions">${actions.join('')}</div>` : ''}
    </div>
  </li>`;
}

function footerHTML() {
  return `<div class="check-footer">
    <h3 class="sub-title">Useful links</h3>
    <div class="check-actions">
      <a class="btn sm ghost" href="${safeUrl(HELP_LINKS.tmdb.url)}" target="_blank" rel="noopener">
        ${icon('key')} ${esc(HELP_LINKS.tmdb.label)}</a>
      <a class="btn sm ghost" href="${safeUrl(HELP_LINKS.prowlarr_indexers.url)}" target="_blank" rel="noopener">
        ${icon('search')} ${esc(HELP_LINKS.prowlarr_indexers.label)}</a>
      <a class="btn sm ghost" href="${safeUrl(HELP_LINKS.jellyfin_library.url)}" target="_blank" rel="noopener">
        ${icon('library')} ${esc(HELP_LINKS.jellyfin_library.label)}</a>
      <a class="btn sm ghost" href="${safeUrl(HELP_LINKS.wireguard.url)}" target="_blank" rel="noopener">
        ${icon('shield')} ${esc(HELP_LINKS.wireguard.label)}</a>
      <a class="btn sm ghost" href="/" >${icon('play')} Back to the app</a>
    </div>
  </div>`;
}

function skeleton() {
  return `<div class="skeleton sk-line" style="width:45%;height:16px"></div>` +
    Array(5).fill(0).map(() =>
      `<div class="sk-row"><div class="skeleton" style="width:20px;height:20px;border-radius:50%"></div>
       <div style="flex:1">
         <div class="skeleton sk-line" style="width:35%"></div>
         <div class="skeleton sk-line" style="width:85%"></div>
       </div></div>`).join('');
}

/** hasBlockingIssues answers whether the dashboard should land on setup. */
export async function hasBlockingIssues() {
  const cfg = await loadConfigured();
  return issues(cfg).some(i => i.blocking);
}
