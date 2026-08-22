/* ==========================================================================
   Shared client state + a tiny event bus.

   The bus exists so page modules can react to each other without importing
   each other (library.js removing a series must refresh an open modal, but
   modal.js already imports library.js — an import cycle). Everything talks
   through events instead.
   ========================================================================== */

import {apiList, apiObj} from '../shared/api.js';

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
  queueItems: [],        // store.QueueItem[]
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
