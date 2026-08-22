/* ==========================================================================
   Settings section: connections, quality, cache, libraries, account.

   The per-service "Test before save" flow is deliberately kept — it is the
   best thing in the old dashboard and the fastest way to find out that a key
   is wrong before it is persisted.
   ========================================================================== */

import {apiFetch, esc, num, toast, errorState} from '../../shared/api.js';
import {icon} from '../../shared/icons.js';

export function initSettings() {
  document.getElementById('section-settings').addEventListener('click', e => {
    if (e.target.closest('[data-settings-reload]')) { fetchSettings(); return; }
    const t = e.target.closest('[data-test-conn]');
    if (t) { testConn(t.dataset.testConn); return; }
    if (e.target.closest('[data-save-connections]')) { saveConnections(); return; }
    if (e.target.closest('[data-save-quality]')) { saveQuality(); return; }
    if (e.target.closest('[data-save-cache]')) { saveCache(); return; }
    if (e.target.closest('[data-change-password]')) { changePassword(); return; }
    const cp = e.target.closest('[data-copy]');
    if (cp) { copyField(cp.dataset.copy, cp); return; }
    const rv = e.target.closest('[data-reveal]');
    if (rv) {
      const f = document.getElementById(rv.dataset.reveal);
      if (f) {
        const hidden = f.type === 'password';
        f.type = hidden ? 'text' : 'password';
        rv.textContent = hidden ? 'Hide' : 'Show';
      }
      return;
    }
  });
}

export async function fetchSettings() {
  let data;
  try {
    data = await apiFetch('/api/settings');
  } catch (e) {
    document.getElementById('conn-forms').innerHTML =
      errorState(e.message, {retryAttr: 'data-settings-reload="1"'});
    return null;
  }
  renderConnections(data.connections || {});
  renderQuality(data.quality || {});
  renderCache(data.cache || {});
  renderLibraries(data.libraries || []);
  renderWebhook(data.webhook || null);
  return data;
}

// navigator.clipboard is unavailable in a non-secure context (plain http on a LAN is exactly
// this app's normal deployment), so fall back to selecting the field for a manual copy rather
// than failing silently.
async function copyField(id, btn) {
  const f = document.getElementById(id);
  if (!f) return;
  try {
    await navigator.clipboard.writeText(f.value);
    const was = btn.textContent;
    btn.textContent = 'Copied';
    setTimeout(() => { btn.textContent = was; }, 1200);
  } catch {
    f.type = 'text';
    f.select();
    toast('Press Ctrl+C to copy — the browser blocked clipboard access on an insecure origin.');
  }
}

// The webhook secret is generated on first run and is required for playback-stop cleanup to
// work. It used to live only in the database, with no UI and no sqlite3 on the box — so an
// operator had no way to read it and cleanup silently never ran.
function renderWebhook(w) {
  const host = document.getElementById('webhook-info');
  if (!host) return;
  if (!w || !w.secret) {
    host.innerHTML = '<div class="empty">Not available.</div>';
    return;
  }
  host.innerHTML = `
    <div class="form-row">
      <label for="wh-url">URL</label>
      <input id="wh-url" type="text" readonly value="${esc(w.url || '')}">
      <button class="btn" data-copy="wh-url">Copy</button>
    </div>
    <div class="form-row">
      <label for="wh-header">Header</label>
      <input id="wh-header" type="text" readonly value="${esc(w.header || '')}">
      <button class="btn" data-copy="wh-header">Copy</button>
    </div>
    <div class="form-row">
      <label for="wh-secret">Secret</label>
      <input id="wh-secret" type="password" readonly value="${esc(w.secret)}">
      <button class="btn" data-reveal="wh-secret">Show</button>
      <button class="btn" data-copy="wh-secret">Copy</button>
    </div>`;
}

function val(id) {
  const e = document.getElementById(id);
  return e ? e.value.trim() : '';
}
function showMsg(el, msg, type) {
  el.textContent = msg;
  el.className = 'msg ' + type;
}

/* ── Connections ─────────────────────────────────────────────────────────── */

function renderConnections(c) {
  const keyField = (id, set) =>
    `<input type="password" id="${id}" autocomplete="off"
      placeholder="${set ? 'set — leave blank to keep the current key' : 'API key'}"
      aria-label="${esc(id.replace('-', ' '))}">`;
  document.getElementById('conn-forms').innerHTML = `
    <div class="conn-row">
      <div class="conn-name">TMDB</div>
      <div class="conn-fields">${keyField('tmdb-key', c.tmdb && c.tmdb.key_set)}</div>
      <button class="btn sm" type="button" data-test-conn="tmdb">Test</button>
      <span class="conn-result" id="test-tmdb" role="status" aria-live="polite"></span>
    </div>
    <div class="conn-row">
      <div class="conn-name">Prowlarr</div>
      <div class="conn-fields">
        <input type="text" id="prowlarr-url" placeholder="http://127.0.0.1:9696"
          aria-label="Prowlarr URL" value="${esc((c.prowlarr && c.prowlarr.url) || '')}">
        ${keyField('prowlarr-key', c.prowlarr && c.prowlarr.key_set)}
      </div>
      <button class="btn sm" type="button" data-test-conn="prowlarr">Test</button>
      <span class="conn-result" id="test-prowlarr" role="status" aria-live="polite"></span>
    </div>
    <div class="conn-row">
      <div class="conn-name">Jellyfin</div>
      <div class="conn-fields">
        <input type="text" id="jellyfin-url" placeholder="http://127.0.0.1:8096"
          aria-label="Jellyfin URL" value="${esc((c.jellyfin && c.jellyfin.url) || '')}">
        ${keyField('jellyfin-key', c.jellyfin && c.jellyfin.key_set)}
      </div>
      <button class="btn sm" type="button" data-test-conn="jellyfin">Test</button>
      <span class="conn-result" id="test-jellyfin" role="status" aria-live="polite"></span>
    </div>
    <div class="conn-row">
      <div class="conn-name">TorrServer</div>
      <div class="conn-fields">
        <input type="text" id="torrserver-url" placeholder="http://10.42.0.2:8090"
          aria-label="TorrServer URL" value="${esc((c.torrserver && c.torrserver.url) || '')}">
      </div>
      <button class="btn sm" type="button" data-test-conn="torrserver">Test</button>
      <span class="conn-result" id="test-torrserver" role="status" aria-live="polite"></span>
    </div>
    <div class="save-row">
      <button class="btn primary" type="button" data-save-connections="1">Save connections</button>
      <span class="msg" id="conn-msg"></span>
    </div>`;
}

async function testConn(service) {
  const el = document.getElementById('test-' + service);
  el.textContent = 'testing…';
  el.className = 'conn-result testing';
  const body = {service};
  if (service !== 'tmdb') body.url = val(service + '-url');
  if (service !== 'torrserver') body.key = val(service + '-key');
  try {
    const d = await apiFetch('/api/settings/connections/test', {
      method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body),
    });
    el.textContent = d.message || (d.ok ? 'ok' : 'failed');
    el.className = 'conn-result ' + (d.ok ? 'ok' : 'err');
  } catch (e) {
    el.textContent = e.message;
    el.className = 'conn-result err';
  }
}

async function saveConnections() {
  const msg = document.getElementById('conn-msg');
  try {
    await apiFetch('/api/settings/connections', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({
        tmdb_key: val('tmdb-key'),
        prowlarr_url: val('prowlarr-url'), prowlarr_key: val('prowlarr-key'),
        jellyfin_url: val('jellyfin-url'), jellyfin_key: val('jellyfin-key'),
        torrserver_url: val('torrserver-url'),
      }),
    });
  } catch (e) { showMsg(msg, e.message, 'err'); return; }
  showMsg(msg, 'Connections saved and applied.', 'ok');
  toast('Connections saved');
  setTimeout(fetchSettings, 600);
  document.dispatchEvent(new CustomEvent('jf:config-changed'));
}

/* ── Quality ─────────────────────────────────────────────────────────────── */

function renderQuality(q) {
  document.getElementById('quality-form').innerHTML = `
    <div class="set-grid">
      <label for="q-min-seeders">Min seeders</label>
      <input type="number" id="q-min-seeders" value="${num(q.min_seeders)}">
      <label for="q-max-size">Max size (GB)</label>
      <input type="number" id="q-max-size" value="${num(q.max_size_gb)}">
      <label for="q-reject-cam">Reject camera copies</label>
      <label class="set-toggle"><input type="checkbox" id="q-reject-cam"
        ${q.reject_cam ? 'checked' : ''}> Skip CAM / TS / screener rips</label>
      <label for="q-video">Preferred video codecs</label>
      <input type="text" id="q-video" value="${esc(q.video_codecs || '')}" placeholder="h264, h265, hevc">
      <label for="q-audio">Preferred audio codecs</label>
      <input type="text" id="q-audio" value="${esc(q.audio_codecs || '')}" placeholder="aac, ac3, eac3">
      <label for="q-containers">Preferred containers</label>
      <input type="text" id="q-containers" value="${esc(q.containers || '')}" placeholder="mp4, mkv">
    </div>
    <p class="set-note">${icon('info')} These are the filters a failed request reports as the
      reason nothing was picked. If everything fails with “no suitable release found”, loosen
      min seeders first.</p>
    <div class="save-row">
      <button class="btn primary" type="button" data-save-quality="1">Save quality settings</button>
      <span class="msg" id="quality-msg"></span>
    </div>`;
}

async function saveQuality() {
  const msg = document.getElementById('quality-msg');
  try {
    await apiFetch('/api/settings/quality', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({
        min_seeders: parseInt(val('q-min-seeders'), 10) || 0,
        max_size_gb: parseInt(val('q-max-size'), 10) || 0,
        reject_cam: document.getElementById('q-reject-cam').checked,
        video_codecs: val('q-video'), audio_codecs: val('q-audio'), containers: val('q-containers'),
      }),
    });
  } catch (e) { showMsg(msg, e.message, 'err'); return; }
  showMsg(msg, 'Saved — applies to new requests immediately.', 'ok');
  toast('Quality settings saved');
}

/* ── Cache ───────────────────────────────────────────────────────────────── */

function renderCache(c) {
  const sel = (v, want) => v === want ? ' selected' : '';
  document.getElementById('cache-form').innerHTML = `
    <div class="set-grid">
      <label for="c-mode">Cache mode</label>
      <select id="c-mode">
        <option value="ram"${sel(c.mode, 'ram')}>RAM (fastest)</option>
        <option value="disk"${sel(c.mode, 'disk')}>Disk (low-RAM hosts)</option>
      </select>
      <label for="c-size">Cache size (MB)</label>
      <input type="number" id="c-size" value="${num(c.size_mb)}">
      <label for="c-path">Disk path <span class="subtle">(disk mode)</span></label>
      <input type="text" id="c-path" value="${esc(c.path || '')}" placeholder="/srv/jellyfreedom/cache">
      <label for="c-conns">Peer connections</label>
      <input type="number" id="c-conns" value="${num(c.connections)}">
      <label for="c-disc">Disconnect timeout (s)</label>
      <input type="number" id="c-disc" value="${num(c.disconnect_s)}">
      <label for="c-retr">Add-trackers mode</label>
      <select id="c-retr">
        <option value="0"${c.retrackers === 0 ? ' selected' : ''}>Off</option>
        <option value="1"${c.retrackers === 1 ? ' selected' : ''}>Add</option>
        <option value="2"${c.retrackers === 2 ? ' selected' : ''}>Replace</option>
      </select>
      <label for="c-up">Upload limit (KB/s)</label>
      <input type="number" id="c-up" value="${num(c.upload_kb)}">
    </div>
    <div class="save-row">
      <button class="btn primary" type="button" data-save-cache="1">Save &amp; apply to TorrServer</button>
      <span class="msg" id="cache-msg"></span>
    </div>`;
}

async function saveCache() {
  const msg = document.getElementById('cache-msg');
  try {
    await apiFetch('/api/settings/cache', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({
        mode: val('c-mode'), size_mb: parseInt(val('c-size'), 10) || 0, path: val('c-path'),
        connections: parseInt(val('c-conns'), 10) || 0,
        disconnect_s: parseInt(val('c-disc'), 10) || 0,
        retrackers: parseInt(val('c-retr'), 10) || 0,
        upload_kb: parseInt(val('c-up'), 10) || 0,
      }),
    });
  } catch (e) { showMsg(msg, e.message, 'err'); return; }
  showMsg(msg, 'Saved and applied to TorrServer.', 'ok');
  toast('Cache settings applied');
}

/* ── Libraries (read-only) ───────────────────────────────────────────────── */

function renderLibraries(libs) {
  const host = document.getElementById('libraries-list');
  if (!libs.length) { host.innerHTML = '<div class="empty">None configured.</div>'; return; }
  host.innerHTML = `<div class="table-scroll"><table>
    <thead><tr><th>Name</th><th>Type</th><th>Path</th><th>Default</th></tr></thead>
    <tbody>${libs.map(l => `<tr>
      <td><strong>${esc(l.name)}</strong></td>
      <td><span class="badge ${l.type === 'tv' ? 'jellyfin' : 'local'}">${esc(l.type)}</span></td>
      <td class="mono">${esc(l.path)}</td>
      <td>${l.default ? icon('check') : ''}</td>
    </tr>`).join('')}</tbody></table></div>`;
}

/* ── Account ─────────────────────────────────────────────────────────────── */

async function changePassword() {
  const cur = document.getElementById('pw-current').value;
  const nw = document.getElementById('pw-new').value;
  const nw2 = document.getElementById('pw-new2').value;
  const msg = document.getElementById('pw-msg');
  if (nw !== nw2) { showMsg(msg, 'The new passwords do not match.', 'err'); return; }
  if (nw.length < 8) { showMsg(msg, 'The new password must be at least 8 characters.', 'err'); return; }
  try {
    await apiFetch('/api/auth/change-password', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({current: cur, new: nw}),
    });
  } catch (e) { showMsg(msg, e.message, 'err'); return; }
  showMsg(msg, 'Password updated.', 'ok');
  document.getElementById('pw-current').value = '';
  document.getElementById('pw-new').value = '';
  document.getElementById('pw-new2').value = '';
}
