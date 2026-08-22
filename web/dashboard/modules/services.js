/* ==========================================================================
   Health section: systemd unit state + restarts.

   NOTE ON ASSET PATHS: the shared modules live at web/public/shared/ on disk
   because the orchestrator serves web/public at "/" and web/dashboard at
   "/dashboard/". "../../shared/x.js" from /dashboard/modules/ therefore
   resolves to the URL /shared/x.js, which is served from web/public/shared/.
   One copy, reachable from both documents, no backend change needed.

   Fixed here: restartAll() restarted the orchestrator LAST — killing the very
   server serving this page — and then the page silently failed. It now warns
   about exactly that up front and polls until the server answers again.
   ========================================================================== */

import {apiFetch, apiTry, esc, toast, errorState} from '../../shared/api.js';
import {icon} from '../../shared/icons.js';

/* The orchestrator is deliberately restarted LAST and called out separately:
   it is the process serving this page. */
const STACK_ORDER = ['vpntorrent-netns', 'torrserver-netns', 'prowlarr', 'flaresolverr', 'jellyfin'];
const SELF_UNIT = 'jellyfreedom';

export function initServices() {
  document.getElementById('section-health').addEventListener('click', e => {
    const r = e.target.closest('[data-restart]');
    if (r) { restart(r.dataset.restart); return; }
    if (e.target.closest('[data-restart-all]')) { restartAll(); return; }
    if (e.target.closest('[data-reload]')) { location.reload(); return; }
    if (e.target.closest('[data-services-refresh]')) fetchStatus();
  });
}

export async function fetchStatus() {
  const body = document.getElementById('services-body');
  let data;
  try {
    data = await apiFetch('/api/status');
  } catch (e) {
    body.innerHTML = errorState(e.message, {retryAttr: 'data-services-refresh="1"'});
    return null;
  }
  renderServices(data.services || []);
  return data;
}

function renderServices(services) {
  const body = document.getElementById('services-body');
  if (!services.length) {
    body.innerHTML = `<div class="empty">No services reported.</div>`;
    return;
  }
  body.innerHTML = services.map(s => {
    const cls = s.active === 'active' ? 'active' : s.active === 'failed' ? 'failed'
      : s.active === 'inactive' ? 'inactive' : 'unknown';
    const glyph = cls === 'active' ? 'check' : cls === 'failed' ? 'alert'
      : cls === 'inactive' ? 'minus' : 'help';
    const since = String(s.since || '').replace(/CEST|CET|UTC/g, '').trim();
    return `<div class="svc-row">
      <span class="dot ${cls}" aria-hidden="true"></span>
      ${icon(glyph)}
      <span class="svc-name">${esc(s.name)}</span>
      <span class="badge ${cls}">${esc(s.sub || s.active || '')}</span>
      <span class="svc-bind">${esc(s.bind || '')}</span>
      <span class="svc-since">${esc(since)}</span>
      <button class="btn sm" type="button" data-restart="${esc(s.name)}">${icon('refresh')} Restart</button>
    </div>`;
  }).join('');
}

async function restart(svc) {
  // Restarting the orchestrator is not like restarting the other five. It cannot be done
  // through sudo — the policy deliberately withholds `restart jellyfreedom`, because a
  // service that can bounce itself through root is a persistence primitive. Instead the
  // server shuts itself down and systemd starts it again, which means:
  //   * the reply is 202 "accepted", NOT "done";
  //   * this page then loses its server for a few seconds, which is the success path;
  //   * a 409 means a restart is already running, which is benign, not an error.
  // Reported by the owner as "jellyfreedom is non-restartable".
  if (svc === SELF_UNIT) return restartSelf();
  try {
    await apiFetch(`/api/services/${encodeURIComponent(svc)}/restart`, {method: 'POST'});
  } catch (e) { toast(e.message, {ok: false}); return; }
  toast(`Restarted ${svc}`);
  setTimeout(fetchStatus, 2000);
}

async function restartSelf() {
  if (!confirm('Restart JellyFreedom?\n\nThis page will lose its connection for a few '
             + 'seconds and reconnect on its own.\n\nAny in-flight torrent stream will be '
             + 'interrupted.')) return;

  const banner = document.getElementById('restart-banner');
  banner.hidden = false;
  banner.innerHTML = `<div class="callout info">${icon('refresh')}<div class="callout-body">
    <div class="callout-title">Restarting JellyFreedom…</div>
    <p id="restart-progress">Asking the service to restart.</p></div></div>`;
  const progress = document.getElementById('restart-progress');

  try {
    await apiFetch(`/api/services/${encodeURIComponent(SELF_UNIT)}/restart`, {method: 'POST'});
  } catch (e) {
    // status 0 == the connection dropped, which is exactly what we asked for.
    // 409 == a restart is already in flight; join it rather than reporting a failure.
    if (e.status && e.status !== 409) {
      // 412 (the unit would not come back) and 501 are real, actionable refusals: nothing
      // happened and the service is still up, so surface the server's own wording.
      banner.innerHTML = `<div class="callout err">${icon('alert')}<div class="callout-body">
        <div class="callout-title">JellyFreedom was not restarted</div>
        <p>${esc(e.message)}</p></div></div>`;
      return;
    }
  }

  const back = await waitForServer(progress);
  if (back) {
    banner.hidden = true;
    toast('JellyFreedom restarted');
    fetchStatus();
    return;
  }
  banner.innerHTML = `<div class="callout err">${icon('alert')}<div class="callout-body">
    <div class="callout-title">JellyFreedom did not come back</div>
    <p>It has not answered for 40 seconds. Check it on the host with
    <code>systemctl status jellyfreedom</code> and
    <code>journalctl -u jellyfreedom -n 100</code>.</p>
    <div class="callout-actions"><button class="btn sm" type="button"
      data-reload="1">${icon('refresh')} Reload page</button></div>
  </div></div>`;
}

/**
 * restartAll bounces the whole stack. The orchestrator goes last, so the page
 * loses its server for a few seconds; we say that plainly, then wait for it.
 */
async function restartAll() {
  const msg = 'Restart the entire stack?\n\n' +
    'JellyFreedom itself is restarted last, so this page will lose its connection ' +
    'for a few seconds. It will reconnect on its own.\n\n' +
    'Any in-flight torrent stream will be interrupted.';
  if (!confirm(msg)) return;

  const banner = document.getElementById('restart-banner');
  banner.hidden = false;
  banner.innerHTML = `<div class="callout info">${icon('refresh')}<div class="callout-body">
    <div class="callout-title">Restarting the stack…</div>
    <p id="restart-progress">Starting.</p></div></div>`;
  const progress = document.getElementById('restart-progress');

  const failures = [];
  for (const s of STACK_ORDER) {
    progress.textContent = `Restarting ${s}…`;
    try { await apiFetch(`/api/services/${encodeURIComponent(s)}/restart`, {method: 'POST'}); }
    catch (e) { failures.push(`${s}: ${e.message}`); }
    await sleep(500);
  }

  progress.textContent = `Restarting ${SELF_UNIT} — this page will go quiet for a moment…`;
  try { await apiFetch(`/api/services/${encodeURIComponent(SELF_UNIT)}/restart`, {method: 'POST'}); }
  catch (e) {
    // A dropped connection here is EXPECTED: we just asked the server to die.
    if (e.status !== 0) failures.push(`${SELF_UNIT}: ${e.message}`);
  }

  const back = await waitForServer(progress);
  if (!back) {
    banner.innerHTML = `<div class="callout err">${icon('alert')}<div class="callout-body">
      <div class="callout-title">JellyFreedom did not come back</div>
      <p>The orchestrator has not answered for 40 seconds. Check it on the host with
      <code>systemctl status jellyfreedom</code> and <code>journalctl -u jellyfreedom -n 100</code>.</p>
      <div class="callout-actions"><button class="btn sm" type="button"
        data-reload="1">${icon('refresh')} Reload page</button></div>
    </div></div>`;
    return;
  }

  if (failures.length) {
    banner.innerHTML = `<div class="callout warn">${icon('alert')}<div class="callout-body">
      <div class="callout-title">Stack restarted, with problems</div>
      <ul class="setup-list">${failures.map(f => `<li>${esc(f)}</li>`).join('')}</ul>
    </div></div>`;
  } else {
    banner.hidden = true;
    toast('Stack restarted');
  }
  fetchStatus();
}

/** waitForServer polls /healthz until the orchestrator answers again. */
async function waitForServer(progress) {
  for (let i = 0; i < 20; i++) {
    await sleep(2000);
    if (progress) progress.textContent = `Waiting for JellyFreedom to come back… (${(i + 1) * 2}s)`;
    const r = await apiTry('/healthz');
    if (r.ok) return true;
  }
  return false;
}

function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }

