/* ==========================================================================
   Search.

   Fixed here:
   - Enter used to fire THREE searches (an inline onkeydown, a second keydown
     listener, and the 400ms debounce). There is now one path.
   - No abort/sequence guard, so a slow earlier response could overwrite a
     newer one. Both are now in place.
   - A failed search said only "Search failed." and a 500 from the server was
     indistinguishable from a genuinely empty result. It now shows the
     server's reason and a Retry.

   The type filter (All / Movies / TV) is applied on the CLIENT. /search returns
   one mixed list from TMDB's multi-search and takes no type parameter, so
   filtering server-side would mean a second round trip for a decision we can
   already answer from the rows in hand — and it would make the counts lie about
   what the search actually found. It is remembered in prefs and mirrored into
   the query string, so a filtered search is linkable and survives a reload.
   ========================================================================== */

import {apiFetch, esc, sequencer, isAbort, errorState, debounce, num} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {state, savePrefs, setQueryParams, queryParam} from './state.js';
import {renderGrid, annotateCards, cardSkeleton} from './cards.js';
import {navSearch, noteRoute} from './router.js';
import {setSearchMode} from './carousels.js';

const seq = sequencer();
let lastQuery = '';
let lastResults = [];      // the unfiltered answer, so the tabs cost no request

function el(id) { return document.getElementById(id); }

export function initSearch() {
  const input = el('q');
  const form = el('search-form');

  form.addEventListener('submit', e => {
    e.preventDefault();
    debounced.cancel();
    const q = input.value.trim();
    if (q) navSearch(q); else location.hash = '';
  });

  const debounced = debounce(() => {
    const v = input.value.trim();
    if (!v) return;
    const h = '#search/' + encodeURIComponent(v);
    noteRoute(h);
    history.replaceState(null, '', h);
    runSearch(v);
  }, 400);

  input.addEventListener('input', () => {
    const v = input.value.trim();
    if (!v) {
      debounced.cancel();
      history.replaceState(null, '', location.pathname + location.search);
      noteRoute('');
      clearSearch();
      return;
    }
    debounced();
  });

  el('search-results').addEventListener('click', e => {
    const retry = e.target.closest('[data-retry-search]');
    if (retry) { runSearch(lastQuery); return; }
    const tab = e.target.closest('[data-search-type]');
    if (tab) setSearchType(tab.dataset.searchType);
  });

  // A linked ?stype= wins over the stored preference: the person who sent the
  // link chose it deliberately, and it is the more recent decision either way.
  const linked = queryParam('stype');
  if (linked === 'movie' || linked === 'tv' || linked === 'all') state.prefs.searchType = linked;
  syncTypeTabs();
}

/* ── Type filter ──────────────────────────────────────────────────────────── */

function setSearchType(type) {
  const t = type === 'movie' || type === 'tv' ? type : 'all';
  if (state.prefs.searchType === t) return;
  state.prefs.searchType = t;
  savePrefs();
  setQueryParams({stype: t === 'all' ? '' : t});
  syncTypeTabs();
  paintResults();
}

function syncTypeTabs() {
  document.querySelectorAll('#search-results [data-search-type]').forEach(b => {
    const on = b.dataset.searchType === state.prefs.searchType;
    b.classList.toggle('active', on);
    b.setAttribute('aria-pressed', String(on));
  });
}

function filtered() {
  const t = state.prefs.searchType;
  if (t !== 'movie' && t !== 'tv') return lastResults;
  return lastResults.filter(r => (r.media_type === 'tv' ? 'tv' : 'movie') === t);
}

/** setQuery reflects a routed query into the box and runs it. */
export function routeSearch(q) {
  const input = el('q');
  if (input.value !== q) input.value = q;
  if (q) runSearch(q); else clearSearch();
}

export function clearSearch() {
  seq.abort();
  lastQuery = '';
  lastResults = [];
  el('hint').textContent = '';
  el('search-results').hidden = true;
  // carousels.js decides what takes the screen back — the rows, or a browse
  // that was already on when the search started. This module owns #search-results
  // and nothing else, so the two cannot disagree about the hero.
  setSearchMode(false);
}

export async function runSearch(q) {
  q = (q || '').trim();
  if (!q) { clearSearch(); return; }
  lastQuery = q;

  el('hint').textContent = 'Searching…';
  document.querySelector('header')?.classList.remove('transparent');
  const wrap = el('search-results');
  wrap.hidden = false;
  syncTypeTabs();
  const grid = el('results');
  grid.innerHTML = cardSkeleton(12);
  setSearchMode(true);

  const token = seq.next();
  let results;
  try {
    results = await apiFetch('/search?q=' + encodeURIComponent(q), {signal: token.signal});
  } catch (e) {
    if (isAbort(e) || !seq.isCurrent(token)) return;
    el('hint').textContent = '';
    grid.innerHTML = errorState(e.message, {retryAttr: 'data-retry-search="1"'});
    return;
  }
  if (!seq.isCurrent(token)) return;

  lastResults = Array.isArray(results) ? results : [];
  paintResults();
  if (!lastResults.length) return;

  // Library badges are a nice-to-have: a failure here must not blank the grid.
  const ids = lastResults.map(r => num(r.tmdb_id)).filter(Boolean).join(',');
  if (!ids) return;
  try {
    const statuses = await apiFetch('/api/library/status?ids=' + ids, {signal: token.signal});
    if (!seq.isCurrent(token) || !statuses) return;
    for (const [id, info] of Object.entries(statuses)) state.libraryStatus[parseInt(id, 10)] = info;
    annotateCards(grid, filtered());
  } catch (_) { /* badges only */ }
}

/**
 * paintResults renders whatever the type filter lets through. It is called on
 * a fresh answer AND on a tab change, so switching tabs costs nothing.
 *
 * Three outcomes, kept apart on purpose: TMDB found nothing at all; TMDB found
 * things but none of this type (which is the user's own filter talking, and it
 * must offer the way back); and results. A failed request never reaches here —
 * runSearch renders errorState() and returns.
 */
function paintResults() {
  const grid = el('results');
  if (!grid) return;
  const rows = filtered();
  const type = state.prefs.searchType;

  if (!lastResults.length) {
    el('hint').textContent = '';
    grid.innerHTML = `<div class="empty-state">${icon('search')}
      <p>No results for “${esc(lastQuery)}”</p>
      <p class="sub">TMDB found nothing under that name. Try the original-language title, or fewer words.</p></div>`;
    return;
  }
  if (!rows.length) {
    const noun = type === 'tv' ? 'TV shows' : 'movies';
    el('hint').textContent =
      `${lastResults.length} result${lastResults.length === 1 ? '' : 's'} for “${lastQuery}”, none of them ${noun}`;
    grid.innerHTML = `<div class="empty-state">${icon('filter')}
      <p>No ${noun} in these results</p>
      <p class="sub">The search found ${lastResults.length} title${lastResults.length === 1 ? '' : 's'},
        but the ${type === 'tv' ? 'TV' : 'Movies'} filter hides ${lastResults.length === 1 ? 'it' : 'them all'}.</p>
      <div class="empty-actions">
        <button type="button" class="btn primary" data-search-type="all">Show all results</button>
      </div></div>`;
    return;
  }
  // With a type filter on, BOTH numbers are said: how many got through, and how
  // many the search actually found. A bare "8 results" would read as TMDB
  // having found eight, which is the user's own filter being mistaken for the
  // answer — the same class of lie as an error rendered as an empty result.
  const noun = rows.length === 1
    ? (type === 'tv' ? 'TV show' : 'movie')
    : (type === 'tv' ? 'TV shows' : 'movies');
  el('hint').textContent = type === 'all'
    ? `${rows.length} result${rows.length === 1 ? '' : 's'} for “${lastQuery}”`
    : `${rows.length} ${noun} in ${lastResults.length} result${lastResults.length === 1 ? '' : 's'} for “${lastQuery}”`;
  renderGrid(grid, rows);
}
