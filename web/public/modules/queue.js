/* ==========================================================================
   Request queue page.

   Fixed here:
   - The list was re-rendered wholesale every 4s while season expansion lived
     purely as a DOM class, so an expanded season collapsed under the user
     every 4 seconds and focus was destroyed mid-click. Open state now lives in
     JS, and an unchanged poll does not touch the DOM at all.
   - Per-stage progress (contract §6 `stage`) renders as a stepper, falling
     back to the free-text `progress` when the field is absent.
   - "Why did this fail?" (contract §7) shows which filter rejected which
     candidate release, instead of leaving "no suitable release found" as a
     dead end.
   - Retry on a TV episode now carries season+episode through the route.
   ========================================================================== */

import {apiFetch, apiTry, esc, num, toast, errorState} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {state, savePrefs, loadQueue, showTitle, emit} from './state.js';
import {requireLogin} from './auth.js';
import {navItem} from './router.js';

/* Contract §6: closed, ordered set. */
const STAGES = ['queued', 'indexing', 'picking', 'adding', 'verifying', 'writing', 'done'];
const STAGE_LABEL = {
  queued: 'Queued', indexing: 'Searching indexers', picking: 'Picking release',
  adding: 'Adding torrent', verifying: 'Verifying', writing: 'Writing .strm', done: 'Done',
};

const BUCKETS = [
  {key: 'processing', label: 'In Progress', color: 'var(--blue)',  glyph: 'refresh'},
  {key: 'failed',     label: 'Failed',      color: 'var(--red)',   glyph: 'alert'},
  {key: 'done',       label: 'Completed',   color: 'var(--green)', glyph: 'check'},
  {key: 'cancelled',  label: 'Cancelled',   color: 'var(--muted)', glyph: 'minus'},
];

/* Expansion + diagnosis state lives HERE, not in the DOM. */
const openSeasons = new Set();      // "tmdbId:season"
const openDiagnosis = new Map();    // queue id -> {loading, error, data}
let qSearch = '';
let lastSignature = null;
let loadedOnce = false;      // before the first successful load, show skeletons
let loadError = null;        // last poll failure, surfaced instead of "empty"

function el(id) { return document.getElementById(id); }
function bucketOf(s) { return (s === 'pending' || s === 'processing') ? 'processing' : s; }

export function initQueue() {
  el('queue-search').addEventListener('input', e => {
    qSearch = e.target.value.trim().toLowerCase();
    render(true);
  });
  el('queue-sort').addEventListener('change', e => {
    state.prefs.qSort = e.target.value;
    savePrefs();
    render(true);
  });

  const side = document.querySelector('.queue-sidebar');
  side.addEventListener('click', e => {
    const f = e.target.closest('.qs-filter');
    if (f) {
      if (f.dataset.grp === 'status') state.prefs.qFilterStatus = f.dataset.val;
      else state.prefs.qFilterType = f.dataset.val;
      savePrefs();
      render(true);
      return;
    }
    if (e.target.closest('#queue-clear')) clearFinished();
  });

  el('queue-page-list').addEventListener('click', onListClick);
  el('queue-page-list').addEventListener('keydown', e => {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    const head = e.target.closest('.season-group-head');
    if (!head) return;
    e.preventDefault();
    toggleSeason(head.dataset.key);
  });
}

function onListClick(e) {
  const head = e.target.closest('.season-group-head');
  if (head && !e.target.closest('button')) { toggleSeason(head.dataset.key); return; }

  const nav = e.target.closest('[data-queue-nav]');
  if (nav) {
    navItem(nav.dataset.type, Number(nav.dataset.tmdbId));
    return;
  }
  const act = e.target.closest('[data-qact]');
  if (!act) return;
  const id = Number(act.dataset.id);
  switch (act.dataset.qact) {
    case 'cancel':  cancelItem(id); break;
    case 'delete':  deleteItem(id); break;
    case 'retry':   retryItem(id); break;
    case 'diagnose': toggleDiagnosis(id); break;
    case 'reload':  pollQueue(); break;
  }
}

function queueSkeleton() {
  return Array(4).fill(0).map(() =>
    `<div class="sk-row"><div class="skeleton" style="width:44px;height:66px;flex-shrink:0"></div>
     <div style="flex:1">
       <div class="skeleton sk-line" style="width:42%"></div>
       <div class="skeleton sk-line" style="width:26%"></div>
       <div class="skeleton sk-line" style="width:60%"></div>
     </div></div>`).join('');
}

function toggleSeason(key) {
  if (!key) return;
  if (openSeasons.has(key)) openSeasons.delete(key); else openSeasons.add(key);
  render(true);
}

/* ── Poll ────────────────────────────────────────────────────────────────── */

export async function pollQueue() {
  try {
    await loadQueue();
    loadedOnce = true;
    loadError = null;
  } catch (e) {
    // Never leave the page blank and unexplained: a persistently failing
    // /api/queue used to render as "Queue is empty", which is a lie.
    loadError = e.message;
    console.warn('[jf] queue poll failed:', e.message);
    if (el('page-queue').classList.contains('active')) render(true);
    return;
  }
  updateBadge();
  if (el('page-queue').classList.contains('active')) render(false);
}

function updateBadge() {
  const pending = state.queueItems.filter(i => i.status === 'pending' || i.status === 'processing').length;
  const badge = el('queue-badge');
  if (pending > 0) {
    badge.textContent = String(pending);
    badge.hidden = false;
    badge.setAttribute('aria-label', `${pending} request${pending === 1 ? '' : 's'} in progress`);
  } else {
    badge.hidden = true;
  }
}

/* ── Render ──────────────────────────────────────────────────────────────── */

export function renderQueuePage() { render(true); }

function render(force) {
  const list = el('queue-page-list');
  if (!list) return;
  syncControls();

  if (loadError && !loadedOnce) {
    list.innerHTML = errorState(`Could not load the queue: ${loadError}`,
      {retryAttr: 'data-qact="reload" data-id="0"'});
    lastSignature = null;
    return;
  }
  if (!loadedOnce) {
    list.innerHTML = queueSkeleton();
    lastSignature = null;
    return;
  }
  if (loadError) {
    // We have data, but the latest refresh failed — say so without wiping it.
    lastSignature = null;
  }

  const entries = buildEntries();
  const sig = signature(entries);
  // An unchanged poll must not touch the DOM: replacing innerHTML every 4s is
  // what destroyed focus and collapsed expanded seasons.
  if (!force && sig === lastSignature) return;
  lastSignature = sig;

  const total = state.queueItems.length;
  const pending = state.queueItems.filter(i => bucketOf(i.status) === 'processing').length;
  el('queue-count-label').textContent =
    `${total} item${total === 1 ? '' : 's'}${pending ? ` · ${pending} in progress` : ''}`;

  const staleWarning = loadError
    ? `<div class="callout warn">${icon('alert')}<div class="callout-body">
        Live updates stopped: ${esc(loadError)}. What you see below may be out of date.
        <div class="callout-actions"><button class="btn sm" type="button"
          data-qact="reload" data-id="0">${icon('refresh')} Retry now</button></div>
      </div></div>` : '';

  if (!entries.length) {
    list.innerHTML = staleWarning + (total
      ? `<div class="queue-empty">${icon('filter')} No items match these filters.</div>`
      : `<div class="empty-state">${icon('list')}<p>Queue is empty</p>
         <p class="sub">Request a movie or an episode and it will appear here with live progress.</p></div>`);
    return;
  }

  if (state.prefs.qFilterStatus !== 'all') {
    list.innerHTML = staleWarning + entries.map(renderEntry).join('');
    return;
  }
  let html = staleWarning;
  for (const b of BUCKETS) {
    const inBucket = entries.filter(e => (e.kind === 'season' ? e.agg : bucketOf(e.item.status)) === b.key);
    if (!inBucket.length) continue;
    html += `<section class="queue-bucket">
      <h2 class="queue-bucket-title"><span class="bucket-dot" style="background:${b.color}"></span>
        ${icon(b.glyph)} ${b.label} (${inBucket.length})</h2>
      ${inBucket.map(renderEntry).join('')}
    </section>`;
  }
  list.innerHTML = html;
}

function buildEntries() {
  const movies = [];
  const seasonMap = {};
  for (const it of state.queueItems) {
    if (it.media_type === 'tv') {
      const key = it.tmdb_id + ':' + it.season;
      let g = seasonMap[key];
      if (!g) {
        g = seasonMap[key] = {
          kind: 'season', key, tmdbId: it.tmdb_id, season: it.season,
          title: showTitle(it.title), year: it.year, poster: it.poster_url || '', eps: [],
        };
      }
      g.eps.push(it);
      if (!g.poster && it.poster_url) g.poster = it.poster_url;
    } else {
      movies.push({kind: 'movie', item: it});
    }
  }
  const seasons = Object.values(seasonMap).map(g => {
    let failed = 0, active = 0, done = 0, cancelled = 0;
    for (const e of g.eps) {
      const b = bucketOf(e.status);
      if (b === 'failed') failed++;
      else if (b === 'processing') active++;
      else if (b === 'done') done++;
      else cancelled++;
    }
    g.agg = failed ? 'failed' : active ? 'processing' : done ? 'done' : 'cancelled';
    g.counts = {failed, active, done, cancelled, total: g.eps.length};
    return g;
  });

  let entries = [...seasons, ...movies];
  const p = state.prefs;
  if (p.qFilterType !== 'all') {
    entries = entries.filter(e => p.qFilterType === 'tv' ? e.kind === 'season' : e.kind === 'movie');
  }
  if (p.qFilterStatus !== 'all') {
    entries = entries.filter(e => (e.kind === 'season' ? e.agg : bucketOf(e.item.status)) === p.qFilterStatus);
  }
  if (qSearch) {
    entries = entries.filter(e => String(e.kind === 'season' ? e.title : e.item.title).toLowerCase().includes(qSearch));
  }

  const titleOf = e => String(e.kind === 'season' ? e.title : e.item.title).toLowerCase();
  const timeOf = e => e.kind === 'season'
    ? Math.max(...e.eps.map(x => new Date(x.created_at || 0).getTime()))
    : new Date(e.item.created_at || 0).getTime();
  const rank = e => ['processing', 'failed', 'done', 'cancelled']
    .indexOf(e.kind === 'season' ? e.agg : bucketOf(e.item.status));

  if (p.qSort === 'az') entries.sort((a, b) => titleOf(a).localeCompare(titleOf(b)));
  else if (p.qSort === 'oldest') entries.sort((a, b) => timeOf(a) - timeOf(b));
  else if (p.qSort === 'status') entries.sort((a, b) => rank(a) - rank(b) || timeOf(b) - timeOf(a));
  else entries.sort((a, b) => timeOf(b) - timeOf(a));
  return entries;
}

/** signature captures everything the markup depends on, so an unchanged poll
 *  is a no-op. Includes expansion + diagnosis state, which are UI-only. */
function signature(entries) {
  const rows = state.queueItems
    .map(i => `${i.id}|${i.status}|${i.stage || ''}|${i.progress || ''}|${i.error_msg || ''}`)
    .join(';');
  const diag = [...openDiagnosis.entries()]
    .map(([k, v]) => `${k}:${v.loading ? 'L' : v.error ? 'E' : 'D'}`).join(',');
  return [rows, [...openSeasons].sort().join(','), diag, qSearch,
    state.prefs.qFilterStatus, state.prefs.qFilterType, state.prefs.qSort,
    entries.length].join('#');
}

function syncControls() {
  document.querySelectorAll('#page-queue .qs-filter[data-grp="status"]').forEach(b => {
    const on = b.dataset.val === state.prefs.qFilterStatus;
    b.classList.toggle('active', on);
    b.setAttribute('aria-pressed', String(on));
  });
  document.querySelectorAll('#page-queue .qs-filter[data-grp="type"]').forEach(b => {
    const on = b.dataset.val === state.prefs.qFilterType;
    b.classList.toggle('active', on);
    b.setAttribute('aria-pressed', String(on));
  });
  const sortSel = el('queue-sort');
  if (sortSel && sortSel.value !== state.prefs.qSort) sortSel.value = state.prefs.qSort;
}

function renderEntry(e) {
  return e.kind === 'season' ? renderSeasonGroup(e) : renderMovieRow(e.item);
}

function renderMovieRow(item) {
  const id = num(item.id);
  const tmdbId = num(item.tmdb_id);
  const poster = item.poster_url
    ? `<button type="button" class="queue-poster-btn" data-queue-nav="1" data-type="movie" data-tmdb-id="${tmdbId}"
         aria-label="Open ${esc(item.title)}"><img class="queue-poster" src="${esc(item.poster_url)}" alt=""></button>`
    : `<span class="queue-poster-placeholder">${icon('film')}</span>`;
  return `<div class="queue-item">
    ${poster}
    <div class="queue-info">
      <button type="button" class="queue-title" data-queue-nav="1" data-type="movie" data-tmdb-id="${tmdbId}">${esc(item.title)}</button>
      <div class="queue-sub">${esc(item.year || '')} · Movie</div>
      ${statusHTML(item)}
      ${stepperHTML(item)}
      ${rowActions(item)}
      ${diagnosisHTML(item)}
    </div>
  </div>`;
}

function statusHTML(item) {
  switch (item.status) {
    case 'pending':    return `<div class="queue-status qs-pending">${icon('clock')} Pending</div>`;
    case 'processing': return `<div class="queue-progress">${esc(item.progress || 'Processing…')}</div>`;
    case 'done':       return `<div class="queue-status qs-done">${icon('check')} Done</div>`;
    case 'failed':     return `<div class="queue-status qs-failed">${icon('alert')} ${esc(item.error_msg || 'Failed')}</div>`;
    case 'cancelled':  return `<div class="queue-status qs-cancelled">${icon('minus')} Cancelled</div>`;
    default:           return '';
  }
}

/**
 * stepperHTML renders the contract §6 stage token as a stepper. When `stage`
 * is absent (older backend) the prose `progress` line above is the whole
 * story and no stepper is drawn — never a fake one.
 */
function stepperHTML(item) {
  const stage = typeof item.stage === 'string' ? item.stage : '';
  if (!stage) return '';
  if (stage === 'cancelled') return '';
  const failed = stage === 'failed';
  const at = STAGES.indexOf(stage);
  if (!failed && at === -1) return '';
  // On failure we do not know which stage broke, so mark the whole run failed.
  const nodes = STAGES.map((s, i) => {
    let cls = 'step-node';
    if (failed) cls += i === 0 ? ' fail' : '';
    else if (i < at) cls += ' done';
    else if (i === at) cls += ' now';
    const label = i === at || (failed && i === 0) ? `<span>${esc(STAGE_LABEL[s])}</span>` : '';
    return `<span class="${cls}" title="${esc(STAGE_LABEL[s])}"><span class="pip"></span>${label}</span>`;
  }).join('<span class="step-sep" aria-hidden="true"></span>');
  const sr = failed ? 'Failed' : (STAGE_LABEL[stage] || stage);
  return `<div class="stepper" role="img" aria-label="Stage: ${esc(sr)}">${nodes}</div>`;
}

function rowActions(item) {
  const id = num(item.id);
  const btns = [];
  if (item.status === 'pending') {
    btns.push(`<button class="queue-action-btn danger" type="button" data-qact="cancel" data-id="${id}">${icon('x')} Cancel</button>`);
  }
  if (item.status === 'failed') {
    btns.push(`<button class="queue-action-btn primary" type="button" data-qact="retry" data-id="${id}">${icon('refresh')} Retry…</button>`);
    const open = openDiagnosis.has(id);
    btns.push(`<button class="queue-action-btn" type="button" data-qact="diagnose" data-id="${id}"
      aria-expanded="${open}">${icon('help')} Why did this fail?</button>`);
  }
  if (['done', 'failed', 'cancelled'].includes(item.status)) {
    btns.push(`<button class="queue-action-btn danger" type="button" data-qact="delete" data-id="${id}">${icon('trash')} Remove</button>`);
  }
  return btns.length ? `<div class="queue-actions">${btns.join('')}</div>` : '';
}

/* ── Failure diagnosis (contract §7) ─────────────────────────────────────── */

function diagnosisHTML(item) {
  const id = num(item.id);
  const d = openDiagnosis.get(id);
  if (!d) return '';
  if (d.loading) {
    return `<div class="diag"><div class="skeleton sk-line" style="width:55%"></div>
      <div class="skeleton sk-line" style="width:80%"></div>
      <div class="skeleton sk-line" style="width:70%"></div></div>`;
  }
  if (d.error) {
    return `<div class="diag">${errorState(d.error, {compact: true})}</div>`;
  }
  const data = d.data || {};
  const f = data.filters || {};
  const filterBits = [];
  if (f.min_seeders !== undefined) filterBits.push(`min seeders ${num(f.min_seeders)}`);
  if (f.max_size_gb) filterBits.push(`max size ${num(f.max_size_gb)} GB`);
  if (f.reject_cam) filterBits.push('CAM/TS rejected');
  const cands = Array.isArray(data.candidates) ? data.candidates : [];

  if (!cands.length) {
    return `<div class="diag"><div class="callout warn">${icon('alert')}<div class="callout-body">
      <div class="callout-title">No candidate releases were returned at all</div>
      <p>The indexers answered but had nothing for this title. That usually means the indexer set is too
      small, or the title is not out yet. Check that Prowlarr has healthy indexers.</p>
      ${filterBits.length ? `<p style="margin-top:6px">Active filters: ${esc(filterBits.join(' · '))}</p>` : ''}
    </div></div></div>`;
  }

  const rows = cands.map(c => {
    const why = c.rejected_by ? String(c.rejected_by) : '';
    return `<tr>
      <td class="diag-title">${esc(c.title || '')}</td>
      <td>${num(c.seeders)}</td>
      <td>${c.size_gb ? esc(Number(c.size_gb).toFixed(1)) + ' GB' : '—'}</td>
      <td>${why
        ? `<span class="badge failed">${icon('x')} ${esc(why)}</span>`
        : `<span class="badge ok">${icon('check')} passed</span>`}</td>
    </tr>`;
  }).join('');

  return `<div class="diag">
    <p class="diag-lead">${cands.length} release${cands.length === 1 ? ' was' : 's were'} found and
      ${filterBits.length ? `filtered by: <strong>${esc(filterBits.join(' · '))}</strong>` : 'scored'}.
      Loosen these in <a href="/dashboard/#settings">Settings → Quality &amp; Releases</a>.</p>
    <div class="diag-scroll"><table class="diag-table">
      <thead><tr><th>Release</th><th>Seeders</th><th>Size</th><th>Result</th></tr></thead>
      <tbody>${rows}</tbody></table></div>
  </div>`;
}

async function toggleDiagnosis(id) {
  if (openDiagnosis.has(id)) { openDiagnosis.delete(id); render(true); return; }
  if (!requireLogin(() => toggleDiagnosis(id))) return;
  openDiagnosis.set(id, {loading: true});
  render(true);
  const r = await apiTry(`/api/queue/${encodeURIComponent(id)}/diagnosis`);
  if (!openDiagnosis.has(id)) return;
  if (r.ok) {
    openDiagnosis.set(id, {loading: false, data: r.data});
  } else if (r.absent) {
    openDiagnosis.set(id, {
      loading: false,
      error: 'This build does not report per-request diagnostics yet. The Health page and the ' +
             'orchestrator log still have the detail.',
    });
  } else {
    openDiagnosis.set(id, {loading: false, error: r.error});
  }
  render(true);
}

/* ── Season groups ───────────────────────────────────────────────────────── */

function renderSeasonGroup(g) {
  const open = openSeasons.has(g.key);
  const poster = g.poster
    ? `<img class="queue-poster" src="${esc(g.poster)}" alt="">`
    : `<span class="queue-poster-placeholder">${icon('tv')}</span>`;
  const c = g.counts;
  const bits = [];
  if (c.done)      bits.push(countBit('var(--green)', 'check', `${c.done} done`));
  if (c.active)    bits.push(countBit('var(--blue)', 'refresh', `${c.active} in progress`));
  if (c.failed)    bits.push(countBit('var(--red)', 'alert', `${c.failed} failed`));
  if (c.cancelled) bits.push(countBit('var(--muted)', 'minus', `${c.cancelled} cancelled`));

  const eps = g.eps.slice().sort((a, b) => num(a.episode) - num(b.episode)).map(ep => {
    const b = bucketOf(ep.status);
    const color = b === 'failed' ? 'var(--red)' : b === 'processing' ? 'var(--blue)'
      : b === 'done' ? 'var(--green)' : 'var(--muted)';
    const glyph = b === 'failed' ? 'alert' : b === 'processing' ? 'refresh'
      : b === 'done' ? 'check' : 'minus';
    const label = ep.status === 'processing' ? (ep.progress || 'Fetching…')
      : ep.status === 'pending' ? 'Pending'
      : ep.status === 'done' ? 'Done'
      : ep.status === 'failed' ? (ep.error_msg || 'Failed') : 'Cancelled';
    const id = num(ep.id);
    const act = [];
    if (ep.status === 'pending') act.push(`<button class="queue-action-btn danger" type="button" data-qact="cancel" data-id="${id}">Cancel</button>`);
    if (ep.status === 'failed') {
      act.push(`<button class="queue-action-btn primary" type="button" data-qact="retry" data-id="${id}">Retry…</button>`);
      act.push(`<button class="queue-action-btn" type="button" data-qact="diagnose" data-id="${id}" aria-expanded="${openDiagnosis.has(id)}">Why?</button>`);
    }
    if (['done', 'failed', 'cancelled'].includes(ep.status)) {
      act.push(`<button class="queue-action-btn danger" type="button" data-qact="delete" data-id="${id}" aria-label="Remove episode ${num(ep.episode)}">${icon('trash')}</button>`);
    }
    return `<div class="sg-ep">
      <span class="dot" style="background:${color}" aria-hidden="true"></span>
      ${icon(glyph, '')}
      <span class="sg-ep-num">E${String(num(ep.episode)).padStart(2, '0')}</span>
      <span class="sg-ep-name">${esc(label)}</span>
      ${act.join('')}
    </div>${diagnosisHTML(ep)}`;
  }).join('');

  return `<div class="season-group${open ? ' open' : ''}">
    <div class="season-group-head" role="button" tabindex="0" data-key="${esc(g.key)}"
         aria-expanded="${open}" aria-label="${esc(g.title)} season ${num(g.season)}">
      ${poster}
      <div class="sg-summary">
        <div class="sg-title">${esc(g.title)} — Season ${num(g.season)}</div>
        <div class="sg-counts">${bits.join('')}</div>
      </div>
      <span class="sg-chevron" aria-hidden="true">${icon('chevron-right')}</span>
    </div>
    <div class="sg-episodes">${eps}</div>
  </div>`;
}

function countBit(color, glyph, text) {
  return `<span class="sg-count"><span class="dot" style="background:${color}" aria-hidden="true"></span>${icon(glyph)} ${esc(text)}</span>`;
}

/* ── Actions ─────────────────────────────────────────────────────────────── */

async function cancelItem(id) {
  if (!requireLogin(() => cancelItem(id))) return;
  try { await apiFetch(`/api/queue/${encodeURIComponent(id)}/cancel`, {method: 'POST'}); }
  catch (e) { toast(e.message, {ok: false}); return; }
  await pollQueue();
  render(true);
}

async function deleteItem(id) {
  if (!requireLogin(() => deleteItem(id))) return;
  try { await apiFetch(`/api/queue/${encodeURIComponent(id)}`, {method: 'DELETE'}); }
  catch (e) { toast(e.message, {ok: false}); return; }
  openDiagnosis.delete(id);
  await pollQueue();
  render(true);
}

/** retryItem re-opens the title with the picker expanded AND the failed
 *  season/episode preselected. It used to drop the user on the show with no
 *  season chosen and no picker, so they had to re-find the episode by hand. */
function retryItem(id) {
  if (!requireLogin(() => retryItem(id))) return;
  const q = state.queueItems.find(x => num(x.id) === num(id));
  if (!q) { toast('That request is no longer in the queue', {ok: false}); return; }
  navItem(q.media_type, q.tmdb_id, {
    forcePicker: true,
    previousRelease: '',
    season: q.media_type === 'tv' ? num(q.season) : null,
    episode: q.media_type === 'tv' ? num(q.episode) : null,
  });
}

async function clearFinished() {
  if (!requireLogin(() => clearFinished())) return;
  const finished = state.queueItems.filter(i => ['done', 'failed', 'cancelled'].includes(i.status));
  if (!finished.length) { toast('Nothing to clear'); return; }
  if (!confirm(`Clear ${finished.length} finished item${finished.length === 1 ? '' : 's'} from the queue?`)) return;
  let failed = 0;
  for (const i of finished) {
    try { await apiFetch(`/api/queue/${encodeURIComponent(i.id)}`, {method: 'DELETE'}); }
    catch (_) { failed++; }
  }
  if (failed) toast(`Cleared ${finished.length - failed}, ${failed} could not be removed`, {ok: false});
  else toast(`Cleared ${finished.length} item${finished.length === 1 ? '' : 's'}`);
  await pollQueue();
  render(true);
  emit('queue-cleared');
}

export {updateBadge};
