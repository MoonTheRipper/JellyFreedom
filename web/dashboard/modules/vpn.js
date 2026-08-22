/* ==========================================================================
   VPN section: tunnel status, uploaded configs, leak check.
   ========================================================================== */

import {apiFetch, esc, toast, errorState, fmtBytes} from '../../shared/api.js';
import {icon} from '../../shared/icons.js';

export function initVpn() {
  document.getElementById('section-vpn').addEventListener('click', e => {
    if (e.target.closest('[data-vpn-refresh]')) { refreshVpn(); return; }
    if (e.target.closest('[data-vpn-upload]')) { uploadConfig(); return; }
    if (e.target.closest('[data-leak-run]')) { runLeakCheck(); return; }
    const act = e.target.closest('[data-vpn-act]');
    if (!act) return;
    const name = act.dataset.name;
    switch (act.dataset.vpnAct) {
      case 'activate': activateConfig(name); break;
      case 'delete':   deleteConfig(name); break;
      case 'download': downloadConfig(name); break;
    }
  });
}

export async function refreshVpn() {
  await Promise.all([fetchVpnStatus(), fetchConfigs()]);
}

async function fetchVpnStatus() {
  const body = document.getElementById('vpn-body');
  let data;
  try {
    data = await apiFetch('/api/status');
  } catch (e) {
    body.innerHTML = errorState(e.message, {retryAttr: 'data-vpn-refresh="1"', compact: true});
    return;
  }
  renderVpn(data.vpn || {});
}

function renderVpn(v) {
  const connected = !!v.connected;
  document.getElementById('vpn-body').innerHTML = `
    <div class="callout ${connected ? 'good' : 'err'}">${icon(connected ? 'shield' : 'shield-off')}
      <div class="callout-body">
        <div class="callout-title">${connected ? 'Tunnel up' : 'Tunnel DOWN'}</div>
        <p>${connected
          ? 'Torrent traffic is confined to the VPN namespace.'
          : 'Torrent traffic is blocked (fail-closed). Requests will fail until the tunnel is back.'}</p>
      </div></div>
    <div class="kv">
      <span class="kv-label">Interface</span><span class="kv-val">${esc(v.interface || '—')}</span>
      <span class="kv-label">Endpoint</span><span class="kv-val">${esc(v.endpoint || '—')}</span>
      <span class="kv-label">Handshake</span><span class="kv-val">${esc(v.handshake_age || '—')}</span>
      <span class="kv-label">Transfer</span>
      <span class="kv-val">${icon('download')} ${esc(fmtBytes(v.rx_bytes))} &nbsp;
        ${icon('upload')} ${esc(fmtBytes(v.tx_bytes))}</span>
    </div>`;
}

async function fetchConfigs() {
  const box = document.getElementById('vpn-configs');
  if (!box) return;
  let list;
  try {
    list = await apiFetch('/api/vpn/configs');
  } catch (e) {
    box.innerHTML = errorState(e.message, {retryAttr: 'data-vpn-refresh="1"', compact: true});
    return;
  }
  if (!list || !list.length) {
    box.innerHTML = `<div class="callout warn">${icon('alert')}<div class="callout-body">
      <div class="callout-title">No WireGuard config uploaded</div>
      <p>Torrent traffic stays blocked until one is uploaded and activated. Any
      <code>.conf</code> from a provider or a self-hosted peer works — prefer a P2P-friendly
      server, or torrents will connect but never transfer.</p></div></div>`;
    return;
  }
  box.innerHTML = list.map(c => `
    <div class="vpn-cfg ${c.active ? 'active' : ''}">
      <div class="vpn-cfg-main">
        <span class="vpn-cfg-name">${esc(c.name)}</span>
        ${c.active ? `<span class="badge-active">${icon('check')} ACTIVE</span>` : ''}
        <span class="vpn-cfg-meta">${esc(c.endpoint || '')}${c.uploaded ? ' · ' + esc(c.uploaded) : ''}</span>
      </div>
      <div class="vpn-cfg-actions">
        ${c.active ? '' : `<button class="btn sm" type="button" data-vpn-act="activate"
          data-name="${esc(c.name)}">${icon('power')} Activate</button>`}
        <button class="btn sm ghost" type="button" data-vpn-act="download"
          data-name="${esc(c.name)}">${icon('download')} Download</button>
        ${c.active ? '' : `<button class="btn sm danger" type="button" data-vpn-act="delete"
          data-name="${esc(c.name)}">${icon('trash')} Delete</button>`}
      </div>
    </div>`).join('');
}

async function uploadConfig() {
  const fileEl = document.getElementById('vpn-file');
  const nameEl = document.getElementById('vpn-name');
  const f = fileEl.files[0];
  if (!f) { toast('Choose a .conf file first', {ok: false}); return; }
  const content = await f.text();
  const name = nameEl.value.trim() || f.name.replace(/\.conf$/i, '');
  try {
    await apiFetch('/api/vpn/configs', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({name, content}),
    });
  } catch (e) { toast(e.message, {ok: false}); return; }
  toast(`Uploaded ${name}`, {
    action: {label: 'Activate', onClick: () => activateConfig(name)},
  });
  fileEl.value = '';
  nameEl.value = '';
  fetchConfigs();
}

async function activateConfig(name) {
  if (!confirm(`Activate “${name}”?\n\nThis switches the live VPN tunnel and briefly interrupts torrent traffic.`)) return;
  toast(`Activating ${name}… restarting the tunnel`, {timeout: 6000});
  try {
    await apiFetch(`/api/vpn/configs/${encodeURIComponent(name)}/activate`, {method: 'POST'});
  } catch (e) { toast(e.message, {ok: false}); return; }
  toast(`Activated ${name}`);
  setTimeout(refreshVpn, 6000);
}

async function deleteConfig(name) {
  if (!confirm(`Delete “${name}”? The .conf is removed from the server.`)) return;
  try {
    await apiFetch(`/api/vpn/configs/${encodeURIComponent(name)}`, {method: 'DELETE'});
  } catch (e) { toast(e.message, {ok: false}); return; }
  toast(`Deleted ${name}`);
  fetchConfigs();
}

function downloadConfig(name) {
  // Cookie-authenticated GET; the browser saves the .conf (it holds the private key).
  window.location = `/api/vpn/configs/${encodeURIComponent(name)}/download`;
}

/* ── Leak check ──────────────────────────────────────────────────────────── */

async function runLeakCheck() {
  const btn = document.getElementById('leak-btn');
  const result = document.getElementById('leak-result');
  btn.disabled = true;
  btn.textContent = 'Checking…';
  result.innerHTML = `<div class="empty">${icon('clock')} Running — this takes about 10 seconds…</div>`;
  let d;
  try {
    d = await apiFetch('/api/leak');
  } catch (e) {
    result.innerHTML = errorState(`Check failed: ${e.message}`, {retryAttr: 'data-leak-run="1"', compact: true});
    return;
  } finally {
    btn.textContent = 'Run Check';
    btn.disabled = false;
  }
  if (!d) return;

  const pill = (ok, yes, no) =>
    `<span class="pill ${ok ? 'ok' : 'warn'}">${icon(ok ? 'check' : 'alert')} ${esc(ok ? yes : no)}</span>`;
  const since = d.checked_at ? new Date(d.checked_at * 1000).toLocaleTimeString() : '';

  let ipRows;
  if (d.vpn_ipv4) {
    ipRows = `<span class="kv-label">IPv4</span>
      <span class="kv-val ip-row"><code>${esc(d.host_ipv4)}</code> →
        <code>${esc(d.vpn_ipv4)}</code>
        ${pill(!d.leaked, 'Different', 'SAME — LEAKED')}</span>`;
  } else {
    const hasTraffic = d.wg_rx_bytes > 0 || d.wg_tx_bytes > 0;
    ipRows = `<span class="kv-label">Host IPv4</span><span class="kv-val"><code>${esc(d.host_ipv4 || '—')}</code></span>
      <span class="kv-label">VPN IPv4</span>
      <span class="kv-val subtle">not checked (the netns helper could not fetch it)</span>
      <span class="kv-label">Routing proof</span>
      <span class="kv-val">${pill(hasTraffic,
        `${fmtBytes(d.wg_rx_bytes)} in / ${fmtBytes(d.wg_tx_bytes)} out via WG`,
        'No traffic through WG')}</span>`;
  }

  result.innerHTML = `<div class="leak-grid">
      ${ipRows}
      <span class="kv-label">Kill switch</span>
      <span class="kv-val">${pill(d.kill_switch_ok, 'Default route → wg0 only', 'Default route not via wg0')}</span>
      <span class="kv-label">WireGuard</span>
      <span class="kv-val ip-row">${pill(d.wg_connected, 'Connected', 'Disconnected')}
        ${d.wg_handshake ? `<span class="subtle">handshake ${esc(d.wg_handshake)}</span>` : ''}</span>
      <span class="kv-label">IPv6 / veth</span>
      <span class="kv-val">${pill(!d.veth_has_ipv6, 'Disabled', 'Link-local address on veth')}</span>
      <span class="kv-label">ip6tables</span>
      <span class="kv-val ip-row">${d.ip6tables_ok
        ? `<span class="pill ok">${icon('check')} OUTPUT DROP</span>`
        : `<span class="pill neutral">Not auto-verified</span>`}
        ${d.ip6tables_note ? `<span class="subtle">${esc(d.ip6tables_note)}</span>` : ''}</span>
    </div>
    <p class="leak-since">Checked at ${esc(since)}</p>`;
}
