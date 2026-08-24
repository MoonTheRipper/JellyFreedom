/* ==========================================================================
   Release picker.

   Release titles are the single most attacker-controlled string in this app:
   they come straight from whoever uploaded the torrent to whichever indexer.
   They used to be interpolated into onclick="selectRelease('${mag}','${tit}')"
   with a jsStr() that escaped only backslashes and single quotes — an XSS and
   a guaranteed SyntaxError on any title containing an apostrophe.

   Now: the fetched array is kept in module state, rows carry only a numeric
   index, and the magnet never appears in the DOM at all.
   ========================================================================== */

import {apiFetch, esc, fmtSize, sequencer, isAbort, errorState} from '../shared/api.js';
import {icon} from '../shared/icons.js';

const seq = sequencer();

/** One picker is open at a time (movie panel or an episode accordion). */
const picker = {
  releases: [],
  selectedMagnet: null,
  selectedTitle: '',
  previousRelease: '',
  ctx: null,     // {tmdbId, mediaType, season, episode}
};

export function selectedMagnet() { return picker.selectedMagnet; }
export function resetPicker(previousRelease = '') {
  seq.abort();
  picker.releases = [];
  picker.selectedMagnet = null;
  picker.selectedTitle = '';
  picker.previousRelease = previousRelease || '';
}

function panel() { return document.getElementById('release-picker-panel'); }
function label() { return document.getElementById('sel-label'); }

/** togglePicker opens/closes the picker for the given context. */
export async function togglePicker(ctx, {forceOpen = false} = {}) {
  const p = panel();
  if (!p) return;
  if (!forceOpen && !p.hidden) { p.hidden = true; return; }
  p.hidden = false;
  picker.ctx = ctx;
  await load(ctx);
}

export async function reloadPicker() {
  if (picker.ctx) await load(picker.ctx);
}

/* Searching indexers is SLOW — measured at 43s against a real Prowlarr with 16 indexers,
   returning 106 releases. A bare shimmer for three quarters of a minute is indistinguishable
   from a hung UI, which is exactly how it was reported: "new cards when clicked just show the
   shining aesthetic of searching". So the wait says what it is doing, how long it has been
   doing it, and that a slow answer is normal. */
function skeleton(elapsed) {
  const secs = Number.isFinite(elapsed) ? elapsed : 0;
  const slow = secs >= 20
    ? `<p class="rel-wait-note">Still going. Indexers that have gone cold can take a while to
       wake up — this is normal, not a hang.</p>`
    : '';
  return `<div class="rel-wait" role="status" aria-live="polite">
      <div class="rel-wait-head">${icon('refresh', 'spin')}
        <span>Searching your indexers…<span class="rel-wait-secs"> ${secs}s</span></span>
      </div>
      <p class="rel-wait-note">This asks every indexer you have configured and can take up to
        a minute the first time.</p>
      ${slow}
    </div>
    <div class="release-list" aria-hidden="true">${Array(5).fill(0).map(() =>
      `<div class="rel-row"><div class="skeleton sk-line" style="width:100%"></div></div>`).join('')}</div>`;
}

/* startWaitTicker keeps the elapsed count live and returns a stop function. The interval is
   cleared on every exit path below — a stray ticker would keep writing into a panel that has
   since been replaced by results, an error, or a different title. */
function startWaitTicker(p) {
  const began = Date.now();
  let slowShown = false;
  p.innerHTML = skeleton(0);
  const id = setInterval(() => {
    const secs = Math.round((Date.now() - began) / 1000);
    const el = p.querySelector('.rel-wait-secs');
    if (!el) { clearInterval(id); return; }   // panel replaced; stop writing to it
    el.textContent = ` ${secs}s`;
    // >= with a latch, not === : a tick can be late. A GC pause is enough to jump 19 -> 21,
    // and a background tab is throttled to roughly one tick a MINUTE, so an equality test
    // skips the threshold entirely — losing the reassurance in exactly the case it exists
    // for, since a long wait is when someone is most likely to have switched away.
    if (secs >= 20 && !slowShown) {
      slowShown = true;
      p.innerHTML = skeleton(secs);   // upgrade to the "still going" note, once
    }
  }, 1000);
  return () => clearInterval(id);
}

async function load(ctx) {
  const p = panel();
  if (!p) return;
  const stopTicker = startWaitTicker(p);

  let url = `/api/releases?tmdb_id=${encodeURIComponent(ctx.tmdbId)}&type=${encodeURIComponent(ctx.mediaType)}`;
  if (ctx.mediaType === 'tv' && ctx.season && ctx.episode) {
    url += `&season=${encodeURIComponent(ctx.season)}&episode=${encodeURIComponent(ctx.episode)}`;
  }

  const token = seq.next();
  let releases;
  try {
    releases = await apiFetch(url, {signal: token.signal});
  } catch (e) {
    stopTicker();
    if (isAbort(e) || !seq.isCurrent(token)) return;
    // A 500 here used to render as "No releases found", which reads as
    // "your indexers had nothing" — the single most misleading message in
    // the app when Prowlarr is misconfigured.
    p.innerHTML = errorState(`Could not search for releases: ${e.message}`,
      {retryAttr: 'data-rel-retry="1"', compact: true});
    return;
  }
  stopTicker();
  if (!seq.isCurrent(token)) return;

  picker.releases = Array.isArray(releases) ? releases : [];
  if (!picker.releases.length) {
    p.innerHTML = `<div class="callout warn">${icon('alert')}<div class="callout-body">
      <div class="callout-title">Your indexers returned no releases for this title</div>
      <p>The search succeeded — there simply was nothing. If this happens for everything, check that
      Prowlarr has at least one healthy indexer, and that the quality filters are not rejecting
      every result.</p></div></div>`;
    return;
  }

  p.innerHTML = `<div class="release-list" role="listbox" aria-label="Available releases">${
    picker.releases.map(rowHTML).join('')}</div>`;

  const bestIdx = picker.releases.findIndex(r => r.is_best);
  if (bestIdx >= 0) select(bestIdx);
}

function rowHTML(r, i) {
  const isPrev = picker.previousRelease && r.title === picker.previousRelease;
  const match = r.title_match
    ? `<span class="rel-match ok" title="Title matches">${icon('check')}</span>`
    : `<span class="rel-match bad" title="Title does not match cleanly">${icon('alert')}</span>`;
  const chips = [];
  if (isPrev) chips.push(`<span class="rel-tag prev">previous</span>`);
  if (r.is_cam) chips.push(`<span class="rel-tag bad">${icon('alert')} CAM</span>`);
  if (r.quality && !r.is_cam) chips.push(`<span class="rel-tag good">${esc(r.quality)}</span>`);
  for (const t of [r.video_codec, r.audio_codec, r.container,
    r.seeders ? r.seeders + 's' : '', fmtSize(r.size_bytes)]) {
    if (t) chips.push(`<span class="rel-tag">${esc(t)}</span>`);
  }
  return `<button type="button" role="option" aria-selected="${!!r.is_best}"
      class="rel-row${r.is_best ? ' active' : ''}${isPrev ? ' is-prev' : ''}" data-rel-idx="${i}">
    ${match}<span class="rel-title">${esc(r.title)}</span>
    <span class="rel-tags">${chips.join('')}</span>
  </button>`;
}

/** select marks a release as chosen. Index only — no magnet in the DOM. */
export function select(i) {
  const r = picker.releases[i];
  if (!r) return;
  picker.selectedMagnet = r.magnet || null;
  picker.selectedTitle = r.title || '';
  document.querySelectorAll('.rel-row').forEach(el => {
    const on = Number(el.dataset.relIdx) === i;
    el.classList.toggle('active', on);
    el.setAttribute('aria-selected', String(on));
  });
  const lbl = label();
  if (lbl) {
    lbl.textContent = picker.selectedTitle.length > 52
      ? picker.selectedTitle.slice(0, 49) + '…' : picker.selectedTitle;
    lbl.hidden = false;
  }
  if (!r.magnet) {
    // Contract §2 strips magnet for anonymous callers. Say so instead of
    // letting the request silently fall back to the server's own pick.
    const lbl2 = label();
    if (lbl2) lbl2.textContent = picker.selectedTitle + ' (sign in to force this exact release)';
  }
}

/** wirePicker delegates the row clicks for a container that holds a picker. */
export function wirePicker(root, {onRetry} = {}) {
  if (!root || root.dataset.relWired === '1') return;
  root.dataset.relWired = '1';
  root.addEventListener('click', e => {
    const row = e.target.closest('[data-rel-idx]');
    if (row) { select(Number(row.dataset.relIdx)); return; }
    if (e.target.closest('[data-rel-retry]')) { if (onRetry) onRetry(); else reloadPicker(); }
  });
}

export function previousRelease() { return picker.previousRelease; }
export function setPreviousRelease(t) { picker.previousRelease = t || ''; }
