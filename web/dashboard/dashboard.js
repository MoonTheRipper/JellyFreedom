/* ==========================================================================
   JellyFreedom dashboard — entry point.

   Asset URLs: this document is served at /dashboard/, and the shared modules
   live at /shared/ (web/public/shared on disk, because web/public is mounted
   at "/"). "../shared/x" and "../../shared/x" therefore resolve correctly from
   /dashboard/ and /dashboard/modules/ respectively.
   ========================================================================== */

import {setUnauthorizedHandler, visibleInterval, esc} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {loadHealth, componentLabel, HEALTH_COMPONENTS} from '../shared/status.js';
import * as nav from './modules/nav.js';
import {initChecklist, renderChecklist, hasBlockingIssues} from './modules/checklist.js';
import {initServices, fetchStatus} from './modules/services.js';
import {initVpn, refreshVpn} from './modules/vpn.js';
import {initLogs, stopAutoLog} from './modules/logs.js';
import {initUsers, fetchUsers} from './modules/users.js';
import {initTasks, fetchTasks, stopTaskPolling} from './modules/tasks.js';
import {initSettings, fetchSettings} from './modules/settings.js';
import {initUpdate, checkUpdate} from './modules/update.js';
import {initVersion} from './modules/version.js';

let popoverOpen = false;
let health = {known: false, ok: true, degraded: []};

async function init() {
  // The dashboard is admin-only: a 401 means the session is gone, so the login
  // page is the correct destination (unlike the public app, which prompts).
  setUnauthorizedHandler(() => { location.href = '/dashboard/login'; });

  initChecklist();
  initServices();
  initVpn();
  initLogs();
  initUsers();
  initTasks();
  initSettings();
  initUpdate();
  // Registered BEFORE the load-time check below, so the version panel receives
  // that check's state transitions rather than missing them.
  initVersion();
  initHealthDot();

  nav.onEnter('setup', renderChecklist);
  nav.onEnter('health', fetchStatus);
  nav.onEnter('vpn', refreshVpn);
  nav.onEnter('users', fetchUsers);
  nav.onEnter('tasks', fetchTasks);
  nav.onEnter('settings', fetchSettings);
  nav.onLeave('logs', stopAutoLog);
  nav.onLeave('tasks', stopTaskPolling);

  document.addEventListener('jf:config-changed', () => {
    if (nav.currentSection() === 'setup') renderChecklist();
    refreshHealth();
  });

  // A fresh install lands on the setup checklist, not on a list of systemd
  // units that says nothing about what is still missing. An explicit hash
  // (a deep link from the app's setup banner) wins and must not wait on a
  // network round trip first.
  let landing = 'health';
  if (!location.hash.replace(/^#/, '')) {
    try { if (await hasBlockingIssues()) landing = 'setup'; } catch (_) {}
  }
  nav.start({defaultSection: landing});

  // nav.start() already fired the entry hook for whichever section landed,
  // so do not fetch the same thing twice here.
  visibleInterval(() => { if (nav.currentSection() === 'health') fetchStatus(); }, 30000);
  refreshHealth();
  visibleInterval(refreshHealth, 30000);

  // Once per dashboard load. The result is cached server-side for 6h, and the
  // call is fire-and-forget: a build without the endpoint, a failed check or a
  // dev build all render nothing, and none of them may break init().
  checkUpdate().catch(e => console.warn('[jf] update check errored', e));
}

/* ── Header health dot (same control as the public app) ──────────────────── */

function initHealthDot() {
  const dot = document.getElementById('health-dot');
  dot.addEventListener('click', e => {
    e.stopPropagation();
    popoverOpen = !popoverOpen;
    document.getElementById('health-popover').hidden = !popoverOpen;
    dot.setAttribute('aria-expanded', String(popoverOpen));
    if (popoverOpen) paintPopover();
  });
  document.addEventListener('click', e => {
    if (!popoverOpen) return;
    if (e.target.closest('#health-popover') || e.target.closest('#health-dot')) return;
    closePopover();
  });
  document.addEventListener('keydown', e => { if (e.key === 'Escape') closePopover(); });
  document.getElementById('health-popover').addEventListener('click', e => {
    const go = e.target.closest('[data-goto]');
    if (go) { nav.showSection(go.dataset.goto); closePopover(); }
  });
}

function closePopover() {
  popoverOpen = false;
  document.getElementById('health-popover').hidden = true;
  document.getElementById('health-dot').setAttribute('aria-expanded', 'false');
}

async function refreshHealth() {
  health = await loadHealth();
  const dot = document.getElementById('health-dot');
  dot.classList.remove('ok', 'err', 'degraded');
  if (!health.known) {
    dot.setAttribute('aria-label', 'Stack health: unknown');
  } else if (health.ok) {
    dot.classList.add('ok');
    dot.setAttribute('aria-label', 'Stack health: all components OK');
  } else {
    dot.classList.add(health.degraded.length >= 3 ? 'err' : 'degraded');
    dot.setAttribute('aria-label', `Stack health: degraded — ${health.degraded.map(componentLabel).join(', ')}`);
  }
  const strip = document.getElementById('vpn-strip');
  if (health.known && health.degraded.includes('vpn')) {
    strip.innerHTML = `${icon('shield-off')}<span>The VPN tunnel is down — torrent traffic is blocked and every request will fail.</span>
      <button class="btn sm" type="button" data-goto-vpn="1">Open VPN</button>`;
    strip.hidden = false;
  } else {
    strip.hidden = true;
  }
  if (popoverOpen) paintPopover();
}

document.addEventListener('click', e => {
  if (e.target.closest('[data-goto-vpn]')) nav.showSection('vpn');
});

function paintPopover() {
  const pop = document.getElementById('health-popover');
  if (!health.known) {
    pop.innerHTML = `<h4>Stack health</h4>
      <p class="empty">This build does not publish a health summary.</p>
      <div class="callout-actions"><button class="btn sm" type="button" data-goto="health">
        ${icon('activity')} Open Health</button></div>`;
    return;
  }
  pop.innerHTML = `<h4>Stack health</h4>` + HEALTH_COMPONENTS.map(k => {
    const bad = health.degraded.includes(k);
    return `<div class="pop-row">
      <span class="dot ${bad ? 'failed' : 'active'}" aria-hidden="true"></span>
      ${icon(bad ? 'alert' : 'check')}
      <span class="name">${esc(componentLabel(k))}</span>
      <span class="state ${bad ? 'bad' : 'ok'}">${bad ? 'degraded' : 'ok'}</span>
    </div>`;
  }).join('') + `<div class="callout-actions">
    <button class="btn sm" type="button" data-goto="health">${icon('activity')} Health</button>
    <button class="btn sm" type="button" data-goto="setup">${icon('check')} Setup</button>
  </div>`;
}

init().catch(e => {
  console.error('[jf] dashboard init failed', e);
  const body = document.getElementById('services-body');
  if (body) {
    body.innerHTML = `<div class="callout err"><div class="callout-body">
      <div class="callout-title">The dashboard failed to start</div>
      <p>${esc(String(e && e.message ? e.message : e))}</p></div></div>`;
  }
});
