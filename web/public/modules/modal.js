/* ==========================================================================
   Title detail modal: metadata, cast, related, seasons, episodes, requesting.

   Fixed here:
   - It was a plain div with no role, no aria-modal, no focus trap, no focus
     restore and no body scroll lock (so the page scrolled behind the mobile
     bottom sheet). All five are handled by ./dialog.js now.
   - Every onclick="fn('${title}')" is gone; handlers read data-* attributes.
   - "Loading…" text replaced with skeletons matching the shape that follows.
   - Seasons/episodes/releases are abort- and sequence-guarded.
   - A route can now carry /s/<n>/e/<n>, so Retry lands on the failed episode.
   ========================================================================== */

import {apiFetch, esc, num, toast, errorState, sequencer, isAbort, safeUrl} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {state, loadMyLibrary, loadSubscriptions, findSub, on,
  episodeStatus, seasonRingStatus, seasonProgress, RING_COLOR, RING_ICON} from './state.js';
import {openDialog, closeDialog, isDialogOpen, focusFirst} from './dialog.js';
import {requireLogin} from './auth.js';
import {navItem, closeModalRoute, syncHash} from './router.js';
import {removeSeries, removeSeason, removeEpisode, removeLibItem} from './library.js';
import {pollQueue} from './queue.js';
import * as picker from './releases.js';

const EP_LABEL = {ready: 'Ready', stale: 'Expired', pending: 'Queued',
  processing: 'Fetching', failed: 'Failed', none: ''};

const seasonsSeq = sequencer();
const epsSeq = sequencer();

const ctx = {
  item: null,             // {tmdb_id, media_type, ...}
  details: null,          // /api/tmdb/{id}/full
  season: null,
  episode: null,
  episodes: [],
  forcePicker: false,
  pendingSeason: null,    // from the route, applied once seasons load
  pendingEpisode: null,
};

function el(id) { return document.getElementById(id); }
function overlay() { return el('overlay'); }
function content() { return el('modal-content'); }

export function initModal() {
  overlay().addEventListener('click', e => { if (e.target === e.currentTarget) closeModalRoute(); });
  el('modal-close').addEventListener('click', () => closeModalRoute());
  content().addEventListener('click', onContentClick);
  content().addEventListener('keydown', e => {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    const row = e.target.closest('.ep-row');
    if (!row) return;
    e.preventDefault();
    toggleEpisode(Number(row.dataset.ep));
  });
  picker.wirePicker(content(), {onRetry: () => picker.reloadPicker()});

  // Any library mutation from elsewhere refreshes what is on screen.
  on('library-changed', payload => {
    if (!ctx.item) return;
    if (payload && payload.kind === 'series' && payload.tmdbId === ctx.item.tmdb_id) {
      closeModalRoute();
      return;
    }
    if (ctx.item.media_type === 'tv') refreshSeasons();
    else if (ctx.details) renderRich(ctx.details);
  });
}

export function modalIsOpen() { return isDialogOpen(overlay()); }

export function closeModalDOM() {
  seasonsSeq.abort();
  epsSeq.abort();
  picker.resetPicker();
  ctx.item = null; ctx.details = null; ctx.season = null; ctx.episode = null; ctx.episodes = [];
  closeDialog(overlay());
}

/* ── Open ────────────────────────────────────────────────────────────────── */

export async function openItem(item, opts = {}) {
  ctx.item = item;
  ctx.details = null;
  ctx.season = null;
  ctx.episode = null;
  ctx.episodes = [];
  ctx.forcePicker = !!opts.forcePicker;
  ctx.pendingSeason = opts.season || null;
  ctx.pendingEpisode = opts.episode || null;
  picker.resetPicker(opts.previousRelease || '');

  let hash = `#${item.media_type}/${item.tmdb_id}`;
  if (opts.season) {
    hash += `/s/${opts.season}`;
    if (opts.episode) hash += `/e/${opts.episode}`;
  }
  syncHash(hash);

  content().innerHTML = modalSkeleton();
  openDialog(overlay(), el('modal'), {
    label: item.title || 'Title details',
    // This modal's visibility is owned by the router: Escape navigates back,
    // which then closes the DOM AND restores the page scroll. Closing directly
    // would leave the URL pointing at a modal that is no longer on screen.
    onEscape: () => closeModalRoute(),
    onClose: () => { seasonsSeq.abort(); epsSeq.abort(); },
  });

  let details;
  try {
    details = await apiFetch(
      `/api/tmdb/${encodeURIComponent(item.tmdb_id)}/full?type=${encodeURIComponent(item.media_type)}`);
  } catch (e) {
    renderFallback(item, e.message);
    return;
  }
  if (!ctx.item || ctx.item.tmdb_id !== item.tmdb_id) return;   // superseded
  if (!details.poster_url && item.poster_url) details.poster_url = item.poster_url;
  ctx.details = details;
  renderRich(details);
}

function modalSkeleton() {
  return `<div class="modal-backdrop-wrap skeleton" aria-hidden="true"></div>
  <div class="modal-hero">
    <div class="modal-poster skeleton" aria-hidden="true"></div>
    <div class="modal-meta" style="flex:1">
      <div class="skeleton sk-line" style="width:55%;height:20px"></div>
      <div class="skeleton sk-line" style="width:35%"></div>
      <div class="skeleton sk-line" style="width:90%"></div>
      <div class="skeleton sk-line" style="width:80%"></div>
    </div>
  </div>
  <span class="sr-only">Loading title details…</span>`;
}

function renderFallback(item, reason) {
  content().innerHTML = `
    <h2 class="modal-fallback-title">${esc(item.title || 'Title')}</h2>
    <p class="modal-fallback-sub">${esc(item.year || '')} · ${item.media_type === 'tv' ? 'TV Series' : 'Movie'}</p>
    ${errorState(`Could not load details: ${reason}`, {retryAttr: 'data-modal-retry="1"'})}`;
}

/* ── Rich modal ──────────────────────────────────────────────────────────── */

function renderRich(d) {
  const inLib = state.libraryStatus[d.tmdb_id];
  const libItem = state.myLibrary.find(i => i.tmdb_id === d.tmdb_id && i.status === 'ready');
  const libHash = (libItem && libItem.info_hash) || '';

  let html = headerHTML(d) + castHTML(d.cast);
  if (d.media_type === 'movie') {
    html += similarHTML(d.similar, 'movie');
    html += inLibPanelHTML(d, inLib, libHash);
    html += movieRequestHTML(d, inLib);
  } else {
    html += `<div id="tv-content">${seasonsSkeleton()}</div>`;
  }
  content().innerHTML = html;
  focusFirst(el('modal'));

  if (d.media_type === 'movie' && ctx.forcePicker) {
    setTimeout(() => picker.togglePicker(
      {tmdbId: d.tmdb_id, mediaType: 'movie'}, {forceOpen: true}), 60);
  }
  if (d.media_type === 'tv') refreshSeasons();
}

function headerHTML(d) {
  const hasBackdrop = !!d.backdrop_url;
  const backdrop = hasBackdrop ? `<div class="modal-backdrop-wrap">
    <img class="modal-backdrop" src="${esc(d.backdrop_url)}" alt="">
    <div class="modal-backdrop-fade" aria-hidden="true"></div>
  </div>` : '';
  const poster = d.poster_url
    ? `<img class="modal-poster" src="${esc(d.poster_url)}" alt="">`
    : `<div class="modal-poster placeholder">${icon(d.media_type === 'tv' ? 'tv' : 'film')}</div>`;
  const runtime = d.runtime_minutes
    ? `${Math.floor(d.runtime_minutes / 60)}h ${d.runtime_minutes % 60}m` : '';
  const rating = Number(d.vote_average) > 0 ? `★ ${Number(d.vote_average).toFixed(1)}` : '';
  const genres = (d.genres || []).slice(0, 3).join(' · ');
  const chips = [d.year, runtime, rating, genres].filter(Boolean)
    .map(s => `<span class="meta-chip">${esc(s)}</span>`).join('');
  const tagline = d.tagline ? `<p class="tagline">${esc(d.tagline)}</p>` : '';
  const jf = jellyfinLinkHTML(d);
  return `${backdrop}
  <div class="modal-hero"${hasBackdrop ? '' : ' style="margin-top:0"'}>
    ${poster}
    <div class="modal-meta">
      <h2 id="modal-title">${esc(d.title)}</h2>${tagline}
      <div class="meta-row">${chips}
        <button class="copy-link-btn" type="button" data-copy-link="1"
          title="Copy a shareable link to this title">${icon('link')} Copy link</button>
        ${jf}
      </div>
      ${d.overview ? `<p class="meta-overview">${esc(d.overview)}</p>` : ''}
    </div>
  </div>`;
}

/** jellyfinLinkHTML — "Open in Jellyfin" on anything that is actually ready.
 *  jellyfin_url comes from GET /api/configured (contract §5); it is the LAN
 *  URL only and is empty when unset, in which case no link is drawn. */
function jellyfinLinkHTML(d) {
  const cfg = state.configured;
  if (!cfg || !cfg.jellyfin_url) return '';
  const info = state.libraryStatus[d.tmdb_id];
  if (!info || info.status !== 'ready') return '';
  const base = String(cfg.jellyfin_url).replace(/\/+$/, '');
  const href = safeUrl(base + '/');
  if (href === '#') return '';
  return `<a class="copy-link-btn" href="${href}" target="_blank" rel="noopener">
    ${icon('external')} Open in Jellyfin</a>`;
}

function castHTML(cast) {
  if (!cast || !cast.length) return '';
  const items = cast.slice(0, 6).map(c => {
    const img = c.profile_url
      ? `<img src="${esc(c.profile_url)}" loading="lazy" alt="">`
      : `<div class="cast-avatar" aria-hidden="true"></div>`;
    return `<div class="cast-card">${img}
      <div class="cast-name">${esc(c.name)}</div>
      <div class="cast-char">${esc(c.character)}</div></div>`;
  }).join('');
  return `<section class="modal-section"><h3 class="modal-section-title">Cast</h3>
    <div class="cast-strip">${items}</div></section>`;
}

function similarHTML(similar, mediaType) {
  if (!similar || !similar.length) return '';
  const groups = [];
  for (const s of similar) {
    const label = s.group || 'More Like This';
    let g = groups.length && groups[groups.length - 1].label === label ? groups[groups.length - 1] : null;
    if (!g) { g = {label, items: []}; groups.push(g); }
    g.items.push(s);
  }
  const card = s => {
    const type = s.media_type || mediaType;
    const img = s.poster_url
      ? `<img src="${esc(s.poster_url)}" loading="lazy" alt="">`
      : `<span class="mini-thumb" aria-hidden="true"></span>`;
    return `<button type="button" class="mini-card" data-nav-item="1"
        data-type="${esc(type === 'tv' ? 'tv' : 'movie')}" data-tmdb-id="${num(s.tmdb_id)}"
        aria-label="${esc(s.title)}">
      ${img}<span class="mini-card-title">${esc(s.title)}</span></button>`;
  };
  return groups.map(g =>
    `<section class="modal-section"><h3 class="modal-section-title">${esc(g.label)}</h3>
     <div class="mini-strip">${g.items.map(card).join('')}</div></section>`).join('');
}

function inLibPanelHTML(d, inLib, libHash) {
  if (ctx.forcePicker) return '';
  if (!inLib || inLib.status !== 'ready') return '';
  return `<div class="in-lib-panel">
    <h3>${icon('check')} In Library — ${esc(inLib.library || '')}</h3>
    <p>Already in Jellyfin. You can replace it with a different release, or remove it.</p>
    <div class="in-lib-actions">
      <button class="btn" type="button" data-mact="replace">${icon('refresh')} Replace</button>
      ${libHash ? `<button class="btn danger" type="button" data-mact="remove-movie"
        data-hash="${esc(libHash)}" data-title="${esc(d.title)}">${icon('trash')} Remove</button>` : ''}
    </div>
  </div>`;
}

function movieRequestHTML(d, inLib) {
  const heading = ctx.forcePicker ? 'Re-request'
    : (inLib && inLib.status === 'ready' ? 'Request to a different library' : 'Request');
  const reqLabel = state.user ? (ctx.forcePicker ? 'Re-request Movie' : 'Request Movie') : 'Sign in to request';
  const digitalWarn = (d.media_type === 'movie' && !d.digital_release && d.year)
    ? `<div class="callout warn">${icon('alert')}<div class="callout-body">
        No digital release on record yet — results may include pre-release fakes.</div></div>` : '';
  return `<div class="req-panel">
    <h3>${esc(heading)}</h3>
    ${prevNoteHTML()}
    ${digitalWarn}
    ${libSelectHTML('movie')}
    <div class="picker-row">
      <button class="btn" type="button" data-mact="toggle-picker">${icon('chevron-down')} Choose release</button>
      <span class="selected-label" id="sel-label" hidden></span>
    </div>
    <div id="release-picker-panel" hidden></div>
    <button class="req-btn${state.user ? '' : ' needs-auth'}" id="req-btn" type="button"
      data-action="movie" data-mact="request">${esc(reqLabel)}</button>
  </div>`;
}

function prevNoteHTML() {
  if (!ctx.forcePicker) return '';
  const prev = picker.previousRelease();
  if (prev) {
    return `<div class="callout info">${icon('info')}<div class="callout-body">
      Previous: <code>${esc(prev)}</code><br>Pick a different release below, or confirm to re-use it.</div></div>`;
  }
  return `<div class="callout info">${icon('info')}<div class="callout-body">
    Choose an alternative release below.</div></div>`;
}

function libSelectHTML(mediaType) {
  const libs = state.libraries.filter(l => l.type === mediaType);
  if (libs.length <= 1) return '';
  const opts = libs.map(l =>
    `<option value="${esc(l.name)}"${l.default ? ' selected' : ''}>${esc(l.name)}</option>`).join('');
  return `<div class="lib-select-row"><label for="lib-picker">Add to</label>
    <select class="lib-picker" id="lib-picker">${opts}</select></div>`;
}
function selectedLibrary() {
  const e = el('lib-picker');
  return e ? e.value : '';
}

/* ── Seasons ─────────────────────────────────────────────────────────────── */

function seasonsSkeleton() {
  return `<div class="seasons-grid">${Array(8).fill(0).map(() =>
    `<div class="season-item"><div class="skeleton" style="height:52px"></div>
     <div class="skeleton" style="height:26px"></div></div>`).join('')}</div>`;
}

async function refreshSeasons() {
  if (!ctx.item || ctx.item.media_type !== 'tv') return;
  const host = el('tv-content');
  if (!host) return;
  const token = seasonsSeq.next();
  let seasons;
  try {
    seasons = await apiFetch(
      `/api/tmdb/${encodeURIComponent(ctx.item.tmdb_id)}/seasons`, {signal: token.signal});
  } catch (e) {
    if (isAbort(e) || !seasonsSeq.isCurrent(token)) return;
    host.innerHTML = errorState(`Could not load seasons: ${e.message}`, {retryAttr: 'data-mact="retry-seasons"'});
    return;
  }
  if (!seasonsSeq.isCurrent(token)) return;
  renderSeasons(Array.isArray(seasons) ? seasons : []);
}

function renderSeasons(seasons) {
  const host = el('tv-content');
  if (!host) return;
  const d = ctx.details || {};
  const tmdbId = ctx.item.tmdb_id;
  const airing = !!d.is_airing;
  const showName = d.title || ctx.item.title || '';
  const showInLib = state.myLibrary.some(i => i.tmdb_id === tmdbId && i.media_type === 'tv');

  const airingBanner = airing
    ? `<div class="callout info">${icon('satellite')}<div class="callout-body">
        <div class="callout-title">Currently airing</div>
        <p>Subscribe to a season below and new episodes are grabbed automatically as they release.</p>
      </div></div>` : '';
  const seriesRemove = (showInLib && state.user)
    ? `<button class="btn danger series-remove-btn" type="button" data-mact="remove-series"
        data-tmdb-id="${num(tmdbId)}" data-title="${esc(showName)}">${icon('trash')} Remove entire series</button>` : '';
  const libSel = libSelectHTML('tv');

  let html = `${airingBanner}${seriesRemove}${libSel ? `<div class="lib-select-wrap">${libSel}</div>` : ''}
    <h3 class="modal-section-title">Select a season</h3>
    <div class="seasons-grid">`;

  for (const s of seasons) {
    const sn = num(s.season_number);
    const total = num(s.episode_count);
    const p = seasonProgress(tmdbId, sn, total);
    const ring = seasonRingStatus(tmdbId, sn);
    const pip = ring !== 'none'
      ? `<span class="season-pip" style="background:${RING_COLOR[ring]}" aria-hidden="true"></span>` : '';

    const remaining = Math.max(0, total - p.ready - p.pending);
    let btn;
    if (total > 0 && p.ready === total) {
      btn = `<button class="season-req-btn done" type="button" disabled>${icon('check')} Complete</button>`;
    } else if (remaining === 0 && p.pending > 0) {
      btn = `<button class="season-req-btn queued" type="button" disabled>${icon('clock')} Queued</button>`;
    } else {
      const label = p.requested === 0 ? 'Request All' : `Request Remaining (${remaining || '?'})`;
      btn = `<button class="season-req-btn" type="button" id="sreq-${sn}" data-mact="request-season"
        data-season="${sn}" data-total="${total}">${icon('download')} ${esc(label)}</button>`;
    }

    const sub = findSub(tmdbId, sn);
    let bell = '';
    if (airing || sub) {
      bell = `<button class="season-sub-btn${sub ? ' on' : ''}" type="button" data-mact="toggle-sub"
        data-season="${sn}" aria-pressed="${!!sub}"
        title="${sub ? 'Unsubscribe' : 'Auto-grab new episodes'}">
        ${icon(sub ? 'bell' : 'bell-off')} ${sub ? 'Subscribed' : 'Subscribe'}</button>`;
    }
    const seasonRemove = ((p.ready + p.stale) > 0 && state.user)
      ? `<button class="season-remove-btn" type="button" data-mact="remove-season"
          data-season="${sn}">${icon('trash')} Remove season</button>` : '';

    html += `<div class="season-item">
      <button class="season-btn${ctx.season === sn ? ' active' : ''}" type="button"
        data-mact="select-season" data-season="${sn}"
        aria-expanded="${ctx.season === sn}" aria-controls="eps-area">
        ${pip}Season ${sn}<span class="season-sub-count">${p.ready}/${total || '?'} ready</span>
      </button>
      ${btn}${bell}${seasonRemove}
    </div>`;
  }
  html += `</div><div id="eps-area"></div>`;
  host.innerHTML = html;

  // Apply a season/episode carried in on the route (the Retry path).
  if (ctx.pendingSeason) {
    const want = ctx.pendingSeason;
    const wantEp = ctx.pendingEpisode;
    ctx.pendingSeason = null;
    ctx.pendingEpisode = null;
    selectSeason(want, wantEp);
  } else if (ctx.season) {
    selectSeason(ctx.season, ctx.episode);
  }
}

/* ── Episodes ────────────────────────────────────────────────────────────── */

function epsSkeleton() {
  return `<div class="eps-list">${Array(8).fill(0).map(() =>
    `<div class="ep-row skeleton" aria-hidden="true"></div>`).join('')}</div>`;
}

async function selectSeason(n, autoEpisode) {
  ctx.season = n;
  ctx.episode = null;
  document.querySelectorAll('.season-btn').forEach(b => {
    const on = Number(b.dataset.season) === n;
    b.classList.toggle('active', on);
    b.setAttribute('aria-expanded', String(on));
  });
  const area = el('eps-area');
  if (!area) return;
  area.innerHTML = epsSkeleton();

  const token = epsSeq.next();
  let eps;
  try {
    eps = await apiFetch(
      `/api/tmdb/${encodeURIComponent(ctx.item.tmdb_id)}/seasons/${encodeURIComponent(n)}/episodes`,
      {signal: token.signal});
  } catch (e) {
    if (isAbort(e) || !epsSeq.isCurrent(token)) return;
    area.innerHTML = errorState(`Could not load episodes: ${e.message}`,
      {retryAttr: `data-mact="retry-episodes" data-season="${num(n)}"`, compact: true});
    return;
  }
  if (!epsSeq.isCurrent(token)) return;
  ctx.episodes = Array.isArray(eps) ? eps : [];
  renderEpisodes();
  if (autoEpisode) {
    toggleEpisode(autoEpisode, true);
    document.getElementById('ep-panel-' + autoEpisode)?.scrollIntoView({block: 'nearest'});
  }
}

function renderEpisodes() {
  const tmdbId = ctx.item.tmdb_id;
  let html = '<div class="eps-list">';
  for (const e of ctx.episodes) {
    const n = num(e.episode_number);
    const st = episodeStatus(tmdbId, ctx.season, n);
    const open = ctx.episode === n;
    const tag = st !== 'none'
      ? `<span class="ep-tag ep-${esc(st)}">${icon(RING_ICON[st])} ${esc(EP_LABEL[st])}</span>` : '';
    html += `<div class="ep-row ep-st-${esc(st)}${open ? ' active' : ''}" data-ep="${n}"
        role="button" tabindex="0" aria-expanded="${open}" aria-controls="ep-panel-${n}"
        aria-label="Episode ${n}: ${esc(e.name)}${st !== 'none' ? ' — ' + esc(EP_LABEL[st]) : ''}">
      <span class="ep-dot" style="background:${RING_COLOR[st]}" aria-hidden="true"></span>
      <span class="ep-num">E${String(n).padStart(2, '0')}</span>
      <span class="ep-name">${esc(e.name)}</span>
      ${tag}
      <span class="ep-chevron" aria-hidden="true">${icon('chevron-right')}</span>
      <span class="ep-date">${esc(e.air_date ? String(e.air_date).slice(0, 4) : '')}</span>
    </div>
    <div class="ep-panel" id="ep-panel-${n}"></div>`;
  }
  html += '</div>';
  el('eps-area').innerHTML = html;
}

function refreshEpisodeList() {
  if (!ctx.episodes.length) return;
  const open = ctx.episode;
  renderEpisodes();
  if (open) toggleEpisode(open, true);
}

function toggleEpisode(n, forceOpen) {
  const panel = document.getElementById('ep-panel-' + n);
  if (!panel) return;
  const wasOpen = panel.classList.contains('open');
  document.querySelectorAll('.ep-panel').forEach(p => { p.classList.remove('open'); p.innerHTML = ''; });
  document.querySelectorAll('.ep-row').forEach(r => {
    r.classList.remove('active');
    r.setAttribute('aria-expanded', 'false');
  });
  if (wasOpen && !forceOpen) { ctx.episode = null; picker.resetPicker(); return; }

  ctx.episode = n;
  const row = document.querySelector(`.ep-row[data-ep="${n}"]`);
  if (row) { row.classList.add('active'); row.setAttribute('aria-expanded', 'true'); }

  const ep = ctx.episodes.find(x => num(x.episode_number) === n) || {};
  const lib = state.myLibrary.find(i =>
    i.tmdb_id === ctx.item.tmdb_id && i.media_type === 'tv' && i.season === ctx.season && i.episode === n);
  picker.resetPicker(lib ? (lib.release_title || '') : '');

  panel.innerHTML = episodePanelHTML(n, ep.name || '');
  panel.classList.add('open');

  if (lib || ctx.forcePicker) {
    setTimeout(() => picker.togglePicker(
      {tmdbId: ctx.item.tmdb_id, mediaType: 'tv', season: ctx.season, episode: n},
      {forceOpen: true}), 50);
  }
}

function episodePanelHTML(n, name) {
  const st = episodeStatus(ctx.item.tmdb_id, ctx.season, n);
  const inLib = st === 'ready' || st === 'stale';
  const epLbl = `S${String(ctx.season).padStart(2, '0')}E${String(n).padStart(2, '0')}`;
  const reqLabel = !state.user ? 'Sign in to request' : (inLib ? 'Re-request' : 'Request Episode');
  const statusLine = st !== 'none'
    ? `<span class="ep-inline-status ep-${esc(st)}">
        <span class="ep-dot" style="background:${RING_COLOR[st]}" aria-hidden="true"></span>
        ${icon(RING_ICON[st])} ${esc(EP_LABEL[st])}</span>` : '';
  const removeBtn = (inLib && state.user)
    ? `<button class="btn danger ep-remove-btn" type="button" data-mact="remove-episode"
        data-ep="${n}">${icon('trash')} Remove</button>` : '';
  const prevNote = (inLib && state.user)
    ? `<div class="callout info">${icon('info')}<div class="callout-body">
        Already in your library — pick a different release below, or re-request to refresh it.</div></div>` : '';
  return `<div class="ep-inline-panel">
    <div class="ep-inline-head"><strong>${esc(epLbl)}</strong> · ${esc(name)} ${statusLine}</div>
    ${prevNote}
    <div class="picker-row">
      <button class="btn sm" type="button" data-mact="toggle-picker" data-ep="${n}">
        ${icon('chevron-down')} Choose release</button>
      <span class="selected-label" id="sel-label" hidden></span>
    </div>
    <div id="release-picker-panel" hidden></div>
    <div class="ep-inline-actions">
      <button class="req-btn${state.user ? '' : ' needs-auth'}" id="req-btn" type="button"
        data-action="episode" data-mact="request">${esc(reqLabel)}</button>
      ${removeBtn}
    </div>
  </div>`;
}

/* ── Click routing ───────────────────────────────────────────────────────── */

function onContentClick(e) {
  const nav = e.target.closest('[data-nav-item]');
  if (nav) { navItem(nav.dataset.type, Number(nav.dataset.tmdbId)); return; }
  if (e.target.closest('[data-copy-link]')) { copyLink(); return; }
  if (e.target.closest('[data-modal-retry]')) { if (ctx.item) openItem(ctx.item, {}); return; }

  const row = e.target.closest('.ep-row');
  if (row && !e.target.closest('button')) { toggleEpisode(Number(row.dataset.ep)); return; }

  const act = e.target.closest('[data-mact]');
  if (!act) return;
  const season = Number(act.dataset.season);
  switch (act.dataset.mact) {
    case 'toggle-picker':
      picker.togglePicker(act.dataset.ep
        ? {tmdbId: ctx.item.tmdb_id, mediaType: 'tv', season: ctx.season, episode: Number(act.dataset.ep)}
        : {tmdbId: ctx.item.tmdb_id, mediaType: ctx.item.media_type});
      break;
    case 'request':        requestIt(); break;
    case 'request-season': requestSeason(season, Number(act.dataset.total)); break;
    case 'select-season':  selectSeason(season); break;
    case 'toggle-sub':     toggleSubscription(season); break;
    case 'replace':        document.querySelector('.in-lib-panel')?.remove(); break;
    case 'remove-movie':   removeLibItem(act.dataset.hash, act.dataset.title); break;
    case 'remove-series':  removeSeries(Number(act.dataset.tmdbId), act.dataset.title); break;
    case 'remove-season':  removeSeason(ctx.item.tmdb_id, season); break;
    case 'remove-episode':
      removeEpisode(ctx.item.tmdb_id, ctx.season, Number(act.dataset.ep))
        .then(ok => { if (ok) refreshEpisodeList(); });
      break;
    case 'retry-seasons':  refreshSeasons(); break;
    case 'retry-episodes': selectSeason(season); break;
  }
}

function copyLink() {
  const url = location.href;
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(url)
      .then(() => toast('Link copied'), () => fallbackCopy(url));
    return;
  }
  fallbackCopy(url);
}
function fallbackCopy(url) {
  // Non-secure contexts (plain http on the LAN) have no clipboard API at all.
  const ta = document.createElement('textarea');
  ta.value = url;
  ta.setAttribute('readonly', '');
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
  document.body.removeChild(ta);
  toast(ok ? 'Link copied' : 'Copy failed — select the address bar instead', {ok});
}

/* ── Requesting ──────────────────────────────────────────────────────────── */

/* requireLogin RUNS its callback immediately when the user is already signed in
   (auth.js:47). Passing the guarded function itself made these recurse into
   themselves until the stack blew, and because they are async the RangeError
   surfaced as an unhandled rejection — so for a SIGNED-IN user the Request,
   Request Season and Subscribe buttons threw and did nothing visible. Guard and
   work are now separate functions. See the same note in library.js. */

function requestIt() { requireLogin(() => doRequestIt()); }

async function doRequestIt() {
  const btn = el('req-btn');
  if (!btn) return;
  btn.disabled = true;
  btn.textContent = 'Adding to queue…';

  const d = ctx.details || {};
  const body = {
    tmdb_id: ctx.item.tmdb_id,
    type: ctx.item.media_type,
    library: selectedLibrary(),
    title: d.title || ctx.item.title || '',
    year: d.year || ctx.item.year || '',
    poster_url: d.poster_url || ctx.item.poster_url || '',
  };
  if (ctx.item.media_type === 'tv') { body.season = ctx.season; body.episode = ctx.episode; }
  const magnet = picker.selectedMagnet();
  if (magnet) body.magnet = magnet;

  let data;
  try {
    data = await apiFetch('/request', {
      method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body),
    });
  } catch (e) {
    btn.disabled = false;
    if (e.status === 401) { btn.textContent = 'Sign in to request'; return; }
    btn.classList.add('fail');
    btn.textContent = e.message;
    toast(e.message, {ok: false});
    return;
  }

  btn.classList.add('success');
  if (data && data.status === 'ready') {
    btn.textContent = 'Already in library';
    toast(`“${(data && data.title) || 'Item'}” is already available`);
    state.libraryStatus[ctx.item.tmdb_id] = {status: 'ready', title: data.title};
  } else {
    const already = data && data.already;
    btn.textContent = already ? 'Already queued' : 'Added to queue';
    toast(`“${(data && data.title) || 'Item'}” ${already ? 'is already queued' : 'added to queue'}`, {
      action: {label: 'View queue', onClick: () => { closeModalRoute(); location.hash = 'queue'; }},
    });
    state.libraryStatus[ctx.item.tmdb_id] = {status: (data && data.status) || 'pending', title: data && data.title};
  }
  await pollQueue();
  if (ctx.item.media_type === 'tv' && ctx.episode) {
    const row = document.querySelector(`.ep-row[data-ep="${ctx.episode}"]`);
    const dot = row && row.querySelector('.ep-dot');
    if (dot) dot.style.background = RING_COLOR.pending;
  }
  setTimeout(() => loadMyLibrary().catch(() => {}), 8000);
}

function requestSeason(n, total) { requireLogin(() => doRequestSeason(n, total)); }

async function doRequestSeason(n, total) {
  const btn = document.getElementById('sreq-' + n);
  const restore = btn ? btn.innerHTML : '';
  if (btn) { btn.disabled = true; btn.textContent = 'Queuing…'; }

  const d = ctx.details || {};
  const name = d.title || ctx.item.title || '';
  let data;
  try {
    data = await apiFetch('/request/season', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({
        tmdb_id: ctx.item.tmdb_id, season: n, library: selectedLibrary(),
        title: name, year: d.year || ctx.item.year || '',
        poster_url: d.poster_url || ctx.item.poster_url || '',
      }),
    });
  } catch (e) {
    if (btn) { btn.disabled = false; btn.innerHTML = restore; }
    if (e.status !== 401) toast(e.message, {ok: false});
    return;
  }

  const enqueued = num(data && data.enqueued);
  const skipped = num(data && data.skipped);
  if (enqueued === 0) {
    toast(skipped
      ? `Nothing to request — all ${skipped} episode${skipped === 1 ? ' is' : 's are'} already in the library or queue`
      : 'Nothing to request — this season has no episodes to fetch');
  } else {
    toast(`${enqueued} episode${enqueued === 1 ? '' : 's'} queued for ${name}${skipped ? ` (${skipped} already present)` : ''}`, {
      action: {label: 'View queue', onClick: () => { closeModalRoute(); location.hash = 'queue'; }},
    });
  }
  // Two toasts in a row here is exactly the case that used to clobber itself.
  if (data && data.subscribed) {
    toast(`Subscribed — new episodes of ${name} will be grabbed automatically`);
  }
  await pollQueue();
  await loadSubscriptions();
  refreshSeasons();
}

function toggleSubscription(season) { requireLogin(() => doToggleSubscription(season)); }

async function doToggleSubscription(season) {
  const d = ctx.details || {};
  const title = d.title || ctx.item.title || '';
  const existing = findSub(ctx.item.tmdb_id, season);
  try {
    if (existing) {
      await apiFetch(`/api/subscriptions/${encodeURIComponent(existing.id)}`, {method: 'DELETE'});
      toast(`Unsubscribed from ${title} S${season}`);
    } else {
      await apiFetch('/api/subscriptions', {
        method: 'POST', headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({
          tmdb_id: ctx.item.tmdb_id, season, title,
          poster_url: d.poster_url || ctx.item.poster_url || '', library: selectedLibrary(),
        }),
      });
      toast(`Subscribed — new episodes of ${title} S${season} will be grabbed automatically`);
    }
  } catch (e) {
    if (e.status !== 401) toast(`Subscription update failed: ${e.message}`, {ok: false});
    return;
  }
  await loadSubscriptions();
  refreshSeasons();
}
