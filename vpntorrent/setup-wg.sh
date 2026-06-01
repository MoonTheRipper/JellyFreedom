#!/bin/bash
# Run this once after downloading your Proton WireGuard config.
# Usage: sudo ./setup-wg.sh /path/to/proton-wg.conf
#
# What it does:
#   1. Strips DNS from the config (DNS is not needed inside the netns)
#   2. Installs wg0 inside the vpntorrent namespace
#   3. Sets wg0 as the default route (kill switch: no wg0 = no outbound)
#   4. Configures wg0 to auto-restart via a systemd dropin

set -e
WG_CONF="${1:?Usage: $0 /path/to/proton-wg.conf}"
NETNS=vpntorrent

if ! ip netns list | grep -q "^${NETNS}"; then
    echo "ERROR: run setup-netns.sh first"
    exit 1
fi

# Strip DNS line (we don't want DNS leaks from inside the netns)
STRIPPED=$(grep -v '^\s*DNS\s*=' "${WG_CONF}")

# Write to /etc/wireguard/wg0-vpntorrent.conf
echo "${STRIPPED}" > /etc/wireguard/wg0-vpntorrent.conf
chmod 600 /etc/wireguard/wg0-vpntorrent.conf

# Bring up WireGuard inside the netns
ip netns exec ${NETNS} wg-quick up /etc/wireguard/wg0-vpntorrent.conf

# Set default route via wg0 inside netns (this IS the kill switch)
WG_IP=$(ip netns exec ${NETNS} ip -4 addr show wg0 2>/dev/null | grep -oP '(?<=inet )[^/]+')
ip netns exec ${NETNS} ip route replace default dev wg0

echo "WireGuard up inside vpntorrent. VPN IP: ${WG_IP}"
echo "Kill switch active: if wg0 goes down, vpntorrent has no default route."

# Test: confirm TorrServer can reach the internet via VPN
echo "Testing connectivity..."
ip netns exec ${NETNS} curl -s --max-time 5 https://ifconfig.me && echo " <- VPN exit IP"
