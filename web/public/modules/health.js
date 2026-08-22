/* ==========================================================================
   Header health control + the first-run setup banner + the VPN warning strip.

   Three problems this fixes:
   1. The app rendered a completely blank page when TMDB was unconfigured —
      hero hidden, all 16 rows hidden, "Search failed." — and NOTHING told the
      user to configure anything. GET /api/configured has always existed and
      no UI had ever called it.
   2. The health dot was a decorative coloured circle with a title attribute.
      It is now a real button opening a per-component popover.
   3. A user could queue 40 episodes with the VPN tunnel down and get 40
      failures with no warning. A persistent strip now says so up front.
   ========================================================================== */

import {esc, safeUrl} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {loadConfigured, loadHealth, issues, blockingIssues, componentLabel,
  HEALTH_COMPONENTS} from '../shared/status.js';
import {state} from './state.js';

function el(id) { return document.getElementById(id); }

let popoverOpen = false;

export function initHealthUI() {
  const dot = el('health-dot');
  dot.addEventListener('click', e => { e.stopPropagation(); togglePopover(); });
  document.addEventListener('click', e => {
    if (!popoverOpen) return;
    if (e.target.closest('#health-popover') || e.target.closest('#health-dot')) return;
    closePopover();
  });
  document.addEventListener('keydown', e => { if (e.key === 'Escape') closePopover(); });

  el('setup-banner').addEventListener('click', e => {
    if (e.target.closest('[data-banner-dismiss]')) {
      el('setup-banner').hidden = true;
      try { sessionStorage.setItem('jf_banner_dismissed', '1'); } catch (_) {}
    }
  });
}

/* ── Configured banner ───────────────────────────────────────────────────── */

export async function refreshConfigured() {
  const cfg = await loadConfigured({force: true});
  state.configured = cfg;
  renderBanner(cfg);
  renderVpnStrip(cfg);
  return cfg;
}

function renderBanner(cfg) {
  const host = el('setup-banner');
  if (!cfg.known) { host.hidden = true; return; }

  const blocking = blockingIssues(cfg);
  if (!blocking.length) { host.hidden = true; return; }

  let dismissed = false;
  try { dismissed = sessionStorage.getItem('jf_banner_dismissed') === '1'; } catch (_) {}
  // A missing TMDB key or zero indexers leaves the app with literally nothing
  // to show, so that banner is NOT dismissible — hiding it would put the user
  // straight back on a blank page with no explanation.
  const hard = blocking.some(b => b.key === 'tmdb' || b.key === 'prowlarr');
  if (dismissed && !hard) { host.hidden = true; return; }

  const rows = blocking.map(b => `<li><strong>${esc(b.label)}</strong> — ${esc(b.detail)}</li>`).join('');
  const links = blocking
    .filter(b => b.fix && b.fix.external)
    .map(b => `<a class="btn sm" href="${safeUrl(b.fix.external.url)}" target="_blank" rel="noopener">
        ${icon('external')} ${esc(b.fix.external.label)}</a>`)
    .join('');

  host.innerHTML = `<div class="callout warn setup-callout">${icon('alert')}
    <div class="callout-body">
      <div class="callout-title">JellyFreedom is not finished setting up</div>
      <p>Until this is fixed, search and browsing will stay empty — that is not a bug in your network.</p>
      <ul class="setup-list">${rows}</ul>
      <div class="callout-actions">
        <a class="btn primary" href="/dashboard/#setup">${icon('settings')} Finish setup in the dashboard</a>
        ${links}
        ${hard ? '' : `<button class="btn sm ghost" type="button" data-banner-dismiss="1">Dismiss</button>`}
      </div>
    </div></div>`;
  host.hidden = false;
}

/* ── VPN warning strip ───────────────────────────────────────────────────── */

function renderVpnStrip(cfg) {
  const strip = el('vpn-strip');
  if (!cfg.known || !cfg.extended) { strip.hidden = true; return; }
  if (!cfg.vpn_configured) {
    strip.innerHTML = `${icon('shield-off')}<span>No VPN config is active — torrent traffic is blocked by design, so every request will fail.</span>
      <a class="btn sm" href="/dashboard/#vpn">Upload a config</a>`;
    strip.hidden = false;
    return;
  }
  if (!cfg.vpn_active) {
    strip.innerHTML = `${icon('shield-off')}<span>The VPN tunnel is DOWN. Anything you queue now will fail until it comes back.</span>
      <a class="btn sm" href="/dashboard/#vpn">Check the tunnel</a>`;
    strip.hidden = false;
    return;
  }
  strip.hidden = true;
}

/* ── Health dot + popover ────────────────────────────────────────────────── */

export async function refreshHealth() {
  const h = await loadHealth();
  state.health = h;
  const dot = el('health-dot');
  dot.classList.remove('ok', 'err', 'degraded');
  if (!h.known) {
    dot.setAttribute('aria-label', 'Stack health: unknown');
    dot.title = 'Stack health unavailable';
  } else if (h.ok) {
    dot.classList.add('ok');
    dot.setAttribute('aria-label', 'Stack health: all components OK');
    dot.title = 'All components OK';
  } else {
    dot.classList.add(h.degraded.length >= 3 ? 'err' : 'degraded');
    const names = h.degraded.map(componentLabel).join(', ');
    dot.setAttribute('aria-label', `Stack health: degraded — ${names}`);
    dot.title = `Degraded: ${names}`;
  }
  if (popoverOpen) paintPopover();
  return h;
}

function togglePopover() {
  if (popoverOpen) closePopover(); else openPopover();
}

function openPopover() {
  popoverOpen = true;
  el('health-dot').setAttribute('aria-expanded', 'true');
  const pop = el('health-popover');
  pop.hidden = false;
  paintPopover();
}

function closePopover() {
  if (!popoverOpen) return;
  popoverOpen = false;
  el('health-dot').setAttribute('aria-expanded', 'false');
  el('health-popover').hidden = true;
}

function paintPopover() {
  const h = state.health || {known: false, degraded: []};
  const cfg = state.configured;
  const pop = el('health-popover');

  if (!h.known) {
    pop.innerHTML = `<h4>Stack health</h4>
      <p class="empty">This build does not publish a public health summary.
      Open the dashboard for per-service state.</p>
      <div class="callout-actions"><a class="btn sm" href="/dashboard/#health">${icon('activity')} Open Health</a></div>`;
    return;
  }

  const rows = HEALTH_COMPONENTS.map(k => {
    const bad = h.degraded.includes(k);
    return `<div class="pop-row">
      <span class="dot ${bad ? 'failed' : 'active'}" aria-hidden="true"></span>
      ${icon(bad ? 'alert' : 'check')}
      <span class="name">${esc(componentLabel(k))}</span>
      <span class="state ${bad ? 'bad' : 'ok'}">${bad ? 'degraded' : 'ok'}</span>
    </div>`;
  }).join('');

  const setupRows = cfg && cfg.known ? issues(cfg).filter(i => i.state !== 'ok') : [];
  const notes = setupRows.length
    ? `<div class="pop-notes">${setupRows.map(i =>
        `<p>${icon(i.state === 'bad' ? 'alert' : 'info')} <strong>${esc(i.label)}:</strong> ${esc(i.detail)}</p>`).join('')}</div>`
    : '';

  pop.innerHTML = `<h4>Stack health</h4>${rows}${notes}
    <div class="callout-actions"><a class="btn sm" href="/dashboard/#health">${icon('activity')} Open Health</a></div>`;
}
