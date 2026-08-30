#!/usr/bin/env bash
# uninstall.sh — remove JellyFreedom.
#
#   sudo jellyfreedom uninstall            remove our app + plumbing; keep data, config,
#                                          and the third-party stack (Jellyfin etc.)
#   sudo jellyfreedom uninstall --all      also remove TorrServer, FlareSolverr and Prowlarr
#   sudo jellyfreedom uninstall --purge    --all, plus your database, config and VPN configs
#
# --purge destroys your library index and uploaded VPN configs. It asks first.
set -uo pipefail
[ "$(id -u)" -eq 0 ] || { echo "run as root"; exit 1; }

ALL=0; PURGE=0
for a in "$@"; do
  case "$a" in
    --all) ALL=1 ;;
    --purge) ALL=1; PURGE=1 ;;
    --yes|-y) ASSUME_YES=1 ;;
    --help|-h) sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option: $a" >&2; exit 2 ;;
  esac
done
say(){ printf '\033[1;36m==>\033[0m %s\n' "$*"; }

if [ "$PURGE" = 1 ] && [ "${ASSUME_YES:-0}" != 1 ]; then
  echo "--purge will DELETE your database, config and uploaded VPN configs."
  printf 'Type PURGE to confirm: '
  read -r reply </dev/tty || reply=""
  [ "$reply" = "PURGE" ] || { echo "aborted."; exit 1; }
fi

say "stopping and disabling services"
UNITS="jellyfreedom.service jf-netnsproxy.service jf-tmpreaper.timer jf-tmpreaper.service
       vpntorrent-watchdog.timer vpntorrent-watchdog.service
       vpntorrent-portforward.service torrserver-netns.service vpntorrent-netns.service"
[ "$ALL" = 1 ] && UNITS="$UNITS flaresolverr.service prowlarr.service"
for u in $UNITS; do systemctl disable --now "$u" 2>/dev/null || true; done

/usr/sbin/ip netns del vpntorrent 2>/dev/null || true
/usr/sbin/ip link del veth-host 2>/dev/null || true

say "removing units, sudoers, AppArmor override and app files"
rm -f /etc/systemd/system/jellyfreedom.service \
      /etc/systemd/system/vpntorrent-netns.service \
      /etc/systemd/system/torrserver-netns.service \
      /etc/systemd/system/vpntorrent-portforward.service \
      /etc/systemd/system/vpntorrent-watchdog.service \
      /etc/systemd/system/vpntorrent-watchdog.timer \
      /etc/systemd/system/jf-netnsproxy.service \
      /etc/systemd/system/jf-tmpreaper.service \
      /etc/systemd/system/jf-tmpreaper.timer \
      /etc/sudoers.d/jellyfreedom \
      /etc/apparmor.d/local/wg-quick
rm -rf /opt/jellyfreedom /opt/vpntorrent /etc/netns/vpntorrent /run/vpntorrent
rm -f /usr/local/bin/jellyfreedom
systemctl daemon-reload 2>/dev/null || true
command -v apparmor_parser >/dev/null && apparmor_parser -r /etc/apparmor.d/wg-quick 2>/dev/null || true

if [ "$ALL" = 1 ]; then
  # These were installed by us, so --all removes them. Previously they were left behind
  # silently — roughly 1GB of orphaned files the "KEPT" notice never mentioned.
  say "removing the third-party components we installed"
  rm -f /etc/systemd/system/flaresolverr.service /etc/systemd/system/prowlarr.service
  systemctl daemon-reload 2>/dev/null || true
  rm -f /usr/local/bin/torrserver /usr/local/bin/yt-dlp
  rm -rf /opt/flaresolverr /opt/Prowlarr /opt/prowlarr /var/lib/torrserver /var/lib/flaresolverr
  echo "  removed TorrServer, yt-dlp, FlareSolverr and Prowlarr"
  echo "  NOTE: Jellyfin is left installed — remove it with: sudo apt-get remove jellyfin"
fi

if [ "$PURGE" = 1 ]; then
  say "purging data and config"
  rm -rf /var/lib/jellyfreedom /etc/jellyfreedom /var/log/jellyfreedom-install.log
  for u in jellyfreedom torrserver flaresolverr prowlarr; do userdel "$u" 2>/dev/null || true; done
  echo "  removed data, config and service users"
  echo "  NOTE: your media library folders under /srv/jellyfreedom are NOT deleted."
fi

say "done."
if [ "$PURGE" != 1 ]; then
  echo "    KEPT (delete manually for a clean slate):"
  echo "      data:    /var/lib/jellyfreedom   (library index + uploaded VPN configs)"
  echo "      config:  /etc/jellyfreedom"
  echo "      library: /srv/jellyfreedom       (.strm files)"
  echo "      users:   jellyfreedom, torrserver, flaresolverr"
  [ "$ALL" != 1 ] && echo "      apps:    Jellyfin, Prowlarr, TorrServer, FlareSolverr  (use --all)"
fi

# A trailing `[ test ] && cmd` sets the script exit status when the test fails, so be
# explicit rather than letting the last conditional decide it.
exit 0
