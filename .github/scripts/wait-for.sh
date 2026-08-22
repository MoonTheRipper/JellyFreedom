#!/usr/bin/env bash
# wait-for.sh — convergence helpers for the installer smoke tests.
#
# Source this, do not execute it:   . .github/scripts/wait-for.sh
#
# WHY THIS EXISTS
#   CI assertions used to sample state once, immediately after the installer returned, and
#   fail if a service had not finished starting. That produced red builds for healthy code:
#   Prowlarr (a .NET app that can take tens of seconds to boot) was found 'inactive' on a
#   22.04 runner and failed the build.
#
#   The distinction that matters: we are testing whether the installer produces a working
#   system, NOT how fast a particular runner boots .NET. So every readiness check converges
#   with a deadline, and only a genuine timeout is a failure. Each helper prints what it is
#   waiting for and dumps real diagnostics when it gives up — a CI failure that needs a local
#   reproduction to understand is itself a defect.

# wait_unit_active <unit> [timeout_seconds]
# Succeeds as soon as the unit is active. Fails fast (no waiting out the clock) if systemd
# reports a terminal state, because a unit that has already failed will never become active.
wait_unit_active() {
  local unit="$1" timeout="${2:-90}" waited=0 state sub
  printf '::group::waiting for %s to become active (up to %ss)\n' "$unit" "$timeout"
  while :; do
    state="$(systemctl is-active "$unit" 2>/dev/null || true)"
    sub="$(systemctl show -p SubState --value "$unit" 2>/dev/null || true)"
    if [ "$state" = "active" ]; then
      # 'active' alone is not health: a crash-looping unit under Restart=on-failure reads
      # 'active' during every restart, so a single sample cannot tell a working service from
      # one dying every few seconds. Require it to STAY active and not accumulate restarts.
      local r0 r1
      r0="$(systemctl show -p NRestarts --value "$unit" 2>/dev/null || echo 0)"
      sleep 5
      state="$(systemctl is-active "$unit" 2>/dev/null || true)"
      r1="$(systemctl show -p NRestarts --value "$unit" 2>/dev/null || echo 0)"
      if [ "$state" = "active" ] && [ "$(( ${r1:-0} - ${r0:-0} ))" -eq 0 ]; then
        printf 'ok: %s is active and stable after %ss (sub=%s, restarts=%s)\n::endgroup::\n' \
          "$unit" "$waited" "$sub" "${r1:-0}"
        return 0
      fi
      printf '  %ss: %s is flapping (state=%s restarts %s->%s)\n' "$waited" "$unit" "$state" "${r0:-0}" "${r1:-0}"
    fi
    # A unit that failed outright is not going to recover by waiting. Distinguish it from
    # "still starting" so the log says which one happened.
    if [ "$state" = "failed" ]; then
      printf '::endgroup::\n'
      printf '::error::%s entered the failed state after %ss\n' "$unit" "$waited"
      _wait_dump "$unit"
      return 1
    fi
    [ "$waited" -ge "$timeout" ] && break
    sleep 3; waited=$((waited + 3))
    printf '  %ss: state=%s sub=%s\n' "$waited" "${state:-unknown}" "${sub:-unknown}"
  done
  printf '::endgroup::\n'
  printf '::error::%s was still %s after %ss\n' "$unit" "${state:-unknown}" "$timeout"
  _wait_dump "$unit"
  return 1
}

# wait_http <url> [timeout_seconds] [jq_filter]
# Succeeds when the URL answers 2xx and, if a jq filter is given, the body satisfies it.
# Prints the last body on failure — an assertion that fails without showing what it got
# forces a local reproduction.
wait_http() {
  local url="$1" timeout="${2:-90}" filter="${3:-}" waited=0 code body
  printf '::group::waiting for %s (up to %ss)\n' "$url" "$timeout"
  while :; do
    code="$(curl -s -o /tmp/wait_body.$$ -m 10 -w '%{http_code}' "$url" 2>/dev/null || true)"
    if [ "${code:-000}" -ge 200 ] 2>/dev/null && [ "${code:-000}" -lt 300 ] 2>/dev/null; then
      body="$(head -c 2000 /tmp/wait_body.$$ 2>/dev/null || true)"
      if [ -z "$filter" ]; then
        printf 'ok: %s answered %s after %ss\n::endgroup::\n' "$url" "$code" "$waited"
        rm -f /tmp/wait_body.$$; return 0
      fi
      if printf '%s' "$body" | jq -e "$filter" >/dev/null 2>&1; then
        printf 'ok: %s answered %s and matched %s after %ss\n::endgroup::\n' "$url" "$code" "$filter" "$waited"
        rm -f /tmp/wait_body.$$; return 0
      fi
    fi
    [ "$waited" -ge "$timeout" ] && break
    sleep 3; waited=$((waited + 3))
    printf '  %ss: http=%s\n' "$waited" "${code:-000}"
  done
  printf '::endgroup::\n'
  printf '::error::%s did not become ready within %ss (last http=%s)\n' "$url" "$timeout" "${code:-000}"
  printf 'last response body:\n'; head -c 1000 /tmp/wait_body.$$ 2>/dev/null || echo '(empty)'
  rm -f /tmp/wait_body.$$
  return 1
}

# _wait_dump <unit> — the diagnostics a human would ask for straight away.
_wait_dump() {
  local unit="$1"
  printf '::group::diagnostics for %s\n' "$unit"
  systemctl status "$unit" --no-pager -l 2>&1 | head -30 || true
  printf -- '--- journal ---\n'
  journalctl -u "$unit" -n 60 --no-pager 2>&1 | tail -60 || true
  printf '::endgroup::\n'
}
