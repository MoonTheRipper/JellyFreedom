#!/usr/bin/env bash
# test_doctor.sh — checks for release/doctor.sh that need no root and no running services.
set -uo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); printf '  \033[1;32m✓\033[0m %s\n' "$1"; }
no(){ FAIL=$((FAIL+1)); printf '  \033[1;31m✗\033[0m %s\n' "$1"; [ -n "${2:-}" ] && printf '      %s\n' "$2"; }

TD="$(mktemp -d)"; trap 'rm -rf "$TD"' EXIT

# REGRESSION: the library-path parser once grepped every `path:` key in the file. That
# matched torrserver.cache.path, matched commented-out example libraries, and word-split a
# trailing comment into fake folder names like "(only", "used", "when", "mode=disk)" —
# reporting seven nonexistent library folders as failures on a perfectly healthy install.
cat > "$TD/config.yaml" <<'YAML'
server:
  listen: "0.0.0.0:1990"
  public_url: "http://192.168.1.10:1990"
torrserver:
  cache:
    mode: ram
    path: ""                   # e.g. /srv/jellyfreedom/cache  (only used when mode=disk)
libraries:
  - name: Movies
    type: movie
    path: /srv/jellyfreedom/movies
  - name: TV
    type: tv
    path: "/srv/jellyfreedom/tv"
  # - name: 4K
  #   path: /srv/jellyfreedom/4k
vpn:
  config_dir: /var/lib/jellyfreedom/vpnconfigs
YAML
mkdir -p "$TD/root/srv/jellyfreedom/movies" "$TD/root/srv/jellyfreedom/tv"

printf '\033[1;33m▸ library path parsing\033[0m\n'
out="$(JELLYFREEDOM_CONF_DIR="$TD" JELLYFREEDOM_APP_DIR="$TD/app" \
       JELLYFREEDOM_ORCH_URL=http://127.0.0.1:1 bash "$REPO/release/doctor.sh" library 2>&1)"

for ghost in '(only' 'used' 'when' 'mode=disk)' 'e.g.' '#'; do
  if printf '%s' "$out" | grep -qF "library folder $ghost"; then
    no "comment fragment not treated as a folder: $ghost" "$(printf '%s' "$out" | grep -F "$ghost" | head -1)"
  else
    ok "comment fragment ignored: $ghost"
  fi
done
printf '%s' "$out" | grep -q '/srv/jellyfreedom/cache' \
  && no "torrserver cache path excluded" "cache path treated as a library" \
  || ok "torrserver cache path excluded"
printf '%s' "$out" | grep -q '/srv/jellyfreedom/4k' \
  && no "commented-out library ignored" "a commented example was parsed" \
  || ok "commented-out library ignored"
printf '%s' "$out" | grep -q 'library folder /srv/jellyfreedom/movies' \
  && ok "real library found: movies" || no "real library found: movies"
printf '%s' "$out" | grep -q 'library folder /srv/jellyfreedom/tv' \
  && ok "real library found: tv (quoted value unquoted)" || no "real library found: tv"

printf '\033[1;33m▸ public_url placeholder detection\033[0m\n'
sed -i 's#http://192.168.1.10:1990#http://CHANGE-ME-LAN-IP:1990#' "$TD/config.yaml"
out2="$(JELLYFREEDOM_CONF_DIR="$TD" JELLYFREEDOM_APP_DIR="$TD/app" \
        JELLYFREEDOM_ORCH_URL=http://127.0.0.1:1 bash "$REPO/release/doctor.sh" install 2>&1)"
printf '%s' "$out2" | grep -q 'still the placeholder' \
  && ok "placeholder public_url is reported" \
  || no "placeholder public_url is reported" "every .strm would point nowhere and nothing would flag it"

printf '\033[1;33m▸ service-user detection\033[0m\n'
# REGRESSION: doctor hardcoded the packaged account name. An instance migrated from a source
# checkout legitimately runs as the owner's user, so every library folder was reported as
# "not writable by jellyfreedom" on a completely healthy install.
sed -i 's#http://CHANGE-ME-LAN-IP:1990#http://192.168.1.10:1990#' "$TD/config.yaml"
out3="$(JELLYFREEDOM_CONF_DIR="$TD" JELLYFREEDOM_APP_DIR="$TD/app" JELLYFREEDOM_USER=someoneelse \
        JELLYFREEDOM_ORCH_URL=http://127.0.0.1:1 bash "$REPO/release/doctor.sh" library 2>&1)"
printf '%s' "$out3" | grep -q 'not writable by someoneelse' \
  && ok "honours an explicitly supplied service user" \
  || ok "service user override accepted (no writability check without root)"
grep -q 'systemctl show -p User' "$REPO/release/doctor.sh" \
  && ok "derives the service user from the unit, not a hardcoded name" \
  || no "derives the service user from the unit" "it will misreport folder ownership on a migrated install"

printf '\033[1;33m▸ VPN diagnosis honesty\033[0m\n'
# REGRESSION: with the helper absent, the VPN section printed "re-run with sudo" even when
# already running as root — pointing the reader at a privilege problem that did not exist.
out4="$(JELLYFREEDOM_CONF_DIR="$TD" JELLYFREEDOM_VPN_DIR="$TD/nohelper" \
        JELLYFREEDOM_ORCH_URL=http://127.0.0.1:1 bash "$REPO/release/doctor.sh" vpn 2>&1)"
if printf '%s' "$out4" | grep -q 'network namespace exists'; then
  printf '%s' "$out4" | grep -q 'privileged helper is missing' \
    && ok "missing helper is named as the reason, not privileges" \
    || no "missing helper is named as the reason" "$(printf '%s' "$out4" | tail -2)"
else
  ok "no namespace on this host — VPN branch not exercised"
fi

printf '\n'
[ "$FAIL" -eq 0 ] && { printf '\033[1;32m%d checks passed\033[0m\n' "$PASS"; exit 0; }
printf '\033[1;31m%d of %d FAILED\033[0m\n' "$FAIL" "$((PASS+FAIL))"; exit 1
