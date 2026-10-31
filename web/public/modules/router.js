/* ==========================================================================
   Hash routing. Survives reload, gives back/forward, and makes every title
   deep-linkable.

   Routes:  (empty) | #library | #queue | #calendar
            #search/<encoded query>
            #movie/<id> | #tv/<id>[/s/<season>[/e/<episode>]]

   The season/episode tail is new: "Retry" on a failed TV item used to navigate
   to the show with nothing selected, so the user had to hunt for the episode
   that failed. Now the route carries it.
   ========================================================================== */

const PAGES = ['home', 'library', 'queue', 'calendar'];

let currentPage = 'home';
let renderedRoute = '';     // last hash WE rendered, so self-set changes are ignored
let scrollBeforeModal = 0;
let nextItemOpts = null;    // consumed by the next openItem()

/* The modal's own history. baseRoute is the view the modal was opened over —
   '', '#library', or '#search/<q>' — and closing returns to exactly that, so a
   search survives looking at one of its results. trail is every title opened
   since, oldest first, each with the modal scroll it was left at, which is what
   drives the Back arrow and puts you back where you were reading. */
let baseRoute = '';
let trail = [];             // [{hash, scroll, title}]

const pageRenderers = new Map();
let itemOpener = null;
let modalIsOpen = () => false;
let closeModalDOM = () => {};
let onHomeReset = () => {};
let modalScrollTop = () => 0;
let searchRunner = () => {};

export function registerPage(name, fn) { pageRenderers.set(name, fn); }
export function configure(opts) {
  itemOpener = opts.openItem;
  modalIsOpen = opts.modalIsOpen;
  closeModalDOM = opts.closeModalDOM;
  onHomeReset = opts.onHomeReset || onHomeReset;
  searchRunner = opts.runSearch || searchRunner;
  modalScrollTop = opts.modalScrollTop || modalScrollTop;
}

export function getPage() { return currentPage; }

/* ── Navigation ──────────────────────────────────────────────────────────── */
export function showPage(name) { location.hash = name === 'home' ? '' : '#' + name; }

/**
 * navItem(type, id, opts)
 *   opts.forcePicker      pre-expand the release picker
 *   opts.previousRelease  release title to highlight as "previous"
 *   opts.season/.episode  deep-select a TV episode (Retry path)
 */
export function navItem(type, id, opts) {
  nextItemOpts = opts || null;
  let h = `#${type}/${id}`;
  if (opts && opts.season) {
    h += `/s/${opts.season}`;
    if (opts.episode) h += `/e/${opts.episode}`;
  }
  if (location.hash === h) { handleRoute(); return; }  // re-open same target
  location.hash = h;
}
export function navSearch(q) { location.hash = '#search/' + encodeURIComponent(q); }

/** syncHash mirrors the current view in the URL WITHOUT re-rendering. */
export function syncHash(h) {
  renderedRoute = h;
  if (location.hash !== h && !(h === '' && location.hash === '')) location.hash = h;
}
/** noteRoute records a hash we set via replaceState (search-as-you-type). */
export function noteRoute(h) { renderedRoute = h; }

export function closeModalRoute() {
  location.hash = baseRoute;
}

/** canModalGoBack: is there an earlier title in this modal session? */
export function canModalGoBack() { return trail.length > 1; }
/** previousModalTitle names the title the Back arrow returns to, when known. */
export function previousModalTitle() {
  return trail.length > 1 ? (trail[trail.length - 2].title || '') : '';
}
/** noteModalTitle records the open title's name for the Back arrow's label. */
export function noteModalTitle(title) {
  if (trail.length) trail[trail.length - 1].title = title || '';
}

/**
 * modalBack returns to the previous title. Every title opened from inside the
 * modal was a real hash navigation, so the browser's own history already holds
 * the previous one: going back through it keeps the arrow, the browser Back
 * button and a phone's back gesture in step instead of three competing stacks.
 */
export function modalBack() {
  if (trail.length < 2) return;
  history.back();
}

/* ── Rendering ───────────────────────────────────────────────────────────── */
export function setActiveNav(page) {
  document.querySelectorAll('.page').forEach(p => p.classList.remove('active'));
  document.querySelectorAll('.nav-link').forEach(n => {
    const on = n.dataset.page === page;
    n.classList.toggle('active', on);
    n.setAttribute('aria-current', on ? 'page' : 'false');
  });
  const el = document.getElementById('page-' + page);
  if (el) el.classList.add('active');
}

function renderPage(page) {
  currentPage = page;
  setActiveNav(page);
  window.scrollTo({top: 0, behavior: 'instant'});
  const fn = pageRenderers.get(page);
  if (fn) fn();
}

export function handleRoute() {
  const raw = location.hash.replace(/^#/, '');
  const fromRoute = renderedRoute;
  renderedRoute = location.hash;
  const parts = raw.split('/');
  const head = parts[0] || 'home';
  const openNow = modalIsOpen();

  if (head === 'movie' || head === 'tv') {
    const id = parseInt(parts[1], 10);
    if (!id) { location.hash = ''; return; }
    const opts = Object.assign({}, nextItemOpts || {});
    nextItemOpts = null;
    // /s/<n>[/e/<n>] tail
    for (let i = 2; i < parts.length - 1; i++) {
      if (parts[i] === 's') opts.season = parseInt(parts[i + 1], 10) || null;
      if (parts[i] === 'e') opts.episode = parseInt(parts[i + 1], 10) || null;
    }
    const here = `#${head}/${id}`;
    if (!openNow) {
      scrollBeforeModal = window.scrollY;
      // A title opened straight from the address bar has nothing beneath it but
      // the page it lands on; an item hash is never a base.
      baseRoute = /^#(movie|tv)\//.test(fromRoute) ? '' : fromRoute;
      trail = [{hash: here, scroll: 0, title: ''}];
    } else {
      const prev = trail.length > 1 ? trail[trail.length - 2] : null;
      const cur = trail[trail.length - 1];
      if (prev && prev.hash === here) {
        trail.pop();                      // Back — the arrow or the browser's
        opts.restoreScroll = prev.scroll;
      } else if (!cur || cur.hash !== here) {
        // Remember where the title being left was read to; Back returns there.
        if (cur) cur.scroll = modalScrollTop();
        trail.push({hash: here, scroll: 0, title: ''});
      }
    }
    setActiveNav(currentPage);            // keep the page visible behind the modal
    if (itemOpener) itemOpener({tmdb_id: id, media_type: head}, opts);
    return;
  }

  const page = head === 'search' ? 'home' : (PAGES.includes(head) ? head : 'home');

  // Closing a modal back onto the view beneath it — the page, or the search it
  // was opened from: hide the modal and put the scroll back. Nothing is
  // re-rendered or re-searched, so results and scroll are exactly as left.
  // 'instant', never 'auto': auto means "whatever the CSS says", and a smooth
  // scroll-behavior there turned every close into a long animated slide.
  if (openNow && sameRoute(location.hash, baseRoute) && page === currentPage) {
    closeModalDOM();
    trail = [];
    setActiveNav(page);
    window.scrollTo({top: scrollBeforeModal, behavior: 'instant'});
    return;
  }

  closeModalDOM();
  trail = [];
  if (head === 'search') {
    renderPage('home');
    const q = decodeURIComponent(parts.slice(1).join('/'));
    searchRunner(q);
    return;
  }
  if (page === 'home') onHomeReset();
  renderPage(page);
}

/** sameRoute compares two hashes the way a person would: '#' is '', and a
 *  search typed with a space matches the same search the browser re-encoded. */
function sameRoute(a, b) {
  const norm = h => {
    h = String(h || '').replace(/^#/, '');
    try { return decodeURIComponent(h); } catch (_) { return h; }
  };
  return norm(a) === norm(b);
}

export function start() {
  window.addEventListener('hashchange', () => {
    if (location.hash === renderedRoute) return;   // we set it ourselves
    handleRoute();
  });
  handleRoute();
}
