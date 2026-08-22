/* ==========================================================================
   Release calendar.

   Fixed here: every cell and pill was a <div onclick> with a title
   interpolated into the attribute — both an XSS sink and an apostrophe bomb,
   and unreachable by keyboard or D-pad. Cells are now real buttons with
   data-* payloads.
   ========================================================================== */

import {apiFetch, esc, num, errorState} from '../shared/api.js';
import {icon} from '../shared/icons.js';
import {state, savePrefs} from './state.js';
import {navItem} from './router.js';

const MONTHS = ['January', 'February', 'March', 'April', 'May', 'June',
  'July', 'August', 'September', 'October', 'November', 'December'];
const DOW = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const KIND_LABEL = {subscription: 'Subscribed', tv_premiere: 'TV', movie: 'Movie'};

let calData = {entries: [], today: ''};
let calMonth = null;        // Date set to the 1st of the displayed month
let calSelectedDay = '';

function el(id) { return document.getElementById(id); }

export function initCalendar() {
  el('page-calendar').addEventListener('click', e => {
    const nav = e.target.closest('[data-cal-nav]');
    if (nav) { navItem(nav.dataset.type, Number(nav.dataset.tmdbId)); return; }
    const shift = e.target.closest('[data-cal-shift]');
    if (shift) { shiftMonth(Number(shift.dataset.calShift)); return; }
    const view = e.target.closest('[data-cal-view]');
    if (view) {
      state.prefs.calView = view.dataset.calView;
      savePrefs();
      syncView();
      render();
      return;
    }
    const day = e.target.closest('[data-cal-day]');
    if (day) { showDay(day.dataset.calDay); return; }
    if (e.target.closest('[data-cal-retry]')) loadCalendar();
  });
}

export async function loadCalendar() {
  const body = el('calendar-body');
  body.innerHTML = calSkeleton();
  try {
    const d = await apiFetch('/api/calendar');
    calData = (d && Array.isArray(d.entries)) ? d : {entries: [], today: ''};
  } catch (e) {
    body.innerHTML = errorState(e.message, {retryAttr: 'data-cal-retry="1"'});
    return;
  }
  if (!calMonth) {
    const t = calData.today ? new Date(calData.today + 'T00:00:00') : new Date();
    calMonth = new Date(t.getFullYear(), t.getMonth(), 1);
  }
  syncView();
  render();
}

function calSkeleton() {
  const cells = Array(35).fill(0).map(() =>
    `<div class="cal-cell skeleton" aria-hidden="true"></div>`).join('');
  return `<div class="cal-grid">${DOW.map(d => `<div class="cal-dow">${d}</div>`).join('')}${cells}</div>`;
}

function syncView() {
  const v = state.prefs.calView;
  for (const b of document.querySelectorAll('[data-cal-view]')) {
    const on = b.dataset.calView === v;
    b.classList.toggle('active', on);
    b.setAttribute('aria-pressed', String(on));
  }
  el('cal-month-nav').style.visibility = v === 'month' ? 'visible' : 'hidden';
}

function shiftMonth(delta) {
  if (!calMonth) return;
  calMonth = new Date(calMonth.getFullYear(), calMonth.getMonth() + delta, 1);
  calSelectedDay = '';
  render();
}

function entriesByDate() {
  const map = {};
  for (const e of calData.entries) (map[e.date] = map[e.date] || []).push(e);
  return map;
}

function render() {
  if (state.prefs.calView === 'list') renderList(); else renderMonth();
}

function renderMonth() {
  el('cal-month-label').textContent = `${MONTHS[calMonth.getMonth()]} ${calMonth.getFullYear()}`;
  const byDate = entriesByDate();
  const year = calMonth.getFullYear(), month = calMonth.getMonth();
  const firstDow = new Date(year, month, 1).getDay();
  const daysInMonth = new Date(year, month + 1, 0).getDate();
  const pad = n => String(n).padStart(2, '0');

  let cells = DOW.map(d => `<div class="cal-dow">${d}</div>`).join('');
  for (let i = 0; i < firstDow; i++) cells += `<div class="cal-cell empty" aria-hidden="true"></div>`;
  for (let day = 1; day <= daysInMonth; day++) {
    const ds = `${year}-${pad(month + 1)}-${pad(day)}`;
    const items = byDate[ds] || [];
    const isToday = ds === calData.today;
    const kinds = [...new Set(items.map(i => i.kind))];
    const dots = kinds.map(k => `<span class="cal-dot ${esc(k)}" aria-hidden="true"></span>`).join('');
    const pills = items.slice(0, 2).map(i =>
      `<span class="cal-pill">${esc(i.title)}</span>`).join('');
    const more = items.length > 2 ? `<span class="cal-more">+${items.length - 2} more</span>` : '';
    const label = items.length
      ? `${DOW[new Date(ds + 'T00:00:00').getDay()]} ${day} — ${items.length} release${items.length === 1 ? '' : 's'}`
      : `${day}, nothing scheduled`;
    if (items.length) {
      cells += `<button type="button" class="cal-cell has-items${isToday ? ' today' : ''}"
        data-cal-day="${esc(ds)}" aria-label="${esc(label)}"
        aria-pressed="${ds === calSelectedDay}">
        <span class="cal-daynum">${day}</span>
        <span class="cal-pillrow">${pills}${more}</span>
        <span class="cal-dots">${dots}</span>
      </button>`;
    } else {
      cells += `<div class="cal-cell${isToday ? ' today' : ''}"><span class="cal-daynum">${day}</span></div>`;
    }
  }
  const legend = `<div class="cal-legend">
    <span><span class="cal-dot subscription" aria-hidden="true"></span> Subscribed episodes</span>
    <span><span class="cal-dot tv_premiere" aria-hidden="true"></span> TV on air</span>
    <span><span class="cal-dot movie" aria-hidden="true"></span> Upcoming movies</span>
  </div>`;
  const detail = calSelectedDay ? dayDetail(calSelectedDay, byDate[calSelectedDay] || []) : '';
  el('calendar-body').innerHTML = `<div class="cal-grid">${cells}</div>${legend}${detail}`;
}

function showDay(ds) {
  calSelectedDay = calSelectedDay === ds ? '' : ds;
  renderMonth();
  if (calSelectedDay) {
    setTimeout(() => document.querySelector('.cal-day-detail')
      ?.scrollIntoView({behavior: 'smooth', block: 'nearest'}), 50);
  }
}

function dayDetail(ds, items) {
  const d = new Date(ds + 'T00:00:00');
  const head = `${DOW[d.getDay()]} ${MONTHS[d.getMonth()]} ${d.getDate()}`;
  return `<div class="cal-day-detail">
    <h3>${esc(head)} — ${items.length} release${items.length === 1 ? '' : 's'}</h3>
    ${items.map(entryRow).join('')}
  </div>`;
}

function renderList() {
  const byDate = entriesByDate();
  const dates = Object.keys(byDate).sort();
  if (!dates.length) {
    el('calendar-body').innerHTML = `<div class="empty-state">${icon('calendar')}
      <p>No upcoming releases found</p>
      <p class="sub">Subscribe to an airing show and its new episodes are tracked here.</p></div>`;
    return;
  }
  let html = '';
  for (const ds of dates) {
    const d = new Date(ds + 'T00:00:00');
    const rel = relDayLabel(ds);
    const head = `${DOW[d.getDay()]} ${MONTHS[d.getMonth()].slice(0, 3)} ${d.getDate()}`;
    html += `<section class="cal-list-section">
      <h3 class="cal-list-date">${esc(head)}${rel ? ` <span class="rel">· ${esc(rel)}</span>` : ''}</h3>
      ${byDate[ds].map(entryRow).join('')}
    </section>`;
  }
  el('calendar-body').innerHTML = html;
}

function relDayLabel(ds) {
  if (!calData.today) return '';
  const a = new Date(ds + 'T00:00:00'), b = new Date(calData.today + 'T00:00:00');
  const diff = Math.round((a - b) / 86400000);
  if (diff === 0) return 'Today';
  if (diff === 1) return 'Tomorrow';
  if (diff > 1 && diff <= 7) return `in ${diff} days`;
  if (diff < 0) return 'aired';
  return '';
}

function entryRow(e) {
  const type = e.media_type === 'tv' ? 'tv' : 'movie';
  const poster = e.poster_url
    ? `<img class="cal-entry-poster" src="${esc(e.poster_url)}" loading="lazy" alt="">`
    : `<span class="cal-entry-poster-ph">${icon(type === 'tv' ? 'tv' : 'film')}</span>`;
  const kind = KIND_LABEL[e.kind] || 'Release';
  return `<button type="button" class="cal-entry" data-cal-nav="1"
      data-type="${type}" data-tmdb-id="${num(e.tmdb_id)}"
      aria-label="${esc(e.title)}${e.subtitle ? ' ' + esc(e.subtitle) : ''} — ${esc(kind)}">
    ${poster}
    <span class="cal-entry-info">
      <span class="cal-entry-title">${esc(e.title)}</span>
      <span class="cal-entry-sub">
        <span class="cal-kind ${esc(e.kind)}">${esc(kind)}</span>
        ${e.subtitle ? `<span>${esc(e.subtitle)}</span>` : ''}
      </span>
    </span>
  </button>`;
}
