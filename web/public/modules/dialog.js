/* ==========================================================================
   Dialog behaviour shared by the detail modal and the sign-in modal:
   focus trap, focus restore, body scroll lock, Escape.

   The scroll lock matters on phones: without it the page behind the mobile
   bottom sheet scrolls under your finger.
   ========================================================================== */

const FOCUSABLE = 'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),' +
  'textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';

let lockCount = 0;
let savedScrollY = 0;

function lockScroll() {
  if (lockCount++ > 0) return;
  savedScrollY = window.scrollY;
  document.body.style.top = `-${savedScrollY}px`;
  document.body.classList.add('scroll-locked');
}
function unlockScroll() {
  if (--lockCount > 0) return;
  lockCount = 0;
  document.body.classList.remove('scroll-locked');
  document.body.style.top = '';
  window.scrollTo({top: savedScrollY, behavior: 'auto'});
}

const stack = [];

/**
 * openDialog(overlayEl, paneEl, {onClose, onEscape, label})
 *   onEscape — if given, Escape calls this INSTEAD of closing directly.
 * Returns a handle with .close(). Safe to call twice on the same dialog:
 * the second call just refreshes focus.
 */
export function openDialog(overlay, pane, {onClose, onEscape, label} = {}) {
  const already = stack.find(d => d.overlay === overlay);
  if (already) { focusFirst(pane); return already; }

  pane.setAttribute('role', 'dialog');
  pane.setAttribute('aria-modal', 'true');
  if (label) pane.setAttribute('aria-label', label);
  if (!pane.hasAttribute('tabindex')) pane.setAttribute('tabindex', '-1');

  const entry = {overlay, pane, onClose, onEscape, restoreTo: document.activeElement, close};
  stack.push(entry);
  overlay.classList.remove('hidden');
  overlay.removeAttribute('aria-hidden');
  lockScroll();

  entry.keyHandler = e => {
    if (stack[stack.length - 1] !== entry) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      // A dialog whose visibility is owned by the router (the detail modal)
      // must close by navigating, not by hiding itself — otherwise the URL
      // still points at the modal and the page scroll is not restored.
      if (entry.onEscape) entry.onEscape(); else close();
      return;
    }
    if (e.key !== 'Tab') return;
    const items = [...pane.querySelectorAll(FOCUSABLE)].filter(el => el.offsetParent !== null || el === document.activeElement);
    if (!items.length) { e.preventDefault(); pane.focus(); return; }
    const first = items[0], last = items[items.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  };
  document.addEventListener('keydown', entry.keyHandler, true);

  requestAnimationFrame(() => focusFirst(pane));

  function close() {
    const i = stack.indexOf(entry);
    if (i === -1) return;
    stack.splice(i, 1);
    document.removeEventListener('keydown', entry.keyHandler, true);
    overlay.classList.add('hidden');
    overlay.setAttribute('aria-hidden', 'true');
    unlockScroll();
    if (entry.restoreTo && document.contains(entry.restoreTo)) {
      try { entry.restoreTo.focus({preventScroll: true}); } catch (_) {}
    }
    if (onClose) onClose();
  }
  return entry;
}

/** closeDialog closes a dialog that was opened with openDialog. */
export function closeDialog(overlay) {
  const entry = stack.find(d => d.overlay === overlay);
  if (entry) entry.close();
}

export function isDialogOpen(overlay) {
  return stack.some(d => d.overlay === overlay);
}

/** refocus moves focus into the pane again after its contents were replaced. */
export function focusFirst(pane) {
  const target = pane.querySelector('[data-autofocus]') || pane.querySelector(FOCUSABLE) || pane;
  try { target.focus({preventScroll: true}); } catch (_) { /* ignore */ }
}
