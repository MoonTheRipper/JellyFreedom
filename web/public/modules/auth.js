/* ==========================================================================
   Sign-in modal + auth UI state for the public app.
   ========================================================================== */

import {apiFetch, ApiError} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {state, emit, loadMe} from './state.js';
import {openDialog, closeDialog, isDialogOpen} from './dialog.js';

let pendingAction = null;

function el(id) { return document.getElementById(id); }

export function initAuth() {
  el('login-form').addEventListener('submit', onSubmit);
  el('login-cancel').addEventListener('click', () => closeLoginModal());
  el('login-overlay').addEventListener('click', e => {
    if (e.target === e.currentTarget) closeLoginModal();
  });
  el('header-auth-btn').addEventListener('click', () => {
    if (state.user) logout(); else openLoginModal(null);
  });
}

export function openLoginModal(callback) {
  pendingAction = callback || null;
  const overlay = el('login-overlay');
  el('login-username').value = '';
  el('login-password').value = '';
  const err = el('login-err');
  err.textContent = '';
  err.style.display = 'none';
  openDialog(overlay, overlay.querySelector('.login-box'), {
    label: 'Sign in',
    onClose: () => { pendingAction = null; },
  });
}

export function closeLoginModal() {
  closeDialog(el('login-overlay'));
}

export function loginModalOpen() { return isDialogOpen(el('login-overlay')); }

/** requireLogin runs fn if signed in, otherwise prompts and runs it after. */
export function requireLogin(fn) {
  if (state.user) { fn(); return true; }
  openLoginModal(fn);
  return false;
}

/** onUnauthorized is wired into shared/api.js: a 401 anywhere drops the
 *  cached user and offers the sign-in modal instead of silently failing. */
export function onUnauthorized() {
  if (state.user) { state.user = null; emit('auth'); updateAuthUI(); }
  if (!loginModalOpen()) openLoginModal(null);
}

async function onSubmit(e) {
  e.preventDefault();
  const btn = el('login-submit');
  const errEl = el('login-err');
  const username = el('login-username').value.trim();
  const password = el('login-password').value;
  errEl.style.display = 'none';
  if (!username || !password) {
    errEl.textContent = 'Enter username and password';
    errEl.style.display = '';
    return;
  }
  btn.disabled = true;
  btn.textContent = 'Signing in…';
  try {
    // Deliberately NOT apiFetch: a 401 here means "wrong password", which is a
    // normal answer to show inline. Routing it through the shared unauthorized
    // handler would re-open this very modal in a loop. r.ok IS checked below.
    const r = await fetch('/api/auth/login', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({username, password}),
    });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) {
      errEl.textContent = d.error || `Sign-in failed (${r.status})`;
      errEl.style.display = '';
      return;
    }
    state.user = d;
    emit('auth');
    const fn = pendingAction;
    closeLoginModal();
    updateAuthUI();
    if (fn) fn();
  } catch (_) {
    errEl.textContent = 'Cannot reach the server — is JellyFreedom running?';
    errEl.style.display = '';
  } finally {
    btn.disabled = false;
    btn.textContent = 'Sign in';
  }
}

export async function logout() {
  try { await apiFetch('/api/auth/logout', {method: 'POST'}); }
  catch (e) { if (!(e instanceof ApiError) || e.status !== 401) console.warn('[jf] logout', e); }
  state.user = null;
  emit('auth');
  updateAuthUI();
}

export function updateAuthUI() {
  const userEl = el('header-user');
  const btn = el('header-auth-btn');
  if (state.user) {
    userEl.textContent = state.user.username;
    userEl.style.display = '';
    btn.innerHTML = `${icon('power')} Sign out`;
    btn.className = 'header-btn';
    btn.setAttribute('aria-label', `Sign out of ${state.user.username}`);
  } else {
    userEl.style.display = 'none';
    btn.innerHTML = `${icon('key')} Sign in`;
    btn.className = 'header-btn signin';
    btn.setAttribute('aria-label', 'Sign in');
  }
  // Admin-only link: the dashboard is behind RequireAdmin, so pointing every
  // visitor at it just serves them a redirect to a login form.
  const dash = el('header-dash-link');
  if (dash) dash.hidden = !(state.user && state.user.is_admin);

  const req = document.getElementById('req-btn');
  if (req && !req.disabled) {
    const isEp = req.dataset.action === 'episode';
    req.textContent = state.user ? (isEp ? 'Request Episode' : 'Request Movie') : 'Sign in to request';
    req.classList.toggle('needs-auth', !state.user);
  }
}

export async function refreshAuth() {
  await loadMe();
  updateAuthUI();
}

