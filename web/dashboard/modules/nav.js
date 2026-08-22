/* ==========================================================================
   Dashboard section routing.

   Sections are hash-addressable now (#setup, #health, #vpn, #logs, #users,
   #tasks, #settings) so the public app's setup banner and the checklist can
   deep-link straight at the thing that needs fixing.
   ========================================================================== */

const SECTIONS = ['setup', 'health', 'vpn', 'logs', 'users', 'tasks', 'settings'];

const enter = new Map();   // section -> fn run when it becomes visible
const leave = new Map();   // section -> fn run when it stops being visible

let current = null;

export function onEnter(id, fn) { enter.set(id, fn); }
export function onLeave(id, fn) { leave.set(id, fn); }
export function currentSection() { return current; }

export function showSection(id, {replace = false} = {}) {
  if (!SECTIONS.includes(id)) id = 'health';
  if (current === id) return;
  const prev = current;
  current = id;

  document.querySelectorAll('.section').forEach(s => s.classList.remove('active'));
  document.querySelectorAll('.nav-item').forEach(n => {
    const on = n.dataset.section === id;
    n.classList.toggle('active', on);
    n.setAttribute('aria-current', on ? 'page' : 'false');
  });
  const sec = document.getElementById('section-' + id);
  if (sec) sec.classList.add('active');

  const target = '#' + id;
  if (location.hash !== target) {
    if (replace) history.replaceState(null, '', target);
    else location.hash = target;
  }

  // Leaving Logs must stop the auto-refresh timer; it used to keep polling
  // journalctl forever in the background.
  if (prev && leave.has(prev)) leave.get(prev)();
  if (enter.has(id)) enter.get(id)();
}

/**
 * start picks the landing section. An explicit hash always wins. Otherwise a
 * fresh install lands on the setup checklist rather than on Health, which
 * tells a new user nothing about what is still missing.
 */
export function start({defaultSection = 'health'} = {}) {
  document.querySelector('.sidebar').addEventListener('click', e => {
    const item = e.target.closest('.nav-item');
    if (item) showSection(item.dataset.section);
  });
  window.addEventListener('hashchange', () => {
    const id = location.hash.replace(/^#/, '');
    if (SECTIONS.includes(id) && id !== current) showSection(id);
  });
  const fromHash = location.hash.replace(/^#/, '');
  showSection(SECTIONS.includes(fromHash) ? fromHash : defaultSection, {replace: true});
}
