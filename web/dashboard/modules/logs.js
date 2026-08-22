/* ==========================================================================
   Logs section. The auto-refresh timer is now cleared when the section is left
   (it used to keep polling journalctl for the life of the page).
   ========================================================================== */

import {apiFetch} from '../../shared/api.js';
import {icon} from '../../shared/icons.js';

let timer = null;

export function initLogs() {
  document.getElementById('section-logs').addEventListener('click', e => {
    if (e.target.closest('[data-log-load]')) { fetchLogs(); return; }
    const auto = e.target.closest('[data-log-auto]');
    if (auto) toggleAuto(auto);
  });
}

export function stopAutoLog() {
  if (!timer) return;
  clearInterval(timer);
  timer = null;
  const btn = document.querySelector('[data-log-auto]');
  if (btn) {
    btn.classList.remove('on');
    btn.setAttribute('aria-pressed', 'false');
    btn.innerHTML = `${icon('refresh')} Auto`;
  }
}

export async function fetchLogs() {
  const svc = document.getElementById('log-svc').value;
  const n = document.getElementById('log-n').value;
  const pre = document.getElementById('log-output');
  let data;
  try {
    data = await apiFetch(`/api/logs?svc=${encodeURIComponent(svc)}&n=${encodeURIComponent(n)}`);
  } catch (e) {
    pre.textContent = `Could not read the log: ${e.message}`;
    stopAutoLog();
    return;
  }
  pre.textContent = (data.lines || []).join('\n') || '(no output)';
  pre.scrollTop = pre.scrollHeight;
}

function toggleAuto(btn) {
  if (timer) { stopAutoLog(); return; }
  fetchLogs();
  timer = setInterval(() => { if (!document.hidden) fetchLogs(); }, 5000);
  btn.classList.add('on');
  btn.setAttribute('aria-pressed', 'true');
  btn.innerHTML = `${icon('activity')} Auto ●`;
}

