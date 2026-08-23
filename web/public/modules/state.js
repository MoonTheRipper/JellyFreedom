/* ==========================================================================
   Shared client state + a tiny event bus.

   The bus exists so page modules can react to each other without importing
   each other (library.js removing a series must refresh an open modal, but
   modal.js already imports library.js — an import cycle). Everything talks
   through events instead.
   ========================================================================== */

import {apiList, apiObj, apiTry, num} from '../shared/api.js';

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

/** isAiring — zero-cost, from the subscriptions already loaded at boot. It is
 *  a POSITIVE signal only: false means "nothing here says it is airing", not
 *  "this show has ended", so nothing may infer completeness from it. */
export function isAiring(tmdbId) {
  return state.subscriptions.some(s => s.tmdb_id === tmdbId && s.is_airing);
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

/* ── Today, as a string ───────────────────────────────────────────────────
   TMDB air dates are ISO calendar dates (YYYY-MM-DD) with no time and no
   zone. Parsing one into a Date makes it midnight UTC, which is yesterday
   for anyone west of Greenwich — an episode that aired today would read as
   unaired. Comparing the ISO strings directly has no such trap, so `today`
   is formatted from the LOCAL date parts and compared as text. Cached for a
   minute so a full page render does not rebuild it per episode.            */
let todayCache = {at: 0, iso: ''};
function todayISO() {
  const now = Date.now();
  if (todayCache.iso && now - todayCache.at < 60000) return todayCache.iso;
  const d = new Date();
  const p = n => String(n).padStart(2, '0');
  todayCache = {at: now, iso: `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`};
  return todayCache.iso;
}

/** hasAired — "" (TMDB has no date for it) counts as NOT aired, because the
 *  alternative is telling the user an episode exists and they are missing it
 *  on no evidence at all. */
function hasAired(airDate) {
  const d = String(airDate || '').slice(0, 10);
  return !!d && d <= todayISO();
}

export const RING_COLOR = {
  ready: 'var(--green)', pending: 'var(--blue)', processing: 'var(--blue)',
  stale: 'var(--yellow)', failed: 'var(--red)', missing: 'var(--purple)',
  unaired: 'var(--muted)', none: 'var(--border-strong)',
};
/** Every colour-coded state also carries a glyph — colour alone is not a
 *  status (accessibility, and it survives a bad TV panel). */
export const RING_ICON = {
  ready: 'check', pending: 'clock', processing: 'refresh',
  stale: 'refresh', failed: 'alert', missing: 'download',
  unaired: 'calendar', none: 'minus',
};

/** EP_LABEL is the ONE vocabulary for an episode's state, shared by the
 *  library tree and the modal so the two never word the same thing twice.
 *  EP_TAG is the same set abbreviated for the narrow inline chips, where
 *  "Aired, not acquired" would push the episode name out of its row. */
export const EP_LABEL = {
  ready: 'In Jellyfin', stale: 'Expired — re-request', pending: 'Queued',
  processing: 'Fetching', failed: 'Failed', missing: 'Aired, not acquired',
  unaired: 'Not out yet', none: '',
};
export const EP_TAG = {
  ready: 'In Jellyfin', stale: 'Expired', pending: 'Queued',
  processing: 'Fetching', failed: 'Failed', missing: 'Missing',
  unaired: 'Upcoming', none: '',
};

/**
 * episodeStatus: ready | stale | pending | processing | failed | missing | unaired.
 *
 * `none` used to cover two situations that could not be more different: an
 * episode that aired and is NOT in the library (the user has something to do)
 * and an episode that has not been broadcast yet (nothing to do, and nothing
 * has gone wrong). Collapsing them is what made a half-fetched show and a
 * fully-fetched airing show look identical. `airDate` splits them; it comes
 * from tmdb.Episode.AirDate, which /api/tmdb/{id}/seasons/{n}/episodes has
 * always served, so no backend field was added for this.
 *
 * The library is the source of truth per episode, so a stale failed queue row
 * cannot paint a resolved episode red. A library row in any state other than
 * ready/stale is a request that has not resolved yet, which is `pending` —
 * the raw store value ("requested") is not one of the tokens the UI paints.
 */
export function episodeStatus(tmdbId, season, episode, airDate) {
  const lib = state.myLibrary.find(i =>
    i.tmdb_id === tmdbId && i.media_type === 'tv' && i.season === season && i.episode === episode);
  if (lib) return lib.status === 'ready' ? 'ready' : lib.status === 'stale' ? 'stale' : 'pending';
  const q = queuePool(tmdbId, season).find(x =>
    x.tmdb_id === tmdbId && x.season === season && x.episode === episode && x.status !== 'cancelled');
  if (q) return q.status === 'done' ? 'ready' : q.status;
  return hasAired(airDate) ? 'missing' : 'unaired';
}

/* ── Count vectors ────────────────────────────────────────────────────────
   A rolled-up node (a season, a show) is described by a VECTOR of per-state
   counts, never by a headline token. Summing tokens loses the numbers, and
   the numbers are what let a show say "18 of 22 · 4 missing" instead of a
   bare colour — so a show is rolled up by summing its seasons' vectors and
   running the same rollup over the sum.                                    */

const EP_STATES = ['ready', 'stale', 'processing', 'pending', 'failed', 'missing', 'unaired'];

function zeroEpCounts() {
  return {ready: 0, stale: 0, processing: 0, pending: 0, failed: 0, missing: 0, unaired: 0, total: 0};
}

function addEpCounts(a, b) {
  const o = zeroEpCounts();
  for (const k of Object.keys(o)) o[k] = num(a && a[k]) + num(b && b[k]);
  return o;
}

/**
 * unknownEpisodes — how many episodes TMDB says exist that we have NOT
 * classified. It is the honesty term of the whole design: `total` comes from
 * TMDB's episode_count as soon as a season list loads, but an episode is only
 * sorted into missing/unaired once its air date is known, and air dates are
 * fetched one season at a time. Everything not yet classified is counted
 * here rather than being quietly assumed to be one or the other.
 */
export function unknownEpisodes(c) {
  if (!c) return 0;
  let seen = 0;
  for (const k of EP_STATES) seen += num(c[k]);
  return Math.max(0, num(c.total) - seen);
}

/**
 * rollupCounts folds a count vector onto ONE token:
 *
 *     failed   -> something needs a human
 *     working  -> the machine is on it
 *     stale    -> in the library but no longer resolvable
 *     partial  -> aired, and you do not have it            (ACTIONABLE)
 *     unknown  -> TMDB lists episodes we have not checked  (say so, do not guess)
 *     airing   -> you hold every episode that exists yet   (NOT actionable)
 *     complete -> every episode TMDB lists is in Jellyfin
 *     empty    -> nothing here at all
 *
 * Two orderings are the design, not an accident. `failed` outranks `working`
 * for the reason queue.js documents: a season with failures needs attention
 * even while other episodes are still fetching. And `partial` outranks
 * `airing` because "you are missing episodes that have aired" is something
 * you can act on, while "the rest has not been broadcast" is not — an
 * up-to-date airing show must never be reported as needing attention, and a
 * show with holes in it must never be reported as up to date.
 *
 * `airing` and `complete` are deliberately DIFFERENT tokens. A season where
 * you hold all seven episodes that have aired of a running ten-episode season
 * is not finished, and calling it "Complete" (which is what the modal used to
 * do, with the request button disabled) is how the next episode goes unnoticed.
 */
export function rollupCounts(c) {
  if (!c) return 'empty';
  if (num(c.failed) > 0) return 'failed';
  if (num(c.processing) + num(c.pending) > 0) return 'working';
  if (num(c.stale) > 0) return 'stale';
  if (num(c.missing) > 0) return 'partial';
  if (unknownEpisodes(c) > 0) return 'unknown';
  if (num(c.unaired) > 0) return 'airing';
  if (num(c.total) > 0 && num(c.ready) === num(c.total)) return 'complete';
  // Rows in hand but no TMDB total to check them against: we know what we
  // hold, we do not know what exists. That is `unknown`, never `complete`.
  return num(c.ready) > 0 ? 'unknown' : 'empty';
}

/**
 * seasonCounts builds one season's vector.
 *
 *   episodes     — the TMDB episode list, once it has been fetched. With it,
 *                  EVERY episode is classified and `total` is exact.
 *   episodeCount — TMDB's episode_count from the season list. Without the
 *                  episode list we can only classify the episodes we have
 *                  rows for; the remainder lands in unknownEpisodes().
 *
 * With neither, `total` is the number of episodes we have rows for, which
 * rollupCounts reads as "unverified" — it cannot return `complete`.
 */
export function seasonCounts(tmdbId, season, {episodes = null, episodeCount = 0} = {}) {
  const c = zeroEpCounts();
  if (episodes && episodes.length) {
    for (const e of episodes) {
      const st = episodeStatus(tmdbId, season, num(e.episode_number), e.air_date);
      if (c[st] !== undefined) c[st]++;
    }
    c.total = episodes.length;
    return c;
  }
  // Mirrors episodeStatus exactly — library first, then the first non-cancelled
  // queue row — so a season's counts can never disagree with its episode rows.
  const seen = new Set();
  for (const i of state.myLibrary) {
    if (i.tmdb_id !== tmdbId || i.media_type !== 'tv' || i.season !== season) continue;
    if (seen.has(i.episode)) continue;
    seen.add(i.episode);
    if (i.status === 'ready') c.ready++;
    else if (i.status === 'stale') c.stale++;
    else c.pending++;
  }
  for (const q of queuePool(tmdbId, season)) {
    if (q.tmdb_id !== tmdbId || q.media_type !== 'tv' || q.season !== season) continue;
    if (q.status === 'cancelled' || seen.has(q.episode)) continue;
    seen.add(q.episode);
    if (q.status === 'done') c.ready++;
    else if (q.status === 'failed') c.failed++;
    else if (q.status === 'processing') c.processing++;
    else c.pending++;
  }
  // `total` is TMDB's count of episodes that EXIST, and 0 means "we have not
  // asked yet". It must NOT fall back to the number of rows we happen to hold:
  // that would make a show we hold 20 episodes of read as 20 of 20 — Complete —
  // without anything having checked how many episodes there are. Zero here is
  // what makes rollupCounts answer `unknown` instead of `complete`.
  c.total = num(episodeCount);
  return c;
}

/**
 * queuePool picks the best queue rows available for ONE season.
 *
 * state.queueItems is /api/queue's flat feed: the newest 100 rows of a table
 * that holds tens of thousands, so for almost any season the rows that decide
 * its episodes are simply not in it — and an episode being fetched right now
 * would read as "aired, not acquired". A season that has been opened has had
 * its own rows fetched through the scoped filter (ordered by season, episode,
 * capped at 500), which for one season is the whole truth; prefer it.
 */
function queuePool(tmdbId, season) {
  return cachedQueueRows(tmdbId, season) || state.queueItems;
}

/**
 * queueAggFor exposes the server-side queue aggregate for one title. Its
 * numbers count queue ROWS, not episodes — a season can hold 33 failed rows
 * for 20 episodes — so they must never be folded into an episode count
 * vector. They answer exactly one question, uncapped and for free: does this
 * title have queue work behind it that the flat feed cannot see?
 */
export function queueAggFor(tmdbId) {
  const g = state.queueGroups;
  if (!g) return null;
  return g.shows.find(x => num(x.tmdb_id) === num(tmdbId)) || null;
}

/** seasonNumbersOf — every season of a show we have any evidence for: the
 *  TMDB season list once it is cached, plus any season we hold rows for.
 *  The union matters both ways: TMDB knows about seasons you have nothing
 *  from, and TVSeasons drops season 0, so specials only ever appear here. */
export function seasonNumbersOf(tmdbId) {
  const nums = new Set();
  for (const i of state.myLibrary) {
    if (i.tmdb_id === tmdbId && i.media_type === 'tv') nums.add(num(i.season));
  }
  for (const q of state.queueItems) {
    if (q.tmdb_id === tmdbId && q.media_type === 'tv') nums.add(num(q.season));
  }
  const cached = cachedSeasons(tmdbId);
  if (cached) for (const s of cached) nums.add(num(s.season_number));
  return [...nums].sort((a, b) => a - b);
}

/** showCounts sums the season VECTORS — never the season tokens, which would
 *  throw away the counts the labels are built from. */
export function showCounts(tmdbId) {
  const meta = new Map();
  const cached = cachedSeasons(tmdbId);
  if (cached) for (const s of cached) meta.set(num(s.season_number), num(s.episode_count));
  let out = zeroEpCounts();
  for (const sn of seasonNumbersOf(tmdbId)) {
    out = addEpCounts(out, seasonCounts(tmdbId, sn, {
      episodes: cachedEpisodes(tmdbId, sn), episodeCount: meta.get(sn) || 0,
    }));
  }
  return out;
}

/* ── TMDB shape cache ─────────────────────────────────────────────────────
   Season lists and episode lists are fetched LAZILY — a season list when a
   show is expanded, an episode list when a season is — and cached here for
   the lifetime of the page only.

   They are deliberately NOT persisted. An airing season gains episodes and a
   stored episode total has no invalidation story: it would go quietly wrong
   and stay wrong, which is precisely the class of lie this whole change
   exists to remove. A reload is the invalidation.

   internal/tmdb has no cache of its own and a 10s timeout, so one season list
   is one real round-trip to TMDB. The in-flight promise is shared per key, so
   opening, collapsing and re-opening a show cannot start a second request and
   an early answer can never overwrite a later one — the map IS the guard.   */
const seasonsCache = new Map();    // tmdbId -> tmdb.Season[]
const episodesCache = new Map();   // "tmdbId:season" -> tmdb.Episode[]
const queueRowsCache = new Map();  // "tmdbId:season" -> store.QueueItem[] (scoped)
const inFlight = new Map();        // cache key -> promise
const fetchErrors = new Map();     // cache key -> last error message

function epKey(tmdbId, season) { return `${num(tmdbId)}:${num(season)}`; }

export function cachedSeasons(tmdbId) { return seasonsCache.get(num(tmdbId)) || null; }
export function cachedEpisodes(tmdbId, season) { return episodesCache.get(epKey(tmdbId, season)) || null; }
function cachedQueueRows(tmdbId, season) { return queueRowsCache.get(epKey(tmdbId, season)) || null; }
export function shapeError(key) { return fetchErrors.get(key) || null; }
export function shapeLoading(key) { return inFlight.has(key); }

/** noteSeasons / noteEpisodes let a caller that does its own abort-guarded
 *  fetch (the modal) feed this cache, so opening a show in the modal and
 *  expanding it in the library each pay for the other. */
export function noteSeasons(tmdbId, seasons) {
  if (!Array.isArray(seasons)) return;
  seasonsCache.set(num(tmdbId), seasons);
  fetchErrors.delete(String(num(tmdbId)));
  emit('shape');
}
export function noteEpisodes(tmdbId, season, eps) {
  if (!Array.isArray(eps)) return;
  episodesCache.set(epKey(tmdbId, season), eps);
  fetchErrors.delete(epKey(tmdbId, season));
  emit('shape');
}

async function fetchShape(key, load, apply) {
  if (inFlight.has(key)) return inFlight.get(key);
  fetchErrors.delete(key);
  const p = (async () => {
    try {
      const d = await load();
      apply(Array.isArray(d) ? d : []);
    } catch (e) {
      fetchErrors.set(key, e.message);
    } finally {
      inFlight.delete(key);
      emit('shape');
    }
  })();
  inFlight.set(key, p);
  return p;
}

/** loadSeasons fetches a show's season list once. Never throws: the failure
 *  is recorded against the key and rendered against the branch the user
 *  opened, rather than taking the page down. */
export function loadSeasons(tmdbId) {
  const id = num(tmdbId);
  const key = String(id);
  if (seasonsCache.has(id)) return Promise.resolve();
  return fetchShape(key, () => apiList(`/api/tmdb/${encodeURIComponent(id)}/seasons`),
    d => seasonsCache.set(id, d));
}

/** loadEpisodes fetches one season's episode list once — this is where the
 *  air dates come from, and therefore where missing/unaired become knowable. */
export function loadEpisodes(tmdbId, season) {
  const id = num(tmdbId);
  const sn = num(season);
  const key = epKey(id, sn);
  if (episodesCache.has(key)) return Promise.resolve();
  return fetchShape(key,
    () => apiList(`/api/tmdb/${encodeURIComponent(id)}/seasons/${encodeURIComponent(sn)}/episodes`),
    d => episodesCache.set(key, d));
}

/**
 * loadQueueSeason fetches ONE season's queue rows through the scoped filter,
 * which the server orders by season+episode and caps at 500 rather than the
 * flat feed's 100 — for a single season that is every row. Without it an
 * episode being fetched right now, or one that failed last week, is invisible
 * to this page unless it happens to be among the newest hundred requests in
 * the entire queue.
 *
 * `force` is the poll's mode: the rows behind an OPEN season change while the
 * user watches them, so the branch that is on screen is refreshed with the
 * queue and nothing else is.
 */
export function loadQueueSeason(tmdbId, season, {force = false} = {}) {
  const id = num(tmdbId);
  const sn = num(season);
  const key = queueShapeKey(id, sn);
  if (!force && queueRowsCache.has(epKey(id, sn))) return Promise.resolve();
  return fetchShape(key, () => loadQueueFor(id, sn, {mediaType: 'tv'}),
    d => queueRowsCache.set(epKey(id, sn), d));
}

export function seasonShapeKey(tmdbId, season) { return epKey(tmdbId, season); }
export function showShapeKey(tmdbId) { return String(num(tmdbId)); }
export function queueShapeKey(tmdbId, season) { return `q:${epKey(tmdbId, season)}`; }

