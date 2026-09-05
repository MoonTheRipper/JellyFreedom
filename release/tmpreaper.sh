#!/usr/bin/env bash
# tmpreaper.sh — remove browser and extractor scratch that nothing else cleans up.
#
# WHY A SCRIPT AND NOT ExecStart= LINES
#
# This began as three `find` expressions inline in jf-tmpreaper.service. A systemd ExecStart
# is NOT a shell: the `\(` and `\)` that a shell needs were passed to find verbatim, so every
# run died with
#
#     find: paths must precede expression: `\('
#
# and because the lines carried a `-` prefix, systemd ignored the failure and reported the
# service as finished successfully. It never deleted anything, from 0.6.2 until it was found
# by a /tmp that had filled to 100% for the fourth time. The test that was supposed to cover
# it asserted the unit file CONTAINED the right strings, which it did.
#
# A script can be executed by a test. That is the whole reason this file exists.
set -uo pipefail

# Directories that hold third-party scratch. Missing ones are skipped, not an error: a box
# with no snaps has no snap-private-tmp.
SNAP_TMP="${JF_SNAP_TMP:-/tmp/snap-private-tmp}"
HOST_TMP="${JF_HOST_TMP:-/tmp}"
DATA_TMP="${JF_DATA_TMP:-/var/lib/jellyfreedom/tmp}"
AGE_MIN="${JF_REAP_AGE_MIN:-60}"

removed=0

reap() {  # reap <dir> <mindepth> <maxdepth> <extra find args...>
  local dir="$1" mind="$2" maxd="$3"; shift 3
  [ -d "$dir" ] || return 0
  local n
  n=$(find "$dir" -mindepth "$mind" -maxdepth "$maxd" -mmin "+$AGE_MIN" "$@" -print 2>/dev/null | wc -l)
  [ "$n" -gt 0 ] || return 0
  find "$dir" -mindepth "$mind" -maxdepth "$maxd" -mmin "+$AGE_MIN" "$@" -exec rm -rf {} + 2>/dev/null
  removed=$((removed + n))
}

# FlareSolverr drives a browser per request and each launch leaves a scratch directory. Under
# a snap Chromium they land here, root-owned 0700, where FlareSolverr's own cleanup cannot
# even traverse. Measured on a live box: 3,894 directories, 7.7GB, 741k inodes.
#
# Narrow on purpose — inside a snap's tmp, old, AND named like a browser. A stale directory
# costs a little memory; a wrong glob running rm -rf as root deletes somebody's work.
# The path anchor is '*/snap.*/tmp/*', not '*/tmp/*'. The looser form matched the snap's own
# `var/tmp` DIRECTORY at the same depth — its name begins with "tmp" — so the reaper deleted
# a directory the snap was using rather than the scratch inside it. Caught by the fixture,
# which is the entire argument for this being a script.
reap "$SNAP_TMP" 3 3 -path '*/snap.*/tmp/*' \
  \( -name 'org.chromium.*' -o -name '.org.chromium.*' \
     -o -name 'com.google.Chrome*' -o -name '.com.google.Chrome*' \
     -o -name 'scoped_dir*' -o -name 'tmp*' \)

# The same residue from an unconfined Chrome or Chromium, straight into /tmp.
reap "$HOST_TMP" 1 1 \
  \( -name '.org.chromium.*' -o -name '.com.google.Chrome*' -o -name 'scoped_dir*' \)

# yt-dlp's PyInstaller bundle unpacks ~76MB per run and removes it on exit — but it is
# SIGKILLed whenever an extraction times out or the client disconnects mid-resolve, and a
# killed bundle cleans up nothing. Different filesystem, same failure.
reap "$DATA_TMP" 1 1 \( -name '_MEI*' -o -name 'tmp*' -o -name 'yt-dlp*' \)

printf 'tmpreaper: removed %d stale scratch entries\n' "$removed"
exit 0
