#!/usr/bin/env bash
# uninstall.sh — remove JellyFreedom (app + plumbing). Keeps your data and config by default.
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "Run as root"; exit 1; }

echo "==> stopping + disabling services"
for u in jellyfreedom.service vpntorrent-watchdog.timer vpntorrent-watchdog.service \
         vpntorrent-portforward.service torrserver-netns.service vpntorrent-netns.service; do
  systemctl disable --now "$u" 2>/dev/null || true
done
/usr/sbin/ip netns del vpntorrent 2>/dev/null || true
/usr/sbin/ip link del veth-host 2>/dev/null || true

echo "==> removing units, sudoers, apparmor override, app files"
rm -f /etc/systemd/system/jellyfreedom.service \
      /etc/systemd/system/vpntorrent-netns.service \
      /etc/systemd/system/torrserver-netns.service \
      /etc/systemd/system/vpntorrent-portforward.service \
      /etc/systemd/system/vpntorrent-watchdog.service \
      /etc/systemd/system/vpntorrent-watchdog.timer \
      /etc/sudoers.d/jellyfreedom \
      /etc/apparmor.d/local/wg-quick
systemctl daemon-reload
command -v apparmor_parser >/dev/null && apparmor_parser -r /etc/apparmor.d/wg-quick 2>/dev/null || true
rm -rf /opt/jellyfreedom /opt/vpntorrent

echo "==> done."
echo "    KEPT (delete manually if you want a clean slate):"
echo "      data:   /var/lib/jellyfreedom   (db + uploaded VPN configs)"
echo "      config: /etc/jellyfreedom"
echo "      users:  jellyfreedom, torrserver  (userdel to remove)"
