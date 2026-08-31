#!/usr/bin/env bash
# install.sh — JellyFreedom installer (Debian/Ubuntu).
#
# Installs the parts we own (Go orchestrator + web assets + VPN-netns plumbing + the
# privileged helper + systemd units + scoped sudoers + AppArmor override) and provisions the
# supporting stack (TorrServer, FlareSolverr, Jellyfin, Prowlarr).
#
#   sudo ./install.sh                 install / upgrade / repair
#   sudo ./install.sh --repair X      force-reinstall one component
#   sudo ./install.sh --only X        install only one component
#   sudo ./install.sh --skip-preflight
#
# PRINCIPLES (learned the hard way — see the comments at each site):
#   * Never destroy something we cannot rebuild. Rename, never truncate.
#   * Never report success for a component we have not PROVED works.
#   * Idempotent: re-running is the supported repair path and must never break a
#     working install or touch user config, data, or VPN configs.
#   * Every failure names a cause and a next command.
set -uo pipefail

# ---- testability -----------------------------------------------------------------------
# JF_DESTDIR prefixes every path we write, so the test harness can run this whole script
# against a fake root as an unprivileged user. Empty in a real install.
D="${JF_DESTDIR:-}"
# NOTE: this installer never prompts — there is no `read` anywhere in it, and the one
# third-party script that would block on a tty (Jellyfin's) is always invoked with
# SKIP_CONFIRM=true. JF_NONINTERACTIVE is therefore accepted from callers for compatibility
# but needs no variable: non-interactive is the only mode.

# ---- fixed FHS layout ------------------------------------------------------------------
APP_DIR="$D/opt/jellyfreedom"
VPN_DIR="$D/opt/vpntorrent"
CONF_DIR="$D/etc/jellyfreedom"
DATA_DIR="$D/var/lib/jellyfreedom"
UNIT_DIR="$D/etc/systemd/system"
VPNCONF_DIR="$DATA_DIR/vpnconfigs"
FS_DIR="$D/opt/flaresolverr"
FS_HOME="$D/var/lib/flaresolverr"
LIB_ROOT="$D/srv/jellyfreedom"
SVC_USER=jellyfreedom
TS_USER=torrserver
FS_USER=flaresolverr
PW_USER=prowlarr
SRC="$(cd "$(dirname "$0")" && pwd)"

# `jellyfreedom repair` re-runs THIS script out of $APP_DIR, so SRC and APP_DIR are the
# same directory and there is nothing to copy: every install would be a file onto itself,
# and the `rm -rf "$APP_DIR/web"` that precedes the web copy would delete the assets
# before the cp meant to replace them could read them. That is not hypothetical — it is
# what `jellyfreedom repair` did, reporting "cannot stat .../web" for a directory it had
# removed a moment earlier, and a bundle "missing" a helper that was installed and fine.
IN_PLACE=0
if [ -d "$SRC" ] && [ -d "$APP_DIR" ] && [ "$SRC" -ef "$APP_DIR" ]; then IN_PLACE=1; fi

# Pinned versions. TorrServer is resolved at runtime because upstream has retagged before:
# the previously pinned MatriX.141.2 now 404s, which silently left installs with no
# streaming engine at all.
FS_VERSION="v3.5.0"
TS_VERSION_FALLBACK="MatriX.143"

LOGFILE="$D/var/log/jellyfreedom-install.log"

# Paths referenced from sections that --only may skip must be defined unconditionally, or
# `set -u` aborts the run partway. `--only flaresolverr` skipped the TorrServer section and
# then died on an unbound TS_BIN while enabling services — before verification could run.
TS_BIN="$D/usr/local/bin/torrserver"
YTDLP_BIN="$D/usr/local/bin/yt-dlp"
# The extractor's scratch space. NOT /tmp: the official yt-dlp binary is a self-extracting
# bundle that unpacks ~76MB into TMPDIR on every single run, and /tmp is a RAM-backed
# tmpfs on a stock Ubuntu — so leaving it there spends memory per extraction and fails
# with an unreadable PyInstaller error the moment that tmpfs is full.
YTDLP_TMP="$DATA_DIR/tmp"


# Ownership requires root. Under JF_DESTDIR the installer runs unprivileged against a fake
# root, so ownership flags are dropped there — the tests assert placement and mode, which is
# what they can meaningfully check. Real installs (D empty) take the normal path.
xinstall(){
  if [ -z "$D" ]; then command install "$@"; return $?; fi
  local -a args=()
  while [ $# -gt 0 ]; do
    case "$1" in
      -o|-g) shift 2 ;;
      *) args+=("$1"); shift ;;
    esac
  done
  command install "${args[@]}"
}
xchown(){ [ -n "$D" ] && return 0; chown "$@"; }

say(){ printf '\033[1;36m==>\033[0m %s\n' "$*"; }
ok(){  printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }
warn(){ printf '\033[1;33m  [!]\033[0m %s\n' "$*"; }
err(){ printf '\033[1;31m  ✗\033[0m %s\n' "$*" >&2; }
hint(){ printf '      → %s\n' "$*"; }

# Component outcomes, printed as one table at the end. A component is only "ok" once it has
# been PROVED to work, not merely downloaded.
declare -A STATUS
mark(){ STATUS["$1"]="$2"; }

ONLY=""; REPAIR=""; SKIP_PREFLIGHT=0
while [ $# -gt 0 ]; do
  case "$1" in
    --repair) REPAIR="${2:-all}"; shift 2 ;;
    --only)   ONLY="${2:-}"; shift 2 ;;
    --skip-preflight) SKIP_PREFLIGHT=1; shift ;;
    --help|-h) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) err "unknown option: $1"; exit 2 ;;
  esac
done
want(){ [ -z "$ONLY" ] || [ "$ONLY" = "$1" ]; }
repairing(){ [ "$REPAIR" = "all" ] || [ "$REPAIR" = "$1" ]; }

if [ -z "$D" ]; then
  [ "${JF_ASSUME_ROOT:-0}" = "1" ] || [ "$(id -u)" -eq 0 ] || { err "run as root (sudo ./install.sh)"; exit 1; }
  mkdir -p "$(dirname "$LOGFILE")" 2>/dev/null || true
  # Everything from here is teed to a log. A third-party installer's output must NEVER be
  # discarded again: silently swallowing it is what turned a blocking prompt into an
  # unexplained hang.
  exec > >(tee -a "$LOGFILE") 2>&1
fi
command -v apt-get >/dev/null || { err "this installer targets Debian/Ubuntu (apt)"; exit 1; }

export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=a

# The closing summary must print even when a step fails, so the user is never left staring
# at a half-finished install with no guidance.
FINISHED=0
summary(){
  local rc=$?
  printf '\n\033[1;36m── Components ─────────────────────────────\033[0m\n'
  local k bad=0
  for k in orchestrator torrserver websources flaresolverr jellyfin prowlarr vpn; do
    case "${STATUS[$k]:-skipped}" in
      ok)       printf '  \033[1;32m✓\033[0m %-14s ready\n' "$k" ;;
      degraded) printf '  \033[1;33m!\033[0m %-14s installed but NOT working\n' "$k"; bad=1 ;;
      "running as root (reduced isolation)")
                printf '  \033[1;33m!\033[0m %-14s working, but running as root\n' "$k"; bad=1 ;;
      skipped)  printf '  \033[2m·\033[0m %-14s skipped\n' "$k" ;;
      *)        printf '  \033[1;31m✗\033[0m %-14s %s\n' "$k" "${STATUS[$k]}"; bad=1 ;;
    esac
  done
  [ -z "$D" ] && printf '\n  full log: %s\n' "$LOGFILE"
  if [ "$FINISHED" != 1 ]; then
    printf '\n\033[1;31mThe installer did not finish.\033[0m Fix the error above and re-run — it is\n'
    printf 'idempotent and will skip what already succeeded:  sudo %s\n' "$0"
    exit "${rc:-1}"
  fi
  if [ "$bad" = 1 ]; then
    printf '\n\033[1;33mInstalled, with problems.\033[0m Diagnose with:  sudo jellyfreedom doctor\n'
  fi
  cat <<BANNER

Next steps:
  1. Open the dashboard:  http://<this-host>:1990/dashboard/  — create the admin account.
  2. Settings → Connections: set your TMDB key, and the Prowlarr / Jellyfin URLs + keys.
  3. IMPORTANT: open Prowlarr at http://<this-host>:9696 and add at least one indexer.
     With zero indexers every search returns nothing and no error explains why.
  4. VPN → upload a WireGuard .conf from any provider (pick a P2P-friendly server) → Activate.
  5. In Jellyfin, add these as libraries:  ${LIB_ROOT#"$D"}/movies  and  ${LIB_ROOT#"$D"}/tv
  6. Check everything:  sudo jellyfreedom doctor

Paths:  app=${APP_DIR#"$D"}  data=${DATA_DIR#"$D"}  config=${CONF_DIR#"$D"}
Logs:   journalctl -u jellyfreedom -f
BANNER
}
trap summary EXIT

cat <<'BANNER'

  JellyFreedom installer
  ----------------------
  Installs the orchestrator + VPN plumbing, and provisions TorrServer, FlareSolverr,
  Jellyfin and Prowlarr.

  * Pulls several hundred MB. Allow 5-15 minutes on a normal connection.
  * NON-DESTRUCTIVE: existing components and all your config, data and VPN configs are
    detected and preserved. Re-running this installer is the supported repair path.

BANNER

# ==========================================================================================
# preflight — decide everything BEFORE writing anything
# ==========================================================================================
ARCH_GO=""; ARCH_OK=1
case "$(uname -m)" in
  x86_64|amd64)  ARCH_GO=amd64; TS_ARCH=amd64; PW_ARCH=x64;   YTDLP_ASSET=yt-dlp_linux ;;
  aarch64|arm64) ARCH_GO=arm64; TS_ARCH=arm64; PW_ARCH=arm64; YTDLP_ASSET=yt-dlp_linux_aarch64 ;;
  armv7l|armhf)  ARCH_GO=arm;   TS_ARCH=arm7;  PW_ARCH=arm;   YTDLP_ASSET=yt-dlp_linux_armv7l ;;
  *) ARCH_OK=0 ;;
esac

preflight(){
  say "Preflight"
  if [ "$ARCH_OK" != 1 ]; then
    err "unsupported architecture: $(uname -m)"
    hint "JellyFreedom publishes amd64 and arm64 builds; build from source for anything else."
    exit 1
  fi
  ok "architecture $(uname -m) ($ARCH_GO)"

  local id=""
  if [ -r /etc/os-release ]; then . /etc/os-release; id="${ID:-}"; fi
  case "$id" in
    ubuntu|debian) ok "distro ${PRETTY_NAME:-$id}" ;;
    *) warn "untested distro '${id:-unknown}' — this installer targets Debian and Ubuntu" ;;
  esac

  local free_mb
  free_mb=$(df -Pm /opt 2>/dev/null | awk 'NR==2{print $4}')
  if [ -n "$free_mb" ] && [ "$free_mb" -lt 4096 ]; then
    err "only ${free_mb}MB free on /opt — the stack needs ~4GB (FlareSolverr alone extracts to ~705MB)"
    hint "free some space and re-run"
    exit 1
  fi
  ok "disk space"

  # Some packages stage large archives in /tmp. On a small VPS /tmp is a tmpfs sized at half
  # of RAM, which is a common and confusing install failure.
  local tmp_mb; tmp_mb=$(df -Pm /tmp 2>/dev/null | awk 'NR==2{print $4}')
  if [ -n "$tmp_mb" ] && [ "$tmp_mb" -lt 2048 ]; then
    warn "/tmp has only ${tmp_mb}MB free — some package installs stage archives there"
    hint "if a package install fails, free space or grow /tmp: mount -o remount,size=3G /tmp"
  fi

  local mem_mb; mem_mb=$(awk '/MemTotal/{printf "%d", $2/1024}' /proc/meminfo 2>/dev/null)
  if [ -n "$mem_mb" ] && [ "$mem_mb" -lt 2048 ]; then
    warn "only ${mem_mb}MB RAM — the default TorrServer cache is 2GB; lower cache.size_mb in the config"
  else
    ok "memory ${mem_mb:-?}MB"
  fi

  # Port conflicts are silent and maddening. 8192 is FlareSolverr's optional Prometheus port.
  if command -v ss >/dev/null; then
    local p conflict=0
    for p in 1990 8090 8096 8191 8192 9696; do
      if ss -tlnH "sport = :$p" 2>/dev/null | grep -q .; then
        local proc; proc=$(ss -tlnpH "sport = :$p" 2>/dev/null | sed -n 's/.*users:((\"\([^"]*\)\".*/\1/p' | head -1)
        case "$proc" in
          orchestrator|torrserver|jellyfin|flaresolverr|Prowlarr|"") ok "port $p (${proc:-in use by us})" ;;
          *) warn "port $p is already used by '$proc'"; conflict=1 ;;
        esac
      fi
    done
    [ "$conflict" = 1 ] && hint "stop the conflicting service or change our port before continuing"
  fi

  # Ubuntu cloud images run unattended-upgrades on first boot and hold the dpkg lock for
  # several minutes. Without this wait the installer dies at step one having done nothing.
  if [ -z "$D" ] && command -v fuser >/dev/null; then
    local waited=0
    while fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1; do
      [ "$waited" = 0 ] && say "waiting for another package manager to finish (unattended-upgrades)"
      sleep 5; waited=$((waited+5))
      if [ "$waited" -ge 300 ]; then
        err "the dpkg lock is still held after 5 minutes"
        hint "wait for unattended-upgrades to finish, then re-run"
        exit 1
      fi
    done
    [ "$waited" -gt 0 ] && ok "package manager free after ${waited}s"
  fi
}
[ "$SKIP_PREFLIGHT" = 1 ] || preflight

# ==========================================================================================
# OS dependencies
# ==========================================================================================
say "OS dependencies"
apt_install(){
  apt-get install -y -qq "$@" </dev/null
}
if [ -z "$D" ]; then
  apt-get update -qq || warn "apt-get update failed — continuing with the current package lists"
  # xvfb is required: FlareSolverr starts Xvfb itself via xvfbwrapper and does not bundle it.
  if ! apt_install wireguard-tools natpmpc iproute2 iptables jq curl ca-certificates tar xvfb python3; then
    warn "some packages failed to install; retrying individually"
    for p in wireguard-tools natpmpc iproute2 iptables jq curl ca-certificates tar xvfb python3; do
      apt_install "$p" || warn "could not install $p"
    done
  fi
else
  apt-get install -y -qq wireguard-tools natpmpc iproute2 iptables jq curl ca-certificates tar xvfb python3
fi
ok "dependencies"

# ==========================================================================================
# users and directories
# ==========================================================================================
say "Service users"
ensure_user(){
  if id -u "$1" >/dev/null 2>&1; then ok "$1 exists"
  else useradd --system --no-create-home --shell /usr/sbin/nologin "$1" && ok "created $1"; fi
}
ensure_user "$SVC_USER"
ensure_user "$TS_USER"
ensure_user "$FS_USER"

# The dashboard shows service logs by shelling out to journalctl. A plain system account can
# only read ITS OWN journal entries, so without this the Logs section is silently empty —
# which is exactly what happened when this deployment moved off a human account that
# happened to be in 'adm'. systemd-journal is read-only access to the journal; 'adm' would
# also grant it but is a much broader group.
if [ -z "$D" ] && getent group systemd-journal >/dev/null 2>&1; then
  usermod -aG systemd-journal "$SVC_USER" 2>/dev/null && ok "$SVC_USER can read the journal (dashboard logs)"
fi

# FlareSolverr drives a real Chrome. Running it as root used to give it incidental access to
# the GPU render nodes; a dedicated service account has none, and Chrome's GPU process then
# dies taking the renderer with it — surfacing as the notoriously unhelpful
# "invalid session id: session deleted as the browser has closed the connection".
# Grant only the device groups that exist on this host.
if [ -z "$D" ]; then
  for grp in video render; do
    if getent group "$grp" >/dev/null 2>&1; then
      usermod -aG "$grp" "$FS_USER" 2>/dev/null && ok "$FS_USER added to '$grp' (GPU device access)"
    fi
  done
fi

say "Directories"
# Code is root-owned; only DATA is service-owned. A service account that can rewrite its own
# binary and also restart its own unit has self-modifying persistence — and the orchestrator
# never needs to write here, so there is no cost to closing it.
xinstall -d -o root -g root -m 755 "$APP_DIR" "$APP_DIR/bin"
xinstall -d -o "$SVC_USER" -g "$SVC_USER" "$DATA_DIR"
xinstall -d -o "$SVC_USER" -g "$SVC_USER" -m 700 "$VPNCONF_DIR"
xinstall -d -o "$SVC_USER" -g "$SVC_USER" -m 700 "$YTDLP_TMP"
xinstall -d "$VPN_DIR" "$CONF_DIR"
xinstall -d -o "$TS_USER" -g "$TS_USER" "$D/var/lib/torrserver"
xinstall -d -o "$FS_USER" -g "$FS_USER" "$FS_HOME"
# Create the library folders NOW. The closing banner tells the user to add these to Jellyfin,
# and Jellyfin cannot browse to a path that does not exist yet.
xinstall -d -o "$SVC_USER" -g "$SVC_USER" "$LIB_ROOT" "$LIB_ROOT/movies" "$LIB_ROOT/tv"
ok "ready"

# ==========================================================================================
# our own components
# ==========================================================================================
say "Orchestrator, web assets and tooling"
# Code is root-owned; only DATA is service-owned. The service account also holds
# `systemctl restart jellyfreedom`, so a binary it could rewrite plus a self-restart is
# self-modifying persistence. Nothing in the orchestrator ever writes to APP_DIR, so there
# is no cost to closing that door.
if [ "$IN_PLACE" = 1 ]; then
  # Nothing to copy, and nothing missing: this IS the installed copy. Everything after
  # this stage — units, sudoers, AppArmor, service starts, verification, per-component
  # repair — is what a repair is actually for, and still runs.
  ok "running from the installed copy — application files left as they are"
  if [ -x "$VPN_DIR/jf-netns-helper" ]; then
    ok "privileged helper present"
  else
    err "the privileged helper is missing from ${VPN_DIR#"$D"}"
    hint "reinstall it with: sudo jellyfreedom --update"
  fi
  hint "to replace the binary or the web assets themselves: sudo jellyfreedom --update"
else
  xinstall -o root -g root -m 755 "$SRC/bin/orchestrator" "$APP_DIR/bin/orchestrator"
  rm -rf "$APP_DIR/web"; cp -r "$SRC/web" "$APP_DIR/web"; xchown -R root:root "$APP_DIR/web"
  if [ -f "$SRC/VERSION" ]; then xinstall -m 644 "$SRC/VERSION" "$APP_DIR/VERSION"; fi
  xinstall -m 755 "$SRC/vpntorrent/setup-netns.sh" "$VPN_DIR/setup-netns.sh"
  xinstall -m 755 "$SRC/vpntorrent/watchdog.sh"    "$VPN_DIR/watchdog.sh"
  xinstall -m 755 "$SRC/vpntorrent/portforward.sh" "$VPN_DIR/portforward.sh"

  # The privileged helper is the ONLY path to root. It must be root-owned in a root-owned
  # directory: if the service user can edit it, the sudo rules below become a root shell.
  if [ -f "$SRC/vpntorrent/jf-netns-helper" ]; then
    xinstall -o root -g root -m 755 "$SRC/vpntorrent/jf-netns-helper" "$VPN_DIR/jf-netns-helper"
    xchown root:root "$VPN_DIR" 2>/dev/null || true
    chmod 755 "$VPN_DIR" 2>/dev/null || true
    ok "privileged helper installed root-owned"
  else
    err "the bundle is missing vpntorrent/jf-netns-helper"
    hint "VPN status and activation cannot work without it — re-download the release bundle"
  fi
fi

# Persist the uninstaller and the control CLI. The one-liner deletes its temp dir on exit, so
# anything not copied here leaves the user with no way to remove or repair the install.
# Root-owned and argument-free: it is reachable through NOPASSWD sudo, so the service user
# must not be able to modify it (APP_DIR is root-owned for the same reason).
if [ -f "$SRC/jf-update" ]; then
  xinstall -o root -g root -m 755 "$SRC/jf-update" "$APP_DIR/jf-update"
fi
xinstall -m 755 "$SRC/uninstall.sh" "$APP_DIR/uninstall.sh"
xinstall -m 755 "$SRC/install.sh"   "$APP_DIR/install.sh"
if [ -f "$SRC/doctor.sh" ];      then xinstall -m 755 "$SRC/doctor.sh" "$APP_DIR/doctor.sh"; fi
if [ -f "$SRC/jellyfreedom" ];   then xinstall -m 755 "$SRC/jellyfreedom" "$D/usr/local/bin/jellyfreedom"; fi
if [ -f "$SRC/config.sample.yaml" ]; then xinstall -m 644 "$SRC/config.sample.yaml" "$APP_DIR/config.sample.yaml"; fi
ok "installed"
mark orchestrator ok

# Which channel this install follows, so `jellyfreedom --update` keeps to it instead of
# silently moving a nightly box onto stable (or the reverse) at the next update. An upgrade
# with nothing specified keeps whatever is already recorded; a fresh install defaults to
# stable, because a user who did not ask for nightlies should not receive them.
channel="${JELLYFREEDOM_CHANNEL:-}"
if [ -z "$channel" ] && [ -f "$APP_DIR/CHANNEL" ]; then
  channel="$(tr -d '[:space:]' < "$APP_DIR/CHANNEL" 2>/dev/null || true)"
fi
case "${channel:-stable}" in
  nightly) channel=nightly ;;
  *)       channel=stable ;;
esac
printf '%s\n' "$channel" > "$APP_DIR/CHANNEL"
ok "channel: $channel"

say "Config"
if [ -f "$CONF_DIR/config.yaml" ]; then
  ok "existing config left untouched"
else
  xinstall -o "$SVC_USER" -g "$SVC_USER" -m 640 "$SRC/config.sample.yaml" "$CONF_DIR/config.yaml"
  # public_url is written into every .strm file Jellyfin reads. Left as the placeholder it
  # produces a library of pointers to a nonexistent host — which looks like "it says ready
  # but nothing plays". Fill it in from the primary LAN address.
  lan_ip=""
  if command -v ip >/dev/null; then
    lan_ip=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.*src \([0-9.]*\).*/\1/p' | head -1)
  fi
  [ -n "$lan_ip" ] || lan_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  if [ -n "$lan_ip" ]; then
    sed -i "s#CHANGE-ME-LAN-IP#$lan_ip#g" "$CONF_DIR/config.yaml"
    ok "wrote a starter config (public_url set to http://$lan_ip:1990)"
  else
    warn "could not detect this machine's LAN IP"
    hint "set server.public_url in $CONF_DIR/config.yaml to http://<this-host-ip>:1990 before requesting anything"
  fi
fi

# ==========================================================================================
# TorrServer — the streaming engine. Without it nothing plays, so a failure here is fatal.
# ==========================================================================================
if want torrserver; then
say "TorrServer"
if [ -x "$TS_BIN" ] && ! repairing torrserver; then
  ok "present at ${TS_BIN#"$D"} — left alone"
  mark torrserver ok
else
  # Resolve the current release rather than trusting a pin: the previously hardcoded
  # MatriX.141.2 tag was removed upstream, so every fresh install silently ended up with
  # no streaming engine while the installer still printed "Installed."
  ts_ver=""
  if command -v curl >/dev/null; then
    ts_ver=$(curl -4 -fsSL --max-time 20 https://api.github.com/repos/YouROK/TorrServer/releases/latest 2>/dev/null \
             | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  fi
  [ -n "$ts_ver" ] || ts_ver="$TS_VERSION_FALLBACK"
  say "  fetching TorrServer $ts_ver ($TS_ARCH)"
  if curl -4 -fsSL --retry 3 --max-time 300 \
       "https://github.com/YouROK/TorrServer/releases/download/$ts_ver/TorrServer-linux-$TS_ARCH" \
       -o "$TS_BIN.new"; then
    chmod +x "$TS_BIN.new"; mv -f "$TS_BIN.new" "$TS_BIN"
    ok "TorrServer $ts_ver"
    mark torrserver ok
  else
    rm -f "$TS_BIN.new"
    err "could not download TorrServer $ts_ver for $TS_ARCH"
    hint "check network access to github.com, then re-run. Nothing will stream until this succeeds."
    mark torrserver "download failed"
  fi
fi
fi

# ==========================================================================================
# yt-dlp — the extractor behind paste-a-link web sources.
#
# OPTIONAL, unlike TorrServer: nothing else depends on it, and a box without it simply has
# the Links section disabled with one sentence explaining why. So a failure here is a
# warning, never fatal.
#
# The official self-contained build is used rather than the distro package. apt's yt-dlp is
# routinely months stale, and a stale extractor is precisely the thing that breaks — sites
# change their players constantly, and "update yt-dlp" is the fix for most of it. This one
# also updates itself in place with `yt-dlp -U`.
# ==========================================================================================
if want websources; then
say "yt-dlp (web sources)"
if [ -x "$YTDLP_BIN" ] && ! repairing websources; then
  ok "present at ${YTDLP_BIN#"$D"} — left alone"
  mark websources ok
else
  say "  fetching yt-dlp ($YTDLP_ASSET)"
  if curl -4 -fsSL --retry 3 --max-time 300 \
       "https://github.com/yt-dlp/yt-dlp/releases/latest/download/$YTDLP_ASSET" \
       -o "$YTDLP_BIN.new"; then
    chmod +x "$YTDLP_BIN.new"; mv -f "$YTDLP_BIN.new" "$YTDLP_BIN"
    ok "yt-dlp installed"
    mark websources ok
  else
    rm -f "$YTDLP_BIN.new"
    warn "could not download yt-dlp for $YTDLP_ASSET — web sources will be unavailable"
    hint "everything else works without it. Install it later and re-run: sudo jellyfreedom repair websources"
    mark websources "download failed"
  fi
fi
fi



# ==========================================================================================
# FlareSolverr browser provisioning
#
# The browser is chosen by PROBE, not by guesswork: install a candidate, restart, and try a
# real page fetch. Only a successful fetch counts. This matters because the upstream startup
# self-test resolves the binary and reads its version but NAVIGATES NOWHERE, so a browser
# that dies on its first real request looks perfectly healthy.
# ==========================================================================================
CHROME_DIR="$FS_DIR/_internal/chrome"

fs_purge_driver_cache(){
  # The cached chromedriver is keyed to Chrome's MAJOR version, so it must be purged on any
  # browser change or the next launch fails with a version mismatch. Derived from the
  # service's HOME — never a hardcoded /root, which breaks the moment the unit gains User=.
  rm -rf "$FS_HOME/.local/share/undetected_chromedriver" "$FS_HOME/.cache/selenium" 2>/dev/null || true
}
fs_is_elf(){ [ -f "$1" ] && [ "$(head -c4 "$1" 2>/dev/null | tr -d '\0')" = "$(printf '\x7fELF' | tr -d '\0')" ]; }
fs_point_at(){
  # Reversible: rename the bundle aside, then symlink. NEVER truncate — the previous
  # installer overwrote a 462MB binary with a 61-byte wrapper and left no way back.
  # (chmod -x is not an alternative: FlareSolverr raises instead of falling back.)
  if [ -e "$CHROME_DIR/chrome" ] && [ ! -e "$CHROME_DIR/chrome.real" ]; then
    mv "$CHROME_DIR/chrome" "$CHROME_DIR/chrome.real"
  fi
  rm -f "$CHROME_DIR/chrome"
  ln -sfn "$1" "$CHROME_DIR/chrome"
  fs_purge_driver_cache
}
fs_use_bundled(){
  # Heal a bundle a previous installer destroyed, then use it untouched. Upstream already
  # passes --no-sandbox/--disable-dev-shm-usage/--no-zygote itself, so a wrapper adding them
  # was always redundant and only hid which binary really ran.
  if ! fs_is_elf "$CHROME_DIR/chrome" && fs_is_elf "$CHROME_DIR/chrome.real"; then
    rm -f "$CHROME_DIR/chrome"; mv "$CHROME_DIR/chrome.real" "$CHROME_DIR/chrome"
    fs_purge_driver_cache; echo "restored the bundled Chrome a previous install had replaced"; return 0
  fi
  fs_is_elf "$CHROME_DIR/chrome" && { echo "bundled Chrome"; return 0; }
  return 1
}
fs_use_system_elf(){
  # A real distro browser. Skips /usr/bin/chromium-browser on Ubuntu, which is a SHELL SHIM
  # to the snap, not a browser — installing it succeeds while providing nothing.
  local c
  for c in /opt/google/chrome/chrome /usr/lib/chromium/chromium \
           /usr/lib/chromium-browser/chromium-browser /usr/bin/chromium; do
    if fs_is_elf "$c"; then fs_point_at "$c"; echo "$c"; return 0; fi
  done
  return 1
}
fs_install_browser(){
  # NOTE: this function's stdout IS its return value (the caller captures it to name the
  # browser), so every noisy sub-command must be redirected. apt and dpkg print progress on
  # stdout even under -qq, which otherwise ends up interpolated into the progress message.
  local a; a="$(dpkg --print-architecture 2>/dev/null || echo amd64)"
  if [ -r /etc/os-release ]; then . /etc/os-release; fi
  if [ "${ID:-}" = debian ] && apt_install chromium chromium-driver >&2 2>/dev/null; then
    fs_use_system_elf && return 0
  fi
  case "$a" in
    amd64|arm64)
      local t; t="$(mktemp -d)"
      if curl -4 -fsSL --proto '=https' --max-time 600 \
           "https://dl.google.com/linux/direct/google-chrome-stable_current_${a}.deb" -o "$t/gc.deb" 2>&1 >&2 \
         && apt_install "$t/gc.deb" >&2; then
        rm -rf "$t"; fs_point_at /opt/google/chrome/chrome; echo "google-chrome-stable"; return 0
      fi
      rm -rf "$t" ;;
  esac
  return 1
}
fs_use_snap(){
  # Last resort. Snap confinement (private /tmp, root-refusal in some setups) makes this the
  # least portable option, so it is only reached when everything else has failed a probe.
  if command -v snap >/dev/null; then
    snap list chromium >/dev/null 2>&1 || snap install chromium >/dev/null 2>&1 || true
    if [ -x /snap/bin/chromium ]; then
      printf '#!/bin/sh\nexec /snap/bin/chromium "$@"\n' > "$CHROME_DIR/.snapwrap"
      chmod 755 "$CHROME_DIR/.snapwrap"
      fs_point_at "$CHROME_DIR/.snapwrap"; echo "snap chromium (least portable)"; return 0
    fi
  fi
  return 1
}
# fs_probe — the gate. 0 only when a REAL page fetch succeeds.
fs_probe(){
  local resp restarts0 restarts
  # A crash-looping service will never answer, so waiting the full window for each browser
  # would spend ~10 minutes cycling a chain that cannot succeed. Watch systemd's restart
  # counter and give up as soon as it is clearly looping.
  restarts0="$(systemctl show -p NRestarts --value flaresolverr.service 2>/dev/null || echo 0)"
  for _ in $(seq 1 18); do
    [ "$(curl -4 -s -o /dev/null -w '%{http_code}' --max-time 3 http://127.0.0.1:8191/health 2>/dev/null)" = "200" ] && break
    restarts="$(systemctl show -p NRestarts --value flaresolverr.service 2>/dev/null || echo 0)"
    if [ "$(( ${restarts:-0} - ${restarts0:-0} ))" -ge 3 ]; then
      return 1   # crash-looping: this browser is not going to work
    fi
    sleep 5
  done
  [ "$(curl -4 -s -o /dev/null -w '%{http_code}' --max-time 3 http://127.0.0.1:8191/health 2>/dev/null)" = "200" ] || return 1
  resp="$(curl -4 -s --max-time 90 -XPOST http://127.0.0.1:8191/v1 -H 'Content-Type: application/json' \
          -d '{"cmd":"request.get","url":"https://example.com","maxTimeout":60000}' 2>/dev/null)"
  printf '%s' "$resp" | grep -q '"status": *"ok"'
}

# ==========================================================================================
# FlareSolverr
#
# This component broke every previous install on Ubuntu, in a way that re-running could not
# repair. Two compounding mistakes:
#   1. `apt-get install chromium-browser` SUCCEEDS on Ubuntu while installing no browser —
#      it is a transitional package whose only executable is a shell shim that execs
#      /snap/bin/chromium. Because apt exits 0, the `|| snap install` fallback never ran.
#   2. The installer then TRUNCATED the bundled 462MB Chrome ELF with a 61-byte wrapper
#      pointing at that dead shim. There was no backup, so only a 233MB re-download could
#      recover — and re-running saw the (broken) files present and reported success.
#
# The rules now: never truncate anything (rename, so it is reversible), never install a
# system browser unconditionally, and never report success until a real HTTPS fetch through
# FlareSolverr has actually worked.
# ==========================================================================================
if want flaresolverr; then
say "FlareSolverr"
if [ "$ARCH_GO" != "amd64" ]; then
  # Upstream publishes exactly two assets, both x64. Say so plainly instead of downloading
  # an x86 binary onto an arm board and leaving a permanently broken service behind.
  warn "FlareSolverr ships x64 binaries only — skipping on $(uname -m)"
  hint "Cloudflare-protected indexers will not work. Run it from source with a system chromium if you need it."
  mark flaresolverr skipped
elif [ -z "$D" ] && ! command -v curl >/dev/null; then
  mark flaresolverr "curl unavailable"
else
  if [ -x "$FS_DIR/flaresolverr" ] && ! repairing flaresolverr; then
    ok "FlareSolverr present — left alone"
  else
    tmp=$(mktemp -d)
    say "  downloading FlareSolverr $FS_VERSION (~233MB)"
    if curl -4 -fsSL --retry 3 --max-time 900 \
         "https://github.com/FlareSolverr/FlareSolverr/releases/download/$FS_VERSION/flaresolverr_linux_x64.tar.gz" \
         -o "$tmp/fs.tgz"; then
      # Verify the archive before removing anything, so a corrupt download cannot leave a
      # half-extracted tree that later runs mistake for a working install.
      if tar tzf "$tmp/fs.tgz" >/dev/null 2>&1; then
        rm -rf "$FS_DIR"
        if tar xzf "$tmp/fs.tgz" -C "$D/opt"; then ok "installed FlareSolverr $FS_VERSION"
        else err "extracting FlareSolverr failed"; mark flaresolverr "extract failed"; fi
      else
        err "the FlareSolverr download is not a valid archive"
        mark flaresolverr "bad download"
      fi
    else
      err "could not download FlareSolverr"
      mark flaresolverr "download failed"
    fi
    rm -rf "$tmp"
  fi

  if [ -x "$FS_DIR/flaresolverr" ]; then
    xchown -R "$FS_USER":"$FS_USER" "$FS_DIR" 2>/dev/null || true

    # Always rewrite the unit — skipping it when present made a unit from a failed first
    # attempt permanent.
    cat > "$UNIT_DIR/flaresolverr.service" <<EOF
[Unit]
Description=FlareSolverr
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$FS_USER
# HOME must be explicit: the chromedriver cache path is a hardcoded ~/.local/share/... that
# is resolved at import time, and an unset HOME on a system account creates a literal "~"
# directory under WorkingDirectory instead.
Environment=HOME=${FS_HOME#"$D"}
# Was 0.0.0.0 — FlareSolverr fetches arbitrary URLs with no auth, so binding it to every
# interface published an open SSRF proxy on the LAN.
Environment=HOST=127.0.0.1
Environment=LOG_LEVEL=info
WorkingDirectory=${FS_HOME#"$D"}
# Prune profile directories left behind by a previous life before starting. Scoped by owner
# and age so it can only ever touch this service's own leftovers. Observed on a live box:
# eight orphaned Chrome processes reparented to init, holding 1.19GB of RAM, plus six stale
# profile dirs — the residue of earlier crashes.
ExecStartPre=-/usr/bin/find /tmp -maxdepth 1 -name 'tmp*' -user ${FS_USER} -mmin +60 -exec rm -rf {} +
ExecStart=${FS_DIR#"$D"}/flaresolverr
Restart=on-failure
RestartSec=3
# FlareSolverr spawns Chrome, which spawns renderers. Killing the whole control group is the
# default, but state it explicitly: without it a crash can leave browsers running as orphans
# that accumulate across restarts and quietly consume gigabytes.
KillMode=control-group
TimeoutStopSec=20
# DO NOT add LimitAS= — Chrome reserves a very large allocator region and aborts.
# DO NOT add PrivateTmp=yes — it breaks the system-chromium fallback.

[Install]
WantedBy=multi-user.target
EOF
    ok "unit written (runs as $FS_USER, bound to localhost)"
    mark flaresolverr degraded   # upgraded to ok only once the probe below passes
  fi
fi
fi

# ==========================================================================================
# Jellyfin
# ==========================================================================================
if want jellyfin; then
say "Jellyfin"
if systemctl cat jellyfin.service >/dev/null 2>&1 && ! repairing jellyfin; then
  ok "present — left alone (your library and config are untouched)"
  mark jellyfin ok
elif [ -n "$D" ]; then
  mark jellyfin skipped
else
  # We add the apt repository ourselves rather than piping upstream's convenience script.
  # That script is actively hostile to automation: it reads /dev/tty so piping into bash
  # cannot skip its prompt, it hard-requires 2GB free on BOTH /var/lib and /tmp (a small VPS
  # has a tmpfs /tmp sized at half of RAM and fails outright), it rejects distro codenames it
  # has not been taught, and it can exit 0 having failed to install anything. Doing it
  # directly is deterministic and has none of those failure modes.
  jf_ok=0
  jf_id="${ID:-ubuntu}"; jf_code="${VERSION_CODENAME:-}"
  case "$jf_id" in ubuntu|debian) ;; *) jf_id=ubuntu ;; esac

  # Use this release's codename if Jellyfin publishes for it; otherwise fall back to the
  # newest suite that does, rather than skipping Jellyfin entirely.
  jf_suite=""
  for c in "$jf_code" noble jammy focal bookworm bullseye; do
    [ -n "$c" ] || continue
    if curl -4 -fsS --max-time 15 -o /dev/null "https://repo.jellyfin.org/$jf_id/dists/$c/Release" 2>/dev/null; then
      jf_suite="$c"; break
    fi
  done

  if [ -z "$jf_suite" ]; then
    err "no Jellyfin apt suite is published for $jf_id"
    hint "install Jellyfin manually from https://jellyfin.org/docs/general/installation/linux"
    mark jellyfin "no apt suite"
  else
    [ "$jf_suite" = "$jf_code" ] || warn "Jellyfin has no repo for '$jf_code' — using '$jf_suite'"
    install -d -m 755 /etc/apt/keyrings
    if curl -4 -fsSL --max-time 30 https://repo.jellyfin.org/jellyfin_team.gpg.key \
         | gpg --dearmor --yes -o /etc/apt/keyrings/jellyfin.gpg 2>/dev/null; then
      cat > /etc/apt/sources.list.d/jellyfin.sources <<EOF
Types: deb
URIs: https://repo.jellyfin.org/$jf_id
Suites: $jf_suite
Components: main
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/jellyfin.gpg
EOF
      apt-get update -qq 2>/dev/null || true
      if apt_install jellyfin; then jf_ok=1; fi
    fi
    if [ "$jf_ok" = 1 ] && systemctl cat jellyfin.service >/dev/null 2>&1; then
      ok "installed Jellyfin (suite: $jf_suite)"
      mark jellyfin ok
    else
      err "the Jellyfin package did not install"
      hint "see $LOGFILE, then: apt-get install jellyfin"
      mark jellyfin "install failed"
    fi
  fi
fi
fi

# ==========================================================================================
# Prowlarr
# ==========================================================================================
if want prowlarr; then
say "Prowlarr"
if { systemctl cat prowlarr.service >/dev/null 2>&1 || [ -x "$D/opt/Prowlarr/Prowlarr" ]; } && ! repairing prowlarr; then
  ok "present — left alone"
  mark prowlarr ok
elif [ -n "$D" ]; then
  mark prowlarr skipped
else
  id -u "$PW_USER" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin "$PW_USER"
  xinstall -d -o "$PW_USER" -g "$PW_USER" "$D/var/lib/prowlarr"
  tmp=$(mktemp -d)
  if curl -4 -fsSL --retry 3 --max-time 600 \
       "https://prowlarr.servarr.com/v1/update/master/updatefile?os=linux&runtime=netcore&arch=$PW_ARCH" \
       -o "$tmp/prowlarr.tgz" && tar tzf "$tmp/prowlarr.tgz" >/dev/null 2>&1; then
    rm -rf "$D/opt/Prowlarr"          # upstream's own installer clears the dir first
    if tar xzf "$tmp/prowlarr.tgz" -C "$D/opt"; then
      xchown -R "$PW_USER":"$PW_USER" "$D/opt/Prowlarr"
      # Matches upstream's unit: Group, UMask and the shutdown settings were all missing.
      cat > "$UNIT_DIR/prowlarr.service" <<EOF
[Unit]
Description=Prowlarr
After=syslog.target network-online.target
Wants=network-online.target

[Service]
User=$PW_USER
Group=$PW_USER
UMask=0002
Type=simple
ExecStart=/opt/Prowlarr/Prowlarr -nobrowser -data=/var/lib/prowlarr
TimeoutStopSec=20
KillMode=process
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
      ok "installed Prowlarr"
      mark prowlarr ok
    else
      err "extracting Prowlarr failed"; mark prowlarr "extract failed"
    fi
  else
    err "could not download Prowlarr for $PW_ARCH"
    mark prowlarr "download failed"
  fi
  rm -rf "$tmp"
fi
fi

# ==========================================================================================
# systemd units for the parts we own
# ==========================================================================================
say "systemd units"

# Preserve the User= of an existing deployment. Rewriting it to $SVC_USER on an install that
# was set up to run as someone else leaves the service unable to read its own 0640 config or
# write its database — and it looked like a clean upgrade right up until it failed.
RUN_USER="$SVC_USER"
if [ -z "$D" ] && systemctl cat jellyfreedom.service >/dev/null 2>&1; then
  existing=$(systemctl show -p User --value jellyfreedom.service 2>/dev/null)
  if [ -n "$existing" ] && [ "$existing" != "$SVC_USER" ]; then
    RUN_USER="$existing"
    warn "this instance runs as '$RUN_USER' — keeping that, not switching to $SVC_USER"
  fi
fi
# Whoever runs it must actually own its data and config.
xchown -R "$RUN_USER" "$DATA_DIR" 2>/dev/null || true
xchown "$RUN_USER" "$CONF_DIR/config.yaml" 2>/dev/null || true
# Deliberately NOT chowned to the run user — see the note above.
xchown -R root:root "$APP_DIR/bin" "$APP_DIR/web" 2>/dev/null || true
xchown -R "$RUN_USER" "$LIB_ROOT" 2>/dev/null || true

cat > "$UNIT_DIR/jellyfreedom.service" <<EOF
[Unit]
Description=JellyFreedom Orchestrator
After=network-online.target vpntorrent-netns.service torrserver-netns.service
Wants=network-online.target

[Service]
Type=simple
User=$RUN_USER
WorkingDirectory=${APP_DIR#"$D"}
ExecStart=${APP_DIR#"$D"}/bin/orchestrator --config ${CONF_DIR#"$D"}/config.yaml --db ${DATA_DIR#"$D"}/jellyfreedom.db --assets ${APP_DIR#"$D"}/web
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

cat > "$UNIT_DIR/vpntorrent-netns.service" <<EOF
[Unit]
Description=vpntorrent network namespace setup
After=network-online.target
Wants=network-online.target torrserver-netns.service

[Service]
Type=oneshot
RemainAfterExit=yes
Environment=VPNTORRENT_CONFIG_DIR=${VPNCONF_DIR#"$D"}
ExecStart=${VPN_DIR#"$D"}/setup-netns.sh
ExecStop=/usr/sbin/ip netns del vpntorrent

[Install]
WantedBy=multi-user.target
EOF

cat > "$UNIT_DIR/torrserver-netns.service" <<EOF
[Unit]
Description=TorrServer (inside the vpntorrent netns)
After=vpntorrent-netns.service network-online.target
Requires=vpntorrent-netns.service
# BindsTo so TorrServer follows the namespace: without it, restarting the netns deletes and
# recreates the namespace while TorrServer keeps a handle on the old, now-orphaned one.
BindsTo=vpntorrent-netns.service

[Service]
Type=simple
User=$TS_USER
NetworkNamespacePath=/var/run/netns/vpntorrent
BindReadOnlyPaths=/etc/netns/vpntorrent/resolv.conf:/etc/resolv.conf
ExecStart=/usr/local/bin/torrserver --port 8090 --path /var/lib/torrserver
Restart=on-failure
RestartSec=3
StartLimitIntervalSec=300
StartLimitBurst=5

[Install]
WantedBy=multi-user.target
EOF

cat > "$UNIT_DIR/jf-netnsproxy.service" <<EOF
[Unit]
Description=JellyFreedom VPN proxy (inside the vpntorrent netns)
After=vpntorrent-netns.service network-online.target
Requires=vpntorrent-netns.service
# BindsTo for the same reason TorrServer uses it: restarting the netns deletes and
# recreates the namespace, and a process that only Requires= it would keep a handle on the
# old, orphaned one — looking healthy while having no VPN at all.
BindsTo=vpntorrent-netns.service
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
User=$RUN_USER
# systemd enters the namespace before dropping privileges, so this process is inside the
# tunnel and behind the kill switch without holding a capability of its own. That is the
# whole mechanism: the orchestrator runs in the HOST namespace (it serves the LAN) and
# cannot enter a netns without CAP_SYS_ADMIN, so it dials through this instead.
NetworkNamespacePath=/var/run/netns/vpntorrent
# NetworkNamespacePath= does not apply /etc/netns/<ns>/resolv.conf the way `ip netns exec`
# does, so bind it in — otherwise lookups hit the host's 127.0.0.53 stub, which nothing
# answers inside the namespace, and every extraction fails to resolve its site.
BindReadOnlyPaths=/etc/netns/vpntorrent/resolv.conf:/etc/resolv.conf
# No arguments: it derives its addressing from the veth inside the namespace. It prefers
# /run/vpntorrent/netns.env, but that directory is 0700 root (it holds the sanitised
# WireGuard config), so a service user cannot read it — deriving is the real path.
ExecStart=${APP_DIR#"$D"}/bin/orchestrator netns-proxy
Restart=on-failure
RestartSec=3

NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
# AF_NETLINK is required, not optional: the proxy derives its own listen address from
# the namespace's veth, and Go's net.Interfaces() does that over a NETLINK_ROUTE socket.
# Without it the call fails with "address family not supported by protocol", the service
# cannot find 10.42.0.2, and it exits 1 on every start until the restart limit is hit.
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
EOF

cat > "$UNIT_DIR/vpntorrent-portforward.service" <<EOF
[Unit]
Description=vpntorrent port-forward keeper (NAT-PMP; optional, provider-dependent)
After=torrserver-netns.service vpntorrent-netns.service
BindsTo=vpntorrent-netns.service

[Service]
Type=simple
ExecStart=${VPN_DIR#"$D"}/portforward.sh
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

cat > "$UNIT_DIR/vpntorrent-watchdog.service" <<EOF
[Unit]
Description=vpntorrent VPN + TorrServer watchdog
After=torrserver-netns.service
Wants=vpntorrent-netns.service

[Service]
Type=oneshot
ExecStart=${VPN_DIR#"$D"}/watchdog.sh
TimeoutStartSec=120
EOF

cat > "$UNIT_DIR/vpntorrent-watchdog.timer" <<'EOF'
[Unit]
Description=Run the vpntorrent watchdog every 60s

[Timer]
OnBootSec=90
OnUnitActiveSec=60
AccuracySec=10

[Install]
WantedBy=timers.target
EOF
cat > "$UNIT_DIR/jf-tmpreaper.service" <<'EOF'
[Unit]
Description=Reap browser scratch directories left in /tmp

[Service]
Type=oneshot
# WHY THIS EXISTS
#
# FlareSolverr drives a browser per request, and each launch leaves a scratch directory
# behind. On a snap Chromium those land in /tmp/snap-private-tmp/snap.chromium/tmp, which
# is root-owned 0700 — so flaresolverr.service's own ExecStartPre cleanup cannot see them,
# let alone delete them. Observed on a live box: 3,894 directories, 7.7GB, 741k inodes,
# filling a 7.8GB tmpfs three times in four days. A full /tmp then breaks package installs,
# the updater (which stages downloads there) and the installer's own preflight.
#
# Deliberately narrow: entries must be inside a snap's tmp, untouched for an hour, AND
# carry a browser's name. A stale directory costs a little memory; a wrong glob here would
# delete somebody's work.
ExecStart=-/usr/bin/find /tmp/snap-private-tmp -mindepth 3 -maxdepth 3 -path '*/tmp/*' -mmin +60 \
  \( -name 'org.chromium.*' -o -name '.org.chromium.*' -o -name 'com.google.Chrome*' \
     -o -name '.com.google.Chrome*' -o -name 'scoped_dir*' -o -name 'tmp*' \) \
  -exec rm -rf {} +
# The same residue from an unconfined Chrome or Chromium, which writes straight into /tmp.
ExecStart=-/usr/bin/find /tmp -mindepth 1 -maxdepth 1 -mmin +60 \
  \( -name '.org.chromium.*' -o -name '.com.google.Chrome*' -o -name 'scoped_dir*' \) \
  -exec rm -rf {} +
EOF

cat > "$UNIT_DIR/jf-tmpreaper.timer" <<'EOF'
[Unit]
Description=Reap browser scratch in /tmp hourly

[Timer]
OnBootSec=10min
OnUnitActiveSec=1h
AccuracySec=5min
Persistent=true

[Install]
WantedBy=timers.target
EOF

ok "units written"

# ==========================================================================================
# sudoers — the privilege boundary
# ==========================================================================================
say "Scoped sudoers"
# Every rule names an exact command with fixed arguments. NO WILDCARDS.
#
# The previous policy granted `ip netns exec vpntorrent /usr/bin/curl *`, which is root
# equivalence: curl can write any file as root (-o /etc/sudoers.d/x) and read any file
# (file:///etc/shadow). It also used /bin/systemctl, which sudo never matches because it
# compares the resolved path (/usr/bin/systemctl) and does not follow the merged-usr
# symlink — so on Ubuntu every service restart from the dashboard was silently denied.
cat > "$D/etc/sudoers.d/jellyfreedom" <<EOF
$SVC_USER ALL=(root) NOPASSWD: /usr/bin/systemctl restart jellyfin.service
$SVC_USER ALL=(root) NOPASSWD: /usr/bin/systemctl restart torrserver-netns.service
$SVC_USER ALL=(root) NOPASSWD: /usr/bin/systemctl restart vpntorrent-netns.service
$SVC_USER ALL=(root) NOPASSWD: /usr/bin/systemctl restart prowlarr.service
$SVC_USER ALL=(root) NOPASSWD: /usr/bin/systemctl restart flaresolverr.service
$SVC_USER ALL=(root) NOPASSWD: ${VPN_DIR#"$D"}/jf-netns-helper status
$SVC_USER ALL=(root) NOPASSWD: ${VPN_DIR#"$D"}/jf-netns-helper exit-ip
$SVC_USER ALL=(root) NOPASSWD: ${VPN_DIR#"$D"}/jf-netns-helper leakcheck
$SVC_USER ALL=(root) NOPASSWD: ${VPN_DIR#"$D"}/jf-netns-helper routes
$SVC_USER ALL=(root) NOPASSWD: ${VPN_DIR#"$D"}/jf-netns-helper vpn-up
$SVC_USER ALL=(root) NOPASSWD: ${VPN_DIR#"$D"}/jf-netns-helper vpn-down
$SVC_USER ALL=(root) NOPASSWD: ${APP_DIR#"$D"}/jf-update
EOF
chmod 440 "$D/etc/sudoers.d/jellyfreedom"
if command -v visudo >/dev/null; then
  if visudo -cf "$D/etc/sudoers.d/jellyfreedom" >/dev/null 2>&1; then
    ok "sudoers valid"
  else
    err "the generated sudoers file is invalid — removing it so sudo keeps working"
    rm -f "$D/etc/sudoers.d/jellyfreedom"
    exit 1
  fi
else
  ok "sudoers written"
fi

say "AppArmor override"
if [ -d "$D/etc/apparmor.d" ]; then
  xinstall -d "$D/etc/apparmor.d/local"
  # Two paths matter, and missing the second one breaks the VPN outright:
  #  * the uploaded configs directory, and
  #  * /run/vpntorrent, where the privileged helper writes the SANITISED copy that wg-quick
  #    actually reads. wg-quick runs as root but is AppArmor-confined, so root ownership is
  #    not enough — without this rule it fails with a bare "Permission denied" and the
  #    tunnel silently never comes up.
  cat > "$D/etc/apparmor.d/local/wg-quick" <<EOF
# JellyFreedom: allow wg-quick to read uploaded WireGuard configs
${VPNCONF_DIR#"$D"}/ r,
${VPNCONF_DIR#"$D"}/** r,
# and the sanitised copy the privileged helper hands it
/run/vpntorrent/ r,
/run/vpntorrent/** r,
EOF
  if [ -z "$D" ] && command -v apparmor_parser >/dev/null; then
    apparmor_parser -r /etc/apparmor.d/wg-quick 2>/dev/null && ok "applied" || warn "AppArmor not reloaded (fine if it is not active)"
  else
    ok "written"
  fi
else
  ok "no AppArmor on this host — skipped"
fi

# ==========================================================================================
# start everything
# ==========================================================================================
say "Enabling services"
if [ -n "$D" ]; then
  systemctl daemon-reload
  systemctl enable --now vpntorrent-netns.service
  if [ -x "$TS_BIN" ]; then systemctl enable --now torrserver-netns.service; else warn "skipping torrserver-netns — no TorrServer binary"; fi
  if [ -x "$FS_DIR/flaresolverr" ]; then systemctl enable --now flaresolverr.service; fi
  systemctl enable --now vpntorrent-portforward.service vpntorrent-watchdog.timer jf-tmpreaper.timer
  if [ -x "$YTDLP_BIN" ]; then systemctl enable --now jf-netnsproxy.service; fi
  systemctl enable --now jellyfreedom.service
else
  systemctl daemon-reload
  start_unit(){
    if systemctl enable --now "$1" >/dev/null 2>&1; then ok "$1"
    else err "$1 failed to start"; hint "journalctl -u ${1%.service} -n 50 --no-pager"; return 1; fi
  }
  start_unit vpntorrent-netns.service || mark vpn "netns failed to start"
  if [ -x "$TS_BIN" ]; then start_unit torrserver-netns.service || mark torrserver "failed to start"
  else warn "skipping torrserver-netns — no TorrServer binary"; fi
  if [ -x "$FS_DIR/flaresolverr" ]; then start_unit flaresolverr.service || mark flaresolverr "failed to start"; fi
  start_unit vpntorrent-portforward.service || true
  # The in-namespace proxy. Started BEFORE the orchestrator so the first extraction has
  # somewhere to dial; only started at all when there is an extractor to use it, since
  # without yt-dlp it would be a listening socket with no caller.
  #
  # A restart, not enable --now: on an upgrade the unit is already active and running the
  # OLD binary, and `enable --now` would report success while leaving it there — the same
  # trap the orchestrator's own line below documents.
  if [ -x "$YTDLP_BIN" ]; then
    systemctl enable jf-netnsproxy.service >/dev/null 2>&1 || true
    # Clear the start-limit first. A machine upgrading FROM the broken 0.5.3 has a unit
    # that failed five times in a row, and systemd then refuses every further start with
    # "Start request repeated too quickly" — including the one that would run the fixed
    # unit. Without this the fix installs correctly and still never runs, and the user
    # gets a successful update followed by a dead proxy.
    systemctl reset-failed jf-netnsproxy.service >/dev/null 2>&1 || true
    systemctl restart jf-netnsproxy.service >/dev/null 2>&1 || true
    # `systemctl restart` returns once the process has forked, not once it has stayed up.
    # This unit fails ~200ms in when it cannot read the veth, so restart reported success
    # while the service was already dead and "websources ready" was printed over a service
    # that had failed. Ask again after it has had a moment to fall over.
    sleep 1
    if systemctl is-active --quiet jf-netnsproxy.service; then ok "jf-netnsproxy.service"
    else warn "jf-netnsproxy did not stay up — web sources will be unavailable"
         hint "journalctl -u jf-netnsproxy -n 50 --no-pager"
         mark websources "proxy failed to start"; fi
  fi
  systemctl enable --now vpntorrent-watchdog.timer >/dev/null 2>&1 || true
  # Fire it once now rather than waiting an hour: an install that warned about a full
  # /tmp during preflight should not leave it full.
  systemctl enable --now jf-tmpreaper.timer >/dev/null 2>&1 || true
  systemctl start jf-tmpreaper.service >/dev/null 2>&1 || true
  for u in jellyfin.service prowlarr.service; do
    if systemctl cat "$u" >/dev/null 2>&1; then systemctl enable --now "$u" >/dev/null 2>&1 || warn "$u did not start"; fi
  done
  # Restart rather than enable --now: on an upgrade the unit is already active, and
  # `enable --now` would leave the OLD binary running while reporting success.
  systemctl enable jellyfreedom.service >/dev/null 2>&1 || true
  if systemctl restart jellyfreedom.service; then ok "jellyfreedom.service"
  else err "jellyfreedom failed to start"; hint "journalctl -u jellyfreedom -n 50 --no-pager"; mark orchestrator "failed to start"; fi
fi

# ==========================================================================================
# verification — prove it works; never report success on the strength of a file existing
# ==========================================================================================
if [ -z "$D" ]; then
say "Verifying"

probe(){ curl -4 -s -o /dev/null -w '%{http_code}' --max-time "${2:-10}" "$1" 2>/dev/null; }

# Orchestrator
for _ in 1 2 3 4 5 6 7 8 9 10; do
  [ "$(probe http://127.0.0.1:1990/healthz 3)" = "200" ] && break
  sleep 1
done
if [ "$(probe http://127.0.0.1:1990/healthz 3)" = "200" ]; then ok "orchestrator answering on :1990"
else err "orchestrator is not answering on :1990"; hint "journalctl -u jellyfreedom -n 50 --no-pager"; mark orchestrator "not responding"; fi

# TorrServer runs inside the VPN namespace, so a failure here is often a VPN problem.
if [ -x "$TS_BIN" ]; then
  c=$(probe http://10.42.0.2:8090/echo 5)
  if [ -n "$c" ] && [ "$c" != "000" ]; then ok "TorrServer answering"
  else warn "TorrServer is not answering yet"; hint "it starts inside the VPN namespace — check: journalctl -u torrserver-netns -n 30"; mark torrserver degraded; fi
fi

# FlareSolverr: provisioning is driven BY the probe, walking a fallback chain until a real
# page fetch succeeds. Three rungs of proof, because each proves strictly more than the last:
#   /health  -> the service is up
#   /        -> the browser LAUNCHES. Upstream's own self-test stops here — it reads the
#               binary's version and user-agent but navigates NOWHERE, so a browser that dies
#               on its first real request passes this rung looking perfectly healthy.
#   POST /v1 -> a real HTTPS page was fetched. ONLY this proves the component works.
if [ -x "$FS_DIR/flaresolverr" ]; then
  say "Verifying FlareSolverr (first run downloads a chromedriver; allow ~90s per attempt)"
  fs_done=0
  fs_tried=""   # browsers already proven not to work, so a later rung never repeats one
  for rung in 0 1 2 3; do
    case "$rung" in
      0) chosen="$(fs_use_bundled 2>/dev/null)" || chosen="" ;;
      1) chosen="$(fs_use_system_elf 2>/dev/null)" || chosen="" ;;
      2) say "  bundled and system browsers failed a real fetch — installing a browser"
         chosen="$(fs_install_browser 2>/dev/null)" || chosen="" ;;
      3) chosen="$(fs_use_snap 2>/dev/null)" || chosen="" ;;
    esac
    [ -n "$chosen" ] || continue
    # Guard against a helper leaking sub-command output into its return value.
    chosen="$(printf '%s' "$chosen" | tail -1 | cut -c1-80)"
    # Resolve to the real binary: rung 1 finds an already-installed Chrome and rung 2 would
    # otherwise re-download and re-test the very same one.
    resolved="$(readlink -f "$CHROME_DIR/chrome" 2>/dev/null || echo "$chosen")"
    case " $fs_tried " in
      *" $resolved "*) say "  skipping $chosen — already tried this binary"; continue ;;
    esac
    fs_tried="$fs_tried $resolved"
    say "  trying: $chosen"
    systemctl restart flaresolverr.service >/dev/null 2>&1 || true
    if fs_probe; then
      ok "FlareSolverr works (browser: $chosen)"
      mark flaresolverr ok
      fs_done=1
      break
    fi
    warn "  '$chosen' did not complete a real page fetch"
  done
  if [ "$fs_done" != 1 ] && [ "$(systemctl show -p User --value flaresolverr.service 2>/dev/null)" = "$FS_USER" ]; then
    # Last resort. FlareSolverr drives a real browser, and on some hosts Chrome will not
    # start a session under a dedicated service account while working fine as root. Running
    # a browser as root is genuinely worse, so it is only reached after every browser has
    # failed a real fetch — and it is reported, not hidden.
    warn "no browser completed a real fetch under the '$FS_USER' account — retrying as root"
    xinstall -d "$UNIT_DIR/flaresolverr.service.d"
    cat > "$UNIT_DIR/flaresolverr.service.d/10-run-as-root.conf" <<'EOF'
# Added automatically because FlareSolverr could not start a browser session under its
# dedicated service account on this host. Running a browser as root is a real downgrade:
# delete this file and restart if you later fix the unprivileged path.
[Service]
User=
Environment=HOME=/root
EOF
    systemctl daemon-reload >/dev/null 2>&1 || true
    systemctl restart flaresolverr.service >/dev/null 2>&1 || true
    if fs_probe; then
      warn "FlareSolverr works, but ONLY as root"
      hint "it is an unauthenticated URL fetcher, so this is a real exposure — see docs/security.md"
      hint "to undo: rm ${UNIT_DIR#"$D"}/flaresolverr.service.d/10-run-as-root.conf && systemctl restart flaresolverr"
      mark flaresolverr "running as root (reduced isolation)"
      fs_done=1
    else
      rm -f "$UNIT_DIR/flaresolverr.service.d/10-run-as-root.conf"
      systemctl daemon-reload >/dev/null 2>&1 || true
    fi
  fi
  if [ "$fs_done" != 1 ]; then
    err "FlareSolverr is installed but cannot fetch pages with any available browser"
    hint "this is the failure that presents as 'searches return nothing'"
    hint "look for the FATAL line: journalctl -u flaresolverr -n 80 --no-pager | grep -iE 'fatal|sandbox|driver'"
    hint "then retry just this component: sudo ./install.sh --repair flaresolverr"
    mark flaresolverr degraded
  fi
fi

if systemctl cat jellyfin.service >/dev/null 2>&1; then
  [ "$(probe http://127.0.0.1:8096/System/Info/Public 5)" = "200" ] && ok "Jellyfin answering" || { warn "Jellyfin not answering yet"; mark jellyfin degraded; }
fi
if systemctl cat prowlarr.service >/dev/null 2>&1; then
  [ "$(probe http://127.0.0.1:9696/ping 5)" = "200" ] && ok "Prowlarr answering" || { warn "Prowlarr not answering yet"; mark prowlarr degraded; }
fi

# The namespace comes up with a deny-all kill switch and no tunnel until a config is
# uploaded. That is the intended safe state on a fresh install, not a failure.
if [ -e /var/run/netns/vpntorrent ]; then
  ok "VPN namespace is up (fails closed until you activate a config)"
  mark vpn ok
else
  err "the vpntorrent namespace was not created"
  hint "journalctl -u vpntorrent-netns -n 50 --no-pager"
  mark vpn "namespace missing"
fi
fi

# ==========================================================================================
# Wire the pieces together, so a fresh install needs no follow-up shell commands
#
# Two things used to be silent manual steps a new user had no way to know about:
#   * Prowlarr's API key had to be copied by hand into Settings → Connections.
#   * FlareSolverr had to be registered in Prowlarr as an Indexer Proxy AND tagged onto
#     indexers. Without that, installing FlareSolverr accomplishes precisely nothing:
#     Cloudflare-protected indexers keep failing exactly as before.
# ==========================================================================================
autowire(){
  [ -n "$D" ] && return 0
  local pw_cfg=/var/lib/prowlarr/config.xml pw_key=""

  # Prowlarr writes its config on first start; give it a moment on a fresh install.
  local waited=0
  while [ ! -f "$pw_cfg" ] && [ "$waited" -lt 20 ]; do
    sleep 2; waited=$((waited + 2))
  done
  [ -f "$pw_cfg" ] || return 0
  pw_key="$(sed -n 's:.*<ApiKey>\([^<]*\)</ApiKey>.*:\1:p' "$pw_cfg" | head -1)"
  [ -n "$pw_key" ] || return 0

  # Pre-fill the orchestrator's config only where the user has not already set something.
  # The dashboard stores its own values in the database and those take precedence, so this
  # can never override a deliberate choice.
  if grep -qE '^[[:space:]]*api_key:[[:space:]]*""' "$CONF_DIR/config.yaml" 2>/dev/null; then
    python3 - "$CONF_DIR/config.yaml" "$pw_key" <<'PYEOF' 2>/dev/null && ok "Prowlarr API key detected and pre-filled"
import re, sys
path, key = sys.argv[1], sys.argv[2]
src = open(path).read()
# Only touch the api_key that belongs to the `indexer:` block, and only if it is empty.
def fill(m):
    return m.group(0).replace('api_key: ""', 'api_key: "%s"' % key)
new = re.sub(r'(?ms)^indexer:.*?(?=^\S|\Z)', fill, src, count=1)
if new != src:
    open(path, 'w').write(new)
PYEOF
  fi

  # Register FlareSolverr with Prowlarr — but ONLY if it actually works. Pointing Prowlarr at
  # a dead proxy would make every search slow or failing, which is worse than not having it.
  if [ "${STATUS[flaresolverr]:-}" != "ok" ]; then
    return 0
  fi
  local api="http://127.0.0.1:9696/api/v1" hdr="X-Api-Key: $pw_key"
  if curl -4 -fsS --max-time 10 -H "$hdr" "$api/indexerproxy" 2>/dev/null | grep -q '"implementation":"FlareSolverr"'; then
    ok "Prowlarr already has a FlareSolverr proxy"
    return 0
  fi
  local tag_id
  tag_id="$(curl -4 -fsS --max-time 10 -H "$hdr" "$api/tag" 2>/dev/null \
            | sed -n 's/.*{"id":\([0-9]*\),"label":"flaresolverr"}.*/\1/p' | head -1)"
  if [ -z "$tag_id" ]; then
    tag_id="$(curl -4 -fsS --max-time 10 -H "$hdr" -H 'Content-Type: application/json' \
              -d '{"label":"flaresolverr"}' "$api/tag" 2>/dev/null \
              | sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -1)"
  fi
  [ -n "$tag_id" ] || { warn "could not create a Prowlarr tag for FlareSolverr"; return 0; }
  if curl -4 -fsS --max-time 15 -H "$hdr" -H 'Content-Type: application/json' -X POST "$api/indexerproxy" \
       -d "{\"name\":\"FlareSolverr\",\"implementation\":\"FlareSolverr\",\"configContract\":\"FlareSolverrSettings\",\"tags\":[$tag_id],\"fields\":[{\"name\":\"host\",\"value\":\"http://127.0.0.1:8191/\"},{\"name\":\"requestTimeout\",\"value\":60}]}" \
       >/dev/null 2>&1; then
    ok "registered FlareSolverr as a Prowlarr indexer proxy (tag: flaresolverr)"
    hint "tag an indexer with 'flaresolverr' in Prowlarr for it to use the proxy"
  else
    warn "could not register the FlareSolverr proxy in Prowlarr — add it manually under Settings → Indexer Proxies"
  fi
}
autowire


FINISHED=1
exit 0
