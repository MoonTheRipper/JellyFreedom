#!/usr/bin/env bash
# migrate-local.sh — move a repo-based JellyFreedom deployment into standard installed paths
# so the live service no longer runs out of the source checkout.
#
#   /opt/jellyfreedom/bin/orchestrator   (was <repo>/bin/orchestrator)
#   /opt/jellyfreedom/web                (was <repo>/web)
#   /etc/jellyfreedom/config.yaml        (was <repo>/config.yaml)
#   /var/lib/jellyfreedom/jellyfreedom.db(was <repo>/jellyfreedom.db)
#
# It preserves the current run-user, your config, your DB, and leaves the VPN netns
# (/opt/vpntorrent) and media libraries (/srv/...) and vpnconfigs untouched. Idempotent:
# existing config/DB at the destination are NOT clobbered. Also installs the `jellyfreedom`
# control CLI to /usr/local/bin.
#
#   sudo bash release/migrate-local.sh
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "run with sudo:  sudo bash release/migrate-local.sh" >&2; exit 1; }

REPO="$(cd "$(dirname "$0")/.." && pwd)"
APP_DIR=/opt/jellyfreedom
CONF_DIR=/etc/jellyfreedom
DATA_DIR=/var/lib/jellyfreedom
UNIT=/etc/systemd/system/jellyfreedom.service
SERVICE=jellyfreedom

# Run-user: whoever currently owns the running binary (falls back to the sudo caller).
RUN_USER="$(stat -c '%U' "$REPO/bin/orchestrator" 2>/dev/null || echo "${SUDO_USER:-root}")"
RUN_GRP="$(id -gn "$RUN_USER" 2>/dev/null || echo "$RUN_USER")"

echo "==> repo:      $REPO"
echo "==> run-user:  $RUN_USER:$RUN_GRP"

[ -x "$REPO/bin/orchestrator" ] || { echo "no built binary at $REPO/bin/orchestrator" >&2; exit 1; }
[ -f "$REPO/config.yaml" ]      || { echo "no config at $REPO/config.yaml" >&2; exit 1; }

echo "==> stopping $SERVICE"
systemctl stop "$SERVICE" 2>/dev/null || true

echo "==> creating $APP_DIR $CONF_DIR $DATA_DIR"
install -d -o "$RUN_USER" -g "$RUN_GRP" "$APP_DIR" "$APP_DIR/bin" "$DATA_DIR"
install -d "$CONF_DIR"

echo "==> binary + web assets -> $APP_DIR"
install -o "$RUN_USER" -g "$RUN_GRP" -m 755 "$REPO/bin/orchestrator" "$APP_DIR/bin/orchestrator"
rm -rf "$APP_DIR/web"; cp -r "$REPO/web" "$APP_DIR/web"; chown -R "$RUN_USER":"$RUN_GRP" "$APP_DIR/web"

# Version stamp for `jellyfreedom --version` / update comparisons.
ver="$(cat "$REPO/VERSION" 2>/dev/null || cat "$REPO/dist"/jellyfreedom-*/VERSION 2>/dev/null | head -1 || echo "0.1.0")"
echo "$ver" > "$APP_DIR/VERSION"

echo "==> config -> $CONF_DIR/config.yaml"
if [ -f "$CONF_DIR/config.yaml" ]; then
  echo "    existing config left untouched"
else
  install -o "$RUN_USER" -g "$RUN_GRP" -m 640 "$REPO/config.yaml" "$CONF_DIR/config.yaml"
fi

echo "==> database -> $DATA_DIR/jellyfreedom.db"
if [ -f "$DATA_DIR/jellyfreedom.db" ]; then
  echo "    existing DB left untouched"
elif [ -f "$REPO/jellyfreedom.db" ]; then
  install -o "$RUN_USER" -g "$RUN_GRP" -m 644 "$REPO/jellyfreedom.db" "$DATA_DIR/jellyfreedom.db"
  # carry over WAL/SHM sidecars if present
  for ext in -wal -shm; do
    [ -f "$REPO/jellyfreedom.db$ext" ] && install -o "$RUN_USER" -g "$RUN_GRP" -m 644 "$REPO/jellyfreedom.db$ext" "$DATA_DIR/jellyfreedom.db$ext" || true
  done
else
  echo "    no repo DB found — a fresh one will be created on first start"
fi

echo "==> rewriting $UNIT (User=$RUN_USER, FHS paths)"
cat > "$UNIT" <<UNITEOF
[Unit]
Description=JellyFreedom Orchestrator
After=network-online.target vpntorrent-netns.service
Wants=network-online.target

[Service]
Type=simple
User=$RUN_USER
WorkingDirectory=$APP_DIR
ExecStart=$APP_DIR/bin/orchestrator --config $CONF_DIR/config.yaml --db $DATA_DIR/jellyfreedom.db --assets $APP_DIR/web
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
UNITEOF

echo "==> installing control CLI -> /usr/local/bin/jellyfreedom"
install -m 755 "$REPO/release/jellyfreedom" /usr/local/bin/jellyfreedom

echo "==> reloading + starting"
systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null 2>&1 || true
systemctl start "$SERVICE"
sleep 1
if systemctl is-active --quiet "$SERVICE"; then
  echo "==> done. JellyFreedom now runs from $APP_DIR (no longer from the repo)."
  echo "    version: $(cat "$APP_DIR/VERSION")"
  echo "    update later with:  sudo jellyfreedom --update"
  echo "    the repo at $REPO is now source-only; its bin/ config.yaml *.db are no longer used."
else
  echo "service failed to start — check: journalctl -u $SERVICE -n 50" >&2
  exit 1
fi
