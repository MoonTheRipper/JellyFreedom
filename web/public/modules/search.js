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
   ========================================================================== */

import {apiFetch, esc, sequencer, isAbort, errorState, debounce, num} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {state} from './state.js';
import {renderGrid, annotateCards, cardSkeleton} from './cards.js';
import {navSearch, noteRoute} from './router.js';

const seq = sequencer();
let lastQuery = '';

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
    if (retry) runSearch(lastQuery);
  });
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
  el('hint').textContent = '';
  el('search-results').hidden = true;
  el('carousels').hidden = false;
  const hero = el('hero');
  if (hero && hero.dataset.ready === '1') hero.hidden = false;
  document.dispatchEvent(new CustomEvent('jf:layout'));
}

export async function runSearch(q) {
  q = (q || '').trim();
  if (!q) { clearSearch(); return; }
  lastQuery = q;

  el('hint').textContent = 'Searching…';
  el('carousels').hidden = true;
  const hero = el('hero');
  if (hero) hero.hidden = true;
  document.querySelector('header')?.classList.remove('transparent');
  const wrap = el('search-results');
  wrap.hidden = false;
  const grid = el('results');
  grid.innerHTML = cardSkeleton(12);
  document.dispatchEvent(new CustomEvent('jf:layout'));

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

  results = Array.isArray(results) ? results : [];
  if (!results.length) {
    el('hint').textContent = '';
    grid.innerHTML = `<div class="empty-state">${icon('search')}
      <p>No results for “${esc(q)}”</p>
      <p class="sub">TMDB found nothing under that name. Try the original-language title, or fewer words.</p></div>`;
    return;
  }
  el('hint').textContent = `${results.length} result${results.length === 1 ? '' : 's'} for “${q}”`;
  renderGrid(grid, results);

  // Library badges are a nice-to-have: a failure here must not blank the grid.
  const ids = results.map(r => num(r.tmdb_id)).filter(Boolean).join(',');
  if (!ids) return;
  try {
    const statuses = await apiFetch('/api/library/status?ids=' + ids, {signal: token.signal});
    if (!seq.isCurrent(token) || !statuses) return;
    for (const [id, info] of Object.entries(statuses)) state.libraryStatus[parseInt(id, 10)] = info;
    annotateCards(grid, results);
  } catch (_) { /* badges only */ }
}
