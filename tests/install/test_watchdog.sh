#!/usr/bin/env bash
# The watchdog is EXECUTED here, with every privileged or networked command mocked.
#
# On a box with no WireGuard config activated yet — every fresh install, until the user
# uploads one — the watchdog took the helper's "not configured yet" (vpn-up exit 3) for an
# outage and rebuilt the namespace every minute. Each rebuild restarted TorrServer and,
# through BindsTo=, jf-netnsproxy, until systemd's start limit left the proxy failed for
# good: pasted links stayed broken even after a VPN was activated, because activation never
# restarts the proxy.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

PASS=0; FAIL=0
ok_()  { printf '  \033[1;32m✓\033[0m %s\n' "$1"; PASS=$((PASS+1)); }
bad_() { printf '  \033[1;31m✗\033[0m %s\n     %s\n' "$1" "$2"; FAIL=$((FAIL+1)); }

R="$(mktemp -d)"; trap 'rm -rf "$R"' EXIT

# setup <vpn-up exit code> — a fresh sandbox: the real watchdog beside a mock helper, and
# mocks for everything it would otherwise do to the machine. Probes always fail (no tunnel).
setup() {
  rm -rf "$R/w"; mkdir -p "$R/w/bin" "$R/w/run" "$R/w/opt"
  cp vpntorrent/watchdog.sh "$R/w/opt/watchdog.sh"
  LOGF="$R/w/calls.log"; : > "$LOGF"
  cat > "$R/w/opt/jf-netns-helper" <<EOF
#!/usr/bin/env bash
echo "helper \$*" >> "$LOGF"
case "\$1" in
  vpn-up) [ "$1" = 3 ] && echo "no active WireGuard config at /var/lib/jellyfreedom/vpnconfigs/wg0-vpntorrent.conf" >&2; exit $1 ;;
esac
exit 0
EOF
  local c
  for c in systemctl logger iptables sysctl; do
    printf '#!/usr/bin/env bash\necho "%s $*" >> "%s"\nexit 0\n' "$c" "$LOGF" > "$R/w/bin/$c"
  done
  # logger also gets stdin when piped to
  printf '#!/usr/bin/env bash\nif [ $# -le 2 ]; then while IFS= read -r l; do echo "logger $* $l" >> "%s"; done; else echo "logger $*" >> "%s"; fi\nexit 0\n' "$LOGF" "$LOGF" > "$R/w/bin/logger"
  printf '#!/usr/bin/env bash\nexit 1\n' > "$R/w/bin/ip"      # no namespace route: every probe fails
  printf '#!/usr/bin/env bash\nexit 7\n' > "$R/w/bin/curl"    # nothing answers
  printf '#!/usr/bin/env bash\nexit 0\n' > "$R/w/bin/sleep"   # do not actually wait
  chmod +x "$R/w/bin/"* "$R/w/opt/"*
}
run_wd() { PATH="$R/w/bin:$PATH" VPNTORRENT_RUN_STATE="$R/w/run" bash "$R/w/opt/watchdog.sh" >/dev/null 2>&1; RC=$?; }
has()    { grep -qF -- "$1" "$LOGF"; }
status() { cat "$R/w/run/vpntorrent-status" 2>/dev/null; }

printf '\033[1;33m▸ no VPN config yet: fail closed and leave the namespace alone\033[0m\n'
setup 3
run_wd
[ "$RC" = 0 ] && ok_ "exits 0 — an unconfigured VPN is not a failure" || bad_ "exits 0" "rc=$RC (the timer unit is marked failed every minute)"
has 'helper vpn-up' && ok_ "asked the helper (vpn-up) whether a config exists" || bad_ "asked the helper" "vpn-up was never called"
has 'systemctl restart vpntorrent-netns' && bad_ "no namespace rebuild" "it restarted vpntorrent-netns (and with it jf-netnsproxy)" || ok_ "no namespace rebuild"
has 'systemctl stop torrserver-netns' && bad_ "TorrServer left running" "it was stopped" || ok_ "TorrServer left running"
[ "$(status)" = unconfigured ] && ok_ "status: unconfigured" || bad_ "status: unconfigured" "got '$(status)'"
has 'no WireGuard config activated yet' && ok_ "says why, in plain words" || bad_ "says why" "nothing logged"
n1=$(grep -c 'no WireGuard config activated yet' "$LOGF")
run_wd
n2=$(grep -c 'no WireGuard config activated yet' "$LOGF")
[ "$RC" = 0 ] && [ "$n1" = 1 ] && [ "$n2" = 1 ] && ok_ "logged once, not every minute" || bad_ "logged once" "after two runs: $n2 lines, rc=$RC"

printf '\033[1;33m▸ a configured tunnel that is down is still recovered\033[0m\n'
setup 0
run_wd
has 'helper vpn-down' && has 'helper vpn-up' && ok_ "bounces WireGuard" || bad_ "bounces WireGuard" "$(cat "$LOGF")"
has 'systemctl restart vpntorrent-netns.service' && ok_ "falls back to a full namespace rebuild" || bad_ "full rebuild" "the recovery path was lost"
[ "$RC" = 1 ] && ok_ "exits 1 while it stays down" || bad_ "exits 1" "rc=$RC"
case "$(status)" in "down: needs new wireguard config") ok_ "status says it needs attention" ;; *) bad_ "status" "got '$(status)'" ;; esac

printf '\n'
if [ "$FAIL" = 0 ]; then printf '\033[1;32m%d checks passed\033[0m\n' "$PASS"; exit 0
else printf '\033[1;31m%d passed, %d failed\033[0m\n' "$PASS" "$FAIL"; exit 1; fi
