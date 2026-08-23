#!/bin/bash
# vpntorrent-watchdog: keep the torrent path healthy without manual intervention, and WITHOUT
# needlessly interrupting an active stream.
#  (1) Re-assert the host NAT/forwarding the tunnel depends on (heals an external iptables flush).
#  (2) TorrServer BT client: a crash leaves /echo working but `add` returning 500. Restart only
#      after TWO consecutive failures (ignore transient blips), and skip the probe entirely while
#      a stream is live (a live stream already proves the client works — no churn, no false restart).
#  (3) VPN data path: probe two independent targets; on a real outage re-establish the tunnel.
#  (4) ANONYMITY: assert the namespace's egress address is not the host's own, and that the
#      default route still leaves via wg0. Reachability alone does not prove the traffic is
#      tunnelled, and this script used to discard the very body that says where it came out.
#
# NOTE ON `set -e`: deliberately NOT used here. This script is a prober; a non-zero exit from
# curl/grep/iptables is normal control flow and is what every branch below is testing for.
# `set -e` would turn the first failed probe into a silent abort — the opposite of a watchdog.
set -uo pipefail

NETNS=vpntorrent
ORCH=http://127.0.0.1:1990
HC_HASH=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
STATUS=/run/vpntorrent-status
TS_STRIKES=/run/vpntorrent-ts-strikes
RUN_DIR=/run/vpntorrent

# The veth addressing is decided once, by setup-netns.sh, and published here. Fall back to the
# shipped defaults if the namespace has never been built (nothing to watch in that case anyway).
nv() { # nv <KEY> <DEFAULT>
    local v=""
    [ -r "$RUN_DIR/netns.env" ] && v="$(sed -n "s/^${1}=//p" "$RUN_DIR/netns.env" | head -1 | tr -d '[:space:]')"
    printf '%s' "${v:-$2}"
}
VETH_SUBNET="$(nv VETH_SUBNET 10.42.0.0/30)"
VETH_VPN_IP="$(nv VETH_VPN_IP 10.42.0.2)"
TS="http://${VETH_VPN_IP}:8090"

# The privileged helper owns config sanitisation and tunnel bring-up — this script must never
# hand wg-quick a config itself, or there would be two sanitisers to keep in sync.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELPER="$SCRIPT_DIR/jf-netns-helper"
[ -x "$HELPER" ] || HELPER=/opt/vpntorrent/jf-netns-helper

playback_active(){ curl -s --max-time 4 "$ORCH/api/playback/active" 2>/dev/null | grep -q '"active":true'; }
# NS_EXIT_IP is set by probe() to the address the namespace actually came out on.
# /cdn-cgi/trace reports the caller's egress IP in its body; the previous version threw
# that away with -o /dev/null and asked only "did bytes come back", which a leaking setup
# answers just as cheerfully as a tunnelled one.
NS_EXIT_IP=""
HOST_IP_CACHE=/run/vpntorrent-host-ip

probe(){
  local body url
  for url in https://1.1.1.1/cdn-cgi/trace https://1.0.0.1/cdn-cgi/trace; do
    body="$(ip netns exec "$NETNS" curl -s --max-time 8 "$url" 2>/dev/null)" || continue
    [ -n "$body" ] || continue
    NS_EXIT_IP="$(printf '%s' "$body" | sed -n 's/^ip=//p' | head -1 | tr -d '[:space:]')"
    return 0
  done
  return 1
}

# host_public_ip: the address THIS machine leaves on, i.e. what the ISP sees. Cached for an
# hour — it changes rarely, and this runs every minute.
host_public_ip(){
  if [ -r "$HOST_IP_CACHE" ] && [ $(( $(date +%s) - $(stat -c %Y "$HOST_IP_CACHE" 2>/dev/null || echo 0) )) -lt 3600 ]; then
    cat "$HOST_IP_CACHE"; return 0
  fi
  local ip
  ip="$(curl -s --max-time 8 https://1.1.1.1/cdn-cgi/trace 2>/dev/null | sed -n 's/^ip=//p' | head -1 | tr -d '[:space:]')"
  [ -n "$ip" ] && printf '%s' "$ip" > "$HOST_IP_CACHE"
  printf '%s' "$ip"
}

# default_route_is_tunnel: the kill switch in one line. If the namespace's default route is
# not the WireGuard device, torrent traffic has somewhere else to go.
default_route_is_tunnel(){
  ip netns exec "$NETNS" ip route show default 2>/dev/null | grep -q 'dev wg0-'
}

# anonymity_ok: refuse to call a reachable tunnel healthy until it is also ANONYMOUS.
# Returns 1 only on positive evidence of a leak — an unknown address is never treated as
# one, because stopping the torrent stack on a failed lookup would be its own outage.
anonymity_ok(){
  if ! default_route_is_tunnel; then
    logger -t vpntorrent-watchdog "KILL SWITCH: namespace default route is not wg0 — stopping torrserver"
    echo "leak risk: default route left the tunnel; torrserver stopped" > "$STATUS"
    systemctl stop torrserver-netns.service 2>/dev/null || true
    return 1
  fi
  local host_ip
  host_ip="$(host_public_ip)"
  [ -n "$NS_EXIT_IP" ] && [ -n "$host_ip" ] || return 0   # cannot tell; do not act on a guess
  if [ "$NS_EXIT_IP" = "$host_ip" ]; then
    logger -t vpntorrent-watchdog "LEAK: namespace egress ($NS_EXIT_IP) is the host's own public address — stopping torrserver"
    echo "LEAK: torrent traffic was exiting on this machine's own address; torrserver stopped" > "$STATUS"
    systemctl stop torrserver-netns.service 2>/dev/null || true
    return 1
  fi
  return 0
}

# resume_after_leak: a leak stop is latched in the status file, so once the tunnel is
# genuinely anonymous again the stack comes back on its own rather than waiting for a human
# who may not know it stopped.
resume_after_leak(){
  case "$(cat "$STATUS" 2>/dev/null)" in
    LEAK:*|"leak risk:"*)
      logger -t vpntorrent-watchdog "anonymity restored (egress $NS_EXIT_IP); restarting torrserver"
      systemctl start torrserver-netns.service 2>/dev/null || true
      ;;
  esac
}

# (1) Heal the host NAT/forwarding the WG handshake rides on — an external flush (ufw reload,
# docker install, firewall reset) would otherwise silently break the tunnel until a netns restart.
sysctl -qw net.ipv4.ip_forward=1 2>/dev/null || true
iptables -w 5 -t nat -C POSTROUTING -s "$VETH_SUBNET" -j MASQUERADE 2>/dev/null || \
  iptables -w 5 -t nat -A POSTROUTING -s "$VETH_SUBNET" -j MASQUERADE 2>/dev/null || \
  logger -t vpntorrent-watchdog "could not re-assert the MASQUERADE rule for $VETH_SUBNET"

# (2) TorrServer BT-client health — 2-strike, and never while streaming.
if curl -s --max-time 5 "$TS/echo" | grep -q MatriX; then
  if playback_active; then
    rm -f "$TS_STRIKES"   # a live stream proves the BT client works — clear strikes, don't churn/restart
  else
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 -X POST "$TS/torrents" \
      -d "{\"action\":\"add\",\"link\":\"magnet:?xt=urn:btih:${HC_HASH}&dn=hc\",\"title\":\"hc\"}")
    curl -s --max-time 5 -X POST "$TS/torrents" -d "{\"action\":\"rem\",\"hash\":\"${HC_HASH}\"}" >/dev/null 2>&1
    if [ "$code" = "500" ]; then
      n=$(( $(cat "$TS_STRIKES" 2>/dev/null || echo 0) + 1 ))
      echo "$n" > "$TS_STRIKES"
      if [ "$n" -ge 2 ]; then
        logger -t vpntorrent-watchdog "TorrServer BT client crashed (add->500 x$n); restarting torrserver-netns"
        systemctl restart torrserver-netns.service
        rm -f "$TS_STRIKES"
      else
        logger -t vpntorrent-watchdog "TorrServer add->500 (strike $n/2); confirming next cycle"
      fi
    else
      rm -f "$TS_STRIKES"
    fi
  fi
else
  rm -f "$TS_STRIKES"   # TS down/restarting — systemd Restart= handles it
fi

# (3) VPN data path — two strikes 5s apart before acting (ignore momentary blips). A genuinely
# down tunnel means any stream is already broken, so recovery here may restart freely.
if probe; then
  anonymity_ok || exit 1
  resume_after_leak
  echo "ok" > "$STATUS"; exit 0
fi
sleep 5
if probe; then
  anonymity_ok || exit 1
  resume_after_leak
  echo "ok" > "$STATUS"; exit 0
fi

logger -t vpntorrent-watchdog "tunnel has no data path; bouncing WireGuard"
echo "down: bouncing wg" > "$STATUS"
if [ -x "$HELPER" ]; then
  # vpn-up re-sanitises the active config, re-pins the endpoint route and refreshes the
  # handshake firewall rule — so a bounce also picks up a config the user just activated
  # in the dashboard, including one pointing at a different VPN server.
  "$HELPER" vpn-down 2>&1 | logger -t vpntorrent-watchdog
  "$HELPER" vpn-up   2>&1 | logger -t vpntorrent-watchdog
else
  logger -t vpntorrent-watchdog "privileged helper missing at $HELPER — cannot bounce the tunnel; reinstall JellyFreedom"
fi
sleep 8
if probe && anonymity_ok; then logger -t vpntorrent-watchdog "recovered via wg bounce"; resume_after_leak; echo "ok" > "$STATUS"; exit 0; fi

logger -t vpntorrent-watchdog "wg bounce failed; full netns rebuild"
systemctl stop torrserver-netns.service 2>/dev/null || true
systemctl restart vpntorrent-netns.service 2>/dev/null || true
systemctl start torrserver-netns.service 2>/dev/null || true
systemctl restart vpntorrent-portforward.service 2>/dev/null || true   # was orphaned by the netns restart
sleep 10
if probe && anonymity_ok; then logger -t vpntorrent-watchdog "recovered via full rebuild"; resume_after_leak; echo "ok" > "$STATUS"; exit 0; fi

logger -t vpntorrent-watchdog "STILL DOWN after rebuild — VPN server likely rotated/unreachable; a new WireGuard config may be needed"
echo "down: needs new wireguard config" > "$STATUS"
exit 1
