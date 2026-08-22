/* ==========================================================================
   Poster cards + grids, shared by search, carousels and the library.

   Every card is a real <button>. They used to be <div onclick> with no
   tabindex, no role and no key handler, which meant a TV remote's D-pad could
   not reach a single poster — the whole grid was unusable on a TV browser,
   and by keyboard anywhere.
   ========================================================================== */

import {esc, num} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {state} from './state.js';

/** cardSkeleton renders shimmer placeholders so a grid never flashes empty. */
export function cardSkeleton(n = 12, cls = 'card') {
  return Array(n).fill(0).map(() =>
    `<div class="${cls} skeleton" aria-hidden="true"></div>`).join('');
}
export function libSkeleton(n = 12) {
  return Array(n).fill(0).map(() =>
    `<div class="lib-card-wrap"><div class="card skeleton" aria-hidden="true"></div>
     <div class="lib-card-info"><div class="skeleton sk-line" style="width:70%"></div></div></div>`).join('');
}

/**
 * cardHTML renders one poster card.
 * Every interpolated value passes through esc()/num() — release and TMDB
 * titles are attacker-influenced text and must never reach markup raw.
 */
export function cardHTML(r, {carousel = false} = {}) {
  const tmdbId = num(r.tmdb_id);
  const mediaType = r.media_type === 'tv' ? 'tv' : 'movie';
  const title = r.title || '';
  const year = r.year || '';
  const posterUrl = r.poster_url || '';
  const cls = carousel ? 'carousel-card' : 'card';
  const img = posterUrl
    ? `<img src="${esc(posterUrl)}" alt="" loading="lazy">`
    : `<span class="card-placeholder">${icon(mediaType === 'tv' ? 'tv' : 'film')}</span>`;
  const rate = Number(r.vote_average);
  const rateBadge = Number.isFinite(rate) && rate > 0
    ? `<span class="card-rating">${icon('flame')} ${rate.toFixed(1)}</span>` : '';
  const actionLabel = mediaType === 'tv'
    ? `${icon('play')} Browse` : `${icon('plus')} Request`;
  const info = state.libraryStatus[tmdbId];
  const inLib = info && info.status === 'ready';
  const badge = info ? statusBadgeHTML(info) : '';
  return `<button type="button" class="${cls}${inLib ? ' in-library' : ''}${info && info.status === 'stale' ? ' is-expired' : ''}"
      data-nav-item="1" data-type="${mediaType}" data-tmdb-id="${tmdbId}"
      data-title="${esc(title)}" data-year="${esc(year)}" data-poster="${esc(posterUrl)}"
      aria-label="${esc(title)}${year ? ' (' + esc(year) + ')' : ''} — ${mediaType === 'tv' ? 'TV series' : 'movie'}">
    ${img}
    <span class="card-scrim" aria-hidden="true"></span>
    ${rateBadge}
    <span class="card-overlay">
      <span class="card-title">${esc(title)}</span>
      <span class="card-meta">
        <span>${esc(year)}</span>
        <span class="badge ${mediaType}">${mediaType === 'tv' ? 'TV' : 'Movie'}</span>
      </span>
    </span>
    <span class="card-action" aria-hidden="true">${actionLabel}</span>
    ${badge}
  </button>`;
}

/** statusBadgeHTML — colour AND glyph AND text, never colour alone. */
export function statusBadgeHTML(info) {
  if (!info) return '';
  if (info.status === 'ready') {
    return `<span class="status-badge ready">${icon('check')} ${esc(info.library || 'Library')}</span>`;
  }
  if (info.status === 'stale') {
    return `<span class="status-badge stale">${icon('refresh')} Expired</span>`;
  }
  return '';
}

/** renderGrid replaces a container's contents with cards. */
export function renderGrid(container, results, opts = {}) {
  if (!container) return;
  container.innerHTML = (results || []).map(r => cardHTML(r, opts)).join('');
}

/**
 * annotateCards re-applies library badges after /api/library/status resolves.
 * Cards are addressed by data-tmdb-id within the given container only, so a
 * carousel cannot clobber a search result's badge.
 */
export function annotateCards(container, results) {
  if (!container) return;
  for (const r of (results || [])) {
    const id = num(r.tmdb_id);
    const info = state.libraryStatus[id];
    if (!info) continue;
    const card = container.querySelector(`[data-tmdb-id="${id}"]`);
    if (!card) continue;
    const old = card.querySelector('.status-badge');
    if (old) old.remove();
    card.insertAdjacentHTML('beforeend', statusBadgeHTML(info));
    card.classList.toggle('in-library', info.status === 'ready');
    card.classList.toggle('is-expired', info.status === 'stale');
  }
}

/** itemFromEl rebuilds an item object from a card's data-* attributes. */
export function itemFromEl(el) {
  return {
    tmdb_id: Number(el.dataset.tmdbId),
    media_type: el.dataset.type,
    title: el.dataset.title || '',
    year: el.dataset.year || '',
    poster_url: el.dataset.poster || '',
    season: Number(el.dataset.season || 0) || null,
    episode: Number(el.dataset.episode || 0) || null,
  };
}
