/* ==========================================================================
   Shared client state + a tiny event bus.

   The bus exists so page modules can react to each other without importing
   each other (library.js removing a series must refresh an open modal, but
   modal.js already imports library.js — an import cycle). Everything talks
   through events instead.
   ========================================================================== */

import {apiList, apiObj, apiTry} from '../shared/api.js';

const listeners = new Map();
export function on(evt, fn) {
  if (!listeners.has(evt)) listeners.set(evt, new Set());
  listeners.get(evt).add(fn);
  return () => listeners.get(evt).delete(fn);
}
export function emit(evt, payload) {
  const set = listeners.get(evt);
  if (!set) return;
  for (const fn of [...set]) {
    try { fn(payload); } catch (e) { console.error('[jf] listener failed for', evt, e); }
  }
}

/* ── Data ─────────────────────────────────────────────────────────────────
   All snake_case (API contract §1). The PascalCase -> snake_case conversion
   happens once, at the fetch boundary in shared/api.js.                     */
export const state = {
  user: null,            // {id, username, is_admin} | null
  libraries: [],         // [{name, type, default, adult}]
  myLibrary: [],         // store.Item[]
  libraryStatus: {},     // tmdb_id -> {status, library, title}
  queueItems: [],        // store.QueueItem[] — the FLAT feed, capped at 100 rows
  /* queueGroups is the whole queue aggregated server-side: one entry per title
     and one per (title, season) with per-status counts. null means "this build
     has no grouped feed", which is a real state — see loadQueueGroups. */
  queueGroups: null,     // {total, active, counts, shows[], movies[]} | null
  subscriptions: [],     // store.Subscription[]
  configured: null,      // shared/status.js shape
  health: {known: false, ok: true, degraded: []},
  prefs: {libFilter: 'all', qFilterStatus: 'all', qFilterType: 'all', qSort: 'newest', calView: 'month'},
};

/* ── Preferences ─────────────────────────────────────────────────────────── */
const PREF_KEY = 'jf_prefs';
export function restorePrefs() {
  try {
    const p = JSON.parse(localStorage.getItem(PREF_KEY) || '{}');
    for (const k of Object.keys(state.prefs)) if (p[k]) state.prefs[k] = p[k];
  } catch (_) { /* private mode / blocked storage: defaults are fine */ }
}
export function savePrefs() {
  try { localStorage.setItem(PREF_KEY, JSON.stringify(state.prefs)); } catch (_) {}
}

/* ── Loaders ─────────────────────────────────────────────────────────────── */

export async function loadLibraries() {
  try { state.libraries = await apiList('/api/libraries'); }
  catch (_) { state.libraries = []; }
  return state.libraries;
}

/** loadMyLibrary refreshes the library and the tmdb_id -> status index. */
export async function loadMyLibrary() {
  const items = await apiList('/api/library');
  state.myLibrary = items;
  const idx = {};
  for (const it of items) {
    const cur = idx[it.tmdb_id];
    if (!cur || cur.status !== 'ready') {
      idx[it.tmdb_id] = {status: it.status, library: it.library_name, title: it.title};
    }
  }
  state.libraryStatus = idx;
  emit('library');
  return items;
}

export async function loadQueue() {
  state.queueItems = await apiList('/api/queue');
  emit('queue');
  return state.queueItems;
}

/* ── The grouped queue ────────────────────────────────────────────────────
   /api/queue is ORDER BY created_at DESC LIMIT 100. That cap is not a display
   detail: with 844 TV episodes in flight, no amount of client-side grouping
   can show the user their shows, because 96% of the rows never arrive. The
   grouped feed answers with ~100 GROUPS instead — one per title, one per
   season — regardless of how many rows sit behind them.                    */

const ZERO_COUNTS = {pending: 0, processing: 0, done: 0, failed: 0, cancelled: 0, total: 0, active: 0};

/** normCounts coerces every bucket to a finite number. The counts drive both
 *  the rolled-up status glyph and the page's headline totals, so a missing or
 *  malformed field must read as 0 rather than poison arithmetic into NaN. */
function normCounts(c) {
  const o = {...ZERO_COUNTS};
  if (c && typeof c === 'object') {
    for (const k of Object.keys(ZERO_COUNTS)) {
      const n = Number(c[k]);
      if (Number.isFinite(n)) o[k] = n;
    }
  }
  return o;
}

function normGroup(g, mediaType) {
  const seasons = Array.isArray(g.seasons) ? g.seasons : [];
  return {
    tmdb_id: Number(g.tmdb_id) || 0,
    media_type: g.media_type === 'tv' || g.media_type === 'movie' ? g.media_type : mediaType,
    title: String(g.title || ''),
    poster_url: String(g.poster_url || ''),
    counts: normCounts(g.counts),
    newest: g.newest || '',
    seasons: seasons.map(s => ({
      season: Number(s.season) || 0,
      counts: normCounts(s.counts),
      newest: s.newest || '',
    })),
  };
}

/**
 * loadQueueGroups fetches the aggregated tree. It NEVER throws, because the
 * queue page has to keep working in three different worlds:
 *
 *   - this build serves /api/queue/groups  -> state.queueGroups is the tree
 *   - an older orchestrator does not       -> 404/401/403 (an unknown /api/
 *     path falls through to the admin-only mux), so we clear the tree and the
 *     page falls back to the flat feed it has always rendered
 *   - the request just failed this once    -> keep the last good tree rather
 *     than blanking a page that was fine 4 seconds ago
 *
 * Nothing is latched: an "absent" answer is re-tested on the next poll, so a
 * transient 401 during a session refresh heals itself.
 */
export async function loadQueueGroups() {
  const r = await apiTry('/api/queue/groups');
  if (!r.ok) {
    if (r.absent) state.queueGroups = null;
    emit('queue-groups');
    return state.queueGroups;
  }
  const d = r.data && typeof r.data === 'object' ? r.data : {};
  const shows = Array.isArray(d.shows) ? d.shows : [];
  const movies = Array.isArray(d.movies) ? d.movies : [];
  state.queueGroups = {
    total: Number(d.total) || 0,
    active: Number(d.active) || 0,
    counts: normCounts(d.counts),
    shows: shows.map(g => normGroup(g, 'tv')),
    movies: movies.map(g => normGroup(g, 'movie')),
  };
  emit('queue-groups');
  return state.queueGroups;
}

/**
 * loadQueueFor fetches the leaf rows of ONE branch of the tree — a whole title
 * (season === null) or a single season. It throws like loadQueue, so the caller
 * can show the failure against the branch the user just opened.
 *
 * Season 0 is a REAL season: movies carry it and TV specials are season 0, so
 * "no season filter" is spelled as null/undefined and never as 0. The server
 * keys the filter on the parameter being PRESENT for exactly that reason.
 *
 * The client-side re-filter at the end is not redundant: an older orchestrator
 * ignores these query parameters and answers with the newest 100 rows of the
 * whole queue, which would otherwise be painted as one season's episodes.
 */
export async function loadQueueFor(tmdbId, season, {mediaType = '', activeOnly = false} = {}) {
  const id = Number(tmdbId) || 0;
  const p = new URLSearchParams();
  p.set('tmdb_id', String(id));
  if (mediaType) p.set('media_type', mediaType);
  if (season !== null && season !== undefined) p.set('season', String(Number(season) || 0));
  if (activeOnly) p.set('active', '1');
  const rows = await apiList(`/api/queue?${p.toString()}`);
  return rows.filter(r =>
    Number(r.tmdb_id) === id &&
    (!mediaType || r.media_type === mediaType) &&
    (season === null || season === undefined || Number(r.season) === Number(season)));
}

export async function loadSubscriptions() {
  try { state.subscriptions = await apiList('/api/subscriptions'); }
  catch (_) { state.subscriptions = []; }
  emit('subs');
  return state.subscriptions;
}

export async function loadMe() {
  try { state.user = await apiObj('/api/me'); }
  catch (_) { state.user = null; }
  emit('auth');
  return state.user;
}

/* ── Derived helpers ─────────────────────────────────────────────────────── */

export function findSub(tmdbId, season) {
  return state.subscriptions.find(s => s.tmdb_id === tmdbId && s.season === season) || null;
}

export function isAdmin() { return !!(state.user && state.user.is_admin); }

/** canManage — admins manage anything; users manage what they requested.
 *  requested_by is stripped for anonymous callers (contract §2), so an
 *  anonymous viewer simply gets `false`, which is the correct answer. */
export function canManage(item) {
  if (!state.user) return false;
  if (state.user.is_admin) return true;
  return !!item.requested_by && item.requested_by === state.user.username;
}

/** showTitle strips a trailing " S01E08" from an episode title. */
export function showTitle(t) {
  return String(t || '').replace(/\s*[Ss]\d{1,2}[Ee]\d{1,3}.*$/, '').trim() || t;
}

export const RING_COLOR = {
  ready: 'var(--green)', pending: 'var(--blue)', processing: 'var(--blue)',
  stale: 'var(--yellow)', failed: 'var(--red)', none: 'var(--border-strong)',
};
/** Every colour-coded state also carries a glyph — colour alone is not a
 *  status (accessibility, and it survives a bad TV panel). */
export const RING_ICON = {
  ready: 'check', pending: 'clock', processing: 'refresh',
  stale: 'refresh', failed: 'alert', none: 'minus',
};

/** episodeStatus: ready | stale | pending | processing | failed | none.
 *  The library is the source of truth per episode, so a stale failed queue
 *  row cannot paint a resolved episode red. */
export function episodeStatus(tmdbId, season, episode) {
  const lib = state.myLibrary.find(i =>
    i.tmdb_id === tmdbId && i.media_type === 'tv' && i.season === season && i.episode === episode);
  if (lib) return lib.status;
  const q = state.queueItems.find(x =>
    x.tmdb_id === tmdbId && x.season === season && x.episode === episode && x.status !== 'cancelled');
  if (q) return q.status === 'done' ? 'ready' : q.status;
  return 'none';
}

export function seasonRingStatus(tmdbId, season) {
  let failed = false, pending = false, stale = false, ready = false;
  const libEps = new Set();
  for (const i of state.myLibrary) {
    if (i.tmdb_id !== tmdbId || i.media_type !== 'tv' || i.season !== season) continue;
    libEps.add(i.episode);
    if (i.status === 'ready') ready = true; else if (i.status === 'stale') stale = true;
  }
  for (const q of state.queueItems) {
    if (q.tmdb_id !== tmdbId || q.season !== season || libEps.has(q.episode)) continue;
    if (q.status === 'failed') failed = true;
    else if (q.status === 'pending' || q.status === 'processing') pending = true;
  }
  if (failed) return 'failed';
  if (pending) return 'pending';
  if (stale) return 'stale';
  if (ready) return 'ready';
  return 'none';
}

export function seasonProgress(tmdbId, season, totalEps) {
  const seen = new Set();
  let ready = 0, pending = 0, failed = 0, stale = 0;
  for (const i of state.myLibrary) {
    if (i.tmdb_id !== tmdbId || i.media_type !== 'tv' || i.season !== season) continue;
    seen.add(i.episode);
    if (i.status === 'ready') ready++; else if (i.status === 'stale') stale++;
  }
  for (const q of state.queueItems) {
    if (q.tmdb_id !== tmdbId || q.season !== season || seen.has(q.episode)) continue;
    if (q.status === 'pending' || q.status === 'processing') { pending++; seen.add(q.episode); }
    else if (q.status === 'failed') { failed++; seen.add(q.episode); }
  }
  return {ready, pending, failed, stale, requested: seen.size, total: totalEps || 0};
}
