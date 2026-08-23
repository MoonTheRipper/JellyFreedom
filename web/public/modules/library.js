/* ==========================================================================
   My Library page + the remove/re-request operations used by the modal too.

   THE TREE. The page listed one card per show and, inside it, a row of
   coloured season pips — which is every season reduced to a colour, with no
   counts, no episode behind it and no way in. A show that was half-fetched, a
   show that was still airing and a show that was genuinely finished all
   looked the same, so "which of my shows is missing something?" could only be
   answered by opening each one and reading its seasons by hand.

   Level 1 is now the show, level 2 the season, level 3 the episode — the same
   three levels the queue page grew in 39e8477, using its markup and its class
   names so the two pages read as one product. Level 2 needs TMDB's season
   list (fetched when a show is expanded) and level 3 needs that season's
   episode list with its AIR DATES (fetched when a season is expanded), which
   is what separates "aired and you do not have it" from "not broadcast yet".
   Nothing is fetched for a collapsed node, so opening the page still costs
   the two calls it always did.

   Acting on an episode is NOT rebuilt here: an episode row navigates to
   #tv/{id}/s/{n}/e/{m}, the route the modal already understands, and the
   modal opens on that episode with its release picker — the same route Retry
   on the queue page uses.

   Removal helpers live here (not in modal.js) and announce their result on
   the event bus, so the modal can refresh itself without library.js importing
   it — that would be a cycle.
   ========================================================================== */

import {apiFetch, esc, num, toast, errorState} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {state, savePrefs, loadMyLibrary, loadQueueFor, canManage, showTitle, isAiring,
        showCounts, seasonCounts, seasonNumbersOf, rollupCounts, unknownEpisodes,
        cachedSeasons, cachedEpisodes, loadSeasons, loadEpisodes, loadQueueSeason,
        queueAggFor, shapeError, shapeLoading, seasonShapeKey, showShapeKey, queueShapeKey,
        episodeStatus, EP_TAG, EP_LABEL, RING_COLOR, RING_ICON, emit, on} from './state.js';
import {libSkeleton, statusBadgeHTML} from './cards.js';
import {requireLogin} from './auth.js';
import {navItem} from './router.js';
import {pollQueue} from './queue.js';

/**
 * ROLLUP maps a rolled-up node — a season, or a whole show — onto one status.
 * The tokens come from state.js's rollupCounts; this table is only how they
 * are worded and coloured. `ring` names an entry in RING_COLOR / RING_ICON so
 * every state carries a GLYPH as well as a colour.
 *
 * `airing` and `complete` are separate on purpose and must never render the
 * same badge: holding all seven episodes that have aired of a running season
 * is "Up to date", not "Complete", and a badge that says Complete is how the
 * eighth episode goes unnoticed. See rollupCounts for the full precedence.
 */
const ROLLUP = {
  failed:   {ring: 'failed',     label: 'Needs attention'},
  working:  {ring: 'processing', label: 'In progress'},
  stale:    {ring: 'stale',      label: 'Expired'},
  partial:  {ring: 'missing',    label: 'Missing episodes'},
  unknown:  {ring: 'none',       label: 'Not checked'},
  airing:   {ring: 'unaired',    label: 'Up to date'},
  complete: {ring: 'ready',      label: 'Complete'},
  empty:    {ring: 'none',       label: 'Nothing yet'},
};

/* Which rollups mean "a human should look at this". Ordered: the strip is
   sorted by this, so failures sit above expiries above holes. */
const ATTENTION = ['failed', 'working', 'stale', 'partial'];

/* Expansion lives HERE, not in the DOM, so a refresh cannot collapse what the
   user opened — the same reason queue.js keeps its two open-sets in JS. */
const openShows = new Set();     // tmdb id (number)
const openSeasons = new Set();   // "id:season"
let lastSignature = null;

function el(id) { return document.getElementById(id); }
function seasonKey(tmdbId, season) { return `${num(tmdbId)}:${num(season)}`; }
function pageActive() { const p = el('page-library'); return !!p && p.classList.contains('active'); }

export function initLibrary() {
  const page = el('page-library');

  page.addEventListener('click', e => {
    const tab = e.target.closest('.filter-tab');
    if (tab) {
      state.prefs.libFilter = tab.dataset.filter;
      savePrefs();
      renderLibraryPage();
      return;
    }

    const act = e.target.closest('[data-lib-act]');
    if (act) {
      e.stopPropagation();
      onAction(act);
      return;
    }

    // Both tree heads toggle. They are role="button" (not <button>) because a
    // show head sits in a row that also carries real buttons, and a button
    // inside a button is invalid markup.
    const showHead = e.target.closest('.show-group-head');
    if (showHead) { toggleShow(num(showHead.dataset.tmdbId)); return; }
    const seasonHead = e.target.closest('.season-group-head');
    if (seasonHead) { toggleSeason(seasonHead.dataset.key); }
  });

  page.addEventListener('keydown', e => {
    if (e.key !== 'Enter' && e.key !== ' ' && e.key !== 'Spacebar') return;
    const head = e.target.closest('.show-group-head, .season-group-head');
    if (!head) return;
    e.preventDefault();
    if (head.classList.contains('show-group-head')) toggleShow(num(head.dataset.tmdbId));
    else toggleSeason(head.dataset.key);
  });

  // A lazily-fetched season list or episode list arriving, a queue poll, or a
  // library refresh all change what the tree should say. paint() is a DOM
  // no-op when the model has not changed, so this cannot fight the user.
  on('shape', () => { if (pageActive()) paint(false); });
  on('library', () => { if (pageActive()) paint(false); });
  // Signing in or out changes what this page may offer — the manage buttons,
  // and the "Mine" filter's whole contents. The username is in the signature,
  // so this repaints exactly once per change.
  on('auth', () => { if (pageActive()) paint(false); });
  on('queue', () => {
    if (!pageActive()) return;
    // Bounded twice over: only the seasons the user actually opened, and only
    // while this page is the one on screen. Each refresh emits 'shape', which
    // repaints — and repaints to nothing when the rows have not changed.
    for (const key of openSeasons) {
      const [id, sn] = String(key).split(':');
      loadQueueSeason(num(id), num(sn), {force: true});
    }
    paint(false);
  });
}

function onAction(act) {
  const tmdbId = num(act.dataset.tmdbId);
  switch (act.dataset.libAct) {
    case 'remove-item':   removeLibItem(act.dataset.hash, act.dataset.title); break;
    case 'remove-series': removeSeries(tmdbId, act.dataset.title); break;
    case 'remove-season': removeSeason(tmdbId, num(act.dataset.season)); break;
    case 'rerequest':
      requireLogin(() => navItem(act.dataset.type, tmdbId, {
        forcePicker: true,
        previousRelease: act.dataset.prev || '',
        season: num(act.dataset.season) || null,
        episode: num(act.dataset.episode) || null,
      }));
      break;
    case 'open':     navItem('tv', tmdbId); break;
    case 'season':   navItem('tv', tmdbId, {season: num(act.dataset.season)}); break;
    case 'episode':  openEpisode(tmdbId, num(act.dataset.season), num(act.dataset.episode),
                                 act.dataset.st || ''); break;
    case 'jump':     jumpToActive(tmdbId); break;
    case 'retry-shape': retryShape(tmdbId, act.dataset.season); break;
    case 'retry-load': renderLibraryPage(); break;
  }
}

function syncTabs() {
  document.querySelectorAll('#page-library .filter-tab').forEach(t => {
    const on_ = t.dataset.filter === state.prefs.libFilter;
    t.classList.toggle('active', on_);
    t.setAttribute('aria-pressed', String(on_));
  });
}

/* ── Expansion ───────────────────────────────────────────────────────────── */

function toggleShow(tmdbId) {
  if (!tmdbId) return;
  if (openShows.has(tmdbId)) {
    openShows.delete(tmdbId);
  } else {
    openShows.add(tmdbId);
    // Level 2 needs TMDB's season list — how many episodes each season HAS,
    // which nothing local knows. One call, once per show per page load.
    loadSeasons(tmdbId);
  }
  paint(true);
}

function toggleSeason(key) {
  if (!key) return;
  if (openSeasons.has(key)) {
    openSeasons.delete(key);
  } else {
    openSeasons.add(key);
    const [id, sn] = String(key).split(':');
    // Level 3 needs the episode list, and with it the AIR DATES that decide
    // missing vs unaired. Until it lands the season says "not checked" rather
    // than guessing which of the two it is.
    loadEpisodes(num(id), num(sn));
    // …and this season's own queue rows. state.queueItems is the newest 100
    // rows of the whole queue, so without the scoped fetch an episode that is
    // being fetched right now reads as "aired, not acquired".
    loadQueueSeason(num(id), num(sn));
  }
  paint(true);
}

function retryShape(tmdbId, season) {
  if (season === '' || season === undefined || season === null) {
    loadSeasons(tmdbId);
  } else {
    loadEpisodes(tmdbId, num(season));
    loadQueueSeason(tmdbId, num(season), {force: true});
  }
  paint(true);
}

/* ── Load + paint ────────────────────────────────────────────────────────── */

export async function renderLibraryPage() {
  syncTabs();
  const grid = el('library-grid');
  if (!state.myLibrary.length) grid.innerHTML = libSkeleton();

  try {
    await Promise.all([loadMyLibrary(), pollQueue()]);
  } catch (e) {
    el('library-shows').innerHTML = '';
    el('library-attention').hidden = true;
    grid.innerHTML = errorState(e.message, {retryAttr: 'data-lib-act="retry-load"'});
    lastSignature = null;
    return;
  }
  paint(true);
}

function paint(force) {
  const grid = el('library-grid');
  const showsHost = el('library-shows');
  if (!grid || !showsHost) return;
  syncTabs();

  const filter = state.prefs.libFilter;
  if (filter === 'mine' && !state.user) {
    el('library-attention').hidden = true;
    showsHost.innerHTML = '';
    grid.innerHTML = `<div class="empty-state">${icon('key')}<p>Sign in to see your own requests</p></div>`;
    lastSignature = null;
    return;
  }

  let movies = state.myLibrary.filter(i => i.media_type === 'movie');
  let shows = buildShows(state.myLibrary.filter(i => i.media_type === 'tv'));
  if (filter === 'mine') {
    movies = movies.filter(i => i.requested_by === state.user.username);
    shows = shows.filter(s => s.requesters.has(state.user.username));
  }
  const wantShows = filter === 'all' || filter === 'tv' || filter === 'mine';
  const wantMovies = filter === 'all' || filter === 'movie' || filter === 'mine';

  const sig = signature(shows, movies, filter);
  if (!force && sig === lastSignature) return;
  lastSignature = sig;

  renderAttention(wantShows ? shows : []);
  showsHost.innerHTML = wantShows ? shows.map(renderShow).join('') : '';
  grid.innerHTML = wantMovies ? movies.map(renderLibCard).join('') : '';

  if (!(wantShows && shows.length) && !(wantMovies && movies.length)) {
    grid.innerHTML = `<div class="empty-state">${icon('library')}
      <p>Nothing here${filter === 'mine' ? " — you haven't requested anything yet" : ''}</p>
      <p class="sub">Search for a title and request it; it lands here once it resolves.</p></div>`;
  }
}

/**
 * signature captures everything the markup is built from, so a poll that
 * changed nothing does not touch the DOM — replacing innerHTML on a timer is
 * what destroys focus and collapses whatever the user just opened.
 *
 * It is built from the MODEL (counts, rollups, expansion) rather than from the
 * 888 raw library rows: the model is what the markup depends on, and it is two
 * orders of magnitude smaller to compare.
 */
function signature(shows, movies, filter) {
  const cs = c => `${c.ready}/${c.stale}/${c.processing}/${c.pending}/${c.failed}/${c.missing}/${c.unaired}/${c.total}`;
  const showSig = s => `${s.tmdbId}:${s.roll}:${cs(s.counts)}:${s.shape}:${aggFailed(s.agg)}/${aggActive(s.agg)}` +
    s.seasons.map(x => `|${x.season}:${x.roll}:${cs(x.counts)}:${x.shape}:${x.eps.length}` +
      (shapeError(queueShapeKey(s.tmdbId, x.season)) ? ':qerr' : '')).join('');
  return [
    filter,
    state.user ? state.user.username : '',
    shows.map(showSig).join(';'),
    movies.map(m => `${m.tmdb_id}:${m.status}:${m.info_hash || ''}`).join(';'),
    [...openShows].sort().join(','),
    [...openSeasons].sort().join(','),
  ].join('#');
}

/* ── Model ───────────────────────────────────────────────────────────────── */

/**
 * buildShows collapses the flat library rows into one node per show, then
 * hangs the season vectors off the ones that are open.
 *
 * The library rows decide which shows exist, exactly as before: a title with
 * nothing in the library but rows in the queue belongs to the queue page, and
 * state.queueItems is the newest 100 rows of a 26,000-row table, so treating
 * it as a source of TITLES would list an arbitrary subset. Seasons are a
 * different matter — seasonNumbersOf unions library, queue and TMDB, so a
 * season that so far exists only as queued rows still appears under its show.
 */
function buildShows(tvItems) {
  const map = new Map();
  for (const it of tvItems) {
    let g = map.get(it.tmdb_id);
    if (!g) {
      g = {
        tmdbId: it.tmdb_id, title: showTitle(it.title), year: it.year,
        poster: it.poster_url || '', requesters: new Set(),
      };
      map.set(it.tmdb_id, g);
    }
    if (it.requested_by) g.requesters.add(it.requested_by);
    if (!g.poster && it.poster_url) g.poster = it.poster_url;
    if (!g.year && it.year) g.year = it.year;
  }

  const out = [];
  for (const g of map.values()) {
    g.counts = showCounts(g.tmdbId);
    g.roll = rollupCounts(g.counts);
    g.agg = queueAggFor(g.tmdbId);
    g.airing = isAiring(g.tmdbId);
    g.open = openShows.has(g.tmdbId);
    g.shape = shapeState(showShapeKey(g.tmdbId), !!cachedSeasons(g.tmdbId));
    g.seasonNums = seasonNumbersOf(g.tmdbId);
    g.seasons = g.open ? g.seasonNums.map(sn => seasonNode(g, sn)) : [];
    out.push(g);
  }
  // What needs a human comes first — the same reasoning that puts Failed above
  // In Progress on the queue page. Sorting by season count alone (what this
  // page used to do) buried the three shows with problems under twenty-five
  // that were fine, which is the opposite of what a status page is for.
  // Within a group the old order stands: most seasons first, then by title.
  return out.sort((a, b) =>
    (needsAttention(a) ? attentionRank(a) : 9) - (needsAttention(b) ? attentionRank(b) : 9) ||
    b.seasonNums.length - a.seasonNums.length ||
    String(a.title).localeCompare(String(b.title)));
}

function seasonNode(show, sn) {
  const meta = (cachedSeasons(show.tmdbId) || []).find(s => num(s.season_number) === sn);
  const eps = cachedEpisodes(show.tmdbId, sn) || [];
  const counts = seasonCounts(show.tmdbId, sn, {
    episodes: eps, episodeCount: meta ? num(meta.episode_count) : 0,
  });
  const key = seasonKey(show.tmdbId, sn);
  return {
    season: sn, key, counts, eps,
    roll: rollupCounts(counts),
    open: openSeasons.has(key),
    shape: shapeState(seasonShapeKey(show.tmdbId, sn), eps.length > 0),
  };
}

/** shapeState: 'ok' | 'loading' | 'err' | 'none' — where the lazily fetched
 *  TMDB shape for this node stands. It is part of the signature, so a fetch
 *  starting or failing repaints the branch that is waiting on it. */
function shapeState(key, have) {
  if (have) return 'ok';
  if (shapeLoading(key)) return 'loading';
  return shapeError(key) ? 'err' : 'none';
}

/* ── Shared status furniture (queue.js's markup, so both pages match) ─────── */

function rollupChip(roll) {
  const r = ROLLUP[roll] || ROLLUP.empty;
  return `<span class="qr-chip qr-${esc(roll)}" style="--qr:${esc(RING_COLOR[r.ring])}">` +
    `${icon(RING_ICON[r.ring])} ${esc(r.label)}</span>`;
}

function countBit(ring, text) {
  return `<span class="sg-count"><span class="dot" style="background:${RING_COLOR[ring]}"
    aria-hidden="true"></span>${icon(RING_ICON[ring])} ${esc(text)}</span>`;
}

/**
 * countsLine states the numbers behind the chip.
 *
 * Honesty rule: with no TMDB total in hand it says how much you HAVE
 * ("12 episodes · 3 seasons") and never how much of a whole that is. "18 of
 * 22" only appears once a season list has actually answered.
 */
function countsLine(c, {seasons = 0} = {}) {
  const unknown = unknownEpisodes(c);
  const bits = [];
  if (num(c.total) > 0) {
    bits.push(`<span class="sg-count sg-total">${num(c.ready)} of ${num(c.total)}</span>`);
  } else {
    const held = num(c.ready) + num(c.stale) + num(c.processing) + num(c.pending) + num(c.failed);
    bits.push(`<span class="sg-count sg-total">${held} episode${held === 1 ? '' : 's'}</span>`);
  }
  if (num(c.processing)) bits.push(countBit('processing', `${num(c.processing)} fetching`));
  if (num(c.pending))    bits.push(countBit('pending', `${num(c.pending)} queued`));
  if (num(c.failed))     bits.push(countBit('failed', `${num(c.failed)} failed`));
  if (num(c.stale))      bits.push(countBit('stale', `${num(c.stale)} expired`));
  if (num(c.missing))    bits.push(countBit('missing', `${num(c.missing)} missing`));
  if (num(c.unaired))    bits.push(countBit('unaired', `${num(c.unaired)} upcoming`));
  if (unknown)           bits.push(countBit('none', `${unknown} not checked`));
  if (seasons) bits.push(`<span class="sg-count">${icon('list')} ${seasons} season${seasons === 1 ? '' : 's'}</span>`);
  return bits.join('');
}

/**
 * queueChip states what the server-side queue aggregate knows and nothing
 * local can: how much queue work stands behind this title across the WHOLE
 * table, not just the newest hundred rows.
 *
 * It is a separate chip on purpose. Those numbers count queue ROWS — a season
 * can hold 33 failed rows for 20 episodes — so folding them into the episode
 * vector would make "18 of 22" nonsense. Kept apart and labelled "in queue",
 * they are exactly the number the queue page shows for the same title.
 */
function queueChip(agg) {
  if (!agg || !agg.counts) return '';
  const failed = num(agg.counts.failed);
  const active = num(agg.counts.active);
  if (failed > 0) {
    return `<span class="qr-chip qr-failed" style="--qr:${RING_COLOR.failed}">${icon('alert')} ${failed} failed in queue</span>`;
  }
  if (active > 0) {
    return `<span class="qr-chip qr-working" style="--qr:${RING_COLOR.processing}">${icon('refresh')} ${active} in queue</span>`;
  }
  return '';
}

function aggFailed(agg) { return agg && agg.counts ? num(agg.counts.failed) : 0; }
function aggActive(agg) { return agg && agg.counts ? num(agg.counts.active) : 0; }

/** needsAttention — the rollups that mean a human should look. A show that is
 *  merely airing, or complete, or has nothing in it, is not one of them.
 *
 *  The aggregate can put a show here that the episode vector cannot: failing
 *  and in-flight rows for a show live in a table this page only ever sees the
 *  newest hundred rows of.
 *
 *  What does NOT belong here is a show whose library is complete and whose
 *  queue merely holds old failed rows for episodes it already has — junk in
 *  the queue is the queue page's problem, not a hole in the library. Hence
 *  the aggregate only promotes a show that is not already accounted for. */
function needsAttention(s) {
  if (ATTENTION.includes(s.roll)) return true;
  if (s.roll === 'complete' || s.roll === 'airing') return false;
  return aggFailed(s.agg) > 0 || aggActive(s.agg) > 0;
}

/** attentionRank orders the strip: what broke, then what is running, then
 *  what expired, then what is missing. */
function attentionRank(s) {
  if (ATTENTION.includes(s.roll)) return ATTENTION.indexOf(s.roll);
  return aggFailed(s.agg) > 0 ? 0 : 1;
}

/** jumpable — offer the jump only where there is something to jump TO. Every
 *  state doJump can resolve is one of these. */
function jumpable(s) {
  const c = s.counts;
  if (num(c.processing) + num(c.pending) + num(c.failed) + num(c.stale) + num(c.missing) > 0) return true;
  return aggFailed(s.agg) > 0 || aggActive(s.agg) > 0;
}

/* ── Needs attention ─────────────────────────────────────────────────────── */

function renderAttention(shows) {
  const host = el('library-attention');
  if (!host) return;
  const hits = shows.filter(needsAttention)
    .sort((a, b) => attentionRank(a) - attentionRank(b) ||
      String(a.title).localeCompare(String(b.title)));
  if (!hits.length) { host.hidden = true; host.innerHTML = ''; return; }

  const chips = hits.map(s => {
    const roll = ATTENTION.includes(s.roll) ? s.roll : (aggFailed(s.agg) ? 'failed' : 'working');
    const r = ROLLUP[roll];
    const why = attentionWhy(s, roll);
    return `<button type="button" class="attn-chip attn-${esc(roll)}" data-lib-act="jump"
      data-tmdb-id="${num(s.tmdbId)}" style="--qr:${esc(RING_COLOR[r.ring])}"
      aria-label="${esc(s.title)} — ${esc(r.label)}, ${esc(why)}. Open the episode.">
      ${icon(RING_ICON[r.ring])}<span class="attn-name">${esc(s.title)}</span>
      <span class="attn-why">${esc(why)}</span></button>`;
  }).join('');

  host.hidden = false;
  host.innerHTML = `<div class="lib-attention-head">${icon('alert')}
      Needs attention <span class="lib-attention-n">${hits.length}</span></div>
    <div class="lib-attention-chips">${chips}</div>`;
}

function attentionWhy(s, roll) {
  const c = s.counts;
  if (roll === 'failed')  return `${num(c.failed) || aggFailed(s.agg)} failed`;
  if (roll === 'working') return num(c.processing) ? `${num(c.processing)} fetching`
    : num(c.pending) ? `${num(c.pending)} queued` : `${aggActive(s.agg)} in queue`;
  if (roll === 'stale')   return `${num(c.stale)} expired`;
  return `${num(c.missing)} missing`;
}

/* ── Level 1: a show ─────────────────────────────────────────────────────── */

function renderShow(s) {
  const id = num(s.tmdbId);
  const poster = s.poster
    ? `<img class="queue-poster" src="${esc(s.poster)}" alt="" loading="lazy">`
    : `<span class="queue-poster-placeholder">${icon('tv')}</span>`;

  // Honesty rule: the badge appears only once the season list has answered.
  // Before that all we know is what we HOLD, so an actionable state (failing,
  // fetching, expired) is still worth stating — but "Complete" is not
  // knowable and is never claimed.
  const verified = s.shape === 'ok';
  const chip = (verified || ATTENTION.includes(s.roll)) ? rollupChip(s.roll) : '';
  const qchip = queueChip(s.agg);
  const airing = s.airing
    ? `<span class="qr-chip qr-airing-hint" style="--qr:var(--blue)">${icon('satellite')} Airing</span>` : '';

  const n = s.seasonNums.length;
  const label = `${s.title}, ${n} season${n === 1 ? '' : 's'}` +
    (verified || ATTENTION.includes(s.roll) ? `, ${ROLLUP[s.roll].label}` : '');

  const actions = [
    jumpable(s) ? `<button class="queue-action-btn show-jump" type="button" data-lib-act="jump"
      data-tmdb-id="${id}" aria-label="Jump to the episode that needs attention in ${esc(s.title)}"
      >${icon('activity')}<span>Jump to active</span></button>` : '',
    `<button class="queue-action-btn" type="button" data-lib-act="open" data-tmdb-id="${id}"
      aria-label="Open ${esc(s.title)}">${icon('play')}<span>Open</span></button>`,
    canManageShow(s) ? `<button class="queue-action-btn danger" type="button" data-lib-act="remove-series"
      data-tmdb-id="${id}" data-title="${esc(s.title)}"
      aria-label="Remove the series ${esc(s.title)}">${icon('trash')}<span>Remove</span></button>` : '',
  ].join('');

  return `<div class="show-group lib-show${s.open ? ' open' : ''}">
    <div class="show-row">
      <div class="show-group-head" role="button" tabindex="0" data-tmdb-id="${id}"
           aria-expanded="${s.open}" aria-label="${esc(label)}">
        ${poster}
        <div class="sg-summary">
          <div class="sg-title">${esc(s.title)}${s.year ? ` <span class="sg-year">${esc(s.year)}</span>` : ''}</div>
          <div class="sg-counts">${chip}${qchip}${airing}${countsLine(s.counts, {seasons: n})}</div>
        </div>
        <span class="sg-chevron" aria-hidden="true">${icon('chevron-right')}</span>
      </div>
      ${actions}
    </div>
    ${s.open ? `<div class="show-seasons">${showBody(s)}</div>` : ''}
  </div>`;
}

function showBody(s) {
  if (s.shape === 'loading' && !s.seasons.length) {
    return `<div class="sg-wait">${icon('refresh', 'spin')} Loading seasons…</div>`;
  }
  if (s.shape === 'err' && !cachedSeasons(s.tmdbId)) {
    return `<div class="sg-wait-err">${errorState(shapeError(showShapeKey(s.tmdbId)),
      {compact: true, retryAttr: `data-lib-act="retry-shape" data-tmdb-id="${num(s.tmdbId)}"`})}</div>`;
  }
  if (!s.seasons.length) {
    return `<div class="show-hidden">${icon('info')} No seasons reported for this title.</div>`;
  }
  const body = s.seasons.map(x => renderSeason(s, x)).join('');
  // The season list is still in flight, but we already know the seasons we
  // hold rows for — show them rather than a spinner over real data.
  return s.shape === 'loading'
    ? `<div class="sg-wait">${icon('refresh', 'spin')} Checking TMDB for the full season list…</div>${body}`
    : body;
}

function canManageShow(show) {
  if (!state.user) return false;
  if (state.user.is_admin) return true;
  return show.requesters.has(state.user.username);
}

/* ── Level 2: a season ───────────────────────────────────────────────────── */

function renderSeason(show, s) {
  const id = num(show.tmdbId);
  const verified = s.shape === 'ok' || num(s.counts.total) > 0;
  const chip = (verified || ATTENTION.includes(s.roll)) ? rollupChip(s.roll) : '';
  const name = s.season === 0 ? 'Specials' : `Season ${num(s.season)}`;
  const label = `${show.title} ${name}` +
    (verified || ATTENTION.includes(s.roll) ? `, ${ROLLUP[s.roll].label}` : '');

  return `<div class="season-group${s.open ? ' open' : ''}">
    <div class="season-group-head" role="button" tabindex="0" data-key="${esc(s.key)}"
         aria-expanded="${s.open}" aria-label="${esc(label)}">
      <div class="sg-summary">
        <div class="sg-title">${esc(name)}</div>
        <div class="sg-counts">${chip}${countsLine(s.counts)}</div>
      </div>
      <span class="sg-chevron" aria-hidden="true">${icon('chevron-right')}</span>
    </div>
    <div class="sg-episodes">${s.open ? seasonBody(id, s) : ''}</div>
  </div>`;
}

function seasonBody(id, s) {
  if (!s.eps.length) {
    if (s.shape === 'loading') {
      return `<div class="sg-wait">${icon('refresh', 'spin')} Loading episodes…</div>`;
    }
    if (s.shape === 'err') {
      return `<div class="sg-wait-err">${errorState(shapeError(seasonShapeKey(id, s.season)),
        {compact: true, retryAttr: `data-lib-act="retry-shape" data-tmdb-id="${id}" data-season="${num(s.season)}"`})}</div>`;
    }
    return `<div class="show-hidden">${icon('info')} TMDB lists no episodes for this season.</div>`;
  }
  return queueWarning(id, s.season) +
    s.eps.map(e => renderEpisode(id, s.season, e)).join('') + seasonFooter(id, s);
}

/** The episode list stands on its own, but without this season's queue rows
 *  the states on it fall back to the newest-100 flat feed — so a fetch that
 *  failed has to say so rather than letting stale states pass as current. */
function queueWarning(id, season) {
  const err = shapeError(queueShapeKey(id, season));
  if (!err) return '';
  return `<div class="sg-wait-err">${errorState(
    `Live request status could not be loaded (${err}). Episode states below may be out of date.`,
    {compact: true, retryAttr: `data-lib-act="retry-shape" data-tmdb-id="${num(id)}" data-season="${num(season)}"`})}</div>`;
}

function seasonFooter(id, s) {
  const bits = [`<button class="queue-action-btn" type="button" data-lib-act="season"
    data-tmdb-id="${id}" data-season="${num(s.season)}">${icon('list')} Open season</button>`];
  if (state.user && (num(s.counts.ready) + num(s.counts.stale)) > 0) {
    bits.push(`<button class="queue-action-btn danger" type="button" data-lib-act="remove-season"
      data-tmdb-id="${id}" data-season="${num(s.season)}">${icon('trash')} Remove season</button>`);
  }
  return `<div class="sg-ep-actions">${bits.join('')}</div>`;
}

/* ── Level 3: an episode ─────────────────────────────────────────────────── */

/** An episode row is a real <button> — it contains no nested control, so it
 *  gets Enter, Space and a focus ring from the platform rather than from us.
 *  It navigates to the route the modal already owns; nothing about requesting
 *  is reimplemented here. */
function renderEpisode(id, season, e) {
  const n = num(e.episode_number);
  const st = episodeStatus(id, season, n, e.air_date);
  const code = `S${String(num(season)).padStart(2, '0')}E${String(n).padStart(2, '0')}`;
  const date = String(e.air_date || '').slice(0, 10);
  return `<button type="button" class="sg-ep lib-ep${st === 'processing' ? ' now' : ''}"
      data-lib-act="episode" data-tmdb-id="${id}" data-season="${num(season)}"
      data-episode="${n}" data-st="${esc(st)}"
      aria-label="${esc(code)} ${esc(e.name || '')} — ${esc(EP_LABEL[st] || st)}">
    <span class="dot" style="background:${RING_COLOR[st]}" aria-hidden="true"></span>
    ${icon(RING_ICON[st])}
    <span class="sg-ep-num">E${String(n).padStart(2, '0')}</span>
    <span class="sg-ep-name">${esc(e.name || '')}</span>
    <span class="ep-tag ep-${esc(st)}">${esc(EP_TAG[st] || st)}</span>
    <span class="sg-ep-date">${esc(date)}</span>
  </button>`;
}

/* ── Movies (unchanged: a movie is one row, and a one-child tree is noise) ── */

function renderLibCard(item) {
  const id = num(item.tmdb_id);
  const poster = item.poster_url
    ? `<img src="${esc(item.poster_url)}" alt="" loading="lazy">`
    : `<span class="card-placeholder">${icon('film')}</span>`;
  const isStale = item.status === 'stale';
  const manage = canManage(item);
  // info_hash is required to remove; strm_path/magnet are stripped for
  // anonymous callers (contract §2) and are not used here at all.
  const hash = item.info_hash || '';
  let actions = '';
  if (manage && hash) {
    actions = `<div class="lib-card-actions">
      <button class="btn-sm btn-replace" type="button" data-lib-act="rerequest"
        data-type="${esc(item.media_type)}" data-tmdb-id="${id}"
        data-season="${num(item.season)}" data-episode="${num(item.episode)}"
        data-prev="${esc(item.release_title || '')}">${icon('refresh')} ${isStale ? 'Re-request' : 'Replace'}</button>
      <button class="btn-sm btn-remove" type="button" data-lib-act="remove-item"
        data-hash="${esc(hash)}" data-title="${esc(item.title)}">${icon('trash')} Remove</button>
    </div>`;
  }
  const requesterChip = item.requested_by
    ? `<span class="requester-chip">${icon('users')} ${esc(item.requested_by)}</span>` : '';
  return `<div class="lib-card-wrap">
    <button type="button" class="card${isStale ? ' is-expired' : ''}" data-nav-item="1"
      data-type="${esc(item.media_type)}" data-tmdb-id="${id}"
      data-title="${esc(item.title)}" data-year="${esc(item.year || '')}" data-poster="${esc(item.poster_url || '')}"
      aria-label="${esc(item.title)} — ${isStale ? 'expired, needs re-request' : 'ready'}">
      ${poster}
      <span class="card-scrim" aria-hidden="true"></span>
      <span class="card-overlay">
        <span class="card-title">${esc(item.title)}</span>
        <span class="card-meta"><span>${esc(item.year || '')}</span></span>
      </span>
      ${statusBadgeHTML({status: item.status, library: item.library_name})}
    </button>
    <div class="lib-card-info">${requesterChip}</div>
    ${actions}
  </div>`;
}

/* ── Going to an episode ─────────────────────────────────────────────────── */

/** openEpisode hands off to the route the modal already understands. The
 *  picker is pre-opened only where there is something to pick — a fetching
 *  episode does not need a release chosen for it. */
function openEpisode(tmdbId, season, episode, st) {
  const act = st === 'failed' || st === 'stale' || st === 'missing';
  navItem('tv', tmdbId, {season, episode, forcePicker: act});
}

/* JUMP_ORDER answers "where is this show right now?", which is a different
   question from the chip's "does this need me?" — hence a different order.
   What the machine is doing comes first, then what it will pick up, then what
   broke, then what rotted, then what was never fetched. */
const JUMP_ORDER = ['processing', 'pending', 'failed', 'stale', 'missing'];

function jumpToActive(tmdbId) { requireLogin(() => doJump(tmdbId)); }

async function doJump(tmdbId) {
  const id = num(tmdbId);
  // The scoped fetch, not state.queueItems: the flat feed is the newest 100
  // rows of the whole queue, so the episode being worked on is usually not in
  // it — the same trap queue.js documents on its own jump.
  let rows = [];
  try {
    rows = await loadQueueFor(id, null, {mediaType: 'tv'});
  } catch (e) {
    toast(e.message, {ok: false});
  }

  for (const st of JUMP_ORDER) {
    const hit = pickTarget(id, st, rows);
    if (!hit) continue;
    openShows.add(id);
    loadSeasons(id);
    openSeasons.add(seasonKey(id, hit.season));
    loadEpisodes(id, hit.season);
    if (pageActive()) paint(true);
    openEpisode(id, hit.season, hit.episode, st);
    return;
  }
  toast('Nothing is waiting in this show any more');
  navItem('tv', id);
}

function pickTarget(id, st, rows) {
  if (st === 'processing' || st === 'pending' || st === 'failed') {
    return lowest(rows.filter(r => r.status === st && r.media_type === 'tv')
      .map(r => ({season: num(r.season), episode: num(r.episode)})));
  }
  if (st === 'stale') {
    return lowest(state.myLibrary
      .filter(i => i.tmdb_id === id && i.media_type === 'tv' && i.status === 'stale')
      .map(i => ({season: num(i.season), episode: num(i.episode)})));
  }
  // `missing` is only answerable where the episode list has already been
  // fetched. We never fan out a TMDB call per season to answer one click —
  // which is also why the jump is offered only when the counts already show
  // something to jump to.
  const out = [];
  for (const sn of seasonNumbersOf(id)) {
    for (const e of (cachedEpisodes(id, sn) || [])) {
      const n = num(e.episode_number);
      if (episodeStatus(id, sn, n, e.air_date) === 'missing') out.push({season: sn, episode: n});
    }
  }
  return lowest(out);
}

function lowest(list) {
  return list.sort((a, b) => a.season - b.season || a.episode - b.episode)[0] || null;
}

/* ── Removal operations (also used from the modal) ───────────────────────── */

/* requireLogin RUNS its callback immediately when the user is already signed in
   (auth.js:47), so passing the guarded function itself as the callback made each
   of these call straight back into itself: requireLogin -> removeX -> requireLogin
   -> ... until the stack blew. Because they are async, the RangeError surfaced as
   an unhandled rejection, so for a SIGNED-IN user every Remove button threw and
   did nothing visible. Signed-out was no better: the callback is replayed after a
   successful login, and the replay recursed the same way.

   Split guard from work. `state.user` is checked directly rather than reusing
   requireLogin's return value, because these must still hand back a promise —
   removeEpisode's result drives refreshEpisodeList() in modal.js. */

export function removeLibItem(hash, title) {
  if (!state.user) { requireLogin(() => doRemoveLibItem(hash, title)); return Promise.resolve(false); }
  return doRemoveLibItem(hash, title);
}

async function doRemoveLibItem(hash, title) {
  if (!confirm(`Remove “${title}” from your library?`)) return false;
  try {
    await apiFetch(`/api/library/${encodeURIComponent(hash)}/drop`, {method: 'POST'});
  } catch (e) {
    if (e.status !== 401) toast(e.message, {ok: false});
    return false;
  }
  toast(`Removed ${title}`);
  await loadMyLibrary();
  emit('library-changed', {kind: 'item', hash});
  if (pageActive()) paint(true);
  return true;
}

export function removeSeries(tmdbId, title) {
  if (!state.user) { requireLogin(() => doRemoveSeries(tmdbId, title)); return Promise.resolve(false); }
  return doRemoveSeries(tmdbId, title);
}

async function doRemoveSeries(tmdbId, title) {
  if (!confirm(`Remove the entire series “${title}” from your library? You can re-request it afterward.`)) return false;
  let d;
  try {
    d = await apiFetch(`/api/library/series/${encodeURIComponent(tmdbId)}/drop`, {method: 'POST'});
  } catch (e) {
    if (e.status !== 401) toast(e.message, {ok: false});
    return false;
  }
  const count = (d && d.count) || 0;
  toast(`Removed ${title} (${count} episode${count === 1 ? '' : 's'})`);
  openShows.delete(num(tmdbId));
  await loadMyLibrary();
  emit('library-changed', {kind: 'series', tmdbId});
  if (pageActive()) paint(true);
  return true;
}

export function removeSeason(tmdbId, season) {
  if (!state.user) { requireLogin(() => doRemoveSeason(tmdbId, season)); return Promise.resolve(false); }
  return doRemoveSeason(tmdbId, season);
}

async function doRemoveSeason(tmdbId, season) {
  if (!confirm(`Remove all of Season ${season} from your library?`)) return false;
  let d;
  try {
    d = await apiFetch('/api/library/season/drop', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({tmdb_id: tmdbId, season}),
    });
  } catch (e) {
    if (e.status !== 401) toast(e.message, {ok: false});
    return false;
  }
  const count = (d && d.count) || 0;
  toast(`Removed Season ${season} (${count} episode${count === 1 ? '' : 's'})`);
  await pollQueue();
  await loadMyLibrary();
  emit('library-changed', {kind: 'season', tmdbId, season});
  if (pageActive()) paint(true);
  return true;
}

export function removeEpisode(tmdbId, season, episode) {
  if (!state.user) { requireLogin(() => doRemoveEpisode(tmdbId, season, episode)); return Promise.resolve(false); }
  return doRemoveEpisode(tmdbId, season, episode);
}

async function doRemoveEpisode(tmdbId, season, episode) {
  const lbl = `S${String(season).padStart(2, '0')}E${String(episode).padStart(2, '0')}`;
  if (!confirm(`Remove ${lbl} from your library?`)) return false;
  try {
    await apiFetch('/api/library/episode/drop', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({tmdb_id: tmdbId, season, episode}),
    });
  } catch (e) {
    if (e.status !== 401) toast(e.message, {ok: false});
    return false;
  }
  toast(`Removed ${lbl}`);
  await pollQueue();
  await loadMyLibrary();
  emit('library-changed', {kind: 'episode', tmdbId, season, episode});
  if (pageActive()) paint(true);
  return true;
}
