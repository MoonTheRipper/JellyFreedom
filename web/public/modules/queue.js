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

   THE TREE. /api/queue is capped at 100 newest rows. With 844 TV episodes in
   flight that cap, not the layout, decided what this page could show: every
   season was its own top-level row, most of them never arrived at all, and
   "28 shows" read as a wall of disconnected seasons. The page now renders the
   AGGREGATE feed (/api/queue/groups, ~100 groups for any number of rows) as
   show -> season -> episode, and fetches one season's leaf rows only when the
   user opens it. When the orchestrator has no grouped feed, state.queueGroups
   is null and every flat-mode path below still works exactly as before.
   ========================================================================== */

import {apiFetch, apiTry, esc, num, toast, errorState} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {state, savePrefs, loadQueue, loadQueueGroups, loadQueueFor, showTitle, emit,
        RING_COLOR, RING_ICON} from './state.js';
import {requireLogin} from './auth.js';
import {navItem} from './router.js';

/* Contract §6: closed, ordered set. */
const STAGES = ['queued', 'indexing', 'picking', 'adding', 'verifying', 'writing', 'done'];
const STAGE_LABEL = {
  queued: 'Queued', indexing: 'Searching indexers', picking: 'Picking release',
  adding: 'Adding torrent', verifying: 'Verifying', writing: 'Writing .strm', done: 'Done',
};

/* Failed leads. It used to sit below "In Progress", which is how 1,245 failed
   rows went unnoticed on a live deployment: a queue that is always busy always
   had something blue above the red. Same reasoning as ROLLUP below — a machine
   that is working needs no attention, a failure does. */
const BUCKETS = [
  {key: 'failed',     label: 'Failed',      color: 'var(--red)',   glyph: 'alert'},
  {key: 'processing', label: 'In Progress', color: 'var(--blue)',  glyph: 'refresh'},
  {key: 'done',       label: 'Completed',   color: 'var(--green)', glyph: 'check'},
  {key: 'cancelled',  label: 'Cancelled',   color: 'var(--muted)', glyph: 'minus'},
];

/**
 * ROLLUP maps a rolled-up node (a show, or a season) onto one status.
 *
 * The PRECEDENCE is the design, not an implementation detail:
 *
 *     failed  -> needs a human
 *     working -> the machine is on it
 *     done    -> finished
 *     empty   -> nothing left running (in practice: all cancelled)
 *
 * `failed` MUST outrank `working`. A season holding 33 failed and 86 done
 * episodes needs attention even while other episodes are still fetching, and
 * burying that under a blue "in progress" chip is exactly how a four-figure
 * pile of failures stayed invisible on this deployment.
 *
 * `ring` names an entry in RING_COLOR / RING_ICON so every state carries a
 * GLYPH as well as a colour — colour alone is not a status.
 */
const ROLLUP = {
  failed:  {bucket: 'failed',     ring: 'failed',     label: 'Needs attention'},
  working: {bucket: 'processing', ring: 'processing', label: 'In progress'},
  done:    {bucket: 'done',       ring: 'ready',      label: 'Complete'},
  empty:   {bucket: 'cancelled',  ring: 'none',       label: 'Nothing running'},
};

/** rollup takes either counts shape — the API's {failed, active, done, …} or
 *  the flat builder's — because both carry those three fields. */
function rollup(c) {
  if (!c) return 'empty';
  if (num(c.failed) > 0) return 'failed';
  if (num(c.active) > 0) return 'working';
  if (num(c.done) > 0) return 'done';
  return 'empty';
}

/* Expansion + diagnosis state lives HERE, not in the DOM. */
const openShows = new Set();        // "tv:1396" — level 1
const openSeasons = new Set();      // "1396:3"  — level 2
const openDiagnosis = new Map();    // queue id -> {loading, error, data}
/* Level-3 leaf rows, fetched per open season: key -> {loading, error, items, token} */
const seasonRows = new Map();
let qSearch = '';
let lastSignature = null;
let loadedOnce = false;      // before the first successful load, show skeletons
let loadError = null;        // last poll failure, surfaced instead of "empty"

function el(id) { return document.getElementById(id); }
function bucketOf(s) { return (s === 'pending' || s === 'processing') ? 'processing' : s; }
function treeMode() { return !!state.queueGroups; }
function showKey(mediaType, tmdbId) { return `${mediaType}:${num(tmdbId)}`; }
function seasonKey(tmdbId, season) { return `${num(tmdbId)}:${num(season)}`; }
function msOf(iso) {
  const t = new Date(iso || 0).getTime();
  return Number.isFinite(t) ? t : 0;
}

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
    if (e.key !== 'Enter' && e.key !== ' ' && e.key !== 'Spacebar') return;
    // Both tree heads are role="button" tabindex="0", so both must answer the
    // same two keys — this is the whole keyboard/TV-remote story for the page.
    const head = e.target.closest('.show-group-head, .season-group-head');
    if (!head) return;
    e.preventDefault();
    if (head.classList.contains('show-group-head')) toggleShow(head.dataset.showKey);
    else toggleSeason(head.dataset.key);
  });
}

function onListClick(e) {
  const act = e.target.closest('[data-qact]');
  if (act) {
    switch (act.dataset.qact) {
      case 'cancel':   cancelItem(num(act.dataset.id)); return;
      case 'delete':   deleteItem(num(act.dataset.id)); return;
      case 'retry':    retryItem(num(act.dataset.id)); return;
      case 'diagnose': toggleDiagnosis(num(act.dataset.id)); return;
      case 'reload':   pollQueue(); return;
      case 'jump':     jumpToActive(num(act.dataset.tmdbId), act.dataset.mediaType); return;
      case 'rseason':  ensureSeasonRows(seasonKey(act.dataset.tmdbId, act.dataset.season),
                                        {force: true}); return;
    }
    return;
  }

  const showHead = e.target.closest('.show-group-head');
  if (showHead) { toggleShow(showHead.dataset.showKey); return; }

  const head = e.target.closest('.season-group-head');
  if (head) { toggleSeason(head.dataset.key); return; }

  const nav = e.target.closest('[data-queue-nav]');
  if (nav) navItem(nav.dataset.type, num(nav.dataset.tmdbId));
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

function toggleShow(key) {
  if (!key) return;
  if (openShows.has(key)) openShows.delete(key); else openShows.add(key);
  render(true);
}

function toggleSeason(key) {
  if (!key) return;
  if (openSeasons.has(key)) {
    openSeasons.delete(key);
  } else {
    openSeasons.add(key);
    // Level 3 is lazy: the episodes of THIS season are fetched on expand, not
    // shipped with the tree. In flat mode the rows are already in hand.
    ensureSeasonRows(key);
  }
  render(true);
}

/* ── Level 3: one season's leaf rows ─────────────────────────────────────── */

/**
 * ensureSeasonRows fetches (or refreshes) the episodes of one season.
 *
 * `quiet` is the poll's mode: keep the rows already on screen while the new
 * ones are in flight, so a 4-second refresh never flashes a skeleton over an
 * expanded season. `force` re-fetches even when we already have rows.
 *
 * Every entry carries a monotonic `token`. Opening, collapsing and re-opening
 * a season faster than the network answers is ordinary TV-remote behaviour,
 * and without the token an early response would overwrite a later one.
 */
async function ensureSeasonRows(key, {force = false, quiet = false} = {}) {
  if (!key || !treeMode()) return;
  const cur = seasonRows.get(key);
  if (cur && cur.loading) return;
  if (cur && cur.items && !force) return;

  const parts = String(key).split(':');
  const tmdbId = num(parts[0]);
  const season = num(parts[1]);
  const token = (cur ? num(cur.token) : 0) + 1;
  const keep = cur && cur.items ? cur.items : null;
  seasonRows.set(key, {loading: !(quiet && keep), token, items: keep, error: null});
  if (!quiet) render(true);

  let items = null;
  let error = null;
  try {
    items = await loadQueueFor(tmdbId, season, {mediaType: 'tv'});
  } catch (e) {
    error = e.message;
  }
  const now = seasonRows.get(key);
  if (!now || now.token !== token) return;   // superseded by a newer expand
  seasonRows.set(key, {
    loading: false, token,
    items: items || now.items, error: items ? null : error,
  });
  if (!quiet) render(true);
}

/** refreshOpenSeasons keeps the expanded branches live. It is bounded twice
 *  over: by what the user actually opened — usually one season, never the
 *  whole queue — and by the queue page being the one on screen, since
 *  pollQueue is also called by the library and the modal. */
async function refreshOpenSeasons() {
  if (!treeMode() || !openSeasons.size) return;
  const page = el('page-queue');
  if (!page || !page.classList.contains('active')) return;
  await Promise.all([...openSeasons].map(k => ensureSeasonRows(k, {force: true, quiet: true})));
}

/* ── Poll ────────────────────────────────────────────────────────────────── */

export async function pollQueue() {
  try {
    // The flat feed still has to load: episodeStatus(), seasonRingStatus() and
    // the library page all read state.queueItems. loadQueueGroups never throws
    // — an orchestrator without the grouped route just leaves it null.
    await Promise.all([loadQueue(), loadQueueGroups()]);
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
  await refreshOpenSeasons();
  updateBadge();
  if (el('page-queue').classList.contains('active')) render(false);
}

function updateBadge() {
  // The aggregate counts the WHOLE table. state.queueItems is the newest 100
  // rows, so counting it here reported "100 in progress" against 26,187.
  const agg = state.queueGroups;
  const pending = agg ? num(agg.active)
    : state.queueItems.filter(i => i.status === 'pending' || i.status === 'processing').length;
  const badge = el('queue-badge');
  if (pending > 0) {
    badge.textContent = pending > 999 ? '999+' : String(pending);
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

  const agg = state.queueGroups;
  const total = agg ? num(agg.total) : state.queueItems.length;
  const active = agg ? num(agg.active)
    : state.queueItems.filter(i => bucketOf(i.status) === 'processing').length;
  const failed = agg ? num(agg.counts.failed)
    : state.queueItems.filter(i => i.status === 'failed').length;
  el('queue-count-label').textContent =
    `${total.toLocaleString()} item${total === 1 ? '' : 's'}` +
    (active ? ` · ${active.toLocaleString()} in progress` : '') +
    (failed ? ` · ${failed.toLocaleString()} failed` : '');

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
    const inBucket = entries.filter(e => e.agg === b.key);
    if (!inBucket.length) continue;
    html += `<section class="queue-bucket">
      <h2 class="queue-bucket-title"><span class="bucket-dot" style="background:${b.color}"></span>
        ${icon(b.glyph)} ${b.label} (${inBucket.length})</h2>
      ${inBucket.map(renderEntry).join('')}
    </section>`;
  }
  list.innerHTML = html;
}

/* ── Entries ─────────────────────────────────────────────────────────────── */

/** buildEntries produces LEVEL 1 — one node per title in tree mode, or the
 *  legacy per-season / per-movie list when there is no grouped feed. Every
 *  entry carries {name, mediaType, time, agg} so filtering and sorting have a
 *  single shape to work against. */
function buildEntries() {
  return filterSort(treeMode() ? buildTreeEntries() : buildFlatEntries());
}

function buildTreeEntries() {
  const g = state.queueGroups;
  const entries = [];
  for (const s of g.shows) {
    const roll = rollup(s.counts);
    const title = showTitle(s.title) || `TMDB ${s.tmdb_id}`;
    entries.push({
      kind: 'show', key: showKey('tv', s.tmdb_id), tmdbId: s.tmdb_id, mediaType: 'tv',
      title, name: title, poster: s.poster_url, counts: s.counts, seasons: s.seasons,
      roll, agg: ROLLUP[roll].bucket, time: msOf(s.newest),
    });
  }
  for (const m of g.movies) {
    const roll = rollup(m.counts);
    const title = m.title || `TMDB ${m.tmdb_id}`;
    entries.push({
      kind: 'movie-group', key: showKey('movie', m.tmdb_id), tmdbId: m.tmdb_id,
      mediaType: 'movie', title, name: title, poster: m.poster_url, counts: m.counts,
      seasons: [], roll, agg: ROLLUP[roll].bucket, time: msOf(m.newest),
      // A movie has no seasons, so it stays a flat level-1 row. When its leaf
      // row happens to be inside the newest-100 flat feed we render the real
      // row — stepper, actions, diagnosis and all; otherwise the aggregate is
      // all we honestly have, and the row says so instead of inventing a state.
      item: newestFlatRow('movie', m.tmdb_id),
    });
  }
  return entries;
}

/** newestFlatRow finds this title's most recent row inside the capped flat feed. */
function newestFlatRow(mediaType, tmdbId) {
  let best = null;
  for (const it of state.queueItems) {
    if (it.media_type !== mediaType || num(it.tmdb_id) !== num(tmdbId)) continue;
    if (!best || msOf(it.created_at) > msOf(best.created_at)) best = it;
  }
  return best;
}

function buildFlatEntries() {
  const movies = [];
  const seasonMap = {};
  for (const it of state.queueItems) {
    if (it.media_type === 'tv') {
      const key = seasonKey(it.tmdb_id, it.season);
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
      movies.push({
        kind: 'movie', item: it, mediaType: 'movie', name: String(it.title || ''),
        time: msOf(it.created_at), agg: bucketOf(it.status),
      });
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
    g.counts = {failed, active, done, cancelled, total: g.eps.length};
    g.roll = rollup(g.counts);
    g.agg = ROLLUP[g.roll].bucket;
    g.mediaType = 'tv';
    g.name = String(g.title || '');
    g.time = Math.max(0, ...g.eps.map(x => msOf(x.created_at)));
    return g;
  });
  return [...seasons, ...movies];
}

function filterSort(entries) {
  const p = state.prefs;
  if (p.qFilterType !== 'all') entries = entries.filter(e => e.mediaType === p.qFilterType);
  if (p.qFilterStatus !== 'all') entries = entries.filter(e => e.agg === p.qFilterStatus);
  if (qSearch) entries = entries.filter(e => e.name.toLowerCase().includes(qSearch));

  const titleOf = e => e.name.toLowerCase();
  // Rank follows BUCKETS, so "sort by status" and the section order agree —
  // and both put what needs a human above what is merely busy.
  const rank = e => BUCKETS.findIndex(b => b.key === e.agg);

  if (p.qSort === 'az') entries.sort((a, b) => titleOf(a).localeCompare(titleOf(b)));
  else if (p.qSort === 'oldest') entries.sort((a, b) => a.time - b.time);
  else if (p.qSort === 'status') entries.sort((a, b) => rank(a) - rank(b) || b.time - a.time);
  else entries.sort((a, b) => b.time - a.time);
  return entries;
}

/** signature captures everything the markup depends on, so an unchanged poll
 *  is a no-op. Includes expansion + diagnosis state, which are UI-only.
 *  openShows and openSeasons are BOTH in here: that is the mechanism that
 *  stops a 4-second poll collapsing whatever the user just opened. */
function signature(entries) {
  const cs = c => `${num(c.failed)}/${num(c.active)}/${num(c.done)}/${num(c.cancelled)}/${num(c.total)}`;
  const rowSig = i => `${i.id}|${i.status}|${i.stage || ''}|${i.progress || ''}|${i.error_msg || ''}`;

  let core;
  if (treeMode()) {
    const agg = state.queueGroups;
    core = `${agg.total}|${agg.active}|` + entries.map(e =>
      e.key + ':' + cs(e.counts) +
      e.seasons.map(s => `/${s.season}:${cs(s.counts)}`).join('') +
      (e.item ? '!' + rowSig(e.item) : '')).join(';');
    // Only the OPEN seasons' leaf rows are on screen, so only they can change it.
    core += '#' + [...openSeasons].sort().map(k => {
      const v = seasonRows.get(k);
      if (!v) return `${k}:_`;
      return `${k}:${v.loading ? 'L' : v.error ? 'E' : 'D'}:` +
        (v.items || []).map(rowSig).join(',');
    }).join(';');
  } else {
    core = state.queueItems.map(rowSig).join(';');
  }
  const diag = [...openDiagnosis.entries()]
    .map(([k, v]) => `${k}:${v.loading ? 'L' : v.error ? 'E' : 'D'}`).join(',');
  return [core, [...openShows].sort().join(','), [...openSeasons].sort().join(','), diag, qSearch,
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
  switch (e.kind) {
    case 'show':        return renderShowGroup(e);
    case 'movie-group': return renderMovieGroup(e);
    case 'season':      return renderSeasonGroup(e);
    default:            return renderMovieRow(e.item);
  }
}

/* ── Shared status furniture ─────────────────────────────────────────────── */

/** rollupChip is the one place a rolled-up node states its status. Colour AND
 *  glyph both come from the RING_* maps, so the state survives a monochrome
 *  panel, a colour-blind viewer and a screen reader. */
function rollupChip(roll) {
  const r = ROLLUP[roll] || ROLLUP.empty;
  return `<span class="qr-chip qr-${esc(roll)}" style="--qr:${esc(RING_COLOR[r.ring])}">` +
    `${icon(RING_ICON[r.ring])} ${esc(r.label)}</span>`;
}

function countBit(color, glyph, text) {
  return `<span class="sg-count"><span class="dot" style="background:${color}" aria-hidden="true"></span>${icon(glyph)} ${esc(text)}</span>`;
}

/** countBits renders the per-status tally. Accepts either counts shape. */
function countBits(c) {
  const bits = [];
  if (num(c.done))      bits.push(countBit('var(--green)', 'check', `${num(c.done)} done`));
  if (num(c.active))    bits.push(countBit('var(--blue)', 'refresh', `${num(c.active)} in progress`));
  if (num(c.failed))    bits.push(countBit('var(--red)', 'alert', `${num(c.failed)} failed`));
  if (num(c.cancelled)) bits.push(countBit('var(--muted)', 'minus', `${num(c.cancelled)} cancelled`));
  return bits.join('');
}

/** jumpButton is offered only where there is something live to jump TO. */
function jumpButton(e) {
  if (!num(e.counts.active) && !num(e.counts.failed)) return '';
  return `<button class="queue-action-btn show-jump" type="button" data-qact="jump"
    data-tmdb-id="${num(e.tmdbId)}" data-media-type="${esc(e.mediaType)}"
    aria-label="Jump to the episode being worked on in ${esc(e.title)}"
    >${icon('activity')}<span>Jump to active</span></button>`;
}

/* ── Level 1: a show ─────────────────────────────────────────────────────── */

function renderShowGroup(e) {
  const open = openShows.has(e.key);
  const poster = e.poster
    ? `<img class="queue-poster" src="${esc(e.poster)}" alt="">`
    : `<span class="queue-poster-placeholder">${icon('tv')}</span>`;

  const p = state.prefs;
  const seasons = p.qFilterStatus === 'all'
    ? e.seasons
    : e.seasons.filter(s => ROLLUP[rollup(s.counts)].bucket === p.qFilterStatus);
  const hidden = e.seasons.length - seasons.length;

  let body = '';
  if (open) {
    body = seasons.map(s => renderSeasonGroup(seasonNode(e, s))).join('');
    if (hidden > 0) {
      body += `<div class="show-hidden">${icon('filter')} ${hidden} season${hidden === 1 ? '' : 's'} hidden by the current filter</div>`;
    }
    if (!seasons.length && !hidden) {
      body += `<div class="show-hidden">${icon('info')} No seasons reported for this title.</div>`;
    }
  }

  const n = e.seasons.length;
  const label = `${e.title}, ${n} season${n === 1 ? '' : 's'}, ${ROLLUP[e.roll].label}`;
  return `<div class="show-group${open ? ' open' : ''}">
    <div class="show-row">
      <div class="show-group-head" role="button" tabindex="0" data-show-key="${esc(e.key)}"
           aria-expanded="${open}" aria-label="${esc(label)}">
        ${poster}
        <div class="sg-summary">
          <div class="sg-title">${esc(e.title)}</div>
          <div class="sg-counts">${rollupChip(e.roll)}${countBits(e.counts)}
            <span class="sg-count">${icon('list')} ${n} season${n === 1 ? '' : 's'}</span></div>
        </div>
        <span class="sg-chevron" aria-hidden="true">${icon('chevron-right')}</span>
      </div>
      ${jumpButton(e)}
    </div>
    ${open ? `<div class="show-seasons">${body}</div>` : ''}
  </div>`;
}

/** seasonNode adapts one aggregate season onto the shape renderSeasonGroup
 *  already draws, so level 2 is the SAME component in both modes. `nested`
 *  drops the poster and the repeated show name; `body` supplies level 3,
 *  which in tree mode is fetched rather than already in hand. */
function seasonNode(show, s) {
  const key = seasonKey(show.tmdbId, s.season);
  return {
    kind: 'season', key, tmdbId: show.tmdbId, season: s.season, title: show.title,
    poster: '', nested: true, counts: s.counts, eps: [], body: seasonBody(key, show, s),
  };
}

function seasonBody(key, show, s) {
  if (!openSeasons.has(key)) return '';   // collapsed: nothing to paint
  const v = seasonRows.get(key);
  if (!v || (v.loading && !v.items)) {
    return `<div class="sg-wait">${icon('refresh', 'spin')} Loading ${num(s.counts.total)} episode${num(s.counts.total) === 1 ? '' : 's'}…</div>`;
  }
  if (v.error && !v.items) {
    // retryAttr is raw markup, so every value in it is num()-bounded.
    return `<div class="sg-wait-err">${errorState(v.error, {compact: true,
      retryAttr: `data-qact="rseason" data-tmdb-id="${num(show.tmdbId)}" data-season="${num(s.season)}"`})}</div>`;
  }
  const rows = seasonEpisodesHTML(v.items || []);
  return rows || `<div class="sg-wait">${icon('info')} No episodes came back for this season.</div>`;
}

/* ── Level 1 (movies): flat, because a one-child tree is noise ────────────── */

function renderMovieGroup(e) {
  // The common case: the movie's row is inside the flat feed, so it gets the
  // full treatment it has always had.
  if (e.item && num(e.counts.total) <= 1) return renderMovieRow(e.item);
  if (e.item) return renderMovieRow(e.item, e);

  const poster = e.poster
    ? `<button type="button" class="queue-poster-btn" data-queue-nav="1" data-type="movie" data-tmdb-id="${num(e.tmdbId)}"
         aria-label="Open ${esc(e.title)}"><img class="queue-poster" src="${esc(e.poster)}" alt=""></button>`
    : `<span class="queue-poster-placeholder">${icon('film')}</span>`;
  const n = num(e.counts.total);
  const jump = jumpButton(e);
  return `<div class="queue-item">
    ${poster}
    <div class="queue-info">
      <button type="button" class="queue-title" data-queue-nav="1" data-type="movie" data-tmdb-id="${num(e.tmdbId)}">${esc(e.title)}</button>
      <div class="queue-sub">Movie · ${n} request${n === 1 ? '' : 's'}</div>
      <div class="sg-counts">${rollupChip(e.roll)}${countBits(e.counts)}</div>
      ${jump ? `<div class="queue-actions">${jump}</div>` : ''}
    </div>
  </div>`;
}

function renderMovieRow(item, group) {
  const tmdbId = num(item.tmdb_id);
  const poster = item.poster_url
    ? `<button type="button" class="queue-poster-btn" data-queue-nav="1" data-type="movie" data-tmdb-id="${tmdbId}"
         aria-label="Open ${esc(item.title)}"><img class="queue-poster" src="${esc(item.poster_url)}" alt=""></button>`
    : `<span class="queue-poster-placeholder">${icon('film')}</span>`;
  // A movie requested more than once has one visible row and a hidden tail —
  // say how many rather than letting the headline total disagree with the page.
  const extra = group && num(group.counts.total) > 1
    ? `<div class="sg-counts"><span class="sg-count">${icon('list')} ${num(group.counts.total)} requests</span>` +
      `${rollupChip(group.roll)}${countBits(group.counts)}</div>` : '';
  return `<div class="queue-item">
    ${poster}
    <div class="queue-info">
      <button type="button" class="queue-title" data-queue-nav="1" data-type="movie" data-tmdb-id="${tmdbId}">${esc(item.title)}</button>
      <div class="queue-sub">${esc(item.year || '')} · Movie</div>
      ${extra}
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

/* requireLogin RUNS its callback when the user is already signed in, so the
   callback must be the WORK — never the guarded function itself. Every
   `if (!requireLogin(() => sameFunction(...))) return;` on this page recursed
   until the stack blew, which for the async ones (Cancel, Remove, "Why did
   this fail?", Clear finished) meant an unhandled rejection and a button that
   silently did nothing. Split guard from work, once, everywhere. */
function toggleDiagnosis(id) {
  if (openDiagnosis.has(id)) { openDiagnosis.delete(id); render(true); return; }
  requireLogin(() => loadDiagnosis(id));
}

async function loadDiagnosis(id) {
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

/* ── Level 2: season groups ──────────────────────────────────────────────── */

function renderSeasonGroup(g) {
  const open = openSeasons.has(g.key);
  // Nested under a show, the poster and the show name are already overhead —
  // repeating them once per season is what made the old list unreadable.
  const poster = g.nested ? ''
    : g.poster
      ? `<img class="queue-poster" src="${esc(g.poster)}" alt="">`
      : `<span class="queue-poster-placeholder">${icon('tv')}</span>`;
  const roll = g.roll || rollup(g.counts);
  const title = g.nested
    ? `Season ${num(g.season)}`
    : `${esc(g.title)} — Season ${num(g.season)}`;
  const total = num(g.counts.total);

  // Level 3. `body` is supplied in tree mode (lazily fetched); in flat mode the
  // episodes are already in hand on g.eps.
  const eps = g.body !== undefined ? g.body : seasonEpisodesHTML(g.eps);

  return `<div class="season-group${open ? ' open' : ''}">
    <div class="season-group-head" role="button" tabindex="0" data-key="${esc(g.key)}"
         aria-expanded="${open}"
         aria-label="${esc(g.title)} season ${num(g.season)}, ${total} episode${total === 1 ? '' : 's'}, ${esc(ROLLUP[roll].label)}">
      ${poster}
      <div class="sg-summary">
        <div class="sg-title">${title}</div>
        <div class="sg-counts">${rollupChip(roll)}${countBits(g.counts)}</div>
      </div>
      <span class="sg-chevron" aria-hidden="true">${icon('chevron-right')}</span>
    </div>
    <div class="sg-episodes">${eps}</div>
  </div>`;
}

/* ── Level 3: episode rows ───────────────────────────────────────────────── */

function seasonEpisodesHTML(eps) {
  return (eps || []).slice().sort((a, b) => num(a.episode) - num(b.episode)).map(ep => {
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
    return `<div class="sg-ep${b === 'processing' ? ' now' : ''}">
      <span class="dot" style="background:${color}" aria-hidden="true"></span>
      ${icon(glyph, '')}
      <span class="sg-ep-num">E${String(num(ep.episode)).padStart(2, '0')}</span>
      <span class="sg-ep-name">${esc(label)}</span>
      ${act.join('')}
    </div>${diagnosisHTML(ep)}`;
  }).join('');
}

/* ── Actions ─────────────────────────────────────────────────────────────── */

function cancelItem(id) { requireLogin(() => doCancel(id)); }

async function doCancel(id) {
  try { await apiFetch(`/api/queue/${encodeURIComponent(id)}/cancel`, {method: 'POST'}); }
  catch (e) { toast(e.message, {ok: false}); return; }
  await pollQueue();
  render(true);
}

function deleteItem(id) { requireLogin(() => doDelete(id)); }

async function doDelete(id) {
  try { await apiFetch(`/api/queue/${encodeURIComponent(id)}`, {method: 'DELETE'}); }
  catch (e) { toast(e.message, {ok: false}); return; }
  openDiagnosis.delete(id);
  await pollQueue();
  render(true);
}

/** queueRowById looks in the flat feed AND in the lazily-fetched season rows.
 *  Level 3 is exactly the set of rows the flat feed's 100-row cap leaves out,
 *  so searching only state.queueItems would have made Retry on a deep episode
 *  answer "that request is no longer in the queue" while it sat on screen. */
function queueRowById(id) {
  const n = num(id);
  const hit = state.queueItems.find(x => num(x.id) === n);
  if (hit) return hit;
  for (const v of seasonRows.values()) {
    if (!v.items) continue;
    const r = v.items.find(x => num(x.id) === n);
    if (r) return r;
  }
  return null;
}

/** openQueueRow re-opens the title with the picker expanded AND the
 *  season/episode preselected. Both Retry and "Jump to active" go through
 *  here: it used to drop the user on the show with no season chosen and no
 *  picker, so they had to re-find the episode by hand. */
function openQueueRow(q) {
  navItem(q.media_type, q.tmdb_id, {
    forcePicker: true,
    previousRelease: '',
    season: q.media_type === 'tv' ? num(q.season) : null,
    episode: q.media_type === 'tv' ? num(q.episode) : null,
  });
}

function retryItem(id) { requireLogin(() => doRetry(id)); }

function doRetry(id) {
  const q = queueRowById(id);
  if (!q) { toast('That request is no longer in the queue', {ok: false}); return; }
  openQueueRow(q);
}

/* JUMP_ORDER: what the machine is on now, then what it will pick up next, then
   what needs a human. Distinct from ROLLUP's precedence on purpose — the chip
   answers "does this need me?", the jump answers "where is it right now?". */
const JUMP_ORDER = ['processing', 'pending', 'failed'];

/**
 * jumpToActive answers the actual request behind this whole page: "click a show
 * that is in progress, then the season, then find the episode marked in
 * progress". Rows come back ordered by season then episode, so the first match
 * is the earliest one — and this deliberately re-fetches rather than reading
 * the flat feed, because the episode being worked on is usually one of the
 * thousands of rows outside its 100-row window.
 */
function jumpToActive(tmdbId, mediaType) { requireLogin(() => doJump(tmdbId, mediaType)); }

async function doJump(tmdbId, mediaType) {
  const type = mediaType === 'movie' ? 'movie' : 'tv';
  let rows;
  try {
    rows = await loadQueueFor(tmdbId, null, {mediaType: type});
  } catch (e) {
    toast(e.message, {ok: false});
    return;
  }
  let pick = null;
  for (const st of JUMP_ORDER) {
    pick = rows.find(r => r.status === st);
    if (pick) break;
  }
  if (!pick) {
    toast('Nothing is in progress for this title any more');
    navItem(type, num(tmdbId));
    return;
  }
  if (type === 'tv') {
    // Leave the tree standing open on what we jumped to, so coming back lands
    // the user where they were rather than at a collapsed root.
    openShows.add(showKey('tv', tmdbId));
    const k = seasonKey(tmdbId, pick.season);
    openSeasons.add(k);
    ensureSeasonRows(k);
  }
  openQueueRow(pick);
}

function clearFinished() { requireLogin(() => doClearFinished()); }

async function doClearFinished() {
  const finished = state.queueItems.filter(i => ['done', 'failed', 'cancelled'].includes(i.status));
  if (!finished.length) { toast('Nothing to clear'); return; }
  // There is no bulk-delete endpoint, so this can only clear the rows the flat
  // feed handed us — at most 100. Say so up front rather than reporting
  // "Cleared 100" against a headline of 1,245 and looking broken.
  const agg = state.queueGroups;
  const all = agg ? num(agg.counts.done) + num(agg.counts.failed) + num(agg.counts.cancelled)
    : finished.length;
  const more = all > finished.length
    ? `\n\nThis pass removes ${finished.length} of ${all}. Run it again to continue.` : '';
  if (!confirm(`Clear ${finished.length} finished item${finished.length === 1 ? '' : 's'} from the queue?${more}`)) return;
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
