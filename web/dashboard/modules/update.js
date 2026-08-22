/* ==========================================================================
   In-dashboard self-update banner.

   Contract: GET /api/update/check -> {current,latest,available,notes[],url,
   published_at,checked_at,error}; POST /api/update/apply -> 200 {started} /
   409 already running / 503 updater not installed.

   Two things drive every design choice in this file:

   1. THE SERVER GOES AWAY MID-UPDATE, ON PURPOSE. The updater restarts
      jellyfreedom.service, so /healthz stops answering for several seconds.
      That is the SUCCESS path, not a failure — an unreachable server during
      the poll window renders as "restarting…". Only running out of the 5
      minute budget is an error, and even then the advice is "go read the
      journal", because the update may well have succeeded.

   2. `notes` IS REMOTE TEXT FROM GITHUB. Every value that reaches the DOM
      goes through esc(), the release link goes through safeUrl(), and there
      is not one inline on*= handler here — the buttons carry data-* markers
      and one delegated listener reads them. Same rule as the rest of web/.

   The banner is silent unless there is genuinely something to install: no
   endpoint on this build (404/401), a failed check, a dev build, or a version
   the operator already dismissed all render nothing at all.
   ========================================================================== */

import {apiFetch, apiTry, esc, safeUrl} from '../../shared/api.js';
import {icon} from '../../shared/icons.js';

const HOST_ID = 'update-banner';
const DISMISS_KEY = 'jf.update.dismissed';   // stores the version that was dismissed
const POLL_EVERY_MS = 3000;                  // /healthz probe interval
const POLL_BUDGET_MS = 5 * 60 * 1000;        // 5 minutes, then give up and point at the journal
const PROBE_TIMEOUT_MS = 2500;               // a restarting socket can hang; bound each probe

/* The one mutable view-model FOR THE BANNER. Rendering is a pure function of it. */
let view = {phase: 'hidden', current: '', latest: '', notes: [], url: '', message: ''};
let polling = false;

/* ── Shared check state ───────────────────────────────────────────────────
   `view` drives the transient banner, which the operator can dismiss. `last`
   is the durable record of what the most recent check actually said, and it
   is NEVER cleared by a dismissal — the always-present version panel
   (modules/version.js) reads it, so "I dismissed the banner" can never turn
   into "the dashboard says I am up to date".

   status is one of:
     never       nothing has been asked yet
     checking    a request is in flight
     uptodate    the check succeeded and this build is the newest release
     available   a newer release exists
     dev         a source build; the API deliberately never offers it an update
     failed      the check itself failed (offline, rate limit, bad tag)
     unsupported this build has no /api/update/check (404), or no admin session */
const EMPTY_CHECK = {
  status: 'never', current: '', latest: '', notes: [], url: '',
  publishedAt: '', checkedAt: '', error: '',
};
let last = {...EMPTY_CHECK};

const listeners = new Set();

/** onUpdateState subscribes to every change of the check state OR the apply
 *  phase, and returns an unsubscribe function. */
export function onUpdateState(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/** updateState returns an immutable snapshot: the last check plus the live
 *  apply phase, so a second view can mirror an update in progress. */
export function updateState() {
  return {
    ...last,
    notes: last.notes.slice(),
    phase: view.phase,
    progress: view.message,
    applyLatest: view.latest,
  };
}

function emit() {
  const snap = updateState();
  // One listener throwing must not stop the others, and must never break an
  // update that is mid-flight.
  for (const fn of listeners) {
    try { fn(snap); } catch (e) { console.warn('[jf] update listener failed', e); }
  }
}

/* ── Version helpers ──────────────────────────────────────────────────────
   The backend already compares semver-aware; the frontend only ever needs
   equality, but a "v" prefix and stray whitespace are noise on both sides. */
export function normVer(v) {
  return String(v === null || v === undefined ? '' : v).trim().replace(/^v/i, '');
}
export function sameVer(a, b) {
  const x = normVer(a), y = normVer(b);
  return x !== '' && x === y;
}
/** isDev mirrors update.IsDev in Go: a normalised version starting with "dev"
 *  is a source build. The API reports available:false for one on purpose, so
 *  the UI must say "source build", not "up to date" and not "error". */
export function isDev(v) {
  return normVer(v).toLowerCase().startsWith('dev');
}

/* ── Dismissal (per version) ──────────────────────────────────────────────
   localStorage throws in a few real contexts (Safari private mode, a browser
   set to block site data), and this banner must never be the thing that
   breaks the dashboard. Every access is wrapped. */
function dismissedVersion() {
  try { return normVer(localStorage.getItem(DISMISS_KEY) || ''); } catch (_) { return ''; }
}
function rememberDismissal(v) {
  try { localStorage.setItem(DISMISS_KEY, normVer(v)); } catch (_) {}
}

/* ── Rendering ────────────────────────────────────────────────────────────
   bannerHTML is pure and exported so it can be driven headlessly against
   hostile payloads without a browser. */

function notesHTML(notes) {
  const items = (Array.isArray(notes) ? notes : [])
    .map(n => String(n === null || n === undefined ? '' : n).trim())
    .filter(Boolean)
    .slice(0, 6);                                  // the contract caps at 6; enforce it here too
  if (!items.length) return '';
  return `<ul class="setup-list update-notes">${items.map(n => `<li>${esc(n)}</li>`).join('')}</ul>`;
}

/** progressHTML wraps the live region. role=status + aria-live=polite so the
 *  "restarting… / updated" transitions are announced without stealing focus. */
function progressHTML(text, actions) {
  return `<div class="callout info update-banner">${icon('refresh', 'spin')}
    <div class="callout-body">
      <div class="callout-title">Updating JellyFreedom</div>
      <p class="update-progress" id="update-progress-text" role="status" aria-live="polite">${esc(text)}</p>
      ${actions || ''}
    </div></div>`;
}

export function bannerHTML(v) {
  if (!v || v.phase === 'hidden') return '';

  if (v.phase === 'offer') {
    const link = v.url
      ? `<a class="btn sm ghost" href="${safeUrl(v.url)}" target="_blank" rel="noopener">
           ${icon('external')} Full release notes</a>`
      : '';
    return `<div class="callout info update-banner">${icon('download')}
      <div class="callout-body">
        <div class="callout-title">JellyFreedom ${esc(v.latest)} is available</div>
        <p>This instance is running ${esc(v.current || 'an older version')}.</p>
        ${notesHTML(v.notes)}
        <div class="callout-actions">
          <button class="btn primary sm" type="button" data-update-apply="1">
            ${icon('download')} Update now</button>
          ${link}
          <button class="btn sm ghost" type="button" data-update-dismiss="1">
            ${icon('x')} Dismiss</button>
        </div>
      </div></div>`;
  }

  if (v.phase === 'progress') {
    return progressHTML(v.message);
  }

  if (v.phase === 'done') {
    return `<div class="callout good update-banner">${icon('check')}
      <div class="callout-body">
        <div class="callout-title">Updated to ${esc(v.latest)}</div>
        <p class="update-progress" id="update-progress-text" role="status" aria-live="polite">${esc(v.message)}</p>
        <div class="callout-actions">
          <button class="btn primary sm" type="button" data-update-reload="1">
            ${icon('refresh')} Reload the dashboard</button>
        </div>
      </div></div>`;
  }

  if (v.phase === 'timeout') {
    // The command is a literal written here, never anything from a response,
    // so it is the one place a <code> element is built rather than escaped text.
    return `<div class="callout warn update-banner">${icon('clock')}
      <div class="callout-body">
        <div class="callout-title">Still waiting for the service to come back</div>
        <p class="update-progress" id="update-progress-text" role="status" aria-live="polite">${esc(v.message)}</p>
        <p class="update-cmd"><code>journalctl -u jellyfreedom-update</code></p>
        <div class="callout-actions">
          <button class="btn sm" type="button" data-update-reload="1">${icon('refresh')} Reload anyway</button>
          <button class="btn sm ghost" type="button" data-update-dismiss="1">${icon('x')} Dismiss</button>
        </div>
      </div></div>`;
  }

  // 'failed' — the update never started (503 updater missing, or a hard error).
  return `<div class="callout err update-banner">${icon('alert')}
    <div class="callout-body">
      <div class="callout-title">The update could not be started</div>
      <p class="update-progress" id="update-progress-text" role="status" aria-live="polite">${esc(v.message)}</p>
      <div class="callout-actions">
        <button class="btn sm ghost" type="button" data-update-dismiss="1">${icon('x')} Dismiss</button>
      </div>
    </div></div>`;
}

function host() { return document.getElementById(HOST_ID); }

function render() {
  const el = host();
  if (el) {
    const html = bannerHTML(view);
    el.innerHTML = html;
    el.hidden = html === '';
  }
  emit();
}

/** setProgress updates the EXISTING live region in place where it can.
 *  Replacing the whole node would insert a brand-new aria-live element, and
 *  content present at insertion time is not reliably announced; mutating the
 *  text of a live region already in the tree is. */
function setProgress(message) {
  view.message = message;
  const p = document.getElementById('update-progress-text');
  if (p && view.phase === 'progress') { p.textContent = message; emit(); return; }
  render();
}

function hide() {
  view = {phase: 'hidden', current: '', latest: '', notes: [], url: '', message: ''};
  render();
}

/* ── Load-time check ─────────────────────────────────────────────────────── */

/**
 * checkUpdate asks the backend once per dashboard load. It NEVER throws and
 * NEVER shows anything unless there is a real, undismissed update:
 *   - endpoint absent on this build (404/401/403) -> silent
 *   - error field set, or the request failed      -> console only
 *   - available:false (includes every dev build)  -> silent
 */
export async function checkUpdate({force = false} = {}) {
  last = {...last, status: 'checking'};
  emit();

  // ?refresh=1 bypasses the 6h server-side cache. The automatic load-time call
  // must NOT use it (that would hit GitHub on every dashboard load); the manual
  // "Check now" button must, or it looks broken.
  const r = await apiTry('/api/update/check' + (force ? '?refresh=1' : ''));
  if (!r.ok) {
    // Absent is the normal case on an older build; anything else is worth one console line.
    if (!r.absent) console.warn('[jf] update check failed:', r.error);
    last = {
      ...last,
      status: r.absent ? 'unsupported' : 'failed',
      error: r.absent
        ? 'This build does not provide the update-check endpoint.'
        : (r.error || 'The update check could not be completed.'),
      // The server had no chance to stamp checked_at, but the operator still
      // needs to know when the attempt was made.
      checkedAt: new Date().toISOString(),
    };
    emit();
    return null;
  }
  const d = r.data || {};
  const dev = isDev(d.current);
  last = {
    status: d.error ? 'failed' : dev ? 'dev' : d.available ? 'available' : 'uptodate',
    current: String(d.current || ''),
    latest: String(d.latest || ''),
    notes: Array.isArray(d.notes) ? d.notes : [],
    url: String(d.url || ''),
    publishedAt: String(d.published_at || ''),
    checkedAt: String(d.checked_at || '') || new Date().toISOString(),
    error: String(d.error || ''),
  };
  emit();

  if (d.error) { console.warn('[jf] update check reported:', d.error); return null; }
  if (!d.available) return null;
  if (!normVer(d.latest)) return null;                       // nothing sensible to offer
  if (sameVer(dismissedVersion(), d.latest)) return null;    // dismissed for THIS version only

  view = {
    phase: 'offer',
    current: String(d.current || ''),
    latest: String(d.latest || ''),
    notes: Array.isArray(d.notes) ? d.notes : [],
    url: String(d.url || ''),
    message: '',
  };
  render();
  return d;
}

/* ── Applying ────────────────────────────────────────────────────────────── */

export async function applyUpdate() {
  // The version panel can start an update while the banner is dismissed or was
  // never shown. Seed the banner view-model from the last check so the restart
  // poll below knows which version it is waiting for.
  if (view.phase === 'hidden' || !normVer(view.latest)) {
    view = {
      phase: 'offer',
      current: last.current,
      latest: last.latest,
      notes: last.notes.slice(),
      url: last.url,
      message: '',
    };
  }
  // Every apply affordance on the page, not just the banner's.
  document.querySelectorAll('[data-update-apply]').forEach(b => { b.disabled = true; });

  try {
    await apiFetch('/api/update/apply', {method: 'POST'});
  } catch (e) {
    const status = e && e.status;
    if (status === 409) {
      // Already running — someone else pressed the button, or a previous
      // attempt is mid-flight. Join the progress state rather than erroring.
      beginProgress('An update is already running — waiting for the service to restart…');
      return;
    }
    view.phase = 'failed';
    view.message = status === 503
      ? (e.message || 'The updater is not installed on this instance.')
      : (e.message || 'The update request failed.');
    render();
    return;
  }
  beginProgress('Starting the updater…');
}

function beginProgress(message) {
  view.phase = 'progress';
  view.message = message;
  render();
  if (!polling) waitForRestart();
}

/* ── The restart window ───────────────────────────────────────────────────
   This is the part that is easy to get wrong. Sequence on a healthy update:

     t+0s   /healthz answers, still the OLD version   -> "installing…"
     t+Ns   connection refused / times out            -> "restarting…"   (NORMAL)
     t+Ms   /healthz answers with the NEW version     -> done

   The middle band is not an error and must never be rendered as one. We also
   accept "came back with a version that is neither the old one nor exactly
   `latest`" as success once we have seen it go down, because the tag name and
   the binary's own version string can legitimately differ in punctuation. */

function sleep(ms) { return new Promise(res => setTimeout(res, ms)); }

/** probeVersion returns the reported version string, '' if the server
 *  answered without one, or null if the server could not be reached.
 *  Exported: it is also how the version panel learns the running version when
 *  /api/update/check is unavailable on this build. */
export async function probeVersion() {
  const ctrl = typeof AbortController === 'function' ? new AbortController() : null;
  const timer = ctrl ? setTimeout(() => ctrl.abort(), PROBE_TIMEOUT_MS) : null;
  try {
    // Deliberately a bare fetch rather than the shared apiFetch/apiTry helpers. Those invoke
    // the global 401 handler, which navigates to the login page — and behind an
    // authenticating reverse proxy the restart window is exactly when a 401 is likely. That
    // would throw the operator out to a login screen mid-update. /healthz needs no session,
    // so nothing is lost by bypassing the helper here.
    const r = await fetch('/healthz', {
      cache: 'no-store',
      credentials: 'same-origin',
      signal: ctrl ? ctrl.signal : undefined,
    });
    if (!r.ok) return null;                        // 5xx == still restarting
    const d = await r.json().catch(() => null);
    return String((d && d.version) || '');
  } catch {
    return null;                                   // unreachable or aborted == still restarting
  } finally {
    if (timer) clearTimeout(timer);
  }
}

async function waitForRestart() {
  polling = true;
  const expected = view.latest;
  const before = view.current;
  const deadline = Date.now() + POLL_BUDGET_MS;
  let sawDown = false;

  try {
    while (Date.now() < deadline) {
      await sleep(POLL_EVERY_MS);
      if (view.phase !== 'progress') return;       // dismissed or superseded

      const v = await probeVersion();

      if (v === null) {
        // EXPECTED. systemd is stopping/starting the unit right now.
        sawDown = true;
        setProgress('Restarting the service… this normally takes a few seconds.');
        continue;
      }
      if (sameVer(v, expected) || (sawDown && normVer(v) && !sameVer(v, before))) {
        view.phase = 'done';
        view.latest = normVer(v) || expected;
        view.message = 'The service is back up. Reload the page to pick up the new dashboard assets.';
        render();
        return;
      }
      // Reachable, still the old build: the updater is downloading/installing.
      setProgress(sawDown
        ? 'The service is back, finishing up…'
        : 'Downloading and installing the new version…');
    }

    view.phase = 'timeout';
    view.message = 'The service has not reported the new version within five minutes. '
      + 'The update may still have succeeded — check the updater journal:';
    render();
  } finally {
    polling = false;
  }
}

/* ── Wiring ──────────────────────────────────────────────────────────────── */

export function initUpdate() {
  const el = host();
  if (!el) return;
  // One delegated listener for the whole banner. No inline handlers anywhere:
  // release note text is remote and would be an injection site in an attribute.
  el.addEventListener('click', e => {
    if (e.target.closest('[data-update-apply]')) { applyUpdate(); return; }
    if (e.target.closest('[data-update-reload]')) { location.reload(); return; }
    if (e.target.closest('[data-update-dismiss]')) {
      if (view.latest) rememberDismissal(view.latest);
      hide();
    }
  });
}
