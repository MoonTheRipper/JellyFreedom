/* ==========================================================================
   My Library page + the remove/re-request operations used by the modal too.

   Removal helpers live here (not in modal.js) and announce their result on the
   event bus, so the modal can refresh itself without library.js importing it —
   that would be a cycle.
   ========================================================================== */

import {apiFetch, esc, num, toast, errorState} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {state, savePrefs, loadMyLibrary, canManage, showTitle, seasonRingStatus, RING_COLOR, emit} from './state.js';
import {libSkeleton, statusBadgeHTML} from './cards.js';
import {requireLogin} from './auth.js';
import {navItem} from './router.js';
import {pollQueue} from './queue.js';

function el(id) { return document.getElementById(id); }

export function initLibrary() {
  document.querySelector('#page-library .filter-tabs').addEventListener('click', e => {
    const tab = e.target.closest('.filter-tab');
    if (!tab) return;
    state.prefs.libFilter = tab.dataset.filter;
    savePrefs();
    renderLibraryPage();
  });

  el('library-grid').addEventListener('click', e => {
    const rm = e.target.closest('[data-lib-act]');
    if (!rm) return;
    e.stopPropagation();
    switch (rm.dataset.libAct) {
      case 'remove-item':   removeLibItem(rm.dataset.hash, rm.dataset.title); break;
      case 'remove-series': removeSeries(Number(rm.dataset.tmdbId), rm.dataset.title); break;
      case 'rerequest': {
        const go = () => navItem(rm.dataset.type, Number(rm.dataset.tmdbId), {
          forcePicker: true,
          previousRelease: rm.dataset.prev || '',
          season: Number(rm.dataset.season) || null,
          episode: Number(rm.dataset.episode) || null,
        });
        requireLogin(go);
        break;
      }
      case 'retry-load': renderLibraryPage(); break;
    }
  });
}

function syncTabs() {
  document.querySelectorAll('#page-library .filter-tab').forEach(t => {
    const on = t.dataset.filter === state.prefs.libFilter;
    t.classList.toggle('active', on);
    t.setAttribute('aria-pressed', String(on));
  });
}

export async function renderLibraryPage() {
  syncTabs();
  const grid = el('library-grid');
  grid.innerHTML = libSkeleton();

  let items;
  try {
    [items] = await Promise.all([loadMyLibrary(), pollQueue()]);
  } catch (e) {
    grid.innerHTML = errorState(e.message, {retryAttr: 'data-lib-act="retry-load"'});
    return;
  }

  const filter = state.prefs.libFilter;
  let movies = items.filter(i => i.media_type === 'movie');
  let shows = groupShows(items.filter(i => i.media_type === 'tv'));

  if (filter === 'mine') {
    if (!state.user) {
      grid.innerHTML = `<div class="empty-state">${icon('key')}<p>Sign in to see your own requests</p></div>`;
      return;
    }
    movies = movies.filter(i => i.requested_by === state.user.username);
    shows = shows.filter(s => s.requesters.has(state.user.username));
  }

  let html = '';
  if (filter === 'all' || filter === 'tv' || filter === 'mine') html += shows.map(renderShowCard).join('');
  if (filter === 'all' || filter === 'movie' || filter === 'mine') html += movies.map(renderLibCard).join('');

  grid.innerHTML = html || `<div class="empty-state">${icon('library')}
    <p>Nothing here${filter === 'mine' ? " — you haven't requested anything yet" : ''}</p>
    <p class="sub">Search for a title and request it; it lands here once it resolves.</p></div>`;
}

/** groupShows collapses flat TV episode rows into one entry per show. */
function groupShows(tvItems) {
  const map = {};
  for (const it of tvItems) {
    let g = map[it.tmdb_id];
    if (!g) {
      g = map[it.tmdb_id] = {
        tmdbId: it.tmdb_id, title: showTitle(it.title), year: it.year,
        poster: it.poster_url || '', seasons: new Set(), requesters: new Set(), anyStale: false,
      };
    }
    g.seasons.add(it.season);
    if (it.requested_by) g.requesters.add(it.requested_by);
    if (it.status === 'stale') g.anyStale = true;
    if (!g.poster && it.poster_url) g.poster = it.poster_url;
  }
  return Object.values(map).sort((a, b) => b.seasons.size - a.seasons.size || a.title.localeCompare(b.title));
}

function canManageShow(show) {
  if (!state.user) return false;
  if (state.user.is_admin) return true;
  return show.requesters.has(state.user.username);
}

function renderShowCard(show) {
  const id = num(show.tmdbId);
  const poster = show.poster
    ? `<img src="${esc(show.poster)}" alt="" loading="lazy">`
    : `<span class="card-placeholder">${icon('tv')}</span>`;
  const seasons = [...show.seasons].sort((a, b) => a - b);
  const rings = seasons.map(sn => {
    const st = seasonRingStatus(show.tmdbId, sn);
    return `<span class="season-ring" style="background:${RING_COLOR[st]}"
      title="Season ${num(sn)}: ${esc(st)}">S${num(sn)}</span>`;
  }).join('');
  const badge = show.anyStale
    ? `<span class="status-badge stale">${icon('refresh')} Expired</span>` : '';
  const actions = canManageShow(show)
    ? `<div class="lib-card-actions">
         <button class="btn-sm btn-remove" type="button" data-lib-act="remove-series"
           data-tmdb-id="${id}" data-title="${esc(show.title)}">${icon('trash')} Remove series</button>
       </div>` : '';
  return `<div class="lib-card-wrap">
    <button type="button" class="card show-card" data-nav-item="1" data-type="tv" data-tmdb-id="${id}"
      data-title="${esc(show.title)}" data-year="${esc(show.year || '')}" data-poster="${esc(show.poster || '')}"
      aria-label="${esc(show.title)} — ${seasons.length} season${seasons.length === 1 ? '' : 's'} in library">
      ${poster}
      <span class="card-scrim" aria-hidden="true"></span>
      <span class="card-overlay">
        <span class="card-title">${esc(show.title)}</span>
        <span class="season-rings">${rings}</span>
      </span>
      ${badge}
      <span class="card-action" aria-hidden="true">${icon('play')} Manage</span>
    </button>
    ${actions}
  </div>`;
}

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
  if (el('page-library').classList.contains('active')) renderLibraryPage();
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
  await loadMyLibrary();
  emit('library-changed', {kind: 'series', tmdbId});
  if (el('page-library').classList.contains('active')) renderLibraryPage();
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
  return true;
}
