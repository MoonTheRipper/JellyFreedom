/* ==========================================================================
   Horizontal rows a mouse can actually scroll.

   A row with a hidden scrollbar only scrolls sideways for a trackpad or a
   finger. With a mouse wheel there was no way along it at all — which is how
   the modal's "More like this" strips shipped cut off at the edge. One
   implementation, used by the home carousels and the modal strips alike.
   ========================================================================== */

/**
 * wireWheel turns a vertical wheel over any `selector` row inside `container`
 * into horizontal scrolling of that row. At either end of the row the event is
 * left alone, so the page (or the modal) carries on scrolling vertically
 * instead of the wheel going dead.
 */
export function wireWheel(container, selector) {
  container.addEventListener('wheel', e => {
    const track = e.target.closest(selector);
    if (!track || !container.contains(track)) return;
    if (Math.abs(e.deltaY) <= Math.abs(e.deltaX)) return;   // a trackpad's own sideways swipe
    if (track.scrollWidth <= track.clientWidth) return;
    // deltaMode 1 is lines (Firefox with a mouse wheel), 0 is pixels.
    const dy = e.deltaMode === 1 ? e.deltaY * 32 : e.deltaY;
    const before = track.scrollLeft;
    track.scrollLeft += dy;
    if (track.scrollLeft !== before) e.preventDefault();
  }, {passive: false});
}

/** page scrolls a row by most of its visible width, in direction dir (±1). */
export function page(track, dir) {
  track.scrollBy({left: dir * Math.max(240, track.clientWidth * 0.8), behavior: 'smooth'});
}

/** syncArrows enables each arrow only when there is somewhere to go. */
export function syncArrows(track, prev, next) {
  const max = track.scrollWidth - track.clientWidth - 2;
  const scrollable = max > 0;
  if (prev) prev.disabled = !scrollable || track.scrollLeft <= 2;
  if (next) next.disabled = !scrollable || track.scrollLeft >= max;
  return scrollable;
}
