// The install command lives in the markup once; this reads it from the DOM so
// the two can never drift apart.
document.addEventListener('click', function (e) {
  var btn = e.target.closest('[data-copy-target]');
  if (!btn) return;
  var src = document.getElementById(btn.getAttribute('data-copy-target'));
  if (!src) return;
  var text = src.textContent.trim();
  function done(ok) {
    btn.textContent = ok ? 'Copied!' : 'Press Ctrl+C';
    if (!ok) selectText(src);
    setTimeout(function () { btn.textContent = 'Copy'; }, 1800);
  }
  // navigator.clipboard is undefined on plain http and rejects when the page
  // is not focused, so both the missing-API and the rejection path need a
  // fallback — otherwise the button silently does nothing.
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(function () { done(true); },
                                            function () { done(false); });
  } else {
    done(false);
  }
});
function selectText(el) {
  try {
    var r = document.createRange();
    r.selectNodeContents(el);
    var sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(r);
  } catch (_) { /* selection is a convenience, not a requirement */ }
}
