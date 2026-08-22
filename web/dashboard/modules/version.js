/* ==========================================================================
   Version & updates — the always-present answer to "what am I running, and
   am I current?".

   The self-update BANNER (modules/update.js) is deliberately silent unless
   there is something to install. That made "no banner" ambiguous: up to date,
   check failed, and endpoint missing all looked identical, and the running
   version appeared nowhere in the dashboard at all. This panel is the source
   of truth that is always on screen:

     - the running version                      (/healthz, or `current`)
     - the state, explicitly, including failure (never "silence means fine")
     - when the last check happened
     - a Check now button that is ALWAYS present, not only when an update
       exists, and that passes ?refresh=1 so it bypasses the 6h server cache
     - the same Update now action the banner offers — imported, not re-built

   It shares one state object with the banner (update.js `last`), so
   dismissing the banner cannot make this panel claim to be up to date.

   Same house rules as the rest of web/: every interpolated value goes through
   esc(), the release link through safeUrl(), and there is not one inline
   on*= handler — buttons carry data-* markers read by one delegated listener.
   ========================================================================== */

import {esc, safeUrl, relTime, toast} from '../../shared/api.js';
import {icon} from '../../shared/icons.js';
import {
  checkUpdate, applyUpdate, updateState, onUpdateState, probeVersion, normVer, isDev,
} from './update.js';
import {showSection} from './nav.js';

const HOST_ID = 'version-body';
const CHIP_ID = 'header-version';

/* The running version is learned from /healthz, because /api/update/check can
   be absent (404) or unauthorised on a build that still answers /healthz. */
let running = '';
let runningKnown = false;
let checking = false;

const STATUS_META = {
  never:       {badge: 'inactive', label: 'Not checked yet', glyph: 'help',     tone: ''},
  checking:    {badge: 'inactive', label: 'Checking…',       glyph: 'refresh',  tone: 'info'},
  uptodate:    {badge: 'ok',       label: 'Up to date',      glyph: 'check',    tone: 'good'},
  available:   {badge: 'warn',     label: 'Update available',glyph: 'download', tone: 'info'},
  dev:         {badge: 'local',    label: 'Source build',    glyph: 'info',     tone: 'info'},
  failed:      {badge: 'failed',   label: 'Check failed',    glyph: 'alert',    tone: 'err'},
  unsupported: {badge: 'inactive', label: 'Not available',   glyph: 'info',     tone: 'warn'},
};

/* ── The view-model ───────────────────────────────────────────────────────
   viewModel() is the ONLY place the banner's state and the /healthz probe are
   combined, so versionHTML stays a pure function of plain data and can be
   driven headlessly. */
export function viewModel(snap = updateState()) {
  // A dev build is a dev build whichever side reported it, and the panel must
  // say so even before the first check has come back.
  const status = checking
    ? 'checking'
    : (isDev(running) && (snap.status === 'never' || snap.status === 'unsupported'))
      ? 'dev'
      : snap.status;
  return {
    running: running || snap.current || '',
    runningKnown: runningKnown || !!snap.current,
    status,
    latest: snap.latest || '',
    notes: snap.notes || [],
    url: snap.url || '',
    checkedAt: snap.checkedAt || '',
    publishedAt: snap.publishedAt || '',
    error: snap.error || '',
    phase: snap.phase || 'hidden',
    progress: snap.progress || '',
  };
}

/* ── Rendering ───────────────────────────────────────────────────────────── */

function fieldHTML(key, value, title) {
  const t = title ? ` title="${esc(title)}"` : '';
  return `<div class="ver-field">
    <span class="ver-key">${esc(key)}</span>
    <span class="ver-val"${t}>${esc(value)}</span>
  </div>`;
}

function notesHTML(notes, latest) {
  const items = (Array.isArray(notes) ? notes : [])
    .map(n => String(n === null || n === undefined ? '' : n).trim())
    .filter(Boolean)
    .slice(0, 6);                    // the contract caps at 6; enforce it here too
  if (!items.length) return '';
  return `<div class="ver-notes">
    <h4 class="sub-title">What is in ${esc(normVer(latest) || 'the new release')}</h4>
    <ul class="setup-list">${items.map(n => `<li>${esc(n)}</li>`).join('')}</ul>
  </div>`;
}

/** detailHTML is the one sentence that removes the ambiguity. Each status says
 *  what is true AND, where it matters, what it is not. */
function detailHTML(v) {
  switch (v.status) {
    case 'checking':
      return '<p>Asking GitHub for the latest published release…</p>';
    case 'uptodate':
      return `<p>${esc(normVer(v.running) || 'This build')} is the latest published release.</p>`;
    case 'available':
      return `<p>Version ${esc(normVer(v.latest))} has been published. This instance is running
        ${esc(normVer(v.running) || 'an older version')}.</p>`;
    case 'dev':
      return `<p>This binary was built from source and reports its version as
        <b>dev</b>, so the in-app updater does not apply to it — update it with a
        <b>git pull</b> and a rebuild.</p>` +
        (normVer(v.latest)
          ? `<p class="ver-sub">For reference, the latest published release is
             ${esc(normVer(v.latest))}.</p>`
          : '');
    case 'failed':
      return `<p>${esc(v.error || 'The update check could not be completed.')}</p>
        <p class="ver-sub">This is <b>not</b> the same as being up to date — the check
        did not complete, so nothing is known about newer releases. Try again.</p>`;
    case 'unsupported':
      return `<p>${esc(v.error || 'This build does not provide the update-check endpoint.')}</p>
        <p class="ver-sub">Update this instance from the release page or with
        <b>jellyfreedom --update</b> on the server.</p>`;
    default:
      return `<p>No update check has run yet in this dashboard session.</p>`;
  }
}

/** versionHTML is pure and exported so every state can be asserted headlessly,
 *  including with hostile version strings and release notes. */
export function versionHTML(v) {
  const meta = STATUS_META[v.status] || STATUS_META.never;
  const busy = v.phase === 'progress';

  const fields = [
    fieldHTML('Running version', v.runningKnown ? (normVer(v.running) || 'unknown') : 'unknown'),
    normVer(v.latest) ? fieldHTML('Latest release', normVer(v.latest)) : '',
    fieldHTML('Last checked', v.checkedAt ? relTime(v.checkedAt) : 'never', v.checkedAt || ''),
  ].join('');

  // The apply flow owns the page while it runs; mirror it rather than offering
  // a second, competing set of buttons.
  if (busy) {
    return `<div class="ver-fields">${fields}</div>
      <div class="callout info">${icon('refresh', 'spin')}<div class="callout-body">
        <div class="callout-title">Updating JellyFreedom</div>
        <p role="status" aria-live="polite">${esc(v.progress || 'Working…')}</p>
      </div></div>`;
  }
  if (v.phase === 'done') {
    return `<div class="ver-fields">${fields}</div>
      <div class="callout good">${icon('check')}<div class="callout-body">
        <div class="callout-title">Updated${normVer(v.latest) ? ' to ' + esc(normVer(v.latest)) : ''}</div>
        <p>Reload the dashboard to pick up the new assets.</p>
        <div class="callout-actions">
          <button class="btn primary sm" type="button" data-version-reload="1">
            ${icon('refresh')} Reload the dashboard</button>
        </div>
      </div></div>`;
  }

  const actions = [];
  if (v.status === 'available') {
    actions.push(`<button class="btn primary sm" type="button" data-update-apply="1">
      ${icon('download')} Update now</button>`);
  }
  actions.push(`<button class="btn sm" type="button" data-version-check="1"
    ${v.status === 'checking' ? 'disabled' : ''}>
    ${icon('refresh', v.status === 'checking' ? 'spin' : '')} Check now</button>`);
  if (v.url) {
    actions.push(`<a class="btn sm ghost" href="${safeUrl(v.url)}" target="_blank" rel="noopener">
      ${icon('external')} ${v.status === 'available' ? 'Release notes' : 'Latest release'}</a>`);
  }

  return `<div class="ver-fields">${fields}</div>
    <div class="callout ${esc(meta.tone)} ver-state">${icon(meta.glyph, v.status === 'checking' ? 'spin' : '')}
      <div class="callout-body">
        <div class="callout-title">
          <span class="badge ${esc(meta.badge)}">${esc(meta.label)}</span>
        </div>
        ${detailHTML(v)}
        ${v.status === 'available' ? notesHTML(v.notes, v.latest) : ''}
        <div class="callout-actions">${actions.join('')}</div>
      </div>
    </div>`;
}

/* ── DOM wiring ──────────────────────────────────────────────────────────── */

function host() { return document.getElementById(HOST_ID); }

function paintChip(v) {
  const chip = document.getElementById(CHIP_ID);
  if (!chip) return;
  const label = normVer(v.running);
  // Stay hidden until the version is actually known — a chip reading "unknown"
  // is worse than no chip, and would be shown for the first paint every load.
  if (!v.runningKnown || !label) { chip.hidden = true; return; }
  // textContent, never innerHTML: this string comes off the wire.
  chip.textContent = label.toLowerCase().startsWith('dev') ? 'dev build' : 'v' + label;
  chip.setAttribute('aria-label', `Running JellyFreedom ${label} — open version and updates`);
  chip.classList.toggle('has-update', v.status === 'available');
  chip.hidden = false;
}

export function renderVersion() {
  const v = viewModel();
  const el = host();
  if (el) el.innerHTML = versionHTML(v);
  paintChip(v);
}

/** runCheck forces a fresh check. force=true adds ?refresh=1, which is the
 *  whole point of the manual button: without it the server answers from a 6h
 *  cache and the button looks broken. */
async function runCheck({force = true} = {}) {
  if (checking) return;
  checking = true;
  renderVersion();
  try {
    await checkUpdate({force});
  } catch (e) {
    console.warn('[jf] manual update check failed', e);
    toast('The update check failed — see the browser console.', {ok: false});
  } finally {
    checking = false;
    renderVersion();
  }
}

/** loadRunningVersion asks /healthz, which needs no session and exists on
 *  every build. probeVersion returns null when the server is unreachable. */
async function loadRunningVersion() {
  const v = await probeVersion();
  if (v === null) return;
  running = v;
  runningKnown = true;
  renderVersion();
}

export function initVersion() {
  const el = host();
  if (el) {
    el.addEventListener('click', e => {
      if (e.target.closest('[data-version-check]')) { runCheck({force: true}); return; }
      if (e.target.closest('[data-update-apply]')) { applyUpdate(); return; }
      if (e.target.closest('[data-version-reload]')) { location.reload(); }
    });
  }
  const chip = document.getElementById(CHIP_ID);
  if (chip) {
    chip.addEventListener('click', () => {
      showSection('settings');
      const card = document.getElementById('version-card');
      if (card && card.scrollIntoView) card.scrollIntoView({block: 'start', behavior: 'smooth'});
    });
  }
  // Mirror every banner transition (offer, progress, done) without duplicating
  // the state machine.
  onUpdateState(() => renderVersion());
  renderVersion();
  loadRunningVersion().catch(e => console.warn('[jf] /healthz probe failed', e));
}
