/* ==========================================================================
   JellyFreedom media app — entry point.
   Wires the modules together; no rendering logic of its own.
   ========================================================================== */

import {setUnauthorizedHandler, visibleInterval, esc} from './shared/api.js';
import './shared/icons.js';
import {state, restorePrefs, loadLibraries, loadMyLibrary, loadSubscriptions, on, emit} from './modules/state.js';
import * as router from './modules/router.js';
import {initAuth, updateAuthUI, refreshAuth, onUnauthorized} from './modules/auth.js';
import {initHealthUI, refreshConfigured, refreshHealth} from './modules/health.js';
import {initSearch, clearSearch, routeSearch} from './modules/search.js';
import {renderCarousels} from './modules/carousels.js';
import {loadHero} from './modules/hero.js';
import {initLibrary, renderLibraryPage} from './modules/library.js';
import {initQueue, pollQueue, renderQueuePage} from './modules/queue.js';
import {initCalendar, loadCalendar} from './modules/calendar.js';
import {initModal, openItem, modalIsOpen, closeModalDOM} from './modules/modal.js';

function el(id) { return document.getElementById(id); }

async function init() {
  restorePrefs();

  initAuth();
  initHealthUI();
  initSearch();
  initLibrary();
  initQueue();
  initCalendar();
  initModal();

  router.registerPage('home', () => { document.dispatchEvent(new CustomEvent('jf:layout')); });
  // Leaving Home leaves its search behind; the header box is cleared so it does
  // not show a query whose results are no longer on screen. Back restores both.
  const leaveSearch = () => { el('q').value = ''; clearSearch(); };
  router.registerPage('library', () => { leaveSearch(); renderLibraryPage(); });
  router.registerPage('queue', () => { leaveSearch(); pollQueue(); renderQueuePage(); });
  router.registerPage('calendar', () => { leaveSearch(); loadCalendar(); });
  router.configure({
    openItem,
    modalIsOpen,
    closeModalDOM,
    onHomeReset: () => { el('q').value = ''; clearSearch(); },
    runSearch: routeSearch,
    modalScrollTop: () => el('modal').scrollTop,
  });

  // Top nav + any card anywhere outside a module-owned container.
  document.querySelector('.main-nav').addEventListener('click', e => {
    const link = e.target.closest('.nav-link');
    if (link) router.showPage(link.dataset.page);
  });
  document.addEventListener('click', e => {
    const card = e.target.closest('[data-nav-item]');
    if (!card) return;
    if (e.target.closest('#modal-content')) return;   // modal handles its own
    router.navItem(card.dataset.type, Number(card.dataset.tmdbId));
  });

  // The FIRST auth probe is expected to 401 for a signed-out visitor — that is the
  // answer to "who am I", not a failure. The global handler was armed before it, so
  // apiFetch's 401 branch fired onUnauthorized and every anonymous arrival got the
  // sign-in modal thrown at them before they had asked for anything. (Catching the
  // rejection in loadMe does not help: apiFetch calls the handler and THEN throws.)
  //
  // Arming it afterwards keeps the meaning it should have had all along: a 401 after
  // we know who you are means the session went away, which is worth interrupting for.
  // Nothing between here and the init* calls above touches the API — they only
  // register listeners — so there is no window where a 401 goes unhandled.
  await refreshAuth();
  setUnauthorizedHandler(onUnauthorized);

  // Configured state first: it is what turns a blank first-run page into an
  // explained one, and it must not wait behind sixteen browse rows.
  refreshConfigured().catch(() => {});
  refreshHealth().catch(() => {});
  visibleInterval(() => { refreshHealth().catch(() => {}); }, 30000);
  visibleInterval(() => { refreshConfigured().catch(() => {}); }, 120000);

  loadLibraries();
  loadMyLibrary().catch(() => {});
  loadSubscriptions();
  visibleInterval(() => { loadMyLibrary().catch(() => {}); }, 60000);

  loadHero();
  renderCarousels();
  pollQueue();
  visibleInterval(pollQueue, 4000);

  window.addEventListener('scroll', onScrollHeader, {passive: true});
  document.addEventListener('jf:layout', onScrollHeader);
  onScrollHeader();

  document.addEventListener('keydown', onGlobalKey);
  on('auth', () => {
    updateAuthUI();
    // Who you are decides what the library, the library list and the
    // subscriptions answer with — an anonymous caller may be told nothing at
    // all — so all three are fetched again on every change of identity. A
    // visitor who signed in on the Library page used to keep its anonymous
    // answer ("Nothing here yet") until the next 60-second poll, and signing
    // out kept the whole library on screen. On sign-out the library is
    // cleared first, so the old answer is not shown for even one fetch.
    // (The boot-time probe fires before this is registered: no double fetch.)
    if (!state.user) { state.myLibrary = []; state.libraryStatus = {}; emit('library'); }
    loadMyLibrary().catch(() => {});
    loadLibraries();
    loadSubscriptions();
  });

  router.start();
}

/** Header turns transparent while the home hero is under it. */
function onScrollHeader() {
  const header = document.querySelector('header');
  if (!header) return;
  const onHome = el('page-home').classList.contains('active');
  const hero = el('hero');
  const heroVisible = onHome && hero && !hero.hidden && hero.offsetHeight > 0;
  header.classList.toggle('transparent', !!heroVisible && window.scrollY < (hero.offsetHeight - 120));
}

function onGlobalKey(e) {
  if (e.key === '/' && !/^(INPUT|TEXTAREA|SELECT)$/.test((document.activeElement || {}).tagName)) {
    e.preventDefault();
    if (modalIsOpen()) return;          // the header sits behind the modal
    el('q').focus();
    el('q').select();
  }
}

init().catch(e => {
  console.error('[jf] init failed', e);
  const host = document.getElementById('setup-banner');
  if (host) {
    host.hidden = false;
    host.innerHTML = `<div class="callout err"><div class="callout-body">
      <div class="callout-title">The app failed to start</div>
      <p>${esc(String(e && e.message ? e.message : e))}</p></div></div>`;
  }
});
