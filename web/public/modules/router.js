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

const pageRenderers = new Map();
let itemOpener = null;
let modalIsOpen = () => false;
let closeModalDOM = () => {};
let onHomeReset = () => {};
let searchRunner = () => {};

export function registerPage(name, fn) { pageRenderers.set(name, fn); }
export function configure(opts) {
  itemOpener = opts.openItem;
  modalIsOpen = opts.modalIsOpen;
  closeModalDOM = opts.closeModalDOM;
  onHomeReset = opts.onHomeReset || onHomeReset;
  searchRunner = opts.runSearch || searchRunner;
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
  location.hash = currentPage === 'home' ? '' : '#' + currentPage;
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
  window.scrollTo({top: 0, behavior: 'auto'});
  const fn = pageRenderers.get(page);
  if (fn) fn();
}

export function handleRoute() {
  const raw = location.hash.replace(/^#/, '');
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
    if (!openNow) scrollBeforeModal = window.scrollY;
    setActiveNav(currentPage);            // keep the page visible behind the modal
    if (itemOpener) itemOpener({tmdb_id: id, media_type: head}, opts);
    return;
  }

  const page = head === 'search' ? 'home' : (PAGES.includes(head) ? head : 'home');

  // Closing a modal back onto the page already beneath it: hide the modal and
  // restore scroll — do NOT re-render the page or jump to the top.
  if (openNow && head !== 'search' && page === currentPage) {
    closeModalDOM();
    setActiveNav(page);
    if (page === 'home') onHomeReset();
    window.scrollTo({top: scrollBeforeModal, behavior: 'auto'});
    return;
  }

  closeModalDOM();
  if (head === 'search') {
    renderPage('home');
    const q = decodeURIComponent(parts.slice(1).join('/'));
    searchRunner(q);
    return;
  }
  if (page === 'home') onHomeReset();
  renderPage(page);
}

export function start() {
  window.addEventListener('hashchange', () => {
    if (location.hash === renderedRoute) return;   // we set it ourselves
    handleRoute();
  });
  handleRoute();
}
