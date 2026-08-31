#!/usr/bin/env bash
# harden_prowlarr closes an exposure the installer used to create and then document away:
# stock Prowlarr answers on every interface with authentication disabled for local
# addresses, so any LAN host could GET /initialize.json and receive the API key in
# cleartext — and that key lists every indexer, with its private-tracker credentials.
#
# autowire() returns early under JF_DESTDIR, so the main harness never reaches this code.
# The function is extracted and driven directly against fixture configs instead.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

PASS=0; FAIL=0
ok_()   { printf '  \033[1;32m✓\033[0m %s\n' "$1"; PASS=$((PASS+1)); }
bad_()  { printf '  \033[1;31m✗\033[0m %s\n     %s\n' "$1" "$2"; FAIL=$((FAIL+1)); }

# Pull just the function out of the installer and give it the stubs it calls.
fn="$(awk '/^harden_prowlarr\(\)\{/,/^\}$/' release/install.sh)"
[ -n "$fn" ] || { echo "could not extract harden_prowlarr from release/install.sh"; exit 1; }

run_case() {  # run_case <name> <xml> ; echoes the resulting xml
  local xml="$2" tmp
  tmp="$(mktemp)"
  printf '%s\n' "$xml" > "$tmp"
  (
    ok(){ :; }; warn(){ :; }; hint(){ :; }
    systemctl(){ :; }; chown(){ :; }
    eval "$fn"
    harden_prowlarr "$tmp"
  ) >/dev/null 2>&1
  cat "$tmp"; rm -f "$tmp"
}

printf '\033[1;33m▸ a login exists: require it everywhere, leave reachability alone\033[0m\n'
out="$(run_case forms '<Config><BindAddress>*</BindAddress><ApiKey>k</ApiKey><AuthenticationMethod>Forms</AuthenticationMethod><AuthenticationRequired>DisabledForLocalAddresses</AuthenticationRequired></Config>')"
case "$out" in
  *"<AuthenticationRequired>Enabled</AuthenticationRequired>"*) ok_ "authentication is required for every address" ;;
  *) bad_ "authentication is required for every address" "still: $out" ;;
esac
case "$out" in
  *"<BindAddress>*</BindAddress>"*) ok_ "reachability is unchanged, so a remote UI keeps working" ;;
  *) bad_ "reachability is unchanged" "bind address was rewritten: $out" ;;
esac

printf '\033[1;33m▸ no login at all: requiring one would lock the operator out, so bind to localhost\033[0m\n'
out="$(run_case none '<Config><BindAddress>*</BindAddress><ApiKey>k</ApiKey><AuthenticationMethod>None</AuthenticationMethod><AuthenticationRequired>DisabledForLocalAddresses</AuthenticationRequired></Config>')"
case "$out" in
  *"<BindAddress>127.0.0.1</BindAddress>"*) ok_ "bound to localhost when there are no credentials to enforce" ;;
  *) bad_ "bound to localhost when there are no credentials" "still: $out" ;;
esac
case "$out" in
  *"<AuthenticationRequired>Enabled</AuthenticationRequired>"*) bad_ "did not invent a login requirement" "it set Enabled with no credentials configured" ;;
  *) ok_ "did not invent a login requirement it cannot satisfy" ;;
esac

printf '\033[1;33m▸ an already-correct config is left alone\033[0m\n'
before='<Config><BindAddress>127.0.0.1</BindAddress><ApiKey>k</ApiKey><AuthenticationMethod>Forms</AuthenticationMethod><AuthenticationRequired>Enabled</AuthenticationRequired></Config>'
out="$(run_case idem "$before")"
[ "$out" = "$before" ] && ok_ "idempotent" || bad_ "idempotent" "changed to: $out"

echo
if [ "$FAIL" -gt 0 ]; then printf '\033[1;31m%d of %d checks FAILED\033[0m\n' "$FAIL" "$((PASS+FAIL))"; exit 1; fi
printf '\033[1;32m%d checks passed\033[0m\n' "$PASS"
