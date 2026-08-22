/* ==========================================================================
   Cinematic home hero.

   The decode-before-swap logic is deliberately preserved: the backdrop and the
   text are committed TOGETHER, gated on the new image being fully decoded, so
   a fast dot-click can never leave the title describing the previous image.
   What changed: the rotate timer now stops entirely while the tab is hidden
   instead of ticking and skipping, and the dots are real tab controls.
   ========================================================================== */

import {apiFetch, esc} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {navItem} from './router.js';

let heroItems = [];
let heroIdx = 0;
let heroTimer = null;
let visHandler = null;

function el() { return document.getElementById('hero'); }

export async function loadHero() {
  const host = el();
  if (!host) return;
  let trending;
  try {
    trending = await apiFetch('/api/browse/trending');
  } catch (e) {
    console.warn('[jf] hero unavailable:', e.message);
    hideHero();
    return;
  }
  const picks = (Array.isArray(trending) ? trending : []).filter(t => t.poster_url).slice(0, 5);
  if (!picks.length) { hideHero(); return; }

  const full = await Promise.all(picks.map(t =>
    apiFetch(`/api/tmdb/${encodeURIComponent(t.tmdb_id)}/full?type=${encodeURIComponent(t.media_type)}`)
      .catch(() => null)));
  heroItems = full.filter(d => d && d.backdrop_url);
  if (!heroItems.length) { hideHero(); return; }

  host.innerHTML = `
    <div class="hero-bg" id="hero-bg" aria-hidden="true"></div>
    <div class="hero-inner">
      <p class="hero-eyebrow">${icon('flame')} <b>Trending</b> Now</p>
      <h1 class="hero-title" id="hero-title"></h1>
      <div class="hero-meta" id="hero-meta"></div>
      <p class="hero-overview" id="hero-overview"></p>
      <div class="hero-actions" id="hero-actions"></div>
      <div class="hero-dots" id="hero-dots" role="tablist" aria-label="Featured titles"></div>
    </div>`;
  host.dataset.ready = '1';
  host.hidden = false;

  heroItems.forEach(d => { const i = new Image(); i.src = d.backdrop_url; });
  wire(host);
  renderHero(0);
  startRotate();
  document.dispatchEvent(new CustomEvent('jf:layout'));
}

function hideHero() {
  const host = el();
  if (host) { host.hidden = true; host.dataset.ready = '0'; }
  stopRotate();
}

function wire(host) {
  if (host.dataset.wired === '1') return;
  host.dataset.wired = '1';
  host.addEventListener('click', e => {
    const dot = e.target.closest('[data-hero-dot]');
    if (dot) { renderHero(Number(dot.dataset.heroDot)); startRotate(); return; }
    const go = e.target.closest('[data-hero-nav]');
    if (go) navItem(go.dataset.type, Number(go.dataset.tmdbId));
  });
  host.addEventListener('keydown', e => {
    if (!e.target.closest('[data-hero-dot]')) return;
    if (e.key === 'ArrowRight') { e.preventDefault(); renderHero(heroIdx + 1); startRotate(); }
    if (e.key === 'ArrowLeft')  { e.preventDefault(); renderHero(heroIdx - 1); startRotate(); }
  });
}

function renderHero(i) {
  if (!heroItems.length) return;
  const idx = (i + heroItems.length) % heroItems.length;
  const d = heroItems[idx];
  const bg = document.getElementById('hero-bg');
  if (!bg) return;

  const img = new Image();
  img.alt = '';
  img.src = d.backdrop_url;

  // Commit the backdrop and the text together, only once the new backdrop is
  // fully DECODED. `d` is captured here so a fast dot-click cannot desync them.
  const commit = () => {
    if (heroIdx === idx && bg.querySelector('img.show')) return;
    bg.appendChild(img);
    requestAnimationFrame(() => img.classList.add('show'));
    bg.querySelectorAll('img').forEach(other => {
      if (other !== img) { other.classList.remove('show'); setTimeout(() => other.remove(), 600); }
    });
    heroIdx = idx;
    paintText(d);
    paintDots();
  };
  const skip = () => { heroIdx = idx; };   // broken artwork: move on
  if (img.decode) img.decode().then(commit).catch(skip);
  else { img.onload = commit; img.onerror = skip; }
}

function paintText(d) {
  document.getElementById('hero-title').textContent = d.title || '';
  const genres = (d.genres || []).slice(0, 3).map(g => `<span class="hero-chip">${esc(g)}</span>`).join('');
  const rating = Number(d.vote_average) > 0
    ? `<span class="hero-rate">${icon('flame')} ${Number(d.vote_average).toFixed(1)}</span>` : '';
  const runtime = d.runtime_minutes
    ? `${Math.floor(d.runtime_minutes / 60)}h ${d.runtime_minutes % 60}m` : '';
  const bits = [
    rating,
    d.year ? `<span>${esc(d.year)}</span>` : '',
    `<span class="hero-chip">${d.media_type === 'tv' ? 'Series' : 'Movie'}</span>`,
    runtime ? `<span>${esc(runtime)}</span>` : '',
    genres,
  ].filter(Boolean);
  document.getElementById('hero-meta').innerHTML = bits.join('<span class="dotsep" aria-hidden="true"></span>');
  document.getElementById('hero-overview').textContent = d.overview || '';

  const type = d.media_type === 'tv' ? 'tv' : 'movie';
  const id = Number(d.tmdb_id) || 0;
  const cta = type === 'tv' ? `${icon('play')} Browse Episodes` : `${icon('plus')} Request`;
  document.getElementById('hero-actions').innerHTML =
    `<button type="button" class="hero-btn primary" data-hero-nav="1" data-type="${type}" data-tmdb-id="${id}">${cta}</button>
     <button type="button" class="hero-btn ghost" data-hero-nav="1" data-type="${type}" data-tmdb-id="${id}">${icon('info')} Details</button>`;
}

function paintDots() {
  document.getElementById('hero-dots').innerHTML = heroItems.map((d, n) =>
    `<button type="button" role="tab" class="hero-dot${n === heroIdx ? ' active' : ''}"
       data-hero-dot="${n}" aria-selected="${n === heroIdx}"
       aria-label="Featured ${n + 1} of ${heroItems.length}: ${esc(d.title || '')}"></button>`).join('');
}

function startRotate() {
  stopRotate();
  heroTimer = setInterval(() => renderHero(heroIdx + 1), 7000);
  if (!visHandler) {
    visHandler = () => { if (document.hidden) stopRotate(); else if (heroItems.length) startRotate(); };
    document.addEventListener('visibilitychange', visHandler);
  }
}
function stopRotate() {
  if (heroTimer) { clearInterval(heroTimer); heroTimer = null; }
}
export function heroReady() { return heroItems.length > 0; }
