#!/usr/bin/env bash
# The reaper is EXECUTED here, against a fixture.
#
# Its predecessor was three find expressions inline in a systemd unit, and it never worked:
# an ExecStart is not a shell, so find received a literal `\(` and failed on every run. The
# test at the time asserted the unit file contained the right strings, which it did, while
# /tmp filled to 100% four times.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

PASS=0; FAIL=0
ok_()  { printf '  \033[1;32m✓\033[0m %s\n' "$1"; PASS=$((PASS+1)); }
bad_() { printf '  \033[1;31m✗\033[0m %s\n     %s\n' "$1" "$2"; FAIL=$((FAIL+1)); }
gone() { [ ! -e "$1" ] && ok_ "reaped: ${1##*/}" || bad_ "reaped: ${1##*/}" "still there"; }
kept() { [ -e "$1" ] && ok_ "kept: ${1##*/}" || bad_ "kept: ${1##*/}" "it was deleted"; }

R="$(mktemp -d)"; trap 'rm -rf "$R"' EXIT
mkdir -p "$R/snap/snap.chromium/tmp" "$R/snap/snap.chromium/var/tmp" "$R/host" "$R/data"

# Should go: old and browser-named, in the right place.
mkdir -p "$R/snap/snap.chromium/tmp/org.chromium.Chromium.AAA"
mkdir -p "$R/snap/snap.chromium/tmp/tmp_abcdef"
mkdir -p "$R/host/.org.chromium.Chromium.TOP"
mkdir -p "$R/host/scoped_dir_9"
mkdir -p "$R/data/_MEI123456"
# Should stay.
mkdir -p "$R/snap/snap.chromium/tmp/keep-me"                    # not browser-named
mkdir -p "$R/snap/snap.chromium/var/tmp/org.chromium.WrongDepth" # right name, wrong depth
mkdir -p "$R/host/important-user-work"
echo keep > "$R/host/notes.txt"
mkdir -p "$R/data/keep-this-too"
mkdir -p "$R/snap/snap.chromium/tmp/org.chromium.Chromium.FRESH" # too new

find "$R" -depth -mindepth 1 ! -name 'org.chromium.Chromium.FRESH' -exec touch -d '3 hours ago' {} + 2>/dev/null

printf '\033[1;33m▸ the reaper runs and removes only what it should\033[0m\n'
out="$(JF_SNAP_TMP="$R/snap" JF_HOST_TMP="$R/host" JF_DATA_TMP="$R/data" bash release/tmpreaper.sh 2>&1)"
rc=$?
[ "$rc" = 0 ] && ok_ "exits 0" || bad_ "exits 0" "rc=$rc"
case "$out" in *"removed 5 stale"*) ok_ "reports what it removed ($out)" ;; *) bad_ "reports 5 removed" "said: $out" ;; esac

gone "$R/snap/snap.chromium/tmp/org.chromium.Chromium.AAA"
gone "$R/snap/snap.chromium/tmp/tmp_abcdef"
gone "$R/host/.org.chromium.Chromium.TOP"
gone "$R/host/scoped_dir_9"
gone "$R/data/_MEI123456"

kept "$R/snap/snap.chromium/tmp/keep-me"
kept "$R/snap/snap.chromium/var/tmp/org.chromium.WrongDepth"
kept "$R/host/important-user-work"
kept "$R/host/notes.txt"
kept "$R/data/keep-this-too"
kept "$R/snap/snap.chromium/tmp/org.chromium.Chromium.FRESH"

printf '\033[1;33m▸ a missing directory is not an error\033[0m\n'
out="$(JF_SNAP_TMP="$R/nope" JF_HOST_TMP="$R/nope" JF_DATA_TMP="$R/nope" bash release/tmpreaper.sh 2>&1)"
[ $? = 0 ] && ok_ "skips directories that do not exist" || bad_ "skips missing dirs" "$out"

echo
if [ "$FAIL" -gt 0 ]; then printf '\033[1;31m%d of %d checks FAILED\033[0m\n' "$FAIL" "$((PASS+FAIL))"; exit 1; fi
printf '\033[1;32m%d checks passed\033[0m\n' "$PASS"
