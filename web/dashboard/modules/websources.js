/* ==========================================================================
   Links section: paste a video page URL and turn it into a Jellyfin entry.

   The flow is deliberately two-step — preview, then add — and the reason is
   that extraction is the ONLY way to find out whether a link is supported at
   all, what the video is called, and whether it is a video rather than a
   playlist or a live stream. Doing that first means a bad link produces a
   message, and a good one produces an entry whose title the user has seen and
   can change, instead of a file appearing in their library under a name the
   extractor chose.

   Nothing here ever receives a media URL. The server resolves one at play
   time and keeps it in memory; the browser gets a page URL, a title and a
   thumbnail. See cmd/orchestrator/websources.go.
   ========================================================================== */

import {apiFetch, esc, safeUrl, toast, errorState, relTime} from '../../shared/api.js';
import {icon} from '../../shared/icons.js';

// The last previewed result, held so Add can send the exact URL that was
// previewed rather than re-reading the input — which the user may have edited
// in between, which would add something they never saw.
let previewed = null;

export function initWebSources() {
  const section = document.getElementById('section-websources');

  section.addEventListener('click', e => {
    if (e.target.closest('[data-ws-preview]')) { preview(); return; }
    if (e.target.closest('[data-ws-add]'))     { add(); return; }
    if (e.target.closest('[data-ws-refresh]')) { refreshWebSources(); return; }
    const del = e.target.closest('[data-ws-delete]');
    if (del) remove(del.dataset.wsDelete, del.dataset.title || '');
  });

  // Enter in the URL field previews rather than submitting anything — the
  // cheap, reversible action is the one a stray keypress should reach.
  section.querySelector('#ws-url').addEventListener('keydown', e => {
    if (e.key === 'Enter') { e.preventDefault(); preview(); }
  });
}

export async function refreshWebSources() {
  await Promise.all([fetchStatus(), fetchList()]);
}

/* ── Availability ─────────────────────────────────────────────────────────
   Asked before the form is usable. A box whose yt-dlp is missing shows one
   sentence explaining that, instead of a button that fails on click. */
async function fetchStatus() {
  const box = document.getElementById('ws-status');
  const form = document.getElementById('ws-form');
  let s;
  try {
    s = await apiFetch('/api/websources/status');
  } catch (e) {
    box.innerHTML = errorState(e.message, {retryAttr: 'data-ws-refresh="1"', compact: true});
    return;
  }
  if (s.enabled) {
    form.hidden = false;
    box.innerHTML = `
      <div class="callout good">${icon('shield')}
        <div class="callout-body">
          <div class="callout-title">Ready — everything goes through the VPN</div>
          <p>Links are extracted and streamed inside the tunnel via
             <code>${esc(s.proxy || '')}</code>, so the site never sees your home address.
             ${s.ytdlp_version ? `yt-dlp <code>${esc(s.ytdlp_version)}</code>.` : ''}</p>
        </div></div>`;
    return;
  }
  form.hidden = true;
  box.innerHTML = `
    <div class="callout warn">${icon('alert')}
      <div class="callout-body">
        <div class="callout-title">Unavailable</div>
        <p>${esc(s.reason || 'Web sources are not configured.')}</p>
      </div></div>`;
}

/* ── Preview ──────────────────────────────────────────────────────────── */
async function preview() {
  const url = document.getElementById('ws-url').value.trim();
  const out = document.getElementById('ws-preview');
  if (!url) { toast('Paste a link first', {ok: false}); return; }

  previewed = null;
  out.innerHTML = `<p class="hint">${icon('refresh')} Extracting — this runs over the VPN and can take a few seconds…</p>`;
  let p;
  try {
    p = await apiFetch('/api/websources/preview', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({url}),
    });
  } catch (e) {
    out.innerHTML = `<div class="callout err">${icon('alert')}
      <div class="callout-body"><div class="callout-title">Could not read that link</div>
      <p>${esc(e.message)}</p></div></div>`;
    return;
  }
  previewed = p;

  // safeUrl on the thumbnail: it is an absolute URL chosen by a third-party
  // site and is about to become an img src. It returns an ALREADY-ESCAPED
  // string (or '#'), so it must not be passed through esc() again, and the
  // presence check has to be on the raw value rather than on '#'.
  const thumb = p.thumbnail_url ? safeUrl(p.thumbnail_url) : '';
  out.innerHTML = `
    <div class="ws-preview">
      ${thumb && thumb !== '#' ? `<img class="ws-thumb" src="${thumb}" alt="" loading="lazy" referrerpolicy="no-referrer">` : ''}
      <div class="ws-preview-body">
        <label class="sr-only" for="ws-title">Title</label>
        <input type="text" id="ws-title" value="${esc(p.title || '')}" maxlength="200">
        <div class="ws-meta">
          ${p.uploader ? `<span>${esc(p.uploader)}</span>` : ''}
          ${p.duration_seconds ? `<span>${esc(fmtDuration(p.duration_seconds))}</span>` : ''}
          ${p.height ? `<span>${esc(String(p.height))}p</span>` : ''}
          ${p.ext ? `<span>${esc(p.ext)}</span>` : ''}
          ${p.extractor ? `<span>${esc(p.extractor)}</span>` : ''}
        </div>
        ${p.already_added ? `<p class="hint">${icon('info')} This link is already in the library — adding it again will refresh it.</p>` : ''}
        <div class="ws-actions">
          <label class="sr-only" for="ws-library">Library</label>
          <input type="text" id="ws-library" placeholder="library (optional — the default movie library)">
          <button class="btn primary" type="button" data-ws-add="1">${icon('plus')} Add to library</button>
        </div>
      </div>
    </div>`;
}

/* ── Add ──────────────────────────────────────────────────────────────── */
async function add() {
  if (!previewed) { toast('Preview the link first', {ok: false}); return; }
  const title = (document.getElementById('ws-title')?.value || '').trim();
  const library = (document.getElementById('ws-library')?.value || '').trim();
  try {
    const r = await apiFetch('/api/websources', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({url: previewed.page_url, title, library}),
    });
    toast(`Added “${r.title}” to ${r.library}`);
  } catch (e) {
    toast(e.message, {ok: false});
    return;
  }
  previewed = null;
  document.getElementById('ws-url').value = '';
  document.getElementById('ws-preview').innerHTML = '';
  fetchList();
}

/* ── The list ─────────────────────────────────────────────────────────── */
async function fetchList() {
  const box = document.getElementById('ws-list');
  let list;
  try {
    list = await apiFetch('/api/websources');
  } catch (e) {
    box.innerHTML = errorState(e.message, {retryAttr: 'data-ws-refresh="1"', compact: true});
    return;
  }
  if (!list.length) {
    box.innerHTML = `<p class="hint">No links yet.</p>`;
    return;
  }
  box.innerHTML = list.map(renderRow).join('');
}

/* A link dies in ways a torrent does not — the uploader deletes it, the site
   changes its player, the extractor breaks — and all three look identical from
   Jellyfin. The row leads with which one happened, because that is the only
   place the difference is ever written down. */
function renderRow(s) {
  const broken = !!s.last_error;
  const meta = [
    s.uploader,
    s.duration_seconds ? fmtDuration(s.duration_seconds) : '',
    s.extractor,
    s.added_by ? `added by ${s.added_by}` : '',
  ].filter(Boolean).map(x => esc(x)).join(' · ');

  return `
    <div class="vpn-cfg${broken ? ' ws-broken' : ''}">
      <div class="vpn-cfg-main">
        <div>
          <div class="vpn-cfg-name">${esc(s.title || s.page_url)}</div>
          <div class="vpn-cfg-meta">${meta}</div>
          <div class="vpn-cfg-meta mono">${esc(s.page_url)}</div>
          ${broken
            ? `<div class="ws-error">${icon('alert')} ${esc(s.last_error)}
                 ${s.last_ok ? ` — last worked ${esc(relTime(s.last_ok))}` : ''}</div>`
            : (s.last_ok ? `<div class="vpn-cfg-meta">${icon('check')} played ${esc(relTime(s.last_ok))}</div>` : '')}
        </div>
      </div>
      <div class="vpn-cfg-actions">
        <a class="btn" href="${safeUrl(s.page_url)}" target="_blank" rel="noopener noreferrer">${icon('external')} Open</a>
        <button class="btn danger" type="button" data-ws-delete="${esc(s.id)}" data-title="${esc(s.title || '')}">${icon('trash')} Remove</button>
      </div>
    </div>`;
}

async function remove(id, title) {
  if (!confirm(`Remove “${title || id}”?\n\nThis deletes the library entry and its .strm file.`)) return;
  try {
    await apiFetch('/api/websources/' + encodeURIComponent(id), {method: 'DELETE'});
    toast('Removed');
  } catch (e) {
    toast(e.message, {ok: false});
    return;
  }
  fetchList();
}

function fmtDuration(total) {
  const s = Math.max(0, Math.floor(total));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  const pad = n => String(n).padStart(2, '0');
  return h ? `${h}:${pad(m)}:${pad(sec)}` : `${m}:${pad(sec)}`;
}
