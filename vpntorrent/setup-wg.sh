#!/bin/bash
# One-shot CLI installer for a WireGuard config, for people who would rather not use the
# dashboard's upload form. Usage:  sudo ./setup-wg.sh /path/to/proton-wg.conf
#
# What it does:
#   1. Installs the file as the ACTIVE config, in the orchestrator-owned config directory
#      (the same one the dashboard writes to) — so the CLI and the dashboard cannot disagree
#      about which config is live.
#   2. Hands the rest to jf-netns-helper, which is the single implementation of config
#      sanitisation (stripping PostUp/PostDown/PreUp/PreDown/Table/SaveConfig/DNS), endpoint
#      pinning, tunnel bring-up and the kill-switch rules. This script deliberately does NOT
#      call wg-quick itself: a second sanitiser is a second thing to get wrong.
set -euo pipefail

SRC="${1:-}"
[ -n "$SRC" ] || { echo "Usage: $0 /path/to/proton-wg.conf" >&2; exit 2; }
[ -r "$SRC" ] || { echo "ERROR: cannot read $SRC" >&2; exit 1; }
[ "$(id -u)" = "0" ] || { echo "ERROR: run this with sudo" >&2; exit 1; }

NETNS=vpntorrent
WG_IF=wg0-vpntorrent
CONFIG_DIR="${VPNTORRENT_CONFIG_DIR:-/var/lib/jellyfreedom/vpnconfigs}"
DEST="$CONFIG_DIR/$WG_IF.conf"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELPER="$SCRIPT_DIR/jf-netns-helper"
[ -x "$HELPER" ] || HELPER=/opt/vpntorrent/jf-netns-helper
[ -x "$HELPER" ] || { echo "ERROR: privileged helper not found at $HELPER — reinstall JellyFreedom" >&2; exit 1; }

# Test the namespace file directly (see the same note in jf-netns-helper: `grep -q` in a
# pipeline can SIGPIPE its upstream and make `set -o pipefail` report a false failure).
if [ ! -e "/var/run/netns/${NETNS}" ]; then
    echo "ERROR: the $NETNS namespace does not exist. Run: systemctl start vpntorrent-netns.service" >&2
    exit 1
fi

# Keep the previous config so a bad one can be rolled back by hand.
mkdir -p "$CONFIG_DIR"
if [ -f "$DEST" ]; then
    cp -a "$DEST" "$DEST.previous"
    echo "kept the previous config as $DEST.previous"
fi
install -m 600 "$SRC" "$DEST"
# The directory belongs to the orchestrator's service user; keep ownership consistent so the
# dashboard can still list and replace the config it now shares with this script.
if id -u jellyfreedom >/dev/null 2>&1; then
    chown jellyfreedom:jellyfreedom "$DEST"
fi
echo "installed $SRC as the active config ($DEST)"

"$HELPER" vpn-down || true
"$HELPER" vpn-up

echo
echo "Testing connectivity from inside the namespace..."
if EXIT_IP="$("$HELPER" exit-ip)"; then
    echo "VPN exit IP: $EXIT_IP"
    echo "Kill switch active: if the tunnel goes down, $NETNS has no default route and OUTPUT is DROP."
else
    echo "WARNING: the tunnel came up but no exit IP could be fetched. Check: journalctl -u vpntorrent-netns" >&2
    exit 1
fi
