#!/usr/bin/env bash
# doctor.sh — diagnose a JellyFreedom install and say exactly how to fix what is broken.
#
#   jellyfreedom doctor            run every check
#   jellyfreedom doctor --quiet    only show problems
#   jellyfreedom doctor <name>     run one section (system|install|privs|services|ports|
#                                  orchestrator|prowlarr|flaresolverr|torrserver|websources|
#                                  jellyfin|vpn|library)
#
# Exit status: 0 = no failures, 1 = at least one FAIL.
#
# Design notes:
#  - Runs without the orchestrator: the whole point is diagnosing an instance that is down.
#  - Works unprivileged, degrading checks that need root rather than refusing to run.
#  - Every FAIL/WARN prints a concrete next command. A diagnosis the user cannot act on is
#    not a diagnosis.
#
# `-e` is deliberately NOT set: probing is the entire job here and non-zero is the normal
# result of half these commands.
set -uo pipefail

APP_DIR="${JELLYFREEDOM_APP_DIR:-/opt/jellyfreedom}"
VPN_DIR="${JELLYFREEDOM_VPN_DIR:-/opt/vpntorrent}"
CONF_DIR="${JELLYFREEDOM_CONF_DIR:-/etc/jellyfreedom}"
DATA_DIR="${JELLYFREEDOM_DATA_DIR:-/var/lib/jellyfreedom}"
SVC_USER="${JELLYFREEDOM_USER:-}"
if [ -z "$SVC_USER" ] && command -v systemctl >/dev/null; then
    # The unit is the authority: an instance migrated from a source checkout runs as the
    # owner's account, not the packaged one. Assuming "jellyfreedom" reports every library
    # folder as unwritable on a perfectly healthy install.
    SVC_USER="$(systemctl show -p User --value jellyfreedom.service 2>/dev/null)"
fi
[ -n "$SVC_USER" ] || SVC_USER=jellyfreedom
ORCH_URL="${JELLYFREEDOM_ORCH_URL:-http://127.0.0.1:1990}"
FS_URL="${FLARESOLVERR_URL:-http://127.0.0.1:8191}"
# TorrServer listens inside the vpntorrent namespace, reachable over the veth. setup-netns.sh
# publishes the resolved addressing to /run/vpntorrent/netns.env, so read it rather than
# assuming the default subnet; fall back to localhost for a non-namespaced setup.
TS_HOST="$(sed -n 's/^VETH_VPN_IP=//p' /run/vpntorrent/netns.env 2>/dev/null | head -1)"
[ -n "$TS_HOST" ] || TS_HOST="10.42.0.2"
TS_URL="${TORRSERVER_URL:-http://$TS_HOST:8090}"

QUIET=0; ONLY=""
for a in "$@"; do
  case "$a" in
    --quiet|-q) QUIET=1 ;;
    --help|-h) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) echo "unknown option: $a" >&2; exit 2 ;;
    *) ONLY="$a" ;;
  esac
done

if [ -t 1 ]; then
  C_G=$'\033[1;32m'; C_R=$'\033[1;31m'; C_Y=$'\033[1;33m'; C_B=$'\033[1;36m'; C_D=$'\033[2m'; C_0=$'\033[0m'
else
  C_G=""; C_R=""; C_Y=""; C_B=""; C_D=""; C_0=""
fi

FAILS=0; WARNS=0
section() { [ -n "$ONLY" ] && [ "$ONLY" != "$1" ] && return 1; printf '\n%s▸ %s%s\n' "$C_B" "$2" "$C_0"; return 0; }
pass() { [ "$QUIET" = 1 ] || printf '  %s✓%s %s\n' "$C_G" "$C_0" "$1"; }
warn() { WARNS=$((WARNS+1)); printf '  %s!%s %s\n' "$C_Y" "$C_0" "$1"; [ -n "${2:-}" ] && printf '      %s→ %s%s\n' "$C_D" "$2" "$C_0"; return 0; }
fail() { FAILS=$((FAILS+1)); printf '  %s✗%s %s\n' "$C_R" "$C_0" "$1"; [ -n "${2:-}" ] && printf '      %s→ %s%s\n' "$C_D" "$2" "$C_0"; return 0; }
info() { [ "$QUIET" = 1 ] || printf '    %s%s%s\n' "$C_D" "$1" "$C_0"; }

have() { command -v "$1" >/dev/null 2>&1; }
is_root() { [ "$(id -u)" -eq 0 ]; }
# HTTP status only, empty on connection failure.
code() { curl -4 -s -o /dev/null -w '%{http_code}' --max-time "${2:-8}" "$1" 2>/dev/null; }
body() { curl -4 -s --max-time "${2:-8}" "$1" 2>/dev/null; }

printf '%sJellyFreedom doctor%s   %s\n' "$C_B" "$C_0" "$(date '+%Y-%m-%d %H:%M:%S')"
is_root || printf '%s(running unprivileged — some checks are limited; re-run with sudo for all of them)%s\n' "$C_D" "$C_0"

# ---------------------------------------------------------------- system
if section system "System"; then
  info "$( (. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME") || uname -s ) · kernel $(uname -r) · $(uname -m)"
  case "$(uname -m)" in
    x86_64|amd64|aarch64|arm64) pass "architecture $(uname -m) is supported" ;;
    *) fail "architecture $(uname -m) has no published build" "build from source, or use an amd64/arm64 machine" ;;
  esac
  have apt-get || warn "no apt-get — the installer targets Debian/Ubuntu" "other distros need manual installation of the components"
  avail_kb=$(df -Pk / 2>/dev/null | awk 'NR==2{print $4}')
  if [ -n "$avail_kb" ] && [ "$avail_kb" -lt 2097152 ]; then
    warn "less than 2GB free on /" "TorrServer's cache and Jellyfin metadata need headroom"
  else pass "disk space on / is adequate"; fi
  # /tmp separately from /, because it is usually a tmpfs sized at half of RAM and fills
  # from a direction nothing else warns about: FlareSolverr's browser leaves a scratch
  # directory per request, and on a snap Chromium those land somewhere the service's own
  # cleanup cannot reach. A full /tmp breaks package installs and the updater, which stages
  # its download there — and it says so in a way that never mentions /tmp.
  tmp_kb=$(df -Pk /tmp 2>/dev/null | awk 'NR==2{print $4}')
  if [ -n "$tmp_kb" ] && [ "$tmp_kb" -lt 262144 ]; then
    fail "/tmp has only $((tmp_kb/1024))MB free" \
         "clear it: sudo systemctl start jf-tmpreaper.service — then check the timer is on: systemctl status jf-tmpreaper.timer"
  elif [ -n "$tmp_kb" ]; then pass "/tmp has $((tmp_kb/1024))MB free"; fi
  mem_kb=$(awk '/MemTotal/{print $2}' /proc/meminfo 2>/dev/null)
  if [ -n "$mem_kb" ]; then
    mem_mb=$((mem_kb/1024))
    cache_mb=$(sed -n 's/^[[:space:]]*size_mb:[[:space:]]*\([0-9]*\).*/\1/p' "$CONF_DIR/config.yaml" 2>/dev/null | head -1)
    info "RAM ${mem_mb}MB, TorrServer cache ${cache_mb:-unset}MB"
    if [ -n "$cache_mb" ] && [ "$((cache_mb + 512))" -gt "$mem_mb" ]; then
      fail "TorrServer RAM cache (${cache_mb}MB) does not fit in ${mem_mb}MB of RAM" \
           "lower cache.size_mb in $CONF_DIR/config.yaml, or switch cache.mode to disk"
    fi
  fi
fi

# ---------------------------------------------------------------- install integrity
if section install "Install integrity"; then
  if [ -x "$APP_DIR/bin/orchestrator" ]; then
    pass "orchestrator binary present"
    if have file; then
      bf="$(file -b "$APP_DIR/bin/orchestrator")"
      case "$(uname -m):$bf" in
        x86_64:*x86-64*|aarch64:*aarch64*|arm64:*aarch64*|amd64:*x86-64*) pass "binary matches this CPU" ;;
        *) fail "binary is built for the wrong architecture" "reinstall the correct bundle: sudo jellyfreedom --update" ; info "$bf" ;;
      esac
    fi
  else fail "no orchestrator at $APP_DIR/bin/orchestrator" "sudo jellyfreedom repair"; fi

  [ -d "$APP_DIR/web" ] && pass "web assets present" || fail "web assets missing at $APP_DIR/web" "sudo jellyfreedom repair"
  [ -f "$APP_DIR/VERSION" ] && info "installed version $(cat "$APP_DIR/VERSION" 2>/dev/null)" \
    || warn "no VERSION file" "sudo jellyfreedom repair"
  [ -x "$APP_DIR/uninstall.sh" ] && pass "uninstaller is reachable" \
    || warn "no uninstaller at $APP_DIR/uninstall.sh" "sudo jellyfreedom repair — without it there is no clean removal path"
  [ -f "$CONF_DIR/config.yaml" ] && pass "config present" \
    || fail "no config at $CONF_DIR/config.yaml" "sudo jellyfreedom repair"
  [ -d "$DATA_DIR" ] && pass "data directory present" || fail "no data dir at $DATA_DIR" "sudo jellyfreedom repair"

  # public_url is baked into every .strm file. A placeholder or an unreachable address makes
  # Jellyfin show the item and fail to play it — the classic "says ready, plays nothing".
  pub="$(sed -n 's/^[[:space:]]*public_url:[[:space:]]*//p' "$CONF_DIR/config.yaml" 2>/dev/null | tr -d '"'"'" | head -1)"
  if [ -z "$pub" ]; then
    warn "server.public_url is not set" "set it to http://<this-host-ip>:1990 in $CONF_DIR/config.yaml"
  elif printf '%s' "$pub" | grep -q 'CHANGE-ME'; then
    fail "server.public_url is still the placeholder ($pub)" \
         "every .strm written with this points nowhere, so items appear in Jellyfin and refuse to play.
        Set it to http://<this-host-ip>:1990 in $CONF_DIR/config.yaml, restart, and re-request."
  else
    host="$(printf '%s' "$pub" | sed -e 's#^https\?://##' -e 's#/.*##')"
    if curl -4 -s -o /dev/null --max-time 6 "$pub/healthz" 2>/dev/null; then
      pass "public_url $pub is reachable"
    else
      warn "public_url $pub does not answer from this machine" \
           "Jellyfin must be able to reach it. Confirm $host is this host's LAN address."
    fi
  fi
fi

# ---------------------------------------------------------------- privileges
if section privs "Privileges"; then
  H="$VPN_DIR/jf-netns-helper"
  if [ -x "$H" ]; then
    owner="$(stat -c '%U:%G' "$H" 2>/dev/null)"; mode="$(stat -c '%a' "$H" 2>/dev/null)"
    downer="$(stat -c '%U' "$VPN_DIR" 2>/dev/null)"; dmode="$(stat -c '%a' "$VPN_DIR" 2>/dev/null)"
    [ "$owner" = "root:root" ] && pass "helper is root-owned" \
      || fail "helper is owned by $owner, not root:root" "chown root:root $H — a helper the service user can edit is a root shell"
    [ "$downer" = "root" ] && pass "helper directory is root-owned" \
      || fail "$VPN_DIR is owned by $downer" "chown root:root $VPN_DIR"
    case "$dmode" in *[2367]) fail "$VPN_DIR is group/other-writable (mode $dmode)" "chmod 755 $VPN_DIR" ;; *) : ;; esac
    [ "$mode" = "755" ] && pass "helper mode 0755" || warn "helper mode is $mode" "chmod 755 $H"
  else
    fail "privileged helper missing at $H" "sudo jellyfreedom repair — VPN status and activation cannot work without it"
  fi

  # The dashboard's Logs section shells out to journalctl as the service account. Without
  # membership of systemd-journal (or adm) it can only see its own entries, so the panel is
  # silently empty and looks broken.
  if id -nG "$SVC_USER" 2>/dev/null | grep -qwE 'systemd-journal|adm'; then
    pass "$SVC_USER can read the journal (dashboard logs work)"
  else
    fail "$SVC_USER cannot read the system journal" \
         "the dashboard's Logs section will be empty. Fix: sudo usermod -aG systemd-journal $SVC_USER && sudo systemctl restart jellyfreedom"
  fi

  S=/etc/sudoers.d/jellyfreedom
  if [ -r "$S" ] || is_root; then
    if [ -f "$S" ]; then
      pass "sudoers policy present"
      if grep -q ' /bin/systemctl' "$S" 2>/dev/null; then
        fail "sudoers uses /bin/systemctl, which sudo will never match" \
             "sudo jellyfreedom repair — sudo matches the resolved path /usr/bin/systemctl, so every service restart is denied"
      else pass "sudoers uses a systemctl path sudo can match"; fi
      if grep -qE '(curl|wg|ip|systemctl)[^#]*\*' "$S" 2>/dev/null; then
        fail "sudoers contains a wildcard rule" \
             "sudo jellyfreedom repair — a wildcarded root 'curl' or 'wg show' is equivalent to giving away root"
      else pass "sudoers has no wildcard rules"; fi
      if is_root && have visudo; then
        visudo -cf "$S" >/dev/null 2>&1 && pass "sudoers syntax valid" \
          || fail "sudoers file is INVALID" "fix or remove $S — an invalid file can break sudo for everyone"
      fi
    else fail "no sudoers policy at $S" "sudo jellyfreedom repair"; fi
  else
    info "sudoers not readable as this user — re-run with sudo to check it"
  fi
fi

# ---------------------------------------------------------------- services
UNITS="jellyfreedom vpntorrent-netns torrserver-netns vpntorrent-portforward flaresolverr prowlarr jellyfin"
if section services "Services"; then
  if have systemctl; then
    for u in $UNITS; do
      if ! systemctl cat "$u" >/dev/null 2>&1 && ! systemctl cat "$u.service" >/dev/null 2>&1; then
        case "$u" in
          jellyfin|prowlarr|flaresolverr) warn "$u is not installed" "it is optional but most setups need it — sudo jellyfreedom repair" ;;
          *) fail "$u unit is missing" "sudo jellyfreedom repair" ;;
        esac
        continue
      fi
      st="$(systemctl is-active "$u" 2>/dev/null)"
      case "$st" in
        active) pass "$u active" ;;
        activating) warn "$u is still starting" "re-check in a moment" ;;
        *)
          fail "$u is $st" "journalctl -u $u -n 50 --no-pager"
          if [ "$QUIET" != 1 ]; then
            journalctl -u "$u" -n 4 --no-pager -o cat 2>/dev/null | sed 's/^/      | /'
          fi ;;
      esac
    done
  else warn "no systemctl — cannot check services"; fi
fi

# ---------------------------------------------------------------- ports
if section ports "Ports"; then
  # NOTE: TorrServer (8090) is deliberately absent from this list. It listens INSIDE the
  # vpntorrent namespace, so it never appears in the host's socket table — reporting it as
  # "nothing listening" is a false alarm. It is checked over HTTP in its own section instead.
  # 8192 is FlareSolverr's Prometheus port; off by default, but a conflict on it is silent.
  for p in 1990:orchestrator 8096:jellyfin 8191:flaresolverr 9696:prowlarr 8192:flaresolverr-metrics; do
    port="${p%%:*}"; who="${p##*:}"
    if have ss; then
      line="$(ss -tlnp 2>/dev/null | awk -v P=":$port\$" '$4 ~ P {print; exit}')"
      if [ -n "$line" ]; then
        proc="$(printf '%s' "$line" | sed -n 's/.*users:((\"\([^"]*\)\".*/\1/p')"
        pass "$port listening (${proc:-$who})"
      else
        case "$who" in
          flaresolverr-metrics) : ;;
          *) warn "nothing listening on $port ($who)" "systemctl status $who ; journalctl -u $who -n 30" ;;
        esac
      fi
    fi
  done
fi

# ---------------------------------------------------------------- orchestrator
CONFIGURED=""
if section orchestrator "Orchestrator"; then
  hz="$(body "$ORCH_URL/healthz")"
  if [ -n "$hz" ]; then
    pass "answering on $ORCH_URL"
    info "$hz"
  else
    fail "not answering on $ORCH_URL" "systemctl status jellyfreedom ; journalctl -u jellyfreedom -n 50"
  fi
  CONFIGURED="$(body "$ORCH_URL/api/configured")"
  if [ -n "$CONFIGURED" ]; then
    for k in tmdb prowlarr jellyfin torrserver; do
      if printf '%s' "$CONFIGURED" | grep -q "\"$k\":true"; then pass "$k is configured"
      else fail "$k is NOT configured" "open $ORCH_URL/dashboard/ → Settings → Connections"; fi
    done
  fi
fi

# ---------------------------------------------------------------- prowlarr
if section prowlarr "Prowlarr"; then
  st="$(code http://127.0.0.1:9696/ping)"
  if [ "$st" = "200" ]; then
    pass "Prowlarr answering"
    # The invisible trap: a valid key with ZERO indexers returns empty results forever, and
    # nothing in the UI ever says why.
    n=""
    if [ -n "$CONFIGURED" ]; then
      n="$(printf '%s' "$CONFIGURED" | sed -n 's/.*"indexer_count":\([-0-9]*\).*/\1/p')"
    fi
    case "$n" in
      ""|-1) warn "could not determine how many indexers are configured" \
                  "open http://127.0.0.1:9696 → Indexers, and confirm at least one is added and testing green" ;;
      0)  fail "Prowlarr has ZERO indexers configured" \
               "open http://127.0.0.1:9696 → Indexers → Add. Without one, every search returns nothing and the UI cannot tell you why." ;;
      *)  pass "$n indexer(s) configured" ;;
    esac
  else
    fail "Prowlarr not answering on 9696 (HTTP ${st:-no response})" "systemctl status prowlarr ; journalctl -u prowlarr -n 50"
  fi
fi

# ---------------------------------------------------------------- flaresolverr
if section flaresolverr "FlareSolverr"; then
  # Three rungs, because each proves strictly more than the last. Stopping at rung 2 is
  # what let a broken browser look healthy: the startup self-test resolves the binary and
  # reads its version but NAVIGATES NOWHERE.
  h="$(code "$FS_URL/health")"
  if [ "$h" != "200" ]; then
    fail "FlareSolverr is not listening on $FS_URL (HTTP ${h:-no response})" \
         "systemctl status flaresolverr ; journalctl -u flaresolverr -n 50"
  else
    pass "rung 1/3 — service is up (/health)"
    ua="$(body "$FS_URL/")"
    if printf '%s' "$ua" | grep -q 'Chrome/'; then
      pass "rung 2/3 — browser launches ($(printf '%s' "$ua" | sed -n 's/.*Chrome\/\([0-9]*\).*/Chrome \1/p' | head -1))"
    else
      fail "rung 2/3 — browser will not launch" \
           "journalctl -u flaresolverr -n 80 | grep -iE 'chrome|sandbox|driver'"
    fi
    # Rung 3 is the only one that proves it can actually FETCH.
    resp="$(curl -4 -s --max-time 70 -XPOST "$FS_URL/v1" -H 'Content-Type: application/json' \
            -d '{"cmd":"request.get","url":"https://example.com","maxTimeout":60000}' 2>/dev/null)"
    if printf '%s' "$resp" | grep -q '"status": *"ok"'; then
      pass "rung 3/3 — a real page fetch succeeded"
    else
      msg="$(printf '%s' "$resp" | sed -n 's/.*"message": *"\([^"]*\)".*/\1/p' | head -1)"
      fail "rung 3/3 — FlareSolverr is up but CANNOT FETCH${msg:+: $msg}" \
           "this is the failure that looks like 'no search results'. Try: sudo jellyfreedom repair
        then: journalctl -u flaresolverr -n 80 --no-pager
        Common causes: the bundled Chrome cannot start a sandbox (needs --no-sandbox),
        or its chromedriver cache is stale — repair re-applies both."
      [ -n "$resp" ] && info "$(printf '%s' "$resp" | head -c 300)"
    fi
  fi
fi

# ---------------------------------------------------------------- torrserver
if section torrserver "TorrServer"; then
  st="$(code "$TS_URL/echo")"
  # Fall back to localhost: a user may run TorrServer outside the namespace.
  if { [ -z "$st" ] || [ "$st" = "000" ]; } && [ "$TS_URL" != "http://127.0.0.1:8090" ]; then
    st="$(code http://127.0.0.1:8090/echo)"
    [ -n "$st" ] && [ "$st" != "000" ] && TS_URL="http://127.0.0.1:8090"
  fi
  if [ -n "$st" ] && [ "$st" != "000" ]; then pass "TorrServer answering at $TS_URL (HTTP $st)"
  else fail "TorrServer not answering on $TS_URL" \
            "systemctl status torrserver-netns ; journalctl -u torrserver-netns -n 50
        It runs inside the VPN namespace, so check the VPN section below first."; fi
fi

# ---------------------------------------------------------------- web sources
#
# Three things have to line up for a pasted link to play, and they fail independently:
# the extractor has to exist, the in-namespace proxy has to be listening, and the config
# has to switch the feature on. Reporting them separately is the point — "web sources
# don't work" is otherwise three different problems wearing one symptom.
if section websources "Web sources (paste-a-link)"; then
  ws_on=""
  if [ -f "$CONF_DIR/config.yaml" ]; then
    # The `enabled:` under web_sources:, not any other enabled: in the file. awk rather
    # than grep because the key name is not unique and indentation is what scopes it.
    ws_on="$(awk '/^web_sources:/{inblk=1; next} /^[^[:space:]#]/{inblk=0}
                  inblk && /^[[:space:]]+enabled:/{gsub(/[^a-z]/,"",$2); print $2; exit}' \
             "$CONF_DIR/config.yaml" 2>/dev/null)"
  fi

  ytdlp=""
  for c in /usr/local/bin/yt-dlp /usr/bin/yt-dlp; do [ -x "$c" ] && { ytdlp="$c"; break; }; done
  [ -n "$ytdlp" ] || ytdlp="$(command -v yt-dlp 2>/dev/null)"

  if [ "$ws_on" != "true" ]; then
    # Not a failure: this is the state of every install that predates the feature, and of
    # anyone who does not want it. Say exactly how to turn it on.
    info "web sources are off — web_sources.enabled is not true in $CONF_DIR/config.yaml"
    if [ "$QUIET" != 1 ]; then
      printf '      %s→ to switch them on, add this and restart (sudo systemctl restart jellyfreedom):%s\n' "$C_D" "$C_0"
      printf '        %sweb_sources:%s\n' "$C_D" "$C_0"
      printf '        %s  enabled: true%s\n' "$C_D" "$C_0"
      printf '        %s  temp_dir: %s/tmp%s\n' "$C_D" "$DATA_DIR" "$C_0"
      printf '        %s  proxy_addr: "%s:1080"%s\n' "$C_D" "$TS_HOST" "$C_0"
    fi
  else
    if [ -n "$ytdlp" ]; then
      # --version runs the whole self-extracting bundle, so it also proves TMPDIR works.
      v="$(TMPDIR="$DATA_DIR/tmp" "$ytdlp" --version 2>/dev/null | head -1)"
      if [ -n "$v" ]; then pass "yt-dlp $v at $ytdlp"
      else fail "yt-dlp is present but will not run" \
                "usually no space in its scratch dir. Check: df -h $DATA_DIR ; ls -ld $DATA_DIR/tmp
        It unpacks ~76MB on every run, so it must NOT be pointed at a small or full /tmp."; fi
    else
      fail "yt-dlp is not installed" "sudo jellyfreedom repair websources"
    fi

    if have systemctl; then
      st="$(systemctl is-active jf-netnsproxy.service 2>/dev/null)"
      case "$st" in
        active) pass "jf-netnsproxy active (the VPN proxy web sources dial through)" ;;
        "")     fail "jf-netnsproxy unit is missing" "sudo jellyfreedom repair" ;;
        *)      fail "jf-netnsproxy is $st" "journalctl -u jf-netnsproxy -n 50 --no-pager
        It runs inside the VPN namespace, so check the VPN section below first." ;;
      esac
    fi

    # The proxy listens on the namespace's veth address, so the host CAN reach it — the
    # same way it reaches TorrServer. A refused connection here is the difference between
    # "the feature is misconfigured" and "the tunnel is down".
    if have curl; then
      pst="$(curl -4 -s -o /dev/null --max-time 8 --socks5-hostname "$TS_HOST:1080" \
             -w '%{http_code}' https://api.github.com/ 2>/dev/null)"
      if [ -n "$pst" ] && [ "$pst" != "000" ]; then pass "the VPN proxy reaches the internet (HTTP $pst)"
      else warn "the VPN proxy did not reach the internet through $TS_HOST:1080" \
                "that is expected while the tunnel is down — see the VPN section. Web sources fail closed."; fi
    fi
  fi
fi

# ---------------------------------------------------------------- jellyfin
if section jellyfin "Jellyfin"; then
  st="$(code http://127.0.0.1:8096/System/Info/Public)"
  if [ "$st" = "200" ]; then pass "Jellyfin answering"
  else fail "Jellyfin not answering on 8096 (HTTP ${st:-no response})" "systemctl status jellyfin ; journalctl -u jellyfin -n 50"; fi
fi

# ---------------------------------------------------------------- vpn
if section vpn "VPN and kill switch"; then
  if [ ! -e /var/run/netns/vpntorrent ]; then
    fail "the vpntorrent network namespace does not exist" "sudo systemctl restart vpntorrent-netns"
  else
    pass "network namespace exists"
    H="$VPN_DIR/jf-netns-helper"
    if [ ! -x "$H" ]; then
      info "tunnel state not checked — the privileged helper is missing (see Privileges above)"
    elif ! is_root && ! sudo -n true 2>/dev/null; then
      info "cannot query the tunnel as this user — re-run with sudo"
    else
      runner=""; is_root || runner="sudo -n"
      if $runner "$H" status >/dev/null 2>&1; then
        pass "WireGuard tunnel is up"
        ip="$($runner "$H" exit-ip 2>/dev/null)"
        if [ -n "$ip" ]; then
          host_ip="$(curl -4 -s --max-time 8 https://1.1.1.1/cdn-cgi/trace 2>/dev/null | sed -n 's/^ip=//p')"
          if [ -n "$host_ip" ] && [ "$ip" = "$host_ip" ]; then
            fail "TORRENT TRAFFIC IS NOT GOING THROUGH THE VPN — namespace exit IP equals this host's IP ($ip)" \
                 "stop torrenting and investigate now: sudo systemctl restart vpntorrent-netns"
          else
            pass "namespace exits via the VPN (${ip}), separate from this host's address"
          fi
        fi
      else
        warn "no WireGuard tunnel is up" \
             "this is expected on a fresh install. Upload and activate a config: $ORCH_URL/dashboard/ → VPN.
        Until then the namespace fails closed and torrents cannot reach the internet — which is the intended safe state."
      fi
    fi
  fi
fi

# ---------------------------------------------------------------- library
if section library "Library folders"; then
  # Parse ONLY the `path:` keys inside the top-level `libraries:` block, strip inline
  # comments and quotes, and read line-by-line so a path containing spaces survives.
  # A naive grep for `path:` also matches torrserver.cache.path and word-splits trailing
  # comments into nonexistent folder names.
  dirs_file="$(mktemp)"
  awk '
    /^[A-Za-z_][A-Za-z0-9_]*:/ { inlib = ($0 ~ /^libraries:[[:space:]]*$/) ? 1 : 0 }
    inlib && /^[[:space:]]*-?[[:space:]]*path:[[:space:]]*/ {
      line = $0
      sub(/^[[:space:]]*-?[[:space:]]*path:[[:space:]]*/, "", line)
      sub(/[[:space:]]+#.*$/, "", line)
      gsub(/^["\x27]|["\x27]$/, "", line)
      sub(/[[:space:]]+$/, "", line)
      if (length(line)) print line
    }
  ' "$CONF_DIR/config.yaml" 2>/dev/null > "$dirs_file"

  if [ ! -s "$dirs_file" ]; then
    warn "no library paths found in $CONF_DIR/config.yaml" "check the libraries: section of your config"
  else
    while IFS= read -r d; do
      [ -n "$d" ] || continue
      if [ ! -d "$d" ]; then
        fail "library folder $d does not exist" "sudo install -d -o $SVC_USER -g $SVC_USER \"$d\" — Jellyfin cannot add a folder that is not there"
      elif is_root && ! sudo -u "$SVC_USER" test -w "$d" 2>/dev/null; then
        fail "$d is not writable by $SVC_USER" "sudo chown -R $SVC_USER:$SVC_USER \"$d\""
      else
        pass "library folder $d"
      fi
    done < "$dirs_file"
  fi
  rm -f "$dirs_file"
fi

# ---------------------------------------------------------------- summary
printf '\n'
if [ "$FAILS" -eq 0 ] && [ "$WARNS" -eq 0 ]; then
  printf '%sEverything checks out.%s\n' "$C_G" "$C_0"; exit 0
fi
[ "$WARNS" -gt 0 ] && printf '%s%d warning(s)%s\n' "$C_Y" "$WARNS" "$C_0"
if [ "$FAILS" -gt 0 ]; then
  printf '%s%d failure(s).%s Most problems are fixed by re-running the installer, which keeps your\n' "$C_R" "$FAILS" "$C_0"
  printf 'config, database and VPN configs:  %ssudo jellyfreedom repair%s\n' "$C_B" "$C_0"
  exit 1
fi
exit 0
