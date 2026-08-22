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
   ========================================================================== */

import {apiFetch, esc, num} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {state} from './state.js';
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
