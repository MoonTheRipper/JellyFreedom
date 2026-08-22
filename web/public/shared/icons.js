/* ==========================================================================
   JellyFreedom icon sprite.

   Why a JS module and not a plain shared/icons.svg referenced with
   <use href="icons.svg#id">: cross-DOCUMENT <use> is unreliable (Safari has
   never supported it without a polyfill, and it adds a network round-trip that
   can land after the first render, leaving blank boxes). Keeping the <symbol>
   set as a string and injecting it synchronously on module load gives us one
   copy, zero fetches, zero flash and no build step.

   Paths are hand-traced 24x24 outline glyphs in the Lucide idiom (ISC/MIT
   licensed geometry conventions — stroke width 2, round caps/joins). Nothing
   is hotlinked; everything below ships in-repo.
   ========================================================================== */

const SPRITE = `
<svg xmlns="http://www.w3.org/2000/svg" style="display:none" aria-hidden="true" id="jf-icon-sprite">
<symbol id="i-home" viewBox="0 0 24 24"><path d="M3 10.5 12 3l9 7.5"/><path d="M5 9.5V20a1 1 0 0 0 1 1h4v-6h4v6h4a1 1 0 0 0 1-1V9.5"/></symbol>
<symbol id="i-grid" viewBox="0 0 24 24"><rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/></symbol>
<symbol id="i-list" viewBox="0 0 24 24"><path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01"/></symbol>
<symbol id="i-calendar" viewBox="0 0 24 24"><rect x="3" y="5" width="18" height="16" rx="2"/><path d="M3 10h18M8 3v4M16 3v4"/></symbol>
<symbol id="i-search" viewBox="0 0 24 24"><circle cx="11" cy="11" r="7"/><path d="m20 20-3.6-3.6"/></symbol>
<symbol id="i-play" viewBox="0 0 24 24"><path d="M6 4.5 19 12 6 19.5z"/></symbol>
<symbol id="i-plus" viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></symbol>
<symbol id="i-check" viewBox="0 0 24 24"><path d="m4 12.5 5 5L20 6.5"/></symbol>
<symbol id="i-x" viewBox="0 0 24 24"><path d="M6 6l12 12M18 6 6 18"/></symbol>
<symbol id="i-alert" viewBox="0 0 24 24"><path d="M12 3.5 22 20H2z"/><path d="M12 10v4M12 17.5h.01"/></symbol>
<symbol id="i-info" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 8h.01"/></symbol>
<symbol id="i-refresh" viewBox="0 0 24 24"><path d="M20 11a8 8 0 0 0-13.7-5.2L3 9"/><path d="M3 4v5h5"/><path d="M4 13a8 8 0 0 0 13.7 5.2L21 15"/><path d="M21 20v-5h-5"/></symbol>
<symbol id="i-clock" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3.2 2"/></symbol>
<symbol id="i-pause" viewBox="0 0 24 24"><rect x="7" y="5" width="3.5" height="14" rx="1"/><rect x="13.5" y="5" width="3.5" height="14" rx="1"/></symbol>
<symbol id="i-trash" viewBox="0 0 24 24"><path d="M4 7h16M9 7V5a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2"/><path d="M6 7v12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V7"/><path d="M10 11v6M14 11v6"/></symbol>
<symbol id="i-chevron-right" viewBox="0 0 24 24"><path d="m9 5 7 7-7 7"/></symbol>
<symbol id="i-chevron-left" viewBox="0 0 24 24"><path d="m15 5-7 7 7 7"/></symbol>
<symbol id="i-chevron-down" viewBox="0 0 24 24"><path d="m5 9 7 7 7-7"/></symbol>
<symbol id="i-arrow-left" viewBox="0 0 24 24"><path d="M20 12H4M10 6l-6 6 6 6"/></symbol>
<symbol id="i-external" viewBox="0 0 24 24"><path d="M14 4h6v6"/><path d="M20 4 11 13"/><path d="M19 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1h5"/></symbol>
<symbol id="i-link" viewBox="0 0 24 24"><path d="M10 13a4 4 0 0 0 5.7.4l3-3A4 4 0 0 0 13 4.7l-1.7 1.7"/><path d="M14 11a4 4 0 0 0-5.7-.4l-3 3A4 4 0 0 0 11 19.3l1.7-1.7"/></symbol>
<symbol id="i-bell" viewBox="0 0 24 24"><path d="M18 8a6 6 0 1 0-12 0c0 6-2 7-2 7h16s-2-1-2-7"/><path d="M10.3 20a2 2 0 0 0 3.4 0"/></symbol>
<symbol id="i-bell-off" viewBox="0 0 24 24"><path d="M8.7 4.5A6 6 0 0 1 18 8c0 2.3.3 3.9.7 5"/><path d="M6 9v-1M6 10c0 5-2 5-2 5h12"/><path d="M10.3 20a2 2 0 0 0 3.4 0"/><path d="m3 3 18 18"/></symbol>
<symbol id="i-film" viewBox="0 0 24 24"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M7 4v16M17 4v16M3 9h4M17 9h4M3 15h4M17 15h4"/></symbol>
<symbol id="i-tv" viewBox="0 0 24 24"><rect x="2" y="7" width="20" height="14" rx="2"/><path d="m7 3 5 4 5-4"/></symbol>
<symbol id="i-library" viewBox="0 0 24 24"><path d="M4 4v16M9 4v16"/><path d="m14 5 5 15"/><path d="M2 20h20"/></symbol>
<symbol id="i-shield" viewBox="0 0 24 24"><path d="M12 3 5 6v5.5c0 4.4 3 8 7 9.5 4-1.5 7-5.1 7-9.5V6z"/></symbol>
<symbol id="i-shield-off" viewBox="0 0 24 24"><path d="M12 3 5 6v5.5c0 4.4 3 8 7 9.5 4-1.5 7-5.1 7-9.5V6z"/><path d="m4 4 16 16"/></symbol>
<symbol id="i-activity" viewBox="0 0 24 24"><path d="M3 12h4l3-8 4 16 3-8h4"/></symbol>
<symbol id="i-users" viewBox="0 0 24 24"><path d="M16 20v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="3.5"/><path d="M22 20v-2a4 4 0 0 0-3-3.8"/><path d="M16 3.7a4 4 0 0 1 0 7.6"/></symbol>
<symbol id="i-settings" viewBox="0 0 24 24"><circle cx="12" cy="12" r="3"/><path d="M19.4 14.5a1.5 1.5 0 0 0 .3 1.7l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.5 1.5 0 0 0-2.5 1v.2a2 2 0 1 1-4 0v-.1a1.5 1.5 0 0 0-2.6-1l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.5 1.5 0 0 0-1-2.5H4a2 2 0 1 1 0-4h.1a1.5 1.5 0 0 0 1-2.6l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.5 1.5 0 0 0 2.5-1V4a2 2 0 1 1 4 0v.1a1.5 1.5 0 0 0 2.5 1l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.5 1.5 0 0 0 1 2.5h.2a2 2 0 1 1 0 4h-.1a1.5 1.5 0 0 0-1.4 1"/></symbol>
<symbol id="i-download" viewBox="0 0 24 24"><path d="M12 3v12"/><path d="m7 11 5 5 5-5"/><path d="M4 20h16"/></symbol>
<symbol id="i-upload" viewBox="0 0 24 24"><path d="M12 16V4"/><path d="m7 8 5-5 5 5"/><path d="M4 20h16"/></symbol>
<symbol id="i-power" viewBox="0 0 24 24"><path d="M12 3v9"/><path d="M6.3 6.3a8 8 0 1 0 11.4 0"/></symbol>
<symbol id="i-key" viewBox="0 0 24 24"><circle cx="8" cy="15" r="4"/><path d="m11 12 9-9"/><path d="m17 6 2.5 2.5"/><path d="m14.5 8.5 2.5 2.5"/></symbol>
<symbol id="i-flame" viewBox="0 0 24 24"><path d="M12 3c3 4 5 6 5 9a5 5 0 0 1-10 0c0-1.5.6-2.8 1.5-4 .4 1 1 1.6 1.8 2C10.2 7.8 11 5.4 12 3"/></symbol>
<symbol id="i-satellite" viewBox="0 0 24 24"><path d="M4 13a8 8 0 0 1 7 7"/><path d="M4 17a4 4 0 0 1 3 3"/><circle cx="5" cy="20" r="1.4"/><path d="M14 10 21 3"/><path d="M16 3h5v5"/></symbol>
<symbol id="i-book" viewBox="0 0 24 24"><path d="M4 4.5A2.5 2.5 0 0 1 6.5 2H20v18H6.5A2.5 2.5 0 0 0 4 22z"/></symbol>
<symbol id="i-filter" viewBox="0 0 24 24"><path d="M4 5h16l-6 7v6l-4 2v-8z"/></symbol>
<symbol id="i-help" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M9.5 9.5a2.5 2.5 0 1 1 3.4 2.3c-.6.3-.9.8-.9 1.4v.3"/><path d="M12 17h.01"/></symbol>
<symbol id="i-minus" viewBox="0 0 24 24"><path d="M5 12h14"/></symbol>
</svg>`;

let injected = false;

/** injectSprite adds the <symbol> set to the document exactly once. */
export function injectSprite() {
  if (injected || typeof document === 'undefined') return;
  injected = true;
  const host = document.body || document.documentElement;
  host.insertAdjacentHTML('afterbegin', SPRITE);
}

/**
 * icon returns an <svg><use> snippet for a sprite symbol.
 * `name` is a fixed literal from the set above, never user data — but it is
 * still whitelisted so a caller typo cannot inject markup.
 */
const NAME_RE = /^[a-z0-9-]+$/;
export function icon(name, cls = '') {
  const n = NAME_RE.test(name) ? name : 'help';
  const c = NAME_RE.test(cls) || cls === '' ? cls : '';
  return `<svg class="ic ${c}" aria-hidden="true"><use href="#i-${n}"></use></svg>`;
}

injectSprite();
if (typeof document !== 'undefined' && !document.body) {
  document.addEventListener('DOMContentLoaded', () => {
    injected = false;
    injectSprite();
  }, {once: true});
}
