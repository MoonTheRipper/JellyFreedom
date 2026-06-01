#!/usr/bin/env bash
# install.sh — JellyFreedom release installer (Debian/Ubuntu).
#
# Installs the parts we own (Go orchestrator + web assets + VPN-netns plumbing + systemd units
# + scoped sudoers + AppArmor override) using portable FHS paths and dedicated service users,
# sets up FlareSolverr and TorrServer (single artifacts we can fetch), and tells you which
# heavier prerequisites (Jellyfin, Prowlarr) you must provide.
#
# IDEMPOTENT / NON-DESTRUCTIVE: anything already installed is detected and LEFT ALONE (configs
# are preserved; we never clobber an existing Jellyfin/Prowlarr/TorrServer/FlareSolverr — at
# most we re-apply our own required tweaks).
#
# Run from an extracted release bundle:  sudo ./install.sh
set -euo pipefail

# ---- fixed FHS layout (portable; no user-specific paths) ----
APP_DIR=/opt/jellyfreedom
VPN_DIR=/opt/vpntorrent
CONF_DIR=/etc/jellyfreedom
DATA_DIR=/var/lib/jellyfreedom
VPNCONF_DIR="$DATA_DIR/vpnconfigs"
SVC_USER=jellyfreedom
TS_USER=torrserver
SRC="$(cd "$(dirname "$0")" && pwd)"

# pinned versions of the artifacts we fetch
FS_VERSION="v3.5.0"     # FlareSolverr
TS_VERSION="MatriX.141.2"  # TorrServer

say(){ printf '\033[1;36m==>\033[0m %s\n' "$*"; }
ok(){  printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }
warn(){ printf '\033[1;33m  [!]\033[0m %s\n' "$*"; }
[ "$(id -u)" -eq 0 ] || { echo "Run as root (sudo ./install.sh)"; exit 1; }
command -v apt-get >/dev/null || { echo "This installer targets Debian/Ubuntu (apt)."; exit 1; }

cat <<'BANNER'

  JellyFreedom installer
  ----------------------
  Sets up the FULL stack: the orchestrator + VPN plumbing, and installs the supporting
  services it drives — TorrServer, FlareSolverr, Jellyfin, and Prowlarr.

  IMPORTANT:
    • This pulls a few hundred MB (Jellyfin + ffmpeg, Prowlarr, FlareSolverr, Chromium) —
      give it time and a working network.
    • NON-DESTRUCTIVE: anything already present (Jellyfin, Prowlarr, TorrServer, FlareSolverr,
      or your config) is DETECTED AND LEFT ALONE — never overwritten or reset; your settings
      are preserved. Existing components are at most updated in place.

BANNER
sleep 1

say "Installing OS dependencies (apt — skips anything already present)"
apt-get update -qq
apt-get install -y -qq wireguard-tools natpmpc iproute2 iptables jq curl ca-certificates tar xvfb

say "Service users"
id -u "$SVC_USER" &>/dev/null && ok "$SVC_USER exists" || { useradd --system --no-create-home --shell /usr/sbin/nologin "$SVC_USER"; ok "created $SVC_USER"; }
id -u "$TS_USER"  &>/dev/null && ok "$TS_USER exists"  || { useradd --system --no-create-home --shell /usr/sbin/nologin "$TS_USER"; ok "created $TS_USER"; }

say "Directories (FHS)"
install -d -o "$SVC_USER" -g "$SVC_USER" "$APP_DIR" "$APP_DIR/bin" "$DATA_DIR"
install -d -o "$SVC_USER" -g "$SVC_USER" -m 700 "$VPNCONF_DIR"
install -d "$VPN_DIR" "$CONF_DIR"
install -d -o "$TS_USER" -g "$TS_USER" /var/lib/torrserver
ok "ready"

say "Orchestrator + web assets"
install -o "$SVC_USER" -g "$SVC_USER" -m 755 "$SRC/bin/orchestrator" "$APP_DIR/bin/orchestrator"
rm -rf "$APP_DIR/web"; cp -r "$SRC/web" "$APP_DIR/web"; chown -R "$SVC_USER":"$SVC_USER" "$APP_DIR/web"
install -m 755 "$SRC/vpntorrent/setup-netns.sh" "$VPN_DIR/setup-netns.sh"
install -m 755 "$SRC/vpntorrent/watchdog.sh"    "$VPN_DIR/watchdog.sh"
install -m 755 "$SRC/vpntorrent/portforward.sh" "$VPN_DIR/portforward.sh"
ok "installed"

say "Config (kept if it already exists)"
[ -f "$CONF_DIR/config.yaml" ] && ok "existing config left untouched" || { install -o "$SVC_USER" -g "$SVC_USER" -m 640 "$SRC/config.sample.yaml" "$CONF_DIR/config.yaml"; ok "wrote starter config — edit it"; }

# ---- TorrServer (single Go binary) ----
say "TorrServer"
if [ -x /usr/local/bin/torrserver ]; then
  ok "present at /usr/local/bin/torrserver — left alone"
else
  arch=$(uname -m); case "$arch" in x86_64) tsarch=amd64;; aarch64) tsarch=arm64;; *) tsarch="";; esac
  if [ -n "$tsarch" ] && curl -4 -fsSL "https://github.com/YouROK/TorrServer/releases/download/$TS_VERSION/TorrServer-linux-$tsarch" -o /usr/local/bin/torrserver; then
    chmod +x /usr/local/bin/torrserver; ok "downloaded TorrServer $TS_VERSION"
  else
    warn "could not fetch TorrServer — install the single binary to /usr/local/bin/torrserver manually"
  fi
fi

# ---- FlareSolverr (+ the Chrome-142-crash fix we learned the hard way) ----
say "FlareSolverr (with the bundled-Chrome crash fix)"
# Robust browser: FlareSolverr's bundled Chrome 142 crashes the renderer on kernel 7.x — we run
# the system Chromium instead. Install one if absent.
if ! command -v chromium-browser >/dev/null && ! command -v chromium >/dev/null; then
  apt-get install -y -qq chromium-browser 2>/dev/null || snap install chromium 2>/dev/null || warn "install a system Chromium (apt chromium-browser or snap chromium) for FlareSolverr"
fi
CHROMIUM="$(command -v chromium-browser || command -v chromium || echo /usr/bin/chromium-browser)"
if [ -x /opt/flaresolverr/flaresolverr ]; then
  ok "FlareSolverr present — left alone; re-applying the Chrome fix"
else
  tmp=$(mktemp -d)
  if curl -4 -fsSL "https://github.com/FlareSolverr/FlareSolverr/releases/download/$FS_VERSION/flaresolverr_linux_x64.tar.gz" -o "$tmp/fs.tgz" && tar xzf "$tmp/fs.tgz" -C /opt; then
    ok "installed FlareSolverr $FS_VERSION"
  else
    warn "could not fetch FlareSolverr — install it to /opt/flaresolverr manually, then re-run"
  fi
  rm -rf "$tmp"
fi
# THE FIX (see memory feedback-flaresolverr-chrome142): redirect the bundled-chrome wrapper to
# system Chromium, and clear the stale/broken cached chromedriver so a fresh one is fetched.
if [ -d /opt/flaresolverr/_internal/chrome ]; then
  printf '#!/bin/bash\nexec %s --no-sandbox "$@"\n' "$CHROMIUM" > /opt/flaresolverr/_internal/chrome/chrome
  chmod +x /opt/flaresolverr/_internal/chrome/chrome
  rm -rf /root/.local/share/undetected_chromedriver /root/.cache/selenium/chromedriver 2>/dev/null || true
  ok "chrome wrapper → $CHROMIUM ; chromedriver cache cleared"
fi

# ---- Jellyfin (official apt repo) ----
say "Jellyfin"
if systemctl list-unit-files jellyfin.service &>/dev/null && systemctl cat jellyfin &>/dev/null; then
  ok "present — left alone (your library/config untouched)"
else
  if curl -4 -fsSL https://repo.jellyfin.org/install-debuntu.sh | bash >/dev/null 2>&1; then
    ok "installed Jellyfin"
  else
    warn "Jellyfin install failed — install manually from repo.jellyfin.org, then set its URL/key in the dashboard"
  fi
fi

# ---- Prowlarr (self-contained tarball + service) ----
say "Prowlarr"
if (systemctl list-unit-files prowlarr.service &>/dev/null && systemctl cat prowlarr &>/dev/null) || [ -x /opt/Prowlarr/Prowlarr ]; then
  ok "present — left alone"
else
  id -u prowlarr &>/dev/null || useradd --system --no-create-home --shell /usr/sbin/nologin prowlarr
  install -d -o prowlarr -g prowlarr /var/lib/prowlarr
  arch=$(uname -m); case "$arch" in x86_64) pa=x64;; aarch64) pa=arm64;; *) pa="";; esac
  tmp=$(mktemp -d)
  if [ -n "$pa" ] && curl -4 -fsSL "https://prowlarr.servarr.com/v1/update/master/updatefile?os=linux&runtime=netcore&arch=$pa" -o "$tmp/prowlarr.tgz" && tar xzf "$tmp/prowlarr.tgz" -C /opt; then
    chown -R prowlarr:prowlarr /opt/Prowlarr
    cat > /etc/systemd/system/prowlarr.service <<'EOF'
[Unit]
Description=Prowlarr
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=prowlarr
ExecStart=/opt/Prowlarr/Prowlarr -nobrowser -data=/var/lib/prowlarr
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
    ok "installed Prowlarr → /opt/Prowlarr (data /var/lib/prowlarr)"
  else
    warn "could not fetch Prowlarr — install manually, then set its URL/key in the dashboard"
  fi
  rm -rf "$tmp"
fi

say "systemd units"
cat > /etc/systemd/system/jellyfreedom.service <<EOF
[Unit]
Description=JellyFreedom Orchestrator
After=network-online.target vpntorrent-netns.service
Wants=network-online.target

[Service]
Type=simple
User=$SVC_USER
WorkingDirectory=$APP_DIR
ExecStart=$APP_DIR/bin/orchestrator --config $CONF_DIR/config.yaml --db $DATA_DIR/jellyfreedom.db --assets $APP_DIR/web
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

cat > /etc/systemd/system/vpntorrent-netns.service <<EOF
[Unit]
Description=vpntorrent network namespace setup
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
Environment=VPNTORRENT_CONFIG_DIR=$VPNCONF_DIR
ExecStart=$VPN_DIR/setup-netns.sh
ExecStop=/usr/sbin/ip netns del vpntorrent

[Install]
WantedBy=multi-user.target
EOF

cat > /etc/systemd/system/torrserver-netns.service <<EOF
[Unit]
Description=TorrServer (inside vpntorrent netns)
After=vpntorrent-netns.service network-online.target
Requires=vpntorrent-netns.service

[Service]
Type=simple
User=$TS_USER
NetworkNamespacePath=/var/run/netns/vpntorrent
BindReadOnlyPaths=/etc/netns/vpntorrent/resolv.conf:/etc/resolv.conf
ExecStart=/usr/local/bin/torrserver --port 8090 --path /var/lib/torrserver
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

cat > /etc/systemd/system/vpntorrent-portforward.service <<EOF
[Unit]
Description=vpntorrent port-forward keeper (NAT-PMP, e.g. Proton -> TorrServer listen port)
After=torrserver-netns.service vpntorrent-netns.service

[Service]
Type=simple
ExecStart=$VPN_DIR/portforward.sh
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

cat > /etc/systemd/system/vpntorrent-watchdog.service <<EOF
[Unit]
Description=vpntorrent VPN + TorrServer watchdog
After=torrserver-netns.service
Wants=vpntorrent-netns.service

[Service]
Type=oneshot
ExecStart=$VPN_DIR/watchdog.sh
TimeoutStartSec=120
EOF

cat > /etc/systemd/system/vpntorrent-watchdog.timer <<'EOF'
[Unit]
Description=Run the vpntorrent watchdog every 60s

[Timer]
OnBootSec=90
OnUnitActiveSec=60
AccuracySec=10

[Install]
WantedBy=timers.target
EOF

# FlareSolverr unit — only write it if we didn't find a pre-existing one (don't clobber theirs).
if [ ! -f /etc/systemd/system/flaresolverr.service ] && [ -x /opt/flaresolverr/flaresolverr ]; then
cat > /etc/systemd/system/flaresolverr.service <<'EOF'
[Unit]
Description=FlareSolverr
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment="LOG_LEVEL=info"
Environment="CAPTCHA_SOLVER=none"
# Forces ChromeDriver downloads over IPv4 — GitHub IPv6 can hang on some networks.
RestrictAddressFamilies=AF_INET AF_UNIX
ExecStart=/opt/flaresolverr/flaresolverr
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
fi

say "Scoped sudoers (orchestrator restarts services + reads VPN status; nothing else)"
cat > /etc/sudoers.d/jellyfreedom <<EOF
$SVC_USER ALL=(root) NOPASSWD: /bin/systemctl restart jellyfin
$SVC_USER ALL=(root) NOPASSWD: /bin/systemctl restart torrserver-netns
$SVC_USER ALL=(root) NOPASSWD: /bin/systemctl restart vpntorrent-netns
$SVC_USER ALL=(root) NOPASSWD: /bin/systemctl restart prowlarr
$SVC_USER ALL=(root) NOPASSWD: /bin/systemctl restart flaresolverr
$SVC_USER ALL=(root) NOPASSWD: /bin/systemctl restart jellyfreedom
$SVC_USER ALL=(root) NOPASSWD: /usr/sbin/ip netns exec vpntorrent wg show *
$SVC_USER ALL=(root) NOPASSWD: /usr/sbin/ip netns exec vpntorrent /usr/bin/curl *
EOF
chmod 440 /etc/sudoers.d/jellyfreedom
visudo -cf /etc/sudoers.d/jellyfreedom >/dev/null || { echo "sudoers validation failed"; rm -f /etc/sudoers.d/jellyfreedom; exit 1; }
ok "valid"

say "AppArmor override (lets wg-quick read uploaded configs)"
if [ -d /etc/apparmor.d ]; then
  install -d /etc/apparmor.d/local
  printf '# JellyFreedom: allow wg-quick to read uploaded WireGuard configs\n%s/ r,\n%s/** r,\n' "$VPNCONF_DIR" "$VPNCONF_DIR" > /etc/apparmor.d/local/wg-quick
  command -v apparmor_parser >/dev/null && apparmor_parser -r /etc/apparmor.d/wg-quick 2>/dev/null && ok "applied" || warn "apparmor not reloaded (ok if AppArmor isn't active)"
else
  ok "no AppArmor on this host — skipped"
fi

say "Component check"
[ -x /usr/local/bin/torrserver ] && ok "TorrServer: present" || warn "TorrServer: MISSING (install manually)"
[ -x /opt/flaresolverr/flaresolverr ] && ok "FlareSolverr: present" || warn "FlareSolverr: MISSING (install manually)"
for svc in jellyfin prowlarr; do
  if systemctl list-unit-files "$svc.service" &>/dev/null && systemctl cat "$svc" &>/dev/null; then
    ok "$svc: present"
  else
    warn "$svc: MISSING — install failed; install manually then set its URL/key in the dashboard"
  fi
done

say "Enabling services"
systemctl daemon-reload
systemctl enable --now vpntorrent-netns.service
[ -x /usr/local/bin/torrserver ] && systemctl enable --now torrserver-netns.service || warn "skipping torrserver-netns (binary missing)"
[ -x /opt/flaresolverr/flaresolverr ] && systemctl enable --now flaresolverr.service || true
systemctl list-unit-files jellyfin.service &>/dev/null && systemctl enable --now jellyfin.service 2>/dev/null || true
[ -x /opt/Prowlarr/Prowlarr ] && systemctl enable --now prowlarr.service 2>/dev/null || true
systemctl enable --now vpntorrent-portforward.service vpntorrent-watchdog.timer
systemctl enable --now jellyfreedom.service

cat <<EOF

$(printf '\033[1;32m')Installed.$(printf '\033[0m') Next steps:
  1. Confirm the services above are running (the component check shows their state).
  2. Open the dashboard:  http://<this-host>:1990/dashboard/  — create the admin account.
  3. Settings — set TMDB / Prowlarr / Jellyfin URLs + keys (or edit $CONF_DIR/config.yaml).
  4. VPN → Configurations — upload a WireGuard .conf from ANY provider or self-hosted
     (choose a torrent/P2P-friendly server) and click Activate.
  5. Add the .strm library folders (movies + tv) to Jellyfin.

Paths:  app=$APP_DIR  data=$DATA_DIR  config=$CONF_DIR  vpn=$VPN_DIR
Logs:   journalctl -u jellyfreedom -f
EOF
