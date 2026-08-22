/* ==========================================================================
   Scheduled tasks section.
   ========================================================================== */

import {apiFetch, esc, num, toast, relTime, errorState} from '../../shared/api.js';
import {icon} from '../../shared/icons.js';

const ORDER = ['library', 'metadata', 'system'];
const CAT_NAME = {library: 'Library', metadata: 'Metadata', system: 'System'};

let pollTimer = null;

export function initTasks() {
  document.getElementById('section-tasks').addEventListener('click', e => {
    if (e.target.closest('[data-tasks-refresh]')) { fetchTasks(); return; }
    const run = e.target.closest('[data-task-run]');
    if (run) runTask(run.dataset.taskRun);
  });
}

export function stopTaskPolling() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
}

export async function fetchTasks() {
  const body = document.getElementById('tasks-body');
  let data;
  try {
    data = await apiFetch('/api/tasks');
  } catch (e) {
    body.innerHTML = errorState(e.message, {retryAttr: 'data-tasks-refresh="1"'});
    return;
  }
  if (!data || !data.length) {
    body.innerHTML = '<div class="empty">No tasks registered.</div>';
    return;
  }
  const cats = {};
  for (const t of data) (cats[t.category] = cats[t.category] || []).push(t);
  let html = '';
  for (const cat of ORDER) {
    if (!cats[cat]) continue;
    html += `<h3 class="task-category">${esc(CAT_NAME[cat] || cat)}</h3>`;
    html += cats[cat].map(rowHTML).join('');
  }
  for (const cat of Object.keys(cats)) {
    if (ORDER.includes(cat)) continue;
    html += `<h3 class="task-category">${esc(cat)}</h3>` + cats[cat].map(rowHTML).join('');
  }
  body.innerHTML = html;
}

function rowHTML(t) {
  const dot = t.status === 'running' ? 'running' : t.status === 'error' ? 'error'
    : t.status === 'never' ? 'never' : 'idle';
  const glyph = dot === 'running' ? 'refresh' : dot === 'error' ? 'alert'
    : dot === 'never' ? 'minus' : 'check';
  const last = t.last_run ? relTime(t.last_run) : 'never';
  const next = t.next_run ? relTime(t.next_run)
    : (t.interval && String(t.interval).includes('manual') ? 'manual' : '—');
  const dur = t.last_duration ? ` · ${t.last_duration}` : '';
  const running = t.status === 'running';
  return `<div class="task-row">
    <span class="task-dot ${dot}" aria-hidden="true"></span>
    ${icon(glyph)}
    <div class="task-main">
      <div class="task-head">
        <span class="task-name">${esc(t.name)}</span>
        <span class="task-interval">${esc(t.interval || 'manual')}</span>
      </div>
      <div class="task-desc">${esc(t.description)}</div>
      ${t.last_error ? `<div class="task-err">${icon('alert')} ${esc(t.last_error)}</div>` : ''}
    </div>
    <div class="task-meta">
      last: ${esc(last)}${esc(dur)}<br>next: ${esc(next)}<br>runs: ${num(t.run_count)}
    </div>
    <button class="btn sm" type="button" ${running ? 'disabled' : ''}
      data-task-run="${esc(t.name)}">${running ? 'Running…' : 'Run now'}</button>
  </div>`;
}

async function runTask(name) {
  try {
    await apiFetch(`/api/tasks/${encodeURIComponent(name)}/run`, {method: 'POST'});
  } catch (e) { toast(e.message, {ok: false}); return; }
  toast(`Triggered ${name}`);
  setTimeout(fetchTasks, 600);
  stopTaskPolling();
  let n = 0;
  pollTimer = setInterval(() => {
    fetchTasks();
    if (++n >= 6) stopTaskPolling();
  }, 2000);
}
