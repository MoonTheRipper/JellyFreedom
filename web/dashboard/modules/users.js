/* ==========================================================================
   Users section: local users, admin flags, Jellyfin import.

   Jellyfin usernames are set on the Jellyfin server by someone who need not be
   a JellyFreedom admin, so they are untrusted input on this page. They used to
   be interpolated into onclick="importUser('${esc(u.id)}','${esc(u.name)}')"
   where esc() did not escape apostrophes. Everything is data-* now.
   ========================================================================== */

import {apiFetch, esc, num, toast, errorState} from '../../shared/api.js';
import {icon} from '../../shared/icons.js';

export function initUsers() {
  document.getElementById('section-users').addEventListener('click', e => {
    if (e.target.closest('[data-users-refresh]')) { fetchUsers(); return; }
    if (e.target.closest('[data-user-create]')) { createUser(); return; }
    if (e.target.closest('[data-jf-load]')) { loadJellyfinUsers(); return; }
    const act = e.target.closest('[data-user-act]');
    if (!act) return;
    switch (act.dataset.userAct) {
      case 'toggle-admin':
        toggleAdmin(Number(act.dataset.id), act.dataset.admin === '1', act.dataset.name);
        break;
      case 'delete': deleteUser(Number(act.dataset.id), act.dataset.name); break;
      case 'import': importUser(act.dataset.jfid, act.dataset.name); break;
    }
  });
  document.getElementById('section-users').addEventListener('keydown', e => {
    if (e.key === 'Enter' && e.target.closest('#new-username, #new-password')) {
      e.preventDefault();
      createUser();
    }
  });
}

export async function fetchUsers() {
  const body = document.getElementById('users-body');
  let data;
  try {
    data = await apiFetch('/api/users');
  } catch (e) {
    body.innerHTML = errorState(e.message, {retryAttr: 'data-users-refresh="1"'});
    return;
  }
  if (!data || !data.length) {
    body.innerHTML = '<div class="empty">No users yet.</div>';
    return;
  }
  body.innerHTML = `<div class="table-scroll"><table>
    <thead><tr><th>Username</th><th>Source</th><th>Role</th><th>Created</th><th></th></tr></thead>
    <tbody>${data.map(rowHTML).join('')}</tbody></table></div>`;
}

function rowHTML(u) {
  // The list endpoint returns a DTO whose field is `source`; `auth_source` is
  // the store field name in the contract. Accept either — both snake_case.
  const source = u.source || u.auth_source || 'local';
  const created = u.created_at ? new Date(u.created_at).toLocaleDateString() : '—';
  const isAdmin = !!u.is_admin;
  return `<tr>
    <td><strong>${esc(u.username)}</strong></td>
    <td><span class="badge ${source === 'jellyfin' ? 'jellyfin' : 'local'}">${esc(source)}</span></td>
    <td>${isAdmin
      ? `<span class="badge admin">${icon('key')} admin</span>`
      : `<span class="subtle">user</span>`}</td>
    <td class="subtle">${esc(created)}</td>
    <td class="row-actions">
      <button class="btn sm" type="button" data-user-act="toggle-admin"
        data-id="${num(u.id)}" data-admin="${isAdmin ? '1' : '0'}" data-name="${esc(u.username)}">
        ${isAdmin ? 'Remove admin' : 'Make admin'}</button>
      <button class="btn sm danger" type="button" data-user-act="delete"
        data-id="${num(u.id)}" data-name="${esc(u.username)}">${icon('trash')} Delete</button>
    </td>
  </tr>`;
}

function showMsg(el, msg, type) {
  el.textContent = msg;
  el.className = 'msg ' + type;
}

async function createUser() {
  const username = document.getElementById('new-username').value.trim();
  const password = document.getElementById('new-password').value;
  const isAdmin = document.getElementById('new-is-admin').checked;
  const msg = document.getElementById('create-msg');
  if (!username || password.length < 8) {
    showMsg(msg, 'Username is required and the password must be at least 8 characters.', 'err');
    return;
  }
  try {
    await apiFetch('/api/users', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({username, password, is_admin: isAdmin}),
    });
  } catch (e) { showMsg(msg, e.message, 'err'); return; }
  showMsg(msg, `User “${username}” created.`, 'ok');
  document.getElementById('new-username').value = '';
  document.getElementById('new-password').value = '';
  document.getElementById('new-is-admin').checked = false;
  fetchUsers();
}

async function toggleAdmin(id, current, name) {
  try {
    await apiFetch(`/api/users/${encodeURIComponent(id)}`, {
      method: 'PATCH', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({is_admin: !current}),
    });
  } catch (e) { toast(e.message, {ok: false}); return; }
  toast(`${name}: ${current ? 'admin removed' : 'made admin'}`);
  fetchUsers();
}

async function deleteUser(id, name) {
  if (!confirm(`Delete user “${name}”? This cannot be undone.`)) return;
  try {
    await apiFetch(`/api/users/${encodeURIComponent(id)}`, {method: 'DELETE'});
  } catch (e) { toast(e.message, {ok: false}); return; }
  toast(`Deleted ${name}`);
  fetchUsers();
}

async function loadJellyfinUsers() {
  const btn = document.getElementById('jf-load-btn');
  const body = document.getElementById('jf-users-body');
  btn.disabled = true;
  btn.textContent = 'Loading…';
  let users;
  try {
    users = await apiFetch('/api/jellyfin/users');
  } catch (e) {
    body.innerHTML = errorState(e.message, {retryAttr: 'data-jf-load="1"', compact: true});
    return;
  } finally {
    btn.disabled = false;
    btn.innerHTML = `${icon('users')} Load Jellyfin users`;
  }
  if (!users || !users.length) {
    body.innerHTML = '<div class="empty">No Jellyfin users found.</div>';
    return;
  }
  body.innerHTML = `<div class="table-scroll"><table>
    <thead><tr><th>Jellyfin user</th><th></th></tr></thead>
    <tbody>${users.map(u => `<tr>
      <td>${esc(u.name)}</td>
      <td>${u.imported
        ? `<span class="subtle">already imported</span>`
        : `<button class="btn sm primary" type="button" data-user-act="import"
            data-jfid="${esc(u.id)}" data-name="${esc(u.name)}">Import</button>`}</td>
    </tr>`).join('')}</tbody></table></div>`;
}

async function importUser(jellyfinUserID, name) {
  try {
    await apiFetch('/api/users/import', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({jellyfin_user_id: jellyfinUserID, username: name}),
    });
  } catch (e) { toast(e.message, {ok: false}); return; }
  toast(`Imported ${name}`);
  fetchUsers();
  loadJellyfinUsers();
}
