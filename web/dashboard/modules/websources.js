/* ==========================================================================
   Links section: paste video page addresses and turn them into Jellyfin
   entries.

   The flow is deliberately two-step — read the links, then add them — and the
   reason is that extraction is the ONLY way to find out whether a link is
   supported at all, what the video is called, and whether it is a video rather
   than a playlist or a live stream. Doing that first means a bad link produces
   a message, and a good one produces an entry whose title the user has seen
   and can change, instead of a file appearing in their library under a name
   the extractor chose.

   It takes MANY links at once because that is how the feature is actually
   used: a session's worth of pages, pasted in one go. They are read a couple
   at a time in the background while you keep pasting, and the library is
   chosen once, at the end, for the whole batch.

   Nothing here ever receives a media URL. The server resolves one at play
   time and keeps it in memory; the browser gets a page URL, a title and a
   thumbnail. See cmd/orchestrator/websources.go.
   ========================================================================== */

import {apiFetch, esc, safeUrl, toast, errorState, relTime} from '../../shared/api.js';
import {icon} from '../../shared/icons.js';

// How many extractions run at once. Each one is a yt-dlp process making its
// own requests through the single VPN proxy, so this is deliberately small:
// the tunnel is the bottleneck, and twenty parallel extractions would be
// slower than two as well as looking a great deal like a scraper.
const MAX_CONCURRENT = 2;

// A ceiling on the queue, so a stray paste of a whole page of text cannot turn
// into hundreds of extractions. It is a limit on what is QUEUED, not on what
// can be added over time.
const MAX_QUEUE = 100;

/* Each entry: {key, url, state, data, error, title}
   state: 'queued' | 'reading' | 'ready' | 'failed' | 'adding' | 'added' */
let queue = [];
let running = 0;
let keySeq = 1;
let libraries = [];

export function initWebSources() {
  const section = document.getElementById('section-websources');

  section.addEventListener('click', e => {
    if (e.target.closest('[data-ws-queue]'))   { enqueueFromInput(); return; }
    if (e.target.closest('[data-ws-add-all]')) { addAll(); return; }
    if (e.target.closest('[data-ws-clear]'))   { clearQueue(); return; }
    if (e.target.closest('[data-ws-refresh]')) { refreshWebSources(); return; }
    const retry = e.target.closest('[data-ws-retry]');
    if (retry) { retryOne(retry.dataset.wsRetry); return; }
    const drop = e.target.closest('[data-ws-drop]');
    if (drop) { dropOne(drop.dataset.wsDrop); return; }
    const del = e.target.closest('[data-ws-delete]');
    if (del) remove(del.dataset.wsDelete, del.dataset.title || '');
  });

  // Titles are edited in place, so they live on the queue entry rather than in
  // the DOM. A row can be re-rendered underneath the user at any moment when
  // its extraction finishes, and an edit that only existed in an input would
  // be lost when that happened.
  section.addEventListener('input', e => {
    const box = e.target.closest('[data-ws-title-for]');
    if (!box) return;
    const item = queue.find(q => q.key === box.dataset.wsTitleFor);
    if (item) item.title = e.target.value;
  });

  // Enter inserts a newline in a textarea, which is what pasting a list wants.
  // Ctrl/Cmd+Enter is the submit, and it reaches the cheap, reversible action:
  // queueing reads links, it does not add anything to the library.
  section.querySelector('#ws-url').addEventListener('keydown', e => {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); enqueueFromInput(); }
  });
}

export async function refreshWebSources() {
  await Promise.all([fetchStatus(), fetchList(), loadLibraries()]);
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

/* ── The library chooser ──────────────────────────────────────────────────
   Typed by hand until now, which meant a mistyped name was refused with the
   same "unknown library" the server gives for one you may not use — correct,
   deliberately indistinguishable, and useless as a spelling correction. A
   list cannot be mistyped.

   Movie-type libraries only: a web source is a single video with no season or
   episode, and the server refuses anything else. Offering a TV library here
   would be offering a choice that always fails. */
async function loadLibraries() {
  const sel = document.getElementById('ws-library');
  if (!sel) return;
  try {
    libraries = (await apiFetch('/api/libraries')).filter(l => l.type === 'movie');
  } catch {
    libraries = [];
  }
  if (!libraries.length) {
    sel.innerHTML = `<option value="">the default movie library</option>`;
    return;
  }
  sel.innerHTML = libraries.map(l =>
    `<option value="${esc(l.name)}"${l.default ? ' selected' : ''}>${esc(l.name)}</option>`).join('');
}

/* Only our own relay path is renderable as an image source. Anything else — in particular
   a leftover third-party URL from an older server — is dropped rather than fetched. */
function ownThumbPath(u) {
  return typeof u === 'string' && /^\/api\/websources\/[A-Za-z0-9_%-]+\/thumbnail$/.test(u);
}

/* ── Queueing ─────────────────────────────────────────────────────────────
   Split on whitespace and commas. NOT on colons, however tempting a separator
   they look: every URL contains "://" and splitting there would shred the
   input into halves that are not links at all. */
function parseLinks(text) {
  return text.split(/[\s,]+/).map(s => s.trim()).filter(Boolean);
}

function enqueueFromInput() {
  const input = document.getElementById('ws-url');
  const links = parseLinks(input.value);
  if (!links.length) { toast('Paste at least one link first', {ok: false}); return; }

  let added = 0, dupes = 0, bad = 0, over = 0;
  for (const url of links) {
    if (!/^https?:\/\//i.test(url)) { bad++; continue; }
    // Deduplicated against what is ALREADY in the queue. The same page pasted
    // twice in one batch is a slip, not an instruction to extract it twice.
    if (queue.some(q => q.url === url)) { dupes++; continue; }
    if (queue.length >= MAX_QUEUE) { over++; continue; }
    queue.push({key: 'q' + (keySeq++), url, state: 'queued', data: null, error: '', title: ''});
    added++;
  }

  input.value = '';
  renderQueue();
  pump();

  const notes = [];
  if (added) notes.push(`${added} queued`);
  if (dupes) notes.push(`${dupes} already in the list`);
  if (bad)   notes.push(`${bad} not a link`);
  if (over)  notes.push(`${over} over the ${MAX_QUEUE} limit`);
  toast(notes.join(' · ') || 'Nothing to queue', {ok: added > 0});
}

/* Start extractions until the concurrency budget is used up. Called after
   every queue change and after every completion, so it is the single place
   that decides what runs next. */
function pump() {
  while (running < MAX_CONCURRENT) {
    const next = queue.find(q => q.state === 'queued');
    if (!next) return;
    read(next);
  }
}

async function read(item) {
  item.state = 'reading';
  running++;
  renderRow(item);
  try {
    item.data = await apiFetch('/api/websources/preview', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({url: item.url}),
    });
    // Only seed the title on the first successful read. A retry must not
    // overwrite a title the user has since edited.
    if (!item.title) item.title = item.data.title || '';
    item.state = 'ready';
    item.error = '';
  } catch (e) {
    item.state = 'failed';
    item.error = e.message;
  } finally {
    running--;
    renderRow(item);
    renderActions();
    pump();
  }
}

function retryOne(key) {
  const item = queue.find(q => q.key === key);
  if (!item || item.state === 'reading' || item.state === 'adding') return;
  item.state = 'queued';
  item.error = '';
  renderRow(item);
  renderActions();
  pump();
}

function dropOne(key) {
  const item = queue.find(q => q.key === key);
  // A row being read or added is left alone: the request is already in flight
  // and removing the row would not stop it, it would only hide what happened.
  if (!item || item.state === 'reading' || item.state === 'adding') return;
  queue = queue.filter(q => q.key !== key);
  renderQueue();
}

function clearQueue() {
  const busy = queue.some(q => q.state === 'reading' || q.state === 'adding');
  if (busy && !confirm('Some links are still being read. Clear the list anyway?')) return;
  queue = queue.filter(q => q.state === 'reading' || q.state === 'adding');
  renderQueue();
}

/* ── Adding ───────────────────────────────────────────────────────────────
   One library for the whole batch, chosen once. Added sequentially rather
   than in parallel: the server re-extracts on add (that is what proves the
   link still works before an entry is written), so this is the same load as
   the reading pass and deserves the same restraint. */
async function addAll() {
  const ready = queue.filter(q => q.state === 'ready');
  if (!ready.length) { toast('Nothing ready to add', {ok: false}); return; }
  const library = document.getElementById('ws-library')?.value || '';

  let ok = 0, failed = 0;
  for (const item of ready) {
    item.state = 'adding';
    renderRow(item);
    try {
      const r = await apiFetch('/api/websources', {
        method: 'POST', headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({url: item.data.page_url, title: item.title.trim(), library}),
      });
      item.state = 'added';
      item.addedTo = r.library;
      ok++;
    } catch (e) {
      item.state = 'failed';
      item.error = e.message;
      failed++;
    }
    renderRow(item);
    renderActions();
  }

  toast(failed ? `Added ${ok}, ${failed} failed` : `Added ${ok} to ${library || 'the default library'}`,
        {ok: failed === 0});
  fetchList();
}

/* ── Rendering ────────────────────────────────────────────────────────────
   Rows are replaced individually rather than by re-rendering the list, so an
   extraction finishing three rows down does not yank the caret out of a title
   the user is editing. */
function renderQueue() {
  const box = document.getElementById('ws-queue');
  box.innerHTML = queue.map(rowHTML).join('');
  renderActions();
}

function renderRow(item) {
  const el = document.querySelector(`[data-ws-key="${item.key}"]`);
  if (!el) { renderQueue(); return; }
  // Never replace the row the user is typing in. Its state can only be
  // 'ready' — nothing else is editable — so there is nothing to show that is
  // worth interrupting an edit for.
  if (el.contains(document.activeElement) && item.state === 'ready') return;
  el.outerHTML = rowHTML(item);
}

function renderActions() {
  const bar = document.getElementById('ws-queue-actions');
  const count = document.getElementById('ws-add-count');
  if (!bar || !count) return;
  const ready = queue.filter(q => q.state === 'ready').length;
  const busy = queue.some(q => q.state === 'reading' || q.state === 'adding');
  bar.hidden = queue.length === 0;
  count.textContent = ready ? `Add ${ready} to library` : 'Add to library';
  bar.querySelector('[data-ws-add-all]').disabled = ready === 0 || busy;
}

const STATE_LABEL = {
  queued:  'Waiting',
  reading: 'Reading…',
  ready:   'Ready',
  failed:  'Failed',
  adding:  'Adding…',
  added:   'Added',
};

function rowHTML(item) {
  const d = item.data || {};
  // The server hands back its OWN relay path now, never the source's CDN URL — rendering
  // the latter had the browser fetch the image straight from the site, from the home
  // address, outside the tunnel. safeUrl() requires an https:// prefix and would reject a
  // same-origin path, so the shape is checked explicitly instead of loosening safeUrl for
  // every other caller.
  const thumb = ownThumbPath(d.thumbnail_url) ? esc(d.thumbnail_url) : '';
  const meta = [
    d.uploader,
    d.duration_seconds ? fmtDuration(d.duration_seconds) : '',
    d.height ? `${d.height}p` : '',
    d.ext,
    d.extractor,
  ].filter(Boolean).map(x => esc(String(x))).join(' · ');

  const body = item.state === 'ready' || item.state === 'adding' || item.state === 'added'
    ? `<input type="text" class="ws-q-title" maxlength="200"
         data-ws-title-for="${esc(item.key)}" value="${esc(item.title || '')}"
         ${item.state === 'ready' ? '' : 'disabled'}>
       <div class="ws-meta">${meta}</div>
       ${d.already_added ? `<p class="hint">${icon('info')} Already in the library — adding refreshes it.</p>` : ''}
       ${item.state === 'added' ? `<div class="vpn-cfg-meta">${icon('check')} added to ${esc(item.addedTo || '')}</div>` : ''}`
    : `<div class="vpn-cfg-name">${esc(item.url)}</div>
       ${item.error ? `<div class="ws-error">${icon('alert')} ${esc(item.error)}</div>` : ''}`;

  const canAct = item.state !== 'reading' && item.state !== 'adding';
  return `
    <div class="vpn-cfg ws-q ws-q-${esc(item.state)}" data-ws-key="${esc(item.key)}">
      <div class="vpn-cfg-main">
        ${thumb && thumb !== '#' ? `<img class="ws-thumb" src="${thumb}" alt="" loading="lazy" referrerpolicy="no-referrer">` : ''}
        <div class="ws-q-body">
          <div class="ws-q-state">${esc(STATE_LABEL[item.state] || item.state)}</div>
          ${body}
          <div class="vpn-cfg-meta mono">${esc(item.url)}</div>
        </div>
      </div>
      <div class="vpn-cfg-actions">
        ${item.state === 'failed' ? `<button class="btn" type="button" data-ws-retry="${esc(item.key)}">${icon('refresh')} Retry</button>` : ''}
        ${canAct ? `<button class="btn" type="button" data-ws-drop="${esc(item.key)}">${icon('trash')} Remove</button>` : ''}
      </div>
    </div>`;
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
  box.innerHTML = list.map(renderListRow).join('');
}

/* A link dies in ways a torrent does not — the uploader deletes it, the site
   changes its player, the extractor breaks — and all three look identical from
   Jellyfin. The row leads with which one happened, because that is the only
   place the difference is ever written down. */
function renderListRow(s) {
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
