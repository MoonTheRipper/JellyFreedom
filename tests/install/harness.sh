#!/usr/bin/env bash
# harness.sh — hermetic sandbox for exercising release/install.sh without root.
#
# The installer is run against a fake filesystem root (JF_DESTDIR) with a mock PATH in
# front, so every privileged/network command it calls is intercepted and recorded instead
# of executed. Tests then assert on (a) what the installer *did* to the fake root and
# (b) the exact sequence of commands it *tried* to run.
#
# Usage from a test file:
#     source "$(dirname "$0")/harness.sh"
#     sandbox_new
#     mock_cmd curl 0                 # succeed
#     mock_cmd apt-get 0
#     run_installer
#     assert_exit 0
#     assert_ran 'systemctl enable --now jellyfreedom.service'
#     assert_file "$DEST/etc/jellyfreedom/config.yaml"
#
# No root, no network, no side effects outside $TESTROOT.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TESTROOT="${TESTROOT:-$(mktemp -d "${TMPDIR:-/tmp}/jf-install-test.XXXXXX")}"

# ---- counters ----
TESTS_RUN=0; TESTS_FAILED=0; CURRENT_TEST=""; FAILED_NAMES=()

c_red=$'\033[1;31m'; c_grn=$'\033[1;32m'; c_ylw=$'\033[1;33m'; c_dim=$'\033[2m'; c_off=$'\033[0m'

# ------------------------------------------------------------------ sandbox
# sandbox_new [name] — fresh fake root + mock bin dir for one scenario.
sandbox_new() {
  CURRENT_TEST="${1:-scenario-$((TESTS_RUN + 1))}"
  SANDBOX="$TESTROOT/$(printf '%s' "$CURRENT_TEST" | tr -c 'a-zA-Z0-9._-' '_')"
  rm -rf "$SANDBOX"
  DEST="$SANDBOX/root"          # fake filesystem root the installer writes into
  MOCKBIN="$SANDBOX/mockbin"    # intercepted commands
  LOG="$SANDBOX/commands.log"   # one line per intercepted invocation
  OUTPUT="$SANDBOX/output.txt"  # installer stdout+stderr
  BUNDLE="$SANDBOX/bundle"
  mkdir -p "$DEST" "$MOCKBIN" "$BUNDLE"
  : > "$LOG"
  stage_bundle
  # A believable base system inside the fake root.
  mkdir -p "$DEST/etc/systemd/system" "$DEST/etc/apparmor.d" "$DEST/usr/local/bin" \
           "$DEST/opt" "$DEST/var/lib" "$DEST/etc/sudoers.d" "$DEST/var/log"
  EXIT_CODE=""
}

# stage_bundle — assemble what release/build.sh produces, so the installer sees a real
# bundle layout. Uses the live repo scripts and stubs the compiled artefacts.
stage_bundle() {
  cp "$REPO_ROOT/release/install.sh" "$REPO_ROOT/release/uninstall.sh" \
     "$REPO_ROOT/release/jellyfreedom" "$REPO_ROOT/release/config.sample.yaml" "$BUNDLE/"
  [ -f "$REPO_ROOT/release/doctor.sh" ] && cp "$REPO_ROOT/release/doctor.sh" "$BUNDLE/"
  [ -f "$REPO_ROOT/release/jf-update" ] && cp "$REPO_ROOT/release/jf-update" "$BUNDLE/"
  chmod +x "$BUNDLE/install.sh" "$BUNDLE/uninstall.sh" "$BUNDLE/jellyfreedom"
  [ -f "$BUNDLE/doctor.sh" ] && chmod +x "$BUNDLE/doctor.sh"
  [ -f "$BUNDLE/jf-update" ] && chmod +x "$BUNDLE/jf-update"
  mkdir -p "$BUNDLE/bin" "$BUNDLE/web" "$BUNDLE/vpntorrent"
  printf '#!/bin/sh\necho stub-orchestrator\n' > "$BUNDLE/bin/orchestrator"
  chmod +x "$BUNDLE/bin/orchestrator"
  cp -r "$REPO_ROOT/web/." "$BUNDLE/web/" 2>/dev/null || mkdir -p "$BUNDLE/web/public"
  cp "$REPO_ROOT/vpntorrent/setup-netns.sh" "$REPO_ROOT/vpntorrent/watchdog.sh" \
     "$REPO_ROOT/vpntorrent/portforward.sh" "$BUNDLE/vpntorrent/"
  [ -f "$REPO_ROOT/vpntorrent/jf-netns-helper" ] && cp "$REPO_ROOT/vpntorrent/jf-netns-helper" "$BUNDLE/vpntorrent/"
  chmod +x "$BUNDLE/vpntorrent/"*
  cp "$REPO_ROOT/VERSION" "$BUNDLE/VERSION" 2>/dev/null || echo "0.0.0-test" > "$BUNDLE/VERSION"
  return 0
}

# ------------------------------------------------------------------ mocks
# mock_cmd <name> [exit_code] [stdout] — intercept a command.
mock_cmd() {
  local name="$1" code="${2:-0}" out="${3:-}"
  cat > "$MOCKBIN/$name" <<EOF
#!/usr/bin/env bash
printf '%s' "$name" >> "$LOG"
for a in "\$@"; do printf ' %s' "\$a" >> "$LOG"; done
printf '\n' >> "$LOG"
$( [ -n "$out" ] && printf 'printf %s "%s"\n' "'%s\\n'" "$out" )
exit $code
EOF
  chmod +x "$MOCKBIN/$name"
}

# mock_script <name> <body> — intercept with custom bash logic (still logged).
# The body sees "$@" and may inspect $LOG / $DEST.
mock_script() {
  local name="$1"; shift
  { printf '#!/usr/bin/env bash\n'
    printf 'printf "%%s" "%s" >> "%s"\n' "$name" "$LOG"
    printf 'for a in "$@"; do printf " %%s" "$a" >> "%s"; done\n' "$LOG"
    printf 'printf "\\n" >> "%s"\n' "$LOG"
    printf '%s\n' "$*"
  } > "$MOCKBIN/$name"
  chmod +x "$MOCKBIN/$name"
}

# mock_standard — the default mock set: everything privileged or networked succeeds.
mock_standard() {
  local c
  for c in apt-get useradd groupadd usermod systemctl visudo apparmor_parser \
           snap update-rc.d ldconfig setcap; do
    mock_cmd "$c" 0
  done
  # curl: pretend a successful download by creating the -o target.
  mock_script curl '
    out=""; prev=""
    for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
    if [ -n "$out" ]; then mkdir -p "$(dirname "$out")"; printf "mock-artifact\n" > "$out"; fi
    exit 0'
  # tar: pretend an extraction by creating a plausible payload.
  mock_script tar '
    dir="."; prev=""
    for a in "$@"; do [ "$prev" = "-C" ] && dir="$a"; prev="$a"; done
    mkdir -p "$dir/flaresolverr/_internal/chrome"
    printf "#!/bin/sh\n" > "$dir/flaresolverr/flaresolverr"; chmod +x "$dir/flaresolverr/flaresolverr"
    printf "bundled\n" > "$dir/flaresolverr/_internal/chrome/chrome"
    chmod +x "$dir/flaresolverr/_internal/chrome/chrome"
    mkdir -p "$dir/Prowlarr"; printf "#!/bin/sh\n" > "$dir/Prowlarr/Prowlarr"; chmod +x "$dir/Prowlarr/Prowlarr"
    exit 0'
  # useradd records the user; id reports it thereafter. Without this the installer looks
  # non-idempotent because the stub always claims the account is missing.
  mock_script useradd '
    for a in "$@"; do case "$a" in -*) ;; *) u="$a";; esac; done
    [ -n "${u:-}" ] && echo "$u" >> "$SANDBOX/users.txt"
    exit 0'
  mock_script id '
    u="${*: -1}"
    case "$u" in
      jellyfreedom|torrserver|prowlarr|flaresolverr)
        grep -qx "$u" "$SANDBOX/users.txt" 2>/dev/null && { echo 999; exit 0; } || exit 1 ;;
      *) echo 0; exit 0 ;;
    esac'
}

# ------------------------------------------------------------------ run
# run_installer [args...] — execute the installer inside the sandbox.
# The installer MUST honour JF_DESTDIR (fake root) and JF_ASSUME_ROOT (skip the id -u check)
# for this to work; that testability contract is part of the installer's design.
run_installer() {
  ( cd "$BUNDLE" \
    && PATH="$MOCKBIN:$PATH" \
       JF_DESTDIR="$DEST" \
       JF_ASSUME_ROOT=1 \
       JF_NONINTERACTIVE=1 \
       LOG="$LOG" DEST="$DEST" SANDBOX="$SANDBOX" \
       bash ./install.sh "$@" ) > "$OUTPUT" 2>&1
  EXIT_CODE=$?
}

# run_installer_in_place — run the copy the installer left in APP_DIR, which is exactly
# what `jellyfreedom repair` does. SRC and APP_DIR are then the same directory, so every
# copy would be a file onto itself. Nothing else exercises this path, and two releases'
# worth of bugs went out through it.
run_installer_in_place() {
  ( cd "$DEST/opt/jellyfreedom" \
    && PATH="$MOCKBIN:$PATH" \
       JF_DESTDIR="$DEST" \
       JF_ASSUME_ROOT=1 \
       JF_NONINTERACTIVE=1 \
       LOG="$LOG" DEST="$DEST" SANDBOX="$SANDBOX" \
       bash ./install.sh "$@" ) > "$OUTPUT" 2>&1
  EXIT_CODE=$?
}

# ------------------------------------------------------------------ assertions
_pass() { printf '  %s✓%s %s\n' "$c_grn" "$c_off" "$1"; }
_fail() {
  TESTS_FAILED=$((TESTS_FAILED + 1)); FAILED_NAMES+=("$CURRENT_TEST: $1")
  printf '  %s✗%s %s\n' "$c_red" "$c_off" "$1"
  [ -n "${2:-}" ] && printf '      %s%s%s\n' "$c_dim" "$2" "$c_off"
  return 0
}
_check() { TESTS_RUN=$((TESTS_RUN + 1)); }

assert_exit()      { _check; [ "$EXIT_CODE" = "$1" ] && _pass "exit == $1" || _fail "exit == $1" "got $EXIT_CODE; last output: $(tail -3 "$OUTPUT" | tr '\n' '|')"; }
assert_ran()       { _check; grep -qF -- "$1" "$LOG" && _pass "ran: $1" || _fail "ran: $1" "not in command log"; }
assert_not_ran()   { _check; grep -qF -- "$1" "$LOG" && _fail "did NOT run: $1" "but it did: $(grep -F -- "$1" "$LOG" | head -1)" || _pass "did not run: $1"; }
assert_ran_re()    { _check; grep -qE -- "$1" "$LOG" && _pass "ran =~ $1" || _fail "ran =~ $1" "no match in command log"; }
assert_file()      { _check; [ -f "$1" ] && _pass "file: ${1#$DEST}" || _fail "file: ${1#$DEST}" "missing"; }
assert_no_file()   { _check; [ -f "$1" ] && _fail "no file: ${1#$DEST}" "exists" || _pass "no file: ${1#$DEST}"; }
assert_dir()       { _check; [ -d "$1" ] && _pass "dir: ${1#$DEST}" || _fail "dir: ${1#$DEST}" "missing"; }
assert_exec()      { _check; [ -x "$1" ] && _pass "executable: ${1#$DEST}" || _fail "executable: ${1#$DEST}" "missing or not +x"; }
assert_contains()  { _check; grep -qF -- "$2" "$1" 2>/dev/null && _pass "${1#$DEST} contains: $2" || _fail "${1#$DEST} contains: $2" "not found"; }
# Matches only ACTIVE lines, ignoring comments — so a file may explain WHY a directive is
# forbidden without the explanation itself tripping the check.
assert_not_active(){ _check; sed 's/[[:space:]]*#.*//' "$1" 2>/dev/null | grep -qF -- "$2" && _fail "${1#$DEST} has no active: $2" "found it" || _pass "${1#$DEST} has no active: $2"; }
assert_not_contains(){ _check; grep -qF -- "$2" "$1" 2>/dev/null && _fail "${1#$DEST} lacks: $2" "found it" || _pass "${1#$DEST} lacks: $2"; }
assert_mode()      { _check; local m; m=$(stat -c '%a' "$1" 2>/dev/null); [ "$m" = "$2" ] && _pass "mode $2: ${1#$DEST}" || _fail "mode $2: ${1#$DEST}" "got ${m:-<missing>}"; }
assert_output()    { _check; grep -qiF -- "$1" "$OUTPUT" && _pass "output mentions: $1" || _fail "output mentions: $1" "not printed"; }
assert_not_output(){ _check; grep -qiF -- "$1" "$OUTPUT" && _fail "output does NOT mention: $1" "but it did: $(grep -iF -- "$1" "$OUTPUT" | head -1)" || _pass "output does not mention: $1"; }
assert_unchanged() { _check; [ "$(cat "$1" 2>/dev/null)" = "$2" ] && _pass "preserved: ${1#$DEST}" || _fail "preserved: ${1#$DEST}" "content changed"; }

# A full run must not emit shell errors. This catches the class of defect where a script is
# syntactically valid — so `bash -n` passes — but a mangled line executes as a bogus command.
# A stray "x" glued to a comment once shipped this way: every install printed
# "x#: command not found" and every functional assertion still passed.
assert_no_shell_errors() {
  _check
  local hits
  hits=$(grep -aiE 'command not found|syntax error|unexpected token|: not found$' "$OUTPUT" 2>/dev/null | head -3)
  if [ -n "$hits" ]; then
    _fail "installer emitted no shell errors" "$(printf '%s' "$hits" | tr '\n' '|')"
  else
    _pass "installer emitted no shell errors"
  fi
}

describe() { printf '\n%s▸ %s%s\n' "$c_ylw" "$1" "$c_off"; }

summary() {
  printf '\n'
  if [ "$TESTS_FAILED" -eq 0 ]; then
    printf '%s%d checks passed%s\n' "$c_grn" "$TESTS_RUN" "$c_off"
    [ -n "${KEEP_SANDBOX:-}" ] || rm -rf "$TESTROOT"
    exit 0
  fi
  printf '%s%d of %d checks FAILED%s\n' "$c_red" "$TESTS_FAILED" "$TESTS_RUN" "$c_off"
  printf '%s\n' "${FAILED_NAMES[@]}" | sed 's/^/  - /'
  printf '%ssandbox kept at %s%s\n' "$c_dim" "$TESTROOT" "$c_off"
  exit 1
}
