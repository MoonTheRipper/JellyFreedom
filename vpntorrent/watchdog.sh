#!/bin/bash
# vpntorrent-watchdog: keep the torrent path healthy without manual intervention, and WITHOUT
# needlessly interrupting an active stream.
#  (1) Re-assert the host NAT/forwarding the tunnel depends on (heals an external iptables flush).
#  (2) TorrServer BT client: a crash leaves /echo working but `add` returning 500. Restart only
#      after TWO consecutive failures (ignore transient blips), and skip the probe entirely while
#      a stream is live (a live stream already proves the client works — no churn, no false restart).
#  (3) VPN data path: probe two independent targets; on a real outage re-establish the tunnel.
set -u
NETNS=vpntorrent
WG=wg0-vpntorrent
CONF=/etc/wireguard/wg0-vpntorrent.conf
TS=http://10.42.0.2:8090
ORCH=http://127.0.0.1:1990
HC_HASH=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
STATUS=/run/vpntorrent-status
TS_STRIKES=/run/vpntorrent-ts-strikes

endpoint(){ grep -iE '^[[:space:]]*Endpoint' "$CONF" | head -1 | sed -E 's/.*=[[:space:]]*//; s/:[0-9]+.*//' | tr -d '[:space:]'; }
playback_active(){ curl -s --max-time 4 "$ORCH/api/playback/active" 2>/dev/null | grep -q '"active":true'; }
probe(){
  ip netns exec "$NETNS" curl -s --max-time 8 -o /dev/null https://1.1.1.1/cdn-cgi/trace && return 0
  ip netns exec "$NETNS" curl -s --max-time 8 -o /dev/null https://1.0.0.1/cdn-cgi/trace && return 0
  return 1
}

# (1) Heal the host NAT/forwarding the WG handshake rides on — an external flush (ufw reload,
# docker install, firewall reset) would otherwise silently break the tunnel until a netns restart.
sysctl -qw net.ipv4.ip_forward=1 2>/dev/null || true
iptables -t nat -C POSTROUTING -s 10.42.0.0/30 -j MASQUERADE 2>/dev/null || \
  iptables -t nat -A POSTROUTING -s 10.42.0.0/30 -j MASQUERADE 2>/dev/null || true

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
if probe; then echo "ok" > "$STATUS"; exit 0; fi
sleep 5
if probe; then echo "ok" > "$STATUS"; exit 0; fi

logger -t vpntorrent-watchdog "tunnel has no data path; bouncing WireGuard"
echo "down: bouncing wg" > "$STATUS"
EP="$(endpoint)"
ip netns exec "$NETNS" wg-quick down "$CONF" 2>/dev/null || true
ip netns exec "$NETNS" wg-quick up "$CONF" 2>/dev/null || true
[ -n "$EP" ] && ip netns exec "$NETNS" ip route replace "${EP}/32" via 10.42.0.1 dev veth-vpn 2>/dev/null || true
ip netns exec "$NETNS" ip route replace default dev "$WG" 2>/dev/null || true
sleep 8
if probe; then logger -t vpntorrent-watchdog "recovered via wg bounce"; echo "ok" > "$STATUS"; exit 0; fi

logger -t vpntorrent-watchdog "wg bounce failed; full netns rebuild"
systemctl stop torrserver-netns.service 2>/dev/null || true
systemctl restart vpntorrent-netns.service 2>/dev/null || true
systemctl start torrserver-netns.service 2>/dev/null || true
systemctl restart vpntorrent-portforward.service 2>/dev/null || true   # was orphaned by the netns restart
sleep 10
if probe; then logger -t vpntorrent-watchdog "recovered via full rebuild"; echo "ok" > "$STATUS"; exit 0; fi

logger -t vpntorrent-watchdog "STILL DOWN after rebuild — VPN server likely rotated/unreachable; a new WireGuard config may be needed"
echo "down: needs new wireguard config" > "$STATUS"
exit 1
