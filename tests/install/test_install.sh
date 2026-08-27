#!/usr/bin/env bash
# test_install.sh — behavioural contract for release/install.sh.
#
# These tests define what the installer MUST do. They run hermetically: no root, no
# network, no side effects outside a temp dir. Every privileged or networked command is
# intercepted by the harness and asserted on.
#
#   ./tests/install/test_install.sh
#
# Requires the installer to honour three testability variables:
#   JF_DESTDIR       prefix every filesystem path it writes with this
#   JF_ASSUME_ROOT   skip the euid check
#   JF_NONINTERACTIVE  no prompts, no sleeps

source "$(dirname "${BASH_SOURCE[0]}")/harness.sh"

# ---------------------------------------------------------------- fresh install
describe "fresh install on a clean machine"
sandbox_new fresh
mock_standard
run_installer
assert_exit 0
# the parts we own
assert_exec "$DEST/opt/jellyfreedom/bin/orchestrator"
assert_file "$DEST/etc/jellyfreedom/config.yaml"
assert_mode "$DEST/etc/jellyfreedom/config.yaml" 640
# service users are created, not assumed
assert_ran_re 'useradd .*jellyfreedom'
assert_ran_re 'useradd .*torrserver'
# systemd units
assert_file "$DEST/etc/systemd/system/jellyfreedom.service"
assert_file "$DEST/etc/systemd/system/vpntorrent-netns.service"
assert_file "$DEST/etc/systemd/system/torrserver-netns.service"
assert_ran 'systemctl daemon-reload'
assert_ran_re 'systemctl enable .*jellyfreedom'

assert_no_shell_errors

describe "REGRESSION: sudoers must use the path sudo actually matches"
# sudo matches the resolved command path and does not follow the merged-usr
# /bin -> /usr/bin symlink. A rule written as /bin/systemctl never matches, so every
# dashboard service-restart is silently denied. This shipped broken.
assert_file "$DEST/etc/sudoers.d/jellyfreedom"
assert_contains "$DEST/etc/sudoers.d/jellyfreedom" '/usr/bin/systemctl'
assert_not_contains "$DEST/etc/sudoers.d/jellyfreedom" ' /bin/systemctl'
assert_mode "$DEST/etc/sudoers.d/jellyfreedom" 440
assert_ran_re 'visudo -c'

describe "the dashboard updater is installed safely"
# Reachable through NOPASSWD sudo, so it must be root-owned and take no arguments — the
# caller must not be able to choose what gets installed as root.
assert_exec "$DEST/opt/jellyfreedom/jf-update"
assert_mode "$DEST/opt/jellyfreedom/jf-update" 755
assert_contains "$DEST/etc/sudoers.d/jellyfreedom" '/opt/jellyfreedom/jf-update'
_check; grep -qE 'jf-update[[:space:]]*\*' "$DEST/etc/sudoers.d/jellyfreedom" \
  && _fail "updater sudo rule takes no arguments" "a wildcard would let the caller pick the payload" \
  || _pass "updater sudo rule takes no arguments"
_check; grep -q 'systemd-run' "$REPO_ROOT/release/jf-update" \
  && _pass "updater runs detached from our cgroup" \
  || _fail "updater runs detached" "systemd kills the cgroup on restart, so it would die mid-update"
_check; sed 's/[[:space:]]*#.*//' "$REPO_ROOT/release/jellyfreedom" | grep -q 'CHECKSUM MISMATCH' \
  && _pass "self-update verifies the download before installing" \
  || _fail "self-update verifies the download" "one click would install unverified bytes as root"

describe "REGRESSION: sudoers must contain no wildcards"
# `curl *` as root is arbitrary root file write (-o /etc/sudoers.d/x) and arbitrary root
# file read (file:///etc/shadow). Any RCE in the orchestrator became instant host root.
assert_not_contains "$DEST/etc/sudoers.d/jellyfreedom" 'curl *'
assert_not_contains "$DEST/etc/sudoers.d/jellyfreedom" 'wg show *'
assert_not_contains "$DEST/etc/sudoers.d/jellyfreedom" '*'

describe "REGRESSION: the service must not be able to rewrite its own code"
# The service account also holds `systemctl restart jellyfreedom`, so a writable binary plus a
# self-restart is persistence: overwrite, bounce, done. Nothing in the orchestrator writes to
# APP_DIR, so code stays root-owned and only DATA is service-owned.
# The harness runs unprivileged, so xinstall drops -o/-g and the resulting uid is always the
# test user. Assert the installer's INTENT in the source instead, which is what would change
# if someone reverted this.
_check; grep -qE 'xinstall -o root -g root -m 755 "\$SRC/bin/orchestrator"' "$REPO_ROOT/release/install.sh" \
  && _pass "installer places the binary root-owned" \
  || _fail "installer places the binary root-owned" "the service could overwrite its own code"
_check; sed 's/[[:space:]]*#.*//' "$REPO_ROOT/release/install.sh" | grep -qE 'xchown -R "\$RUN_USER" "\$APP_DIR' \
  && _fail "code is not chowned to the run user" "APP_DIR is handed back to the service account" \
  || _pass "code is not chowned to the run user"
_check; grep -qE 'xinstall -d -o "\$SVC_USER" -g "\$SVC_USER" "\$DATA_DIR"' "$REPO_ROOT/release/install.sh" \
  && _pass "data dir is service-owned (writable)" \
  || _fail "data dir is service-owned" "the service could not write its database"

describe "REGRESSION: AppArmor allows wg-quick to read the sanitised config"
# The helper hands wg-quick a sanitised copy under /run/vpntorrent. wg-quick is root but
# AppArmor-confined, so without this rule it fails with a bare "Permission denied" and the
# tunnel never comes up — observed on a live upgrade.
assert_file "$DEST/etc/apparmor.d/local/wg-quick"
assert_contains "$DEST/etc/apparmor.d/local/wg-quick" '/run/vpntorrent/** r,'
assert_contains "$DEST/etc/apparmor.d/local/wg-quick" 'vpnconfigs/** r,'

describe "REGRESSION: the service account can read the journal"
# The dashboard renders logs by running journalctl as the service account. A plain system
# account sees only its own entries, so the Logs section goes silently empty — observed
# after moving this deployment off a human account that happened to be in 'adm'.
_check; grep -q 'usermod -aG systemd-journal' "$REPO_ROOT/release/install.sh" \
  && _pass "installer grants journal read access" \
  || _fail "installer grants journal read access" "the dashboard Logs section will be empty"

describe "REGRESSION: the privileged helper is installed root-owned"
# The helper is the only path to root. If the service user can write it, it is worthless.
assert_exec "$DEST/opt/vpntorrent/jf-netns-helper"
assert_mode "$DEST/opt/vpntorrent/jf-netns-helper" 755

describe "REGRESSION: public_url must not ship as the placeholder"
# public_url is written into EVERY .strm file Jellyfin reads. Left as CHANGE-ME-LAN-IP the
# whole library points at a host that does not exist, which presents as "it says ready but
# nothing plays" — with nothing anywhere explaining why.
assert_not_contains "$DEST/etc/jellyfreedom/config.yaml" 'CHANGE-ME-LAN-IP'

# ---------------------------------------------------------------- idempotency
describe "second run is safe and preserves user data"
CONFIG="$DEST/etc/jellyfreedom/config.yaml"
printf 'MY-EDITED-CONFIG\n' > "$CONFIG"
: > "$LOG"
run_installer
assert_exit 0
assert_unchanged "$CONFIG" 'MY-EDITED-CONFIG'
assert_no_shell_errors
assert_not_ran 'useradd --system --no-create-home --shell /usr/sbin/nologin jellyfreedom'

# ---------------------------------------------------------------- arch handling
describe "REGRESSION: unsupported architecture fails loudly, never silently"
# install.sh used to fetch flaresolverr_linux_x64.tar.gz unconditionally, so an arm64
# user got an x86-64 binary and a service that failed to exec, with no explanation.
sandbox_new arch-armhf
mock_standard
mock_cmd uname 0 'armv7l'
run_installer
assert_output 'arch'

# ---------------------------------------------------------------- download failures
describe "a failed download is reported, never silently skipped"
sandbox_new download-fail
mock_standard
mock_cmd curl 22        # curl's HTTP-error exit code
run_installer
assert_output 'fail'
assert_not_ran 'systemctl enable --now torrserver-netns.service'

describe "a partial install still reports per-component status"
assert_output 'flaresolverr'
assert_output 'torrserver'

# ---------------------------------------------------------------- preexisting components
describe "existing third-party components are detected and left alone"
sandbox_new preexisting
mock_standard
mkdir -p "$DEST/opt/flaresolverr" "$DEST/usr/local/bin"
printf 'THEIR-BINARY\n' > "$DEST/opt/flaresolverr/flaresolverr"; chmod +x "$DEST/opt/flaresolverr/flaresolverr"
printf 'THEIR-TORRSERVER\n' > "$DEST/usr/local/bin/torrserver"; chmod +x "$DEST/usr/local/bin/torrserver"
run_installer
assert_exit 0
assert_unchanged "$DEST/opt/flaresolverr/flaresolverr" 'THEIR-BINARY'
assert_unchanged "$DEST/usr/local/bin/torrserver" 'THEIR-TORRSERVER'

# ---------------------------------------------------------------- uninstall reachable
describe "REGRESSION: uninstall must be reachable after a curl|bash install"
# get.sh deletes the extracted bundle on exit, and install.sh never copied uninstall.sh
# anywhere persistent — so one-liner users had no documented way to remove the software.
sandbox_new uninstall-reachable
mock_standard
run_installer
assert_exec "$DEST/opt/jellyfreedom/uninstall.sh"

# ---------------------------------------------------------------- library dirs
describe "REGRESSION: library folders exist before the user is told to add them"
# The installer told users to add the .strm folders to Jellyfin, but nothing created them
# until the first successful request — so Jellyfin could not see the path they were told
# to type in.
assert_file "$DEST/opt/jellyfreedom/uninstall.sh"
[ -d "$DEST/srv/jellyfreedom/movies" ] && _check && _pass "library dir: /srv/jellyfreedom/movies" || { _check; _fail "library dir: /srv/jellyfreedom/movies" "not created"; }
[ -d "$DEST/srv/jellyfreedom/tv" ] && _check && _pass "library dir: /srv/jellyfreedom/tv" || { _check; _fail "library dir: /srv/jellyfreedom/tv" "not created"; }

# ---------------------------------------------------------------- flaresolverr
describe "REGRESSION: never truncate the bundled Chrome"
# The old installer overwrote FlareSolverr's 462MB bundled Chrome ELF with a 61-byte wrapper
# pointing at a snap shim that did not exist. There was no backup, re-running reported
# "present — left alone", and only a 233MB re-download could recover. This is the bug that
# made FlareSolverr unfixable for the reporting user.
sandbox_new chrome-preserved
mock_standard
mkdir -p "$DEST/opt/flaresolverr/_internal/chrome"
printf '\177ELF-PRETEND-THIS-IS-462MB-OF-CHROME\n' > "$DEST/opt/flaresolverr/_internal/chrome/chrome"
chmod +x "$DEST/opt/flaresolverr/_internal/chrome/chrome"
printf 'THEIR-FS\n' > "$DEST/opt/flaresolverr/flaresolverr"; chmod +x "$DEST/opt/flaresolverr/flaresolverr"
run_installer
assert_exit 0
assert_contains "$DEST/opt/flaresolverr/_internal/chrome/chrome" 'ELF-PRETEND-THIS-IS-462MB-OF-CHROME'

describe "REGRESSION: never install the transitional chromium-browser package"
# On Ubuntu `apt-get install chromium-browser` SUCCEEDS while installing no browser: it is a
# transitional package whose only executable execs /snap/bin/chromium. Because apt exits 0,
# the `|| snap install` fallback never ran and the wrapper pointed at nothing.
assert_not_ran 'apt-get install -y -qq chromium-browser'
assert_not_ran 'snap install chromium'

describe "REGRESSION: FlareSolverr gets GPU device group access"
# Moving FlareSolverr off root removed its incidental access to /dev/dri, and Chrome's GPU
# process then dies taking the renderer with it. The failure surfaces only as
# "invalid session id: session deleted as the browser has closed the connection".
_check; grep -q 'usermod -aG "$grp" "$FS_USER"' "$REPO_ROOT/release/install.sh" \
  && _pass "service user is added to the video/render groups" \
  || _fail "service user is added to the video/render groups" "Chrome will crash its renderer as a non-root user"

describe "REGRESSION: FlareSolverr unit is hardened correctly"
FSU="$DEST/etc/systemd/system/flaresolverr.service"
assert_file "$FSU"
assert_contains "$FSU" 'User=flaresolverr'
# Was 0.0.0.0: FlareSolverr fetches arbitrary URLs with no auth, so that published an open
# SSRF proxy to the whole LAN.
assert_contains "$FSU" 'Environment=HOST=127.0.0.1'
# The chromedriver cache path is a hardcoded ~/.local/share/... resolved at import time;
# without an explicit HOME a system account creates a literal "~" directory instead.
assert_contains "$FSU" 'Environment=HOME='
# Chrome reserves a very large allocator region and aborts under LimitAS; PrivateTmp breaks
# the system-chromium fallback.
assert_not_active "$FSU" 'LimitAS='
assert_not_active "$FSU" 'PrivateTmp=yes'
# CAPTCHA_SOLVER is read nowhere in FlareSolverr 3.5.0 — dead config.
assert_not_active "$FSU" 'CAPTCHA_SOLVER'
# A crash used to leave Chrome running as an orphan; they accumulated to 1.19GB on a live box.
assert_contains "$FSU" 'KillMode=control-group'
assert_contains "$FSU" "ExecStartPre="

# ---------------------------------------------------------------- torrserver pin
describe "REGRESSION: TorrServer version is resolved, not pinned to a dead tag"
# MatriX.141.2 was hardcoded and later removed upstream, so the download 404'd and every
# fresh install silently ended up with no streaming engine while still printing "Installed."
_check; sed 's/[[:space:]]*#.*//' "$REPO_ROOT/release/install.sh" | grep -q 'MatriX.141.2' \
  && _fail "no dead TorrServer pin" "MatriX.141.2 is still used in code" \
  || _pass "no dead TorrServer pin"
_check; grep -q 'releases/latest' "$REPO_ROOT/release/install.sh" \
  && _pass "TorrServer release is resolved at runtime" \
  || _fail "TorrServer release is resolved at runtime" "no latest-release lookup found"

# ---------------------------------------------------------------- jellyfin
describe "REGRESSION: Jellyfin installer cannot block on a tty prompt"
# Upstream reads /dev/tty specifically so that piping into bash cannot skip its prompt. The
# old call also sent output to /dev/null, so it hung forever with nothing on screen.
_check; grep -q 'SKIP_CONFIRM=true' "$REPO_ROOT/release/install.sh" \
  && _pass "Jellyfin install passes SKIP_CONFIRM" \
  || _fail "Jellyfin install passes SKIP_CONFIRM" "it will hang on the tty prompt"
_check; grep -qE 'install-debuntu\.sh.*>/dev/null' "$REPO_ROOT/release/install.sh" \
  && _fail "Jellyfin output is not discarded" "output is still piped to /dev/null" \
  || _pass "Jellyfin output is not discarded"

describe "REGRESSION: Jellyfin is installed from the apt repo, not the upstream script"
# The upstream convenience script reads /dev/tty (so piping into bash cannot skip its
# prompt), hard-requires 2GB free on BOTH /var/lib and /tmp — which a small VPS with a
# tmpfs /tmp does not have, verified failing on a clean Ubuntu 24.04 container — rejects
# unknown distro codenames, and can exit 0 having installed nothing.
_check; sed 's/[[:space:]]*#.*//' "$REPO_ROOT/release/install.sh" | grep -q 'install-debuntu' \
  && _fail "does not pipe the upstream Jellyfin script" "still shelling out to install-debuntu.sh" \
  || _pass "does not pipe the upstream Jellyfin script"
_check; grep -q 'sources.list.d/jellyfin.sources' "$REPO_ROOT/release/install.sh" \
  && _pass "writes the Jellyfin apt source itself" \
  || _fail "writes the Jellyfin apt source itself" "no deterministic repo install found"
_check; grep -q 'repo.jellyfin.org/\$jf_id/dists/\$c/Release' "$REPO_ROOT/release/install.sh" \
  && _pass "probes for a published suite before using a codename" \
  || _fail "probes for a published suite" "a new distro codename would silently skip Jellyfin"

# ---------------------------------------------------------------- run-user preservation
describe "REGRESSION: an existing deployment's User= is preserved"
# Rewriting User= to the packaged service account left the orchestrator unable to read its
# own 0640 config or write its database, and the installer then aborted before printing any
# guidance.
_check; grep -q 'RUN_USER' "$REPO_ROOT/release/install.sh" \
  && _pass "installer preserves an existing unit User=" \
  || _fail "installer preserves an existing unit User=" "no RUN_USER handling found"

# ---------------------------------------------------------------- --only / --repair
describe "REGRESSION: --only runs to completion for a single component"
# --only skips whole sections, and a path defined inside a skipped section then blew up under
# `set -u` while enabling services: "TS_BIN: unbound variable". The installer aborted before
# it could verify anything, so a targeted repair silently did less than it claimed.
for comp in flaresolverr torrserver jellyfin prowlarr; do
  sandbox_new "only-$comp"
  mock_standard
  run_installer --only "$comp"
  _check
  if grep -aqiE 'unbound variable|command not found|syntax error' "$OUTPUT"; then
    _fail "--only $comp runs clean" "$(grep -aiE 'unbound variable|command not found' "$OUTPUT" | head -1)"
  else
    _pass "--only $comp runs clean"
  fi
  assert_exit 0
done

describe "REGRESSION: --repair runs to completion"
sandbox_new repair-one
mock_standard
run_installer --repair flaresolverr
_check
grep -aqiE 'unbound variable|command not found|syntax error' "$OUTPUT" \
  && _fail "--repair runs clean" "$(grep -aiE 'unbound variable|command not found' "$OUTPUT" | head -1)" \
  || _pass "--repair runs clean"
assert_exit 0

# ---------------------------------------------------------------- web sources
describe "web sources: the extractor, its scratch dir and the in-namespace proxy"
sandbox_new websources
mock_standard
run_installer
assert_exit 0
assert_exec "$DEST/usr/local/bin/yt-dlp"
# NOT /tmp. The yt-dlp binary unpacks ~76MB of itself into TMPDIR on every run, and /tmp
# is a RAM-backed tmpfs on stock Ubuntu — pointing it there spends memory per extraction
# and fails unreadably once that tmpfs fills.
assert_dir "$DEST/var/lib/jellyfreedom/tmp"
assert_contains "$DEST/etc/jellyfreedom/config.yaml" 'temp_dir: /var/lib/jellyfreedom/tmp'
assert_not_contains "$DEST/etc/jellyfreedom/config.yaml" 'temp_dir: /tmp'

assert_file "$DEST/etc/systemd/system/jf-netnsproxy.service"
# The three lines that make it work at all: it must run INSIDE the namespace, it must get
# the namespace's resolvers, and it must be torn down with the namespace rather than left
# holding an orphaned handle to a deleted one.
assert_contains "$DEST/etc/systemd/system/jf-netnsproxy.service" 'NetworkNamespacePath=/var/run/netns/vpntorrent'
assert_contains "$DEST/etc/systemd/system/jf-netnsproxy.service" 'BindReadOnlyPaths=/etc/netns/vpntorrent/resolv.conf'
assert_contains "$DEST/etc/systemd/system/jf-netnsproxy.service" 'BindsTo=vpntorrent-netns.service'
assert_contains "$DEST/etc/systemd/system/jf-netnsproxy.service" 'orchestrator netns-proxy'
# AF_NETLINK, specifically. The proxy derives its listen address from the namespace's
# veth via net.Interfaces(), which is a NETLINK_ROUTE socket. 0.5.3 shipped without it
# and the service exited 1 on every start on every machine, while the installer still
# printed "websources ready" — so assert the whole directive. Matching the bare word
# AF_NETLINK would pass on this comment alone, since the comment is written into the
# unit file too: a check that cannot fail.
assert_contains "$DEST/etc/systemd/system/jf-netnsproxy.service" \
  'RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK'
assert_ran_re 'systemctl (enable|restart) .*jf-netnsproxy'
# Upgrading from the broken 0.5.3 means the unit has already hit its start limit, and
# systemd refuses further starts until the failure is cleared — the fixed unit would
# install and never run.
#
# Asserted against the shipped script rather than the command log on purpose: under
# JF_DESTDIR the installer takes the `enable --now` branch, so the restart path this
# guards is never executed here. That is also why the bug reached a release.
assert_contains "$DEST/opt/jellyfreedom/install.sh" \
  'systemctl reset-failed jf-netnsproxy.service'
assert_no_shell_errors

describe "web sources: a failed yt-dlp download is a warning, never fatal"
sandbox_new websources-nodl
mock_standard
# The installer's other downloads succeed; only yt-dlp's URL fails, with curl's
# HTTP-error exit code.
mock_script curl '
  case "$*" in *yt-dlp*) exit 22 ;; esac
  out=""; prev=""
  for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
  if [ -n "$out" ]; then mkdir -p "$(dirname "$out")"; printf "mock-artifact\n" > "$out"; fi
  exit 0'
run_installer
# Nothing else depends on the extractor, so the install must still finish and still
# start. A box without it simply has the Links section switched off with one sentence.
assert_exit 0
assert_no_file "$DEST/usr/local/bin/yt-dlp"
assert_ran_re 'systemctl (enable|restart) .*jellyfreedom'
assert_output 'web sources will be unavailable'

# ---------------------------------------------------------------- logging
describe "the installer writes a transcript the user can be pointed at"
sandbox_new logging
mock_standard
run_installer
assert_output 'log'

summary
