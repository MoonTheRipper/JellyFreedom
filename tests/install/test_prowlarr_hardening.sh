#!/usr/bin/env bash
# harden_prowlarr closes an exposure the installer used to create and then document away:
# stock Prowlarr answers on every interface with no login, so any LAN host could
# GET /initialize.json and receive the API key in cleartext — and that key lists every
# indexer, with its private-tracker credentials.
#
# Its first fix bound a login-less Prowlarr to localhost, which broke first-run step 3 on
# every headless server (open Prowlarr from another machine and add an indexer). It now gives
# such a Prowlarr a login through Prowlarr's own API instead, and keeps the localhost bind only
# as the fallback. Separately, the fixed `sleep 3` after its restart lost the race with
# Prowlarr's startup on every fresh install, so the FlareSolverr tag was never created.
#
# autowire() returns early under JF_DESTDIR, so the main harness never reaches this code.
# The functions are extracted and driven directly against fixture configs and a mock
# Prowlarr (curl and probe are stubbed) instead.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

PASS=0; FAIL=0
ok_()   { printf '  \033[1;32m✓\033[0m %s\n' "$1"; PASS=$((PASS+1)); }
bad_()  { printf '  \033[1;31m✗\033[0m %s\n     %s\n' "$1" "$2"; FAIL=$((FAIL+1)); }

# Pull the functions and their two globals out of the installer.
extract() { awk -v f="^$1\\\\(\\\\)\\\\{" '$0 ~ f {p=1} p {print} p && /^\}$/ {exit}' release/install.sh; }
fn="$(grep -E '^PROWLARR_(API|LOGIN_FILE)=' release/install.sh)
$(extract prowlarr_wait_up)
$(extract prowlarr_create_login)
$(extract harden_prowlarr)
$(extract prowlarr_register_flaresolverr)"
for f in prowlarr_wait_up prowlarr_create_login harden_prowlarr prowlarr_register_flaresolverr; do
  grep -q "^$f(){" <<<"$fn" || { echo "could not extract $f from release/install.sh"; exit 1; }
done

R="$(mktemp -d)"; trap 'rm -rf "$R"' EXIT

# run_case <xml> [put http code] [pings that fail before Prowlarr answers]
# Leaves: $R/cfg (resulting config.xml), $R/etc/prowlarr-login, $R/calls (every stubbed call
# in order, with its full argv), $R/put.json (the PUT body), $R/out (what the user saw).
run_case() {
  rm -rf "${R:?}/etc" "${R:?}/tmp"; mkdir -p "$R/etc" "$R/tmp"
  : > "$R/calls"; rm -f "$R/put.json" "$R/pings"
  printf '%s\n' "$1" > "$R/cfg"
  (
    export TMPDIR="$R/tmp"
    # Read by the eval'd installer code, which shellcheck cannot see.
    # shellcheck disable=SC2034
    CONF_DIR="$R/etc" D=""
    PUT_CODE="${2:-202}"; FAIL_PINGS="${3:-0}"
    # Stubs. shellcheck cannot see the call sites (they are in the eval'd string).
    # shellcheck disable=SC2329
    ok(){ echo "ok: $*"; }
    # shellcheck disable=SC2329
    warn(){ echo "warn: $*"; }
    # shellcheck disable=SC2329
    hint(){ echo "hint: $*"; }
    # shellcheck disable=SC2329
    chown(){ :; }
    # shellcheck disable=SC2329
    sleep(){ :; }
    # shellcheck disable=SC2329
    systemctl(){ echo "systemctl $*" >> "$R/calls"; }
    # Prowlarr answers /ping only after FAIL_PINGS failed attempts (counted across calls).
    # shellcheck disable=SC2329
    probe(){
      local n; n=$(( $(cat "$R/pings" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$R/pings"
      echo "probe $1" >> "$R/calls"
      if [ "$n" -gt "$FAIL_PINGS" ]; then echo 200; else echo 000; fi
    }
    # A mock Prowlarr API: GET config/host returns the stock host config; PUT records the
    # body and, when accepted, rewrites config.xml the way Prowlarr does.
    # shellcheck disable=SC2329
    curl(){
      printf 'curl' >> "$R/calls"; printf ' %q' "$@" >> "$R/calls"; echo >> "$R/calls"
      local out="" body="" put=0 a
      while [ $# -gt 0 ]; do
        a="$1"; shift
        case "$a" in
          -o) out="$1"; shift ;;
          -X) [ "$1" = PUT ] && put=1; shift ;;
          --data-binary) body="${1#@}"; shift ;;
        esac
      done
      if [ "$put" = 1 ]; then
        cp "$body" "$R/put.json"
        case "$PUT_CODE" in 2??)
          sed -i 's:<AuthenticationMethod>[^<]*</AuthenticationMethod>:<AuthenticationMethod>Forms</AuthenticationMethod>:' "$R/cfg" ;;
        esac
        printf '%s' "$PUT_CODE"; return 0
      fi
      printf '{"bindAddress":"*","port":9696,"apiKey":"k","authenticationMethod":"none","authenticationRequired":"enabled","username":"","password":""}' > "$out"
    }
    eval "$fn"
    harden_prowlarr "$R/cfg" k
  ) > "$R/out" 2>&1
}
xml() { printf '<Config><BindAddress>%s</BindAddress><ApiKey>k</ApiKey><AuthenticationMethod>%s</AuthenticationMethod><AuthenticationRequired>%s</AuthenticationRequired></Config>' "$1" "$2" "$3"; }
has()   { grep -qF -- "$1" "$2"; }
json()  { python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2]))' "$R/put.json" "$1" 2>/dev/null; }
loginpw() { sed -n 's/^password: //p' "$R/etc/prowlarr-login" 2>/dev/null; }

printf '\033[1;33m▸ fresh install, no login: create one and keep Prowlarr on the LAN\033[0m\n'
run_case "$(xml '*' None Enabled)"
[ -f "$R/etc/prowlarr-login" ] && ok_ "login saved to prowlarr-login" || bad_ "login saved" "no file; output: $(cat "$R/out")"
[ "$(stat -c %a "$R/etc/prowlarr-login" 2>/dev/null)" = 600 ] && ok_ "login file is root-only (600)" || bad_ "login file mode" "$(stat -c %a "$R/etc/prowlarr-login" 2>/dev/null)"
pw="$(loginpw)"
[ "${#pw}" = 24 ] && ok_ "a 24-character generated password" || bad_ "generated password" "length ${#pw}"
[ "$(json authenticationMethod)" = forms ] && [ "$(json authenticationRequired)" = enabled ] \
  && ok_ "told Prowlarr: forms login, required on every address" || bad_ "PUT body" "$(cat "$R/put.json" 2>/dev/null)"
[ "$(json username)" = jellyfreedom ] && [ -n "$pw" ] && [ "$(json password)" = "$pw" ] && [ "$(json passwordConfirmation)" = "$pw" ] \
  && ok_ "the saved login is the one Prowlarr was given" || bad_ "saved login matches" "user=$(json username)"
[ "$(json bindAddress)" = '*' ] && has '<BindAddress>*</BindAddress>' "$R/cfg" \
  && ok_ "still reachable on the LAN (bind unchanged)" || bad_ "bind unchanged" "$(cat "$R/cfg")"
[ -n "$pw" ] && ! grep -qF -- "$pw" "$R/calls" "$R/out" && ok_ "password never on a command line or in the output" || bad_ "password kept off argv/output" "it leaked"
[ -z "$(ls -A "$R/tmp")" ] && ok_ "no temporary files left behind" || bad_ "temp cleanup" "$(ls -A "$R/tmp")"
has 'sudo cat' "$R/out" && ok_ "tells the operator where the login is" || bad_ "says where the login is" "$(cat "$R/out")"
awk '/^systemctl restart prowlarr/{r=1; next} r && /^probe .*\/ping/{print "ok"; exit}' "$R/calls" | grep -q ok \
  && ok_ "restarted Prowlarr, then waited for it to answer" || bad_ "waits after the restart" "$(cat "$R/calls")"

printf '\033[1;33m▸ Prowlarr refuses the login: fall back to localhost, leave nothing half-done\033[0m\n'
run_case "$(xml '*' None Enabled)" 500
has '<BindAddress>127.0.0.1</BindAddress>' "$R/cfg" && ok_ "bound to localhost" || bad_ "bound to localhost" "$(cat "$R/cfg")"
[ ! -e "$R/etc/prowlarr-login" ] && ok_ "no login file for a login that does not exist" || bad_ "no stale login file" "file left behind"
has 'SSH tunnel' "$R/out" && ok_ "says how to reach it anyway (SSH tunnel)" || bad_ "explains the tunnel" "$(cat "$R/out")"
[ -z "$(ls -A "$R/tmp")" ] && ok_ "no temporary files left behind" || bad_ "temp cleanup" "$(ls -A "$R/tmp")"

printf '\033[1;33m▸ Prowlarr never answers: same fallback, no PUT attempted\033[0m\n'
run_case "$(xml '*' None Enabled)" 202 100000
has '<BindAddress>127.0.0.1</BindAddress>' "$R/cfg" && ok_ "bound to localhost" || bad_ "bound to localhost" "$(cat "$R/cfg")"
! has '-X PUT' "$R/calls" && ok_ "did not try to change a Prowlarr it cannot reach" || bad_ "no PUT" "$(grep -c PUT "$R/calls") PUTs"

printf '\033[1;33m▸ upgrade from a box already narrowed to localhost: add the login, do not widen\033[0m\n'
run_case "$(xml 127.0.0.1 None Enabled)"
[ -f "$R/etc/prowlarr-login" ] && ok_ "login created" || bad_ "login created" "$(cat "$R/out")"
has '<BindAddress>127.0.0.1</BindAddress>' "$R/cfg" && ok_ "bind left at localhost — widening is the operator's call" || bad_ "bind not widened" "$(cat "$R/cfg")"
has 'BindAddress>*</BindAddress>' "$R/out" && ok_ "says how to widen it" || bad_ "explains widening" "$(cat "$R/out")"

printf '\033[1;33m▸ a login exists: require it everywhere, leave reachability alone\033[0m\n'
run_case "$(xml '*' Forms DisabledForLocalAddresses)"
has '<AuthenticationRequired>Enabled</AuthenticationRequired>' "$R/cfg" && ok_ "authentication is required for every address" || bad_ "required everywhere" "$(cat "$R/cfg")"
has '<BindAddress>*</BindAddress>' "$R/cfg" && ok_ "reachability is unchanged, so a remote UI keeps working" || bad_ "reachability unchanged" "$(cat "$R/cfg")"
[ ! -e "$R/etc/prowlarr-login" ] && ! has '-X PUT' "$R/calls" && ok_ "the operator's own login is not replaced" || bad_ "own login kept" "a login was created"

printf '\033[1;33m▸ a reverse proxy authenticates (External): close it by reachability\033[0m\n'
run_case "$(xml '*' External Enabled)"
has '<BindAddress>127.0.0.1</BindAddress>' "$R/cfg" && ok_ "bound to localhost" || bad_ "bound to localhost" "$(cat "$R/cfg")"
[ ! -e "$R/etc/prowlarr-login" ] && ok_ "no login invented" || bad_ "no login invented" "a login was created"

printf '\033[1;33m▸ an already-correct config is left alone\033[0m\n'
before="$(xml 127.0.0.1 Forms Enabled)"
run_case "$before"
[ "$(cat "$R/cfg")" = "$before" ] && ok_ "idempotent" || bad_ "idempotent" "changed to: $(cat "$R/cfg")"
! has 'systemctl restart' "$R/calls" && ok_ "no needless restart" || bad_ "no needless restart" "it restarted Prowlarr"

printf '\033[1;33m▸ the race: Prowlarr is slow to come back after the restart\033[0m\n'
# Forms + not-required → a change, so a restart. /ping fails 4 times after it, then answers.
run_case "$(xml '*' Forms DisabledForLocalAddresses)" 202 4
n="$(grep -c '^probe .*/ping' "$R/calls")"
[ "$n" = 5 ] && ok_ "kept asking until Prowlarr answered (5 pings), instead of a fixed sleep" || bad_ "waits for Prowlarr" "$n pings: $(cat "$R/calls")"
! has 'did not answer' "$R/out" && ok_ "no false 'did not answer' warning" || bad_ "no false warning" "$(cat "$R/out")"
# And autowire hands harden_prowlarr the key — without it, the login could never be created.
grep -qE '^  harden_prowlarr "\$pw_cfg" "\$pw_key"$' release/install.sh \
  && ok_ "autowire passes the API key to harden_prowlarr" || bad_ "autowire passes the key" "call site changed"

# ---- FlareSolverr registration, against replies shaped exactly like Prowlarr 2.7's:
# pretty-printed, "label" before "id". The sed patterns this replaced never matched them.
# run_reg <tags json> <indexerproxy json> [reply to creating the tag]
#   → $R/calls, $R/proxy.json (the POSTed proxy, if any), $R/out
run_reg() {
  : > "$R/calls"; rm -f "$R/proxy.json"
  printf '%s' "$1" > "$R/tags.json"; printf '%s' "$2" > "$R/proxies.json"
  local created=$'{\n  "label": "flaresolverr",\n  "id": 7\n}'
  printf '%s' "${3:-$created}" > "$R/tagpost.json"
  (
    # Read by the eval'd installer globals, which shellcheck cannot see.
    # shellcheck disable=SC2034
    CONF_DIR="$R/etc"
    # shellcheck disable=SC2329
    ok(){ echo "ok: $*"; }
    # shellcheck disable=SC2329
    warn(){ echo "warn: $*"; }
    # shellcheck disable=SC2329
    hint(){ echo "hint: $*"; }
    # shellcheck disable=SC2329
    curl(){
      printf 'curl' >> "$R/calls"; printf ' %q' "$@" >> "$R/calls"; echo >> "$R/calls"
      local url="" data="" a
      while [ $# -gt 0 ]; do
        a="$1"; shift
        case "$a" in -d) data="$1"; shift ;; -H|--max-time|-X) shift ;; http*) url="$a" ;; esac
      done
      case "$url" in
        */tag)          if [ -n "$data" ]; then cat "$R/tagpost.json"; else cat "$R/tags.json"; fi ;;
        */indexerproxy) if [ -n "$data" ]; then printf '%s' "$data" > "$R/proxy.json"; printf '{\n  "id": 1\n}'; else cat "$R/proxies.json"; fi ;;
        *) return 7 ;;
      esac
    }
    eval "$fn"
    prowlarr_register_flaresolverr k
  ) > "$R/out" 2>&1
}
ptags() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["tags"])' "$R/proxy.json" 2>/dev/null; }

printf '\033[1;33m▸ FlareSolverr proxy, fresh Prowlarr: create the tag, read its id, add the proxy\033[0m\n'
run_reg '[]' '[]'
has 'registered FlareSolverr' "$R/out" && ! has 'could not create a Prowlarr tag' "$R/out" \
  && ok_ "reads the id from Prowlarr's pretty-printed reply" || bad_ "reads the tag id" "$(cat "$R/out")"
[ "$(ptags)" = "[7]" ] && ok_ "proxy added behind the new tag (id 7)" || bad_ "proxy behind the tag" "tags=$(ptags); $(cat "$R/out")"

printf '\033[1;33m▸ the tag already exists among others: reuse it, do not create a second\033[0m\n'
run_reg $'[\n  {\n    "label": "private",\n    "id": 2\n  },\n  {\n    "label": "flaresolverr",\n    "id": 5\n  }\n]' '[]'
[ "$(ptags)" = "[5]" ] && ok_ "proxy added behind the existing tag (id 5)" || bad_ "reuses the tag" "tags=$(ptags)"
! grep -q -- "-d {\"label\"" "$R/calls" && ok_ "no duplicate tag created" || bad_ "no duplicate tag" "it POSTed a new tag"

printf '\033[1;33m▸ the proxy already exists: leave it alone (re-runs must not add another)\033[0m\n'
run_reg '[]' $'[\n  {\n    "name": "FlareSolverr",\n    "implementation": "FlareSolverr",\n    "tags": [\n      1\n    ],\n    "id": 1\n  }\n]'
has 'already has a FlareSolverr proxy' "$R/out" && [ ! -e "$R/proxy.json" ] && ok_ "recognised and left alone" || bad_ "existing proxy recognised" "$(cat "$R/out")"

printf '\033[1;33m▸ Prowlarr answers with nonsense: warn, add nothing\033[0m\n'
run_reg 'not json' 'not json' 'not json'
has 'could not create a Prowlarr tag' "$R/out" && [ ! -e "$R/proxy.json" ] && ok_ "warned, and no proxy pointed at an unknown tag" || bad_ "nonsense handled" "$(cat "$R/out")"

echo
if [ "$FAIL" -gt 0 ]; then printf '\033[1;31m%d of %d checks FAILED\033[0m\n' "$FAIL" "$((PASS+FAIL))"; exit 1; fi
printf '\033[1;32m%d checks passed\033[0m\n' "$PASS"
