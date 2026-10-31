/* ==========================================================================
   One pager for every long list (Library sections, Queue groups).

   The markup carries data-pg="<key>" and data-to="<page>"; each page owns its
   click handling and its page numbers, this module only draws and clamps.
   ========================================================================== */

import {icon} from '../shared/icons.js';

/** clamp keeps a page number inside 1..last for a list of `total` items. */
export function clamp(page, total, size) {
  const last = Math.max(1, Math.ceil(total / size));
  return Math.min(Math.max(1, page || 1), last);
}

/** slice returns the items on `page`. */
export function slice(list, page, size) {
  return list.slice((page - 1) * size, page * size);
}

/**
 * html draws "‹ 25–48 of 230 ›", or nothing when everything fits on one page.
 * `noun` only names the buttons for screen readers ("Next movies").
 */
export function html(key, total, page, size, noun) {
  const last = Math.ceil(total / size);
  if (last <= 1) return '';
  const from = (page - 1) * size + 1;
  const to = Math.min(total, page * size);
  return `<button type="button" class="pg-btn" data-pg="${key}" data-to="${page - 1}"
      ${page <= 1 ? 'disabled' : ''} aria-label="Previous ${noun}">${icon('chevron-left')}</button>
    <span class="pg-label">${from}–${to} <span class="pg-of">of ${total}</span></span>
    <button type="button" class="pg-btn" data-pg="${key}" data-to="${page + 1}"
      ${page >= last ? 'disabled' : ''} aria-label="Next ${noun}">${icon('chevron-right')}</button>`;
}

/** reveal scrolls `section` back under the sticky header if a page turn at the
 *  foot of a long list left its heading above the screen. */
export function reveal(section) {
  if (!section) return;
  const top = section.getBoundingClientRect().top;
  if (top < 70) window.scrollTo({top: window.scrollY + top - 76, behavior: 'instant'});
}
