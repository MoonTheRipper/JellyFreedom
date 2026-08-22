/* ==========================================================================
   JellyFreedom shared runtime: escaping, fetch, toasts, formatting.
   ONE copy — imported by both the public app and the dashboard.
   ========================================================================== */

import {icon} from './icons.js';

/* ── Escaping ─────────────────────────────────────────────────────────────
   esc() escapes the FIVE characters that matter, including the single quote.
   Leaving ' unescaped was a live XSS (release titles come straight from
   indexer uploaders) AND a live functional break (any apostrophe in a title
   — "Ocean's Eleven" — produced a SyntaxError and a dead button when it was
   interpolated into an onclick=""). Both layers are fixed: this function
   escapes ', and no handler is wired through an inline onclick with an
   interpolated value any more — see `delegate()` below.                     */
const ESC_MAP = {'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'};
export function esc(s) {
  return String(s === null || s === undefined ? '' : s).replace(/[&<>"']/g, c => ESC_MAP[c]);
}
/** attr() is esc() named for its use site — values placed inside an
 *  attribute. Kept as a distinct name so audits can grep for either. */
export const attr = esc;

/**
 * safeUrl returns an href that is guaranteed not to execute. esc() stops a
 * value breaking OUT of an attribute, but it does not stop `javascript:` from
 * running once the link is clicked, and several hrefs here are built from
 * server-supplied values (jellyfin_url from /api/configured, the Prowlarr and
 * Jellyfin URLs from /api/settings). Anything that is not http(s) becomes "#".
 */
export function safeUrl(u) {
  const s = String(u || '').trim();
  return /^https?:\/\//i.test(s) ? esc(s) : '#';
}

/** num() coerces to a finite number for safe interpolation into markup. */
export function num(v, fallback = 0) {
  const n = Number(v);
  return Number.isFinite(n) ? n : fallback;
}

/* ── Fetch ───────────────────────────────────────────────────────────────── */

export class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

let unauthorizedHandler = null;
/** setUnauthorizedHandler decides what a 401 means for this document.
 *  Dashboard: redirect to the login page. Public app: open the login modal. */
export function setUnauthorizedHandler(fn) { unauthorizedHandler = fn; }

/**
 * apiFetch performs a request and ALWAYS surfaces the server's reason.
 * - non-2xx  -> throws ApiError carrying the server's `error` field
 * - 401      -> calls the unauthorized handler, then throws
 * Nothing in this codebase may do `fetch(x).then(r => r.json())` any more:
 * that turned a 500 into an empty array, which the UI rendered as
 * "No releases found" — indistinguishable from a genuinely empty result.
 */
export async function apiFetch(path, opts) {
  let r;
  try {
    r = await fetch(path, opts);
  } catch (e) {
    throw new ApiError('Cannot reach the server — is JellyFreedom running?', 0);
  }
  if (r.status === 401) {
    if (unauthorizedHandler) unauthorizedHandler();
    throw new ApiError('Sign in required', 401);
  }
  if (r.status === 403) throw new ApiError('Forbidden — admin access required', 403);
  if (!r.ok) {
    const body = await r.json().catch(() => null);
    throw new ApiError((body && body.error) || `${r.status} ${r.statusText || 'request failed'}`, r.status);
  }
  if (r.status === 204) return null;
  const text = await r.text();
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch (_) {
    throw new ApiError('Server returned a malformed response', r.status);
  }
}

/**
 * apiTry never throws. Use it for endpoints that may not exist yet on this
 * build (the contract's newer routes) or whose failure must not break a page.
 * Returns {ok, data, error, status}. An unknown /api/ path on this server
 * falls through to the admin-only mux, so ABSENT shows up as 401/403/404 —
 * all three are treated as "not available", never as an error to shout about.
 */
export async function apiTry(path, opts) {
  try {
    const data = await apiFetch(path, opts);
    return {ok: true, data, error: null, status: 200};
  } catch (e) {
    const status = e.status || 0;
    return {ok: false, data: null, error: e.message, status, absent: status === 404 || status === 401 || status === 403};
  }
}

/** apiList fetches an endpoint expected to return an array, normalised. */
export async function apiList(path, opts) {
  const d = await apiFetch(path, opts);
  return Array.isArray(d) ? d : [];
}
/** apiObj fetches an endpoint expected to return one object, normalised. */
export async function apiObj(path, opts) {
  return apiFetch(path, opts);
}

/* ── Toasts ───────────────────────────────────────────────────────────────
   A stack, not a single element. Two rapid toasts used to clobber each other,
   which happened on every season request (queued + subscribed).             */
function toastHost() {
  let host = document.getElementById('toast-stack');
  if (!host) {
    host = document.createElement('div');
    host.id = 'toast-stack';
    host.setAttribute('role', 'status');
    host.setAttribute('aria-live', 'polite');
    host.setAttribute('aria-atomic', 'false');
    document.body.appendChild(host);
  }
  return host;
}

/**
 * toast(message, opts)
 *   opts.ok      — false renders the error treatment (default true)
 *   opts.action  — {label, onClick} renders an inline action button
 *   opts.timeout — ms before auto-dismiss (default 3800; 0 = sticky)
 * Legacy call shape toast(msg, false) is still accepted.
 */
export function toast(message, opts = {}) {
  if (typeof opts === 'boolean') opts = {ok: opts};
  const ok = opts.ok !== false;
  const el = document.createElement('div');
  el.className = 'toast ' + (ok ? 'ok' : 'err');
  el.innerHTML = `${icon(ok ? 'check' : 'alert')}<div class="toast-text">${esc(message)}</div>`;
  if (opts.action && opts.action.label) {
    const b = document.createElement('button');
    b.className = 'toast-action';
    b.type = 'button';
    b.textContent = opts.action.label;
    b.addEventListener('click', () => { dismiss(); opts.action.onClick && opts.action.onClick(); });
    el.appendChild(b);
  }
  const host = toastHost();
  host.appendChild(el);
  while (host.children.length > 4) host.removeChild(host.firstElementChild);
  requestAnimationFrame(() => el.classList.add('show'));
  let timer = null;
  function dismiss() {
    if (timer) clearTimeout(timer);
    el.classList.remove('show');
    setTimeout(() => el.remove(), 300);
  }
  const ms = opts.timeout === undefined ? 3800 : opts.timeout;
  if (ms > 0) timer = setTimeout(dismiss, ms);
  return dismiss;
}

/* ── Formatting ──────────────────────────────────────────────────────────── */
export function fmtBytes(b) {
  b = Number(b) || 0;
  if (!b) return '0 B';
  if (b < 1024) return b + ' B';
  if (b < 1048576) return (b / 1024).toFixed(1) + ' KB';
  if (b < 1073741824) return (b / 1048576).toFixed(1) + ' MB';
  return (b / 1073741824).toFixed(2) + ' GB';
}
/** fmtSize is the compact form used on release rows. */
export function fmtSize(b) {
  b = Number(b) || 0;
  if (!b) return '';
  return b < 1073741824 ? Math.round(b / 1048576) + 'MB' : (b / 1073741824).toFixed(1) + 'GB';
}
export function relTime(iso) {
  const then = new Date(iso).getTime();
  if (!Number.isFinite(then)) return '—';
  const diff = Math.round((Date.now() - then) / 1000);
  const abs = Math.abs(diff);
  let s;
  if (abs < 60) s = abs + 's';
  else if (abs < 3600) s = Math.round(abs / 60) + 'm';
  else if (abs < 86400) s = Math.round(abs / 3600) + 'h';
  else s = Math.round(abs / 86400) + 'd';
  return diff >= 0 ? `${s} ago` : `in ${s}`;
}

/* ── DOM helpers ─────────────────────────────────────────────────────────── */

/**
 * delegate wires ONE listener on a container for a selector, and fires it for
 * keyboard activation too. This is the replacement for every
 * onclick="fn('${value}')" site: handlers read data-* attributes instead of
 * having values interpolated into executable attribute text.
 */
export function delegate(root, selector, handler, {keys = true} = {}) {
  const node = typeof root === 'string' ? document.getElementById(root) : root;
  if (!node) return;
  node.addEventListener('click', e => {
    const el = e.target.closest(selector);
    if (el && node.contains(el)) handler(el, e);
  });
  if (!keys) return;
  node.addEventListener('keydown', e => {
    if (e.key !== 'Enter' && e.key !== ' ' && e.key !== 'Spacebar') return;
    const el = e.target.closest(selector);
    if (!el || !node.contains(el)) return;
    // Real <button>/<a> elements already activate on these keys.
    const tag = el.tagName;
    if (tag === 'BUTTON' || tag === 'A' || tag === 'INPUT' || tag === 'SELECT' || tag === 'TEXTAREA') return;
    e.preventDefault();
    handler(el, e);
  });
}

export function debounce(fn, ms) {
  let t = null;
  const wrapped = (...args) => { clearTimeout(t); t = setTimeout(() => fn(...args), ms); };
  wrapped.cancel = () => clearTimeout(t);
  return wrapped;
}

/**
 * sequencer returns {next, isCurrent, abort} — a combined AbortController and
 * monotonic sequence guard so a slow earlier response can never overwrite a
 * newer one.
 */
export function sequencer() {
  let seq = 0;
  let ctrl = null;
  return {
    next() {
      seq++;
      if (ctrl) ctrl.abort();
      ctrl = typeof AbortController === 'function' ? new AbortController() : null;
      return {id: seq, signal: ctrl ? ctrl.signal : undefined};
    },
    isCurrent(token) { return token.id === seq; },
    abort() { if (ctrl) { ctrl.abort(); ctrl = null; } seq++; },
  };
}
/** isAbort reports whether an error is just a superseded request. */
export function isAbort(e) {
  return e && (e.name === 'AbortError' || e.code === 20);
}

/**
 * errorState renders a consistent "it failed and here is why" block, with a
 * retry affordance where retrying makes sense.
 *
 * `retryAttr` is raw attribute text and is NOT escaped, because it has to be
 * markup. It must therefore always be a literal written at the call site (or a
 * literal plus num()-bounded values) — never anything derived from a response.
 * `message` is escaped, so the server's reason is safe to pass through.
 */
export function errorState(message, {retryAttr = '', compact = false} = {}) {
  const retry = retryAttr
    ? `<div class="callout-actions"><button class="btn sm" type="button" ${retryAttr}>${icon('refresh')} Retry</button></div>`
    : '';
  if (compact) {
    return `<div class="callout err">${icon('alert')}<div class="callout-body">${esc(message)}${retry}</div></div>`;
  }
  return `<div class="callout err">${icon('alert')}<div class="callout-body">
    <div class="callout-title">Something went wrong</div>
    <p>${esc(message)}</p>${retry}</div></div>`;
}

/** whenVisible runs fn on an interval, but only while the tab is visible. */
export function visibleInterval(fn, ms) {
  let id = null;
  const start = () => { if (id === null) id = setInterval(() => { if (!document.hidden) fn(); }, ms); };
  const stop = () => { if (id !== null) { clearInterval(id); id = null; } };
  const onVis = () => { if (document.hidden) stop(); else { start(); fn(); } };
  document.addEventListener('visibilitychange', onVis);
  start();
  return {stop() { stop(); document.removeEventListener('visibilitychange', onVis); }};
}
