/* ==========================================================================
   Home browse rows.

   Fixed here:
   - The IntersectionObservers leaked: one per row, disconnected only when the
     row was actually scrolled to. Rows below the fold on a session where the
     user never scrolls kept an observer alive for the life of the page. They
     are now tracked and all disconnected on teardown.
   - Hidden scrollbars with no arrows and no wheel handling meant there was no
     discoverable way to scroll a row on a desktop. Arrows added, with wheel
     translation and arrow-key support.

   This module also owns the BROWSE & FILTER panel, which lives in the same
   region and is mutually exclusive with the rows: filtering is what you do
   when the fixed rows are not what you wanted, so it takes their place rather
   than sitting above them. See the second half of the file.
   ========================================================================== */

import {apiFetch, esc, num, safeUrl, debounce, sequencer, isAbort, errorState,
  toast} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {state, newBrowse, cachedGenres, loadGenres, genreName, searchStudios, loadDiscover,
  browseToQuery, browseFromQuery, setQueryParams, sortsFor, sortLabel,
  MAX_FILTER_IDS, MAX_BROWSE_PAGE, BROWSE_PAGE_SIZE, RATING_VOTE_FLOOR} from './state.js';
import {renderGrid, annotateCards, cardSkeleton} from './cards.js';

export const CAROUSELS = [
  {id: 'trending',   label: 'Trending This Week', url: '/api/browse/trending'},
  {id: 'pop-movies', label: 'Popular Movies',     url: '/api/browse/discover?type=movie&sort=popularity.desc'},
  {id: 'pop-tv',     label: 'Popular TV Shows',   url: '/api/browse/discover?type=tv&sort=popularity.desc'},
  {id: 'action',     label: 'Action & Adventure', url: '/api/browse/discover?type=movie&genres=28,12'},
  {id: 'scifi',      label: 'Sci-Fi & Fantasy',   url: '/api/browse/discover?type=movie&genres=878,14'},
  {id: 'horror',     label: 'Horror',             url: '/api/browse/discover?type=movie&genres=27'},
  {id: 'comedy',     label: 'Comedy',             url: '/api/browse/discover?type=movie&genres=35'},
  {id: 'drama',      label: 'Drama',              url: '/api/browse/discover?type=movie&genres=18'},
  {id: 'marvel',     label: 'Marvel',             url: '/api/browse/discover?type=movie&companies=420'},
  {id: 'dc',         label: 'DC',                 url: '/api/browse/discover?type=movie&companies=128064,9993'},
  {id: 'paramount',  label: 'Paramount',          url: '/api/browse/discover?type=movie&companies=4'},
  {id: 'hbo',        label: 'HBO',                url: '/api/browse/discover?type=tv&networks=49'},
  {id: 'netflix',    label: 'Netflix Originals',  url: '/api/browse/discover?type=tv&networks=213'},
  {id: 'disney',     label: 'Disney+',            url: '/api/browse/discover?type=tv&networks=2739'},
  {id: 'amazon',     label: 'Prime Video',        url: '/api/browse/discover?type=tv&networks=1024'},
  {id: 'apple',      label: 'Apple TV+',          url: '/api/browse/discover?type=tv&networks=2552'},
];

const observers = [];

/** teardown disconnects every observer this module created. */
export function teardownCarousels() {
  while (observers.length) observers.pop().disconnect();
}

export function renderCarousels() {
  // Bootstraps the browse panel too, and does so FIRST: it decides whether the
  // rows are visible at all, and an IntersectionObserver on a display:none
  // container never fires — which is exactly how arriving on a filtered link
  // avoids spending sixteen of the 180/min browse budget on rows nobody asked
  // for. Observer callbacks are delivered in a later task, so this ordering is
  // what makes that hold.
  initBrowse();
  const container = document.getElementById('carousels');
  if (!container) return;
  teardownCarousels();
  container.innerHTML = CAROUSELS.map(c => `
    <section class="carousel-section" id="carousel-${esc(c.id)}" aria-labelledby="ctitle-${esc(c.id)}">
      <div class="carousel-header">
        <h2 class="carousel-title" id="ctitle-${esc(c.id)}">${esc(c.label)}</h2>
        <div class="carousel-arrows">
          <button type="button" class="car-arrow" data-scroll="-1" data-track="track-${esc(c.id)}"
            aria-label="Scroll ${esc(c.label)} left">${icon('chevron-left')}</button>
          <button type="button" class="car-arrow" data-scroll="1" data-track="track-${esc(c.id)}"
            aria-label="Scroll ${esc(c.label)} right">${icon('chevron-right')}</button>
        </div>
      </div>
      <div class="carousel-track" id="track-${esc(c.id)}" tabindex="0" role="group"
        aria-label="${esc(c.label)}">${cardSkeleton(10, 'carousel-card')}</div>
    </section>`).join('');

  for (const c of CAROUSELS) {
    const section = document.getElementById('carousel-' + c.id);
    if (!section) continue;
    const obs = new IntersectionObserver(entries => {
      if (!entries[0].isIntersecting) return;
      obs.disconnect();
      const i = observers.indexOf(obs);
      if (i !== -1) observers.splice(i, 1);
      loadCarousel(c);
    }, {rootMargin: '300px'});
    obs.observe(section);
    observers.push(obs);
  }
  wireScrolling(container);
}

function wireScrolling(container) {
  if (container.dataset.wired === '1') return;
  container.dataset.wired = '1';

  container.addEventListener('click', e => {
    const btn = e.target.closest('.car-arrow');
    if (!btn) return;
    const track = document.getElementById(btn.dataset.track);
    if (!track) return;
    track.scrollBy({left: Number(btn.dataset.scroll) * Math.max(240, track.clientWidth * 0.8), behavior: 'smooth'});
  });

  // Vertical wheel over a row scrolls it horizontally — the affordance every
  // desktop user reaches for first.
  container.addEventListener('wheel', e => {
    const track = e.target.closest('.carousel-track');
    if (!track) return;
    if (Math.abs(e.deltaY) <= Math.abs(e.deltaX)) return;
    const before = track.scrollLeft;
    track.scrollLeft += e.deltaY;
    if (track.scrollLeft !== before) e.preventDefault();
  }, {passive: false});

  container.addEventListener('keydown', e => {
    const track = e.target.closest('.carousel-track');
    if (!track || e.target !== track) return;
    if (e.key === 'ArrowRight') { e.preventDefault(); track.scrollBy({left: 300, behavior: 'smooth'}); }
    if (e.key === 'ArrowLeft')  { e.preventDefault(); track.scrollBy({left: -300, behavior: 'smooth'}); }
  });

  container.addEventListener('scroll', e => {
    const track = e.target.closest ? e.target.closest('.carousel-track') : null;
    if (track) updateArrows(track);
  }, true);
}

function updateArrows(track) {
  const section = track.closest('.carousel-section');
  if (!section) return;
  const max = track.scrollWidth - track.clientWidth - 2;
  const left = section.querySelector('[data-scroll="-1"]');
  const right = section.querySelector('[data-scroll="1"]');
  if (left) left.disabled = track.scrollLeft <= 2;
  if (right) right.disabled = track.scrollLeft >= max;
  if (section.querySelector('.carousel-arrows')) {
    section.querySelector('.carousel-arrows').hidden = max <= 4;
  }
}

async function loadCarousel(c) {
  const track = document.getElementById('track-' + c.id);
  const section = document.getElementById('carousel-' + c.id);
  if (!track || !section) return;
  let results;
  try {
    results = await apiFetch(c.url);
  } catch (e) {
    // One dead browse row must not shout at the user — the setup banner
    // already explains a missing TMDB key. Hide the row and log the reason.
    console.warn('[jf] browse row failed:', c.id, e.message);
    section.hidden = true;
    return;
  }
  if (!Array.isArray(results) || !results.length) { section.hidden = true; return; }
  renderGrid(track, results, {carousel: true});
  updateArrows(track);

  const ids = results.map(r => num(r.tmdb_id)).filter(Boolean).join(',');
  if (!ids) return;
  try {
    const statuses = await apiFetch('/api/library/status?ids=' + ids);
    if (!statuses) return;
    for (const [id, info] of Object.entries(statuses)) state.libraryStatus[parseInt(id, 10)] = info;
    annotateCards(track, results);
  } catch (_) { /* badges only */ }
}

/* ==========================================================================
   Browse & filter.

   The panel is a thin skin over /api/browse/discover; index.html holds the
   static controls and everything whose vocabulary comes from TMDB is rendered
   here. Three things shape the design:

   1. THE RATE LIMIT IS SHARED. All four browse routes sit behind one 180/min
      per-address window, and a single home page load already spends ~17 of it.
      So: genres are fetched lazily on first open and cached per media type;
      every filter change is coalesced through one debounce instead of firing a
      request per checkbox tick; and the studio autocomplete waits 300ms after
      typing stops. Blowing the budget locks the visitor out of their own home
      page, which is a far worse failure than a slightly late result.

   2. AN EMPTY RESULT AND A FAILED REQUEST ARE NOT THE SAME THING. This
      codebase has already shipped a bug where a 500 rendered as "No releases
      found", which reads as "your indexers had nothing". A filter combination
      with no matches says so, names the filters, and offers to loosen each one;
      a failure says it failed, gives the server's reason, and offers a retry.

   3. NAMES COME FROM TMDB AND LAND IN innerHTML. Genre and studio names, and
      logo URLs, are third-party strings. Every one goes through esc(), and
      every URL through safeUrl() — which additionally guarantees a non-http(s)
      value can never become a live href/src.
   ========================================================================== */

function el(id) { return document.getElementById(id); }

const bseq = sequencer();        // discover requests
const studioSeq = sequencer();   // autocomplete requests

let browseWired = false;
let searching = false;           // set by search.js through setSearchMode()
let lastPageCount = 0;           // rows in the last page — the whole pager story
let genreState = 'idle';         // idle | loading | ready | error
let genreError = '';
let studioMatches = [];
let studioActive = -1;

/* One debounce for ALL filter changes, not one per control: ticking four genre
   chips is one intent and must cost one request. 450ms is long enough to
   absorb a burst of clicks and short enough that the result feels attached to
   the last one — and the skeleton goes up immediately, so the wait is not
   visible as lag. */
const scheduleBrowse = debounce(() => runBrowse(), 450);
const scheduleStudios = debounce(() => runStudioSearch(), 300);

/**
 * setSearchMode lets search.js hand this module the one fact it owns — whether
 * a search is on screen — without either module deciding the other's layout.
 * Search wins while it is running; the browse results are still there when it
 * is cleared. carousels.js is the single place that decides which of hero /
 * rows / browse results is visible.
 */
export function setSearchMode(on) {
  searching = !!on;
  applyMode();
  document.dispatchEvent(new CustomEvent('jf:layout'));
}

/**
 * applyMode is idempotent and fires no events, which is why it can safely be a
 * jf:layout listener: hero.js unhides itself when its artwork finally decodes,
 * long after we may have hidden it for a filtered browse, and that is the event
 * it announces itself with.
 */
function applyMode() {
  const browsing = state.browse.on && !searching;
  const bar = el('browse-bar');
  const panel = el('browse-panel');
  const results = el('browse-results');
  const rows = el('carousels');
  const hero = el('hero');
  const toggle = el('browse-toggle');
  if (bar) bar.hidden = searching;
  if (panel) panel.hidden = searching || !state.browse.open;
  if (results) results.hidden = !browsing;
  if (rows) rows.hidden = searching || browsing;
  if (hero) hero.hidden = searching || browsing || hero.dataset.ready !== '1';
  if (toggle) toggle.setAttribute('aria-expanded', String(!!state.browse.open && !searching));
}

/* ── Boot ─────────────────────────────────────────────────────────────────── */

function initBrowse() {
  if (browseWired) { applyMode(); return; }
  browseWired = true;
  if (!el('browse-panel')) return;   // an older index.html: leave the rows alone

  state.browse = browseFromQuery();
  wireBrowse();
  syncControls();
  renderStudioChips();
  renderSummary();
  renderGenres();
  applyMode();
  document.addEventListener('jf:layout', applyMode);

  if (state.browse.on) {
    // A shared link names genres by id. Fetch the vocabulary so the summary can
    // say "Action / Adventure" rather than "2 genres" — one request, and only
    // when the link actually carried genres.
    if (state.browse.genres.length) ensureGenres();
    runBrowse();
  }
}

function wireBrowse() {
  el('browse-toggle').addEventListener('click', () => {
    state.browse.open = !state.browse.open;
    applyMode();
    if (state.browse.open) ensureGenres();
  });
  el('browse-clear').addEventListener('click', () => resetBrowse());
  el('bf-reset').addEventListener('click', () => resetBrowse());

  el('browse-panel').addEventListener('click', e => {
    const type = e.target.closest('[data-bf-type]');
    if (type) { setType(type.dataset.bfType); return; }
    const match = e.target.closest('[data-bf-match]');
    if (match) {
      state.browse.match = match.dataset.bfMatch === 'all' ? 'all' : 'any';
      changed();
      return;
    }
    const genre = e.target.closest('[data-bf-genre]');
    if (genre) { toggleGenre(num(genre.dataset.bfGenre)); return; }
    const rm = e.target.closest('[data-bf-studio-remove]');
    if (rm) { removeStudio(num(rm.dataset.bfStudioRemove)); return; }
    if (e.target.closest('[data-bf-genres-retry]')) { genreState = 'idle'; ensureGenres(); }
  });

  for (const id of ['bf-sort', 'bf-year', 'bf-votes']) {
    el(id).addEventListener('change', () => {
      const b = state.browse;
      if (id === 'bf-sort') b.sort = el(id).value;
      if (id === 'bf-year') b.year = el(id).value;
      if (id === 'bf-votes') b.minVotes = el(id).value;
      changed();
    });
  }

  wireStudioCombo();

  el('browse-results').addEventListener('click', e => {
    const page = e.target.closest('[data-bf-page]');
    if (page && !page.disabled) { turnPage(num(page.dataset.bfPage)); return; }
    if (e.target.closest('[data-bf-retry]')) { scheduleBrowse.cancel(); runBrowse(); return; }
    if (e.target.closest('[data-bf-reset]')) { resetBrowse(); return; }
    const relax = e.target.closest('[data-bf-relax]');
    if (relax) { relaxFilter(relax.dataset.bfRelax); return; }
  });
}

/* ── Model changes ────────────────────────────────────────────────────────── */

/** changed is the single funnel every filter edit goes through. */
function changed({resetPage = true, immediate = false} = {}) {
  const b = state.browse;
  b.on = true;
  if (resetPage) b.page = 1;
  syncControls();
  renderSummary();
  applyMode();
  setQueryParams(browseToQuery(b));
  showBrowseLoading();
  if (immediate) { scheduleBrowse.cancel(); runBrowse(); } else scheduleBrowse();
}

function setType(type) {
  const t = type === 'tv' ? 'tv' : 'movie';
  const b = state.browse;
  if (b.type === t) {
    // Already on this type. Pressing it is still a request to browse it, and
    // after a reset there is nothing on screen to browse — a button that does
    // visibly nothing is the worst answer available, so start the browse.
    if (!b.on) { ensureGenres(); changed(); }
    return;
  }
  b.type = t;
  // Genre ids are NOT shared between the two vocabularies (movie 28 "Action"
  // has no TV counterpart; TV has "Action & Adventure" at 10759). Carrying a
  // selection across would filter on ids that mean something else, or nothing.
  b.genres = [];
  if (!sortsFor(t).some(s => s.value === b.sort)) {
    toast(`${sortLabel(b.sort)} is only available for movies — sorting by ${sortLabel('popularity.desc')} instead.`,
      {ok: false});
    b.sort = 'popularity.desc';
  }
  genreState = 'idle';
  ensureGenres();
  changed();
}

function toggleGenre(id) {
  const b = state.browse;
  const i = b.genres.indexOf(id);
  if (i === -1) {
    if (b.genres.length >= MAX_FILTER_IDS) {
      toast(`At most ${MAX_FILTER_IDS} genres at once.`, {ok: false});
      return;
    }
    b.genres.push(id);
  } else {
    b.genres.splice(i, 1);
  }
  renderGenres();
  changed();
}

function removeStudio(id) {
  const b = state.browse;
  b.studios = b.studios.filter(s => s.id !== id);
  renderStudioChips();
  changed();
}

function turnPage(delta) {
  const b = state.browse;
  const next = Math.max(1, Math.min(MAX_BROWSE_PAGE, num(b.page, 1) + num(delta)));
  if (next === b.page) return;
  b.page = next;
  changed({resetPage: false, immediate: true});
  const head = el('browse-results');
  if (head && head.scrollIntoView) head.scrollIntoView({block: 'start', behavior: 'smooth'});
}

/** relaxFilter drops exactly one filter — the way back out of an empty result
 *  that does not throw away everything the user set up to get there. */
function relaxFilter(which) {
  const b = state.browse;
  if (which === 'match') b.match = 'any';
  if (which === 'year') b.year = '';
  if (which === 'studios') b.studios = [];
  if (which === 'votes') b.minVotes = '';
  if (which === 'page') b.page = 1;
  renderStudioChips();
  changed({resetPage: which !== 'page', immediate: true});
}

function resetBrowse() {
  scheduleBrowse.cancel();
  scheduleStudios.cancel();
  bseq.abort();
  studioSeq.abort();
  const open = state.browse.open;
  state.browse = newBrowse();
  state.browse.open = open;
  studioMatches = [];
  genreState = 'idle';
  const input = el('bf-studio');
  if (input) input.value = '';
  closeStudioList();
  setStudioStatus('');
  syncControls();
  renderStudioChips();
  renderSummary();
  renderGenres();
  applyMode();
  setQueryParams(browseToQuery(state.browse));
  el('browse-grid').innerHTML = '';
  el('browse-count').textContent = '';
  el('browse-pager').hidden = true;
  if (state.browse.open) ensureGenres();
  document.dispatchEvent(new CustomEvent('jf:layout'));
}

/* ── Controls ─────────────────────────────────────────────────────────────── */

/** yearOptions stops at 1900 rather than tmdb.minFilmYear (1888): the 1890s are
 *  twelve dead entries in a dropdown a TV remote has to scroll. The parser still
 *  accepts 1888 from a URL, so nothing is unreachable — only unlisted. */
function yearOptions() {
  const top = new Date().getFullYear() + 2;
  let out = '<option value="">Any year</option>';
  for (let y = top; y >= 1900; y--) out += `<option value="${y}">${y}</option>`;
  return out;
}

function syncControls() {
  const b = state.browse;
  document.querySelectorAll('#browse-panel [data-bf-type]').forEach(x => {
    const on = x.dataset.bfType === b.type;
    x.classList.toggle('active', on);
    x.setAttribute('aria-pressed', String(on));
  });
  document.querySelectorAll('#browse-panel [data-bf-match]').forEach(x => {
    const on = x.dataset.bfMatch === b.match;
    x.classList.toggle('active', on);
    x.setAttribute('aria-pressed', String(on));
  });

  // The sort list is rebuilt only when the media type actually changes what is
  // on offer — replacing <option>s on every render would fight the user's own
  // selection and lose the open dropdown on a touch device.
  const sortSel = el('bf-sort');
  const opts = sortsFor(b.type);
  const want = opts.map(o => o.value).join('|');
  if (sortSel.dataset.built !== want) {
    sortSel.dataset.built = want;
    sortSel.innerHTML = opts.map(o =>
      `<option value="${esc(o.value)}">${esc(o.label)}</option>`).join('');
  }
  if (!opts.some(o => o.value === b.sort)) b.sort = 'popularity.desc';
  sortSel.value = b.sort;

  const yearSel = el('bf-year');
  if (!yearSel.options.length) yearSel.innerHTML = yearOptions();
  yearSel.value = b.year;
  el('bf-votes').value = b.minVotes;

  el('bf-note').textContent = noteFor(b);
  el('browse-clear').hidden = !b.on;
}

/** noteFor surfaces the server's implicit behaviour, so a number the user did
 *  not choose never silently shapes their results. */
function noteFor(b) {
  if (b.sort === 'vote_average.desc' && b.minVotes === '') {
    return `Highest rated also requires at least ${RATING_VOTE_FLOOR} votes, ` +
      `so a short film with one 10/10 does not top the list. Set a minimum yourself to override that.`;
  }
  if (b.genres.length > 1 && b.match === 'all') {
    return `“All of these” is an intersection: a title must carry every one of the ${b.genres.length} ` +
      `genres you picked. Expect far fewer results than “any of these”.`;
  }
  return '';
}

function renderGenres() {
  const host = el('bf-genres');
  if (!host) return;
  if (genreState === 'loading') {
    host.innerHTML = `<span class="bf-loading">${icon('refresh')} Loading genres…</span>`;
    return;
  }
  if (genreState === 'error') {
    host.innerHTML = errorState(genreError || 'The genre list could not be loaded.',
      {retryAttr: 'data-bf-genres-retry="1"', compact: true});
    return;
  }
  const list = cachedGenres(state.browse.type);
  if (!list) { host.innerHTML = ''; return; }
  if (!list.length) {
    host.innerHTML = `<span class="bf-loading">${icon('info')} TMDB returned no genres for this type.</span>`;
    return;
  }
  host.innerHTML = list.map(g => {
    const on = state.browse.genres.includes(g.id);
    // aria-pressed AND a glyph change AND a fill change: never colour alone.
    return `<button type="button" class="bf-chip${on ? ' on' : ''}" data-bf-genre="${num(g.id)}"
      aria-pressed="${on}">${icon(on ? 'check' : 'plus')}<span>${esc(g.name)}</span></button>`;
  }).join('');
}

/** ensureGenres fetches the vocabulary for the CURRENT type, once. The type is
 *  captured so a late answer for movies cannot paint over a TV panel. */
function ensureGenres() {
  const t = state.browse.type;
  if (cachedGenres(t)) { genreState = 'ready'; renderGenres(); return; }
  if (genreState === 'loading') return;
  genreState = 'loading';
  renderGenres();
  loadGenres(t).then(() => {
    if (state.browse.type !== t) return;
    genreState = 'ready';
    renderGenres();
    renderSummary();
  }, e => {
    if (state.browse.type !== t) return;
    genreState = 'error';
    genreError = e.message;
    renderGenres();
  });
}

/* ── Studio autocomplete ──────────────────────────────────────────────────── */

function wireStudioCombo() {
  const input = el('bf-studio');
  const list = el('bf-studio-list');

  input.addEventListener('input', () => {
    const v = input.value.trim();
    if (v.length < 2) {
      scheduleStudios.cancel();
      studioSeq.abort();
      studioMatches = [];
      closeStudioList();
      setStudioStatus(v ? 'Type at least two characters.' : '');
      return;
    }
    setStudioStatus('Searching…');
    scheduleStudios();
  });

  input.addEventListener('keydown', e => {
    if (e.key === 'Escape') { closeStudioList(); return; }
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      if (!studioMatches.length) return;
      e.preventDefault();
      const d = e.key === 'ArrowDown' ? 1 : -1;
      studioActive = Math.max(0, Math.min(studioMatches.length - 1, studioActive + d));
      renderStudioList();
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      if (studioActive >= 0 && studioMatches[studioActive]) addStudio(studioMatches[studioActive]);
      else { scheduleStudios.cancel(); runStudioSearch(); }
    }
  });

  // Closing on blur has to survive the click that caused it, hence the delay.
  input.addEventListener('blur', () => setTimeout(() => {
    if (!list.contains(document.activeElement)) closeStudioList();
  }, 120));

  list.addEventListener('mousedown', e => {
    const opt = e.target.closest('[data-bf-studio]');
    if (!opt) return;
    e.preventDefault();   // keep focus on the input; blur must not race this
    const id = num(opt.dataset.bfStudio);
    const hit = studioMatches.find(s => s.id === id);
    if (hit) addStudio(hit);
  });
}

async function runStudioSearch() {
  const input = el('bf-studio');
  const v = input.value.trim();
  if (v.length < 2) return;
  const token = studioSeq.next();
  let list;
  try {
    list = await searchStudios(v, {signal: token.signal});
  } catch (e) {
    if (isAbort(e) || !studioSeq.isCurrent(token)) return;
    studioMatches = [];
    closeStudioList();
    // A failed lookup is NOT "no such studio". Say which it was.
    setStudioStatus('Studio lookup failed — ' + e.message);
    return;
  }
  if (!studioSeq.isCurrent(token)) return;
  const already = new Set(state.browse.studios.map(s => s.id));
  studioMatches = list.filter(s => !already.has(s.id)).slice(0, 12);
  studioActive = -1;
  renderStudioList();
  setStudioStatus(studioMatches.length
    ? `${studioMatches.length} studio${studioMatches.length === 1 ? '' : 's'} found. Arrow down to choose.`
    : `No studio matches “${v}”.`);
}

function renderStudioList() {
  const list = el('bf-studio-list');
  const input = el('bf-studio');
  if (!studioMatches.length) { closeStudioList(); return; }
  list.innerHTML = studioMatches.map((s, i) => {
    const logo = safeUrl(s.logo_url);
    const art = logo === '#'
      ? `<span class="bf-opt-logo none" aria-hidden="true">${icon('film')}</span>`
      : `<img class="bf-opt-logo" src="${logo}" alt="" loading="lazy">`;
    return `<li role="option" class="bf-opt${i === studioActive ? ' active' : ''}"
      id="bf-studio-opt-${i}" data-bf-studio="${num(s.id)}"
      aria-selected="${i === studioActive}">${art}<span class="bf-opt-name">${esc(s.name)}</span></li>`;
  }).join('');
  list.hidden = false;
  input.setAttribute('aria-expanded', 'true');
  if (studioActive >= 0) input.setAttribute('aria-activedescendant', 'bf-studio-opt-' + studioActive);
  else input.removeAttribute('aria-activedescendant');
}

function closeStudioList() {
  const list = el('bf-studio-list');
  const input = el('bf-studio');
  if (list) { list.hidden = true; list.innerHTML = ''; }
  if (input) {
    input.setAttribute('aria-expanded', 'false');
    input.removeAttribute('aria-activedescendant');
  }
  studioActive = -1;
}

function setStudioStatus(text) {
  const s = el('bf-studio-status');
  if (s) s.textContent = text;   // textContent: a studio name never becomes markup
}

function addStudio(s) {
  const b = state.browse;
  if (b.studios.some(x => x.id === s.id)) return;
  if (b.studios.length >= MAX_FILTER_IDS) {
    toast(`At most ${MAX_FILTER_IDS} studios at once.`, {ok: false});
    return;
  }
  b.studios.push({id: s.id, name: s.name, logo_url: s.logo_url || ''});
  el('bf-studio').value = '';
  studioMatches = [];
  closeStudioList();
  setStudioStatus(`Added ${s.name}.`);
  renderStudioChips();
  changed();
}

function renderStudioChips() {
  const host = el('bf-studio-chips');
  if (!host) return;
  host.innerHTML = state.browse.studios.map(s => {
    const logo = safeUrl(s.logo_url);
    const art = logo === '#' ? '' : `<img class="bf-chip-logo" src="${logo}" alt="" loading="lazy">`;
    return `<span class="bf-chip on studio">${art}<span>${esc(s.name)}</span>
      <button type="button" class="bf-chip-x" data-bf-studio-remove="${num(s.id)}"
        aria-label="Remove the studio ${esc(s.name)}">${icon('x')}</button></span>`;
  }).join('');
}

/* ── Results ──────────────────────────────────────────────────────────────── */

/** summaryText is plain text and is set with textContent everywhere it is used,
 *  so TMDB's names need no escaping on this path — and must not be given any,
 *  or the user reads &amp; in their own filter summary. */
function summaryText(b) {
  const bits = [b.type === 'tv' ? 'TV shows' : 'Movies'];
  if (b.genres.length) {
    const names = b.genres.map(id => genreName(b.type, id)).filter(Boolean);
    bits.push(names.length === b.genres.length
      ? names.join(b.match === 'all' ? ' + ' : ' / ')
      : `${b.genres.length} genre${b.genres.length === 1 ? '' : 's'}`);
  }
  if (b.studios.length) bits.push(b.studios.map(s => s.name).join(' / '));
  if (b.year) bits.push(b.year);
  if (b.sort !== 'popularity.desc') bits.push(sortLabel(b.sort));
  if (b.minVotes !== '') bits.push(`${b.minVotes}+ votes`);
  return bits.join(' · ');
}

function renderSummary() {
  const b = state.browse;
  const text = summaryText(b);
  const sum = el('browse-summary');
  if (sum) sum.textContent = b.on ? text : '';
  const title = el('browse-results-title');
  if (title) title.textContent = text;
  const clear = el('browse-clear');
  if (clear) clear.hidden = !b.on;
}

function showBrowseLoading() {
  const grid = el('browse-grid');
  if (!grid) return;
  if (grid.dataset.loading !== '1') {
    grid.innerHTML = cardSkeleton(BROWSE_PAGE_SIZE);
    grid.dataset.loading = '1';
  }
  el('browse-count').textContent = 'Loading…';
  el('browse-pager').hidden = true;
}

async function runBrowse() {
  const b = state.browse;
  if (!b.on) return;
  const grid = el('browse-grid');
  const count = el('browse-count');
  if (!grid) return;
  showBrowseLoading();

  const token = bseq.next();
  let results;
  try {
    results = await loadDiscover(b, {signal: token.signal});
  } catch (e) {
    if (isAbort(e) || !bseq.isCurrent(token)) return;
    grid.dataset.loading = '0';
    count.textContent = '';
    // Deliberately NOT the empty state. The server's own reason goes in — a
    // 429 from the shared browse budget and a 502 from TMDB read differently,
    // and neither of them means "nothing matched".
    grid.innerHTML = errorState(e.message, {retryAttr: 'data-bf-retry="1"'});
    el('browse-pager').hidden = true;
    return;
  }
  if (!bseq.isCurrent(token)) return;
  grid.dataset.loading = '0';
  lastPageCount = results.length;

  if (!results.length) { renderBrowseEmpty(); return; }
  count.textContent = `${results.length} title${results.length === 1 ? '' : 's'}` +
    (b.page > 1 ? ` on page ${b.page}` : '');
  renderGrid(grid, results);
  renderPager();

  const ids = results.map(r => num(r.tmdb_id)).filter(Boolean).join(',');
  if (!ids) return;
  try {
    const statuses = await apiFetch('/api/library/status?ids=' + ids, {signal: token.signal});
    if (!bseq.isCurrent(token) || !statuses) return;
    for (const [id, info] of Object.entries(statuses)) state.libraryStatus[parseInt(id, 10)] = info;
    annotateCards(grid, results);
  } catch (_) { /* badges only */ }
}

function renderBrowseEmpty() {
  const b = state.browse;
  const noun = b.type === 'tv' ? 'TV shows' : 'movies';
  const outs = [];
  if (b.genres.length > 1 && b.match === 'all') {
    outs.push(`<button type="button" class="btn" data-bf-relax="match">Match any genre instead</button>`);
  }
  if (b.year) outs.push(`<button type="button" class="btn" data-bf-relax="year">Any year</button>`);
  if (b.studios.length) {
    outs.push(`<button type="button" class="btn" data-bf-relax="studios">Drop the studio filter</button>`);
  }
  if (b.minVotes !== '' && b.minVotes !== '0') {
    outs.push(`<button type="button" class="btn" data-bf-relax="votes">Drop the vote minimum</button>`);
  }
  if (b.page > 1) outs.push(`<button type="button" class="btn" data-bf-relax="page">Back to page 1</button>`);
  outs.push(`<button type="button" class="btn primary" data-bf-reset="1">Clear all filters</button>`);

  // The wording carries the distinction that matters: the request SUCCEEDED.
  el('browse-grid').innerHTML = `<div class="empty-state bf-empty">${icon('filter')}
    <p>No ${noun} match these filters</p>
    <p class="sub">The request worked — TMDB simply has nothing for this combination.
      Loosen one of them:</p>
    <div class="empty-actions">${outs.join('')}</div></div>`;
  el('browse-count').textContent = '0 titles';
  renderPager();
}

/**
 * renderPager. /api/browse/discover answers with a bare array — TMDB's
 * total_pages and total_results are not forwarded — so "is there another page"
 * is INFERRED: a full page (TMDB's fixed 20) means there is probably more, a
 * short page is the last one. That is why the label is "Page 3" and never
 * "Page 3 of 41": we do not know the second number and will not invent it.
 */
function renderPager() {
  const pager = el('browse-pager');
  const b = state.browse;
  const prev = pager.querySelector('[data-bf-page="-1"]');
  const next = pager.querySelector('[data-bf-page="1"]');
  prev.disabled = b.page <= 1;
  next.disabled = lastPageCount < BROWSE_PAGE_SIZE || b.page >= MAX_BROWSE_PAGE;
  el('browse-page-label').textContent = `Page ${b.page}`;
  pager.hidden = prev.disabled && next.disabled;
}
