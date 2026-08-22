#!/bin/bash
# vpntorrent-portforward: maintain a NAT-PMP port mapping (expires every 60s) and keep
# TorrServer's PeersListenPort matching the assigned port. Applying a port change needs a
# TorrServer restart, so when the port changes mid-stream we DEFER the restart until playback
# stops (streaming is outbound/leeching and works regardless of the forwarded port; only incoming
# connectivity is briefly reduced while deferred).
#
# Port forwarding via NAT-PMP is an OPTIONAL optimization (it improves connectable seeders). It
# is provider-specific: Proton offers it at gateway 10.2.0.1. Providers without NAT-PMP simply
# never get a mapping — this keeper then backs off and idles quietly; streaming still works
# (leeching is outbound and needs no forwarded port). The system requires only a WireGuard
# tunnel, regardless of provider. Override the gateway with VPNTORRENT_PF_GATEWAY if needed.
#
# NOTE ON `set -e`: deliberately NOT used. Every loop iteration probes things that are
# allowed to fail (no NAT-PMP at the gateway, TorrServer restarting); non-zero exits are
# this script's normal control flow, and `set -e` would kill the keeper on the first one.
set -uo pipefail
NETNS=vpntorrent
GW="${VPNTORRENT_PF_GATEWAY:-10.2.0.1}"
ORCH=http://127.0.0.1:1990
STATUS=/run/vpntorrent-portforward
RUN_DIR=/run/vpntorrent

# The veth addressing is decided once, by setup-netns.sh, and published here — so overriding
# VPNTORRENT_VETH_SUBNET does not require editing this file too.
nv() { # nv <KEY> <DEFAULT>
    local v=""
    [ -r "$RUN_DIR/netns.env" ] && v="$(sed -n "s/^${1}=//p" "$RUN_DIR/netns.env" | head -1 | tr -d '[:space:]')"
    printf '%s' "${v:-$2}"
}
TS="http://$(nv VETH_VPN_IP 10.42.0.2):8090"

req_port(){
  ip netns exec "$NETNS" natpmpc -a 1 0 udp 60 -g "$GW" >/dev/null 2>&1
  ip netns exec "$NETNS" natpmpc -a 1 0 tcp 60 -g "$GW" 2>/dev/null | awk '/Mapped public port/{print $4; exit}'
}
ts_get_port(){
  curl -s --max-time 5 -X POST "$TS/settings" -d '{"action":"get"}' \
    | python3 -c "import sys,json;print(json.load(sys.stdin).get('PeersListenPort',0))" 2>/dev/null
}
ts_set_port(){
  curl -s --max-time 5 -X POST "$TS/settings" -d '{"action":"get"}' \
    | python3 -c "import sys,json;s=json.load(sys.stdin);s['PeersListenPort']=int('$1');print(json.dumps({'action':'set','sets':s}))" \
    | curl -s --max-time 5 -X POST "$TS/settings" -d @- >/dev/null
}
playback_active(){ curl -s --max-time 4 "$ORCH/api/playback/active" 2>/dev/null | grep -q '"active":true'; }

LAST="$(ts_get_port)"
fails=0
while true; do
  PORT="$(req_port)"
  # Only ever let a plain number through: PORT is parsed out of natpmpc's stdout and is
  # interpolated into a shell command and a python snippet below.
  case "$PORT" in ''|*[!0-9]*) PORT="" ;; esac
  if [ -z "$PORT" ]; then
    fails=$((fails+1))
    if [ "$fails" -le 3 ]; then
      echo "fail" > "$STATUS"
      logger -t vpntorrent-portforward "NAT-PMP request failed (try $fails); retrying"
      sleep 20
    else
      # Provider has no NAT-PMP (or it's down). Back off and idle quietly — P2P still works
      # outbound; this only forgoes the inbound-seeder optimization.
      [ "$fails" -eq 4 ] && logger -t vpntorrent-portforward "no NAT-PMP at $GW — port forwarding unavailable (fine for providers without it); backing off"
      echo "unavailable (no NAT-PMP)" > "$STATUS"
      sleep 600
    fi
    continue
  fi
  fails=0
  if [ "$PORT" != "$LAST" ]; then
    if playback_active; then
      # Defer: applying needs a TorrServer restart that would kill the stream. Leave LAST as-is
      # so we re-evaluate next cycle and apply once idle. Streaming continues (outbound) meanwhile.
      logger -t vpntorrent-portforward "forwarded port changed to $PORT but a stream is live — deferring restart"
      echo "deferred current=$LAST new=$PORT" > "$STATUS"
    else
      ts_set_port "$PORT"
      systemctl restart torrserver-netns.service
      logger -t vpntorrent-portforward "forwarded port -> $PORT; updated TorrServer PeersListenPort + restarted"
      LAST="$PORT"
      echo "ok port=$PORT" > "$STATUS"
    fi
  else
    echo "ok port=$PORT" > "$STATUS"
  fi
  sleep 45
done
