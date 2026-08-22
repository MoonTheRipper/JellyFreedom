#!/bin/bash
# Creates the vpntorrent network namespace with a veth bridge to the main namespace.
# TorrServer runs inside this netns. Outbound torrent traffic goes via the WireGuard tunnel.
#
# Kill switch, in two layers:
#   1. Routing — the only default route inside the namespace is the tunnel. No tunnel, no
#      route, no traffic.
#   2. Filtering — an OUTPUT DROP policy that permits loopback, the tunnel, the point-to-point
#      link to the host, and the WireGuard handshake. Nothing else. Layer 1 alone is only as
#      good as the routing table: a stray `Table =` directive, a second wg-quick, or a plain
#      bug could put a default route on the veth, and everything would quietly leave in the
#      clear through the host's NAT. Layer 2 makes "fail closed" a rule instead of an
#      emergent property of a missing route.
#
# This script is idempotent: running it twice is a no-op, not an error. It is also written to
# SUCCEED whenever the namespace itself is healthy — a missing or broken VPN config is a loud
# warning, not a unit failure, because the namespace is fail-closed either way and failing the
# unit would stop TorrServer (Requires=) and take the dashboard's repair path down with it.

set -euo pipefail

NETNS=vpntorrent
VETH_HOST=veth-host
VETH_VPN=veth-vpn
RUN_DIR=/run/vpntorrent

# The point-to-point link between the host and the namespace. Overridable so a host that
# already uses 10.42.0.0/30 can move it; every consumer (watchdog, port-forward keeper, the
# privileged helper) reads the resolved values back out of $RUN_DIR/netns.env, so there is
# exactly one place this is decided.
# NOTE: if you change this you must also update torrserver.url in
# /etc/jellyfreedom/config.yaml, which points at the namespace's address.
VETH_SUBNET="${VPNTORRENT_VETH_SUBNET:-10.42.0.0/30}"

# The privileged helper is the single implementation of config sanitisation, tunnel bring-up
# and the endpoint firewall rule. Prefer the copy next to this script (repo checkout), fall
# back to the installed location.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELPER="$SCRIPT_DIR/jf-netns-helper"
[ -x "$HELPER" ] || HELPER=/opt/vpntorrent/jf-netns-helper

warn() { printf 'setup-netns: WARNING: %s\n' "$*" >&2; }
die()  { printf 'setup-netns: ERROR: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Derive the two host/namespace addresses from the subnet, in 32-bit arithmetic so an
# octet-crossing base (10.42.0.252/30) still works.
# ---------------------------------------------------------------------------
ip_to_int() {
    local a b c d IFS=.
    read -r a b c d <<<"$1"
    # 10# forces base-10: an octet written as 08 would otherwise be read as invalid octal.
    printf '%s' "$(( (10#$a << 24) + (10#$b << 16) + (10#$c << 8) + 10#$d ))"
}
int_to_ip() {
    local n="$1"
    printf '%d.%d.%d.%d' "$(( (n >> 24) & 255 ))" "$(( (n >> 16) & 255 ))" "$(( (n >> 8) & 255 ))" "$(( n & 255 ))"
}

VETH_BASE="${VETH_SUBNET%%/*}"
VETH_PREFIX="${VETH_SUBNET##*/}"
case "$VETH_BASE" in
    [0-9]*.[0-9]*.[0-9]*.[0-9]*) ;;
    *) die "VPNTORRENT_VETH_SUBNET must be an IPv4 CIDR like 10.42.0.0/30, got '$VETH_SUBNET'" ;;
esac
case "$VETH_PREFIX" in
    ''|*[!0-9]*) die "VPNTORRENT_VETH_SUBNET is missing a prefix length (e.g. /30), got '$VETH_SUBNET'" ;;
esac
VETH_BASE_INT="$(ip_to_int "$VETH_BASE")"
VETH_HOST_IP="$(int_to_ip "$(( VETH_BASE_INT + 1 ))")"
VETH_VPN_IP="$(int_to_ip "$(( VETH_BASE_INT + 2 ))")"

# ---------------------------------------------------------------------------
# Namespace + veth pair
# ---------------------------------------------------------------------------
# Create-or-accept-existing, rather than `ip netns list | grep`: under `set -o pipefail` a
# `grep -q` that matches early can SIGPIPE its upstream, and the whole pipeline then reports
# failure even though the namespace IS there — which would send us down the "create it" path
# and abort on "File exists".
if ip netns add "$NETNS" 2>/dev/null; then
    echo "created netns ${NETNS}"
elif [ -e "/var/run/netns/${NETNS}" ]; then
    echo "netns ${NETNS} already exists"
else
    die "could not create the ${NETNS} namespace. Is this kernel built with network namespaces (CONFIG_NET_NS)?"
fi

ip netns exec "$NETNS" ip link set lo up || die "could not bring up loopback inside ${NETNS}"

# Recreate the veth pair cleanly. A deleted netns (or a partial prior run) can leave an
# orphaned veth-host whose peer is gone — an "if not exists" check would then skip creation
# and the later veth-vpn setup would fail ("Cannot find device veth-vpn"). Delete-first is
# robust and idempotent. Deleting one end deletes the pair, including the end inside the
# namespace; the extra deletes below clean up the half-moved states a crashed run can leave.
ip link del "$VETH_HOST" 2>/dev/null || true
ip link del "$VETH_VPN" 2>/dev/null || true
ip netns exec "$NETNS" ip link del "$VETH_VPN" 2>/dev/null || true

ip link add "$VETH_HOST" type veth peer name "$VETH_VPN" \
    || die "could not create the ${VETH_HOST}/${VETH_VPN} veth pair (is the 'veth' kernel module available?)"
ip link set "$VETH_VPN" netns "$NETNS" || die "could not move ${VETH_VPN} into the ${NETNS} namespace"

ip addr replace "${VETH_HOST_IP}/${VETH_PREFIX}" dev "$VETH_HOST" || die "could not address ${VETH_HOST}"
ip link set "$VETH_HOST" up || die "could not bring up ${VETH_HOST}"
ip netns exec "$NETNS" ip addr replace "${VETH_VPN_IP}/${VETH_PREFIX}" dev "$VETH_VPN" || die "could not address ${VETH_VPN}"
ip netns exec "$NETNS" ip link set "$VETH_VPN" up || die "could not bring up ${VETH_VPN}"

# Publish the resolved addressing for the watchdog, the port-forward keeper and the helper,
# so nothing has to re-derive (or hardcode) it.
install -d -o root -g root -m 0700 "$RUN_DIR"
{
    echo "NETNS=${NETNS}"
    echo "VETH_SUBNET=${VETH_SUBNET}"
    echo "VETH_HOST_IP=${VETH_HOST_IP}"
    echo "VETH_VPN_IP=${VETH_VPN_IP}"
} > "$RUN_DIR/netns.env"
chmod 0644 "$RUN_DIR/netns.env"

# ---------------------------------------------------------------------------
# Host side: forwarding + NAT so the WireGuard handshake can reach the VPN server
# ---------------------------------------------------------------------------
sysctl -qw net.ipv4.ip_forward=1 \
    || warn "could not enable net.ipv4.ip_forward — the WireGuard handshake will not get out of the namespace. In a container? Grant CAP_SYS_ADMIN or set it on the host."

iptables -w 5 -t nat -C POSTROUTING -s "$VETH_SUBNET" -j MASQUERADE 2>/dev/null || \
    iptables -w 5 -t nat -A POSTROUTING -s "$VETH_SUBNET" -j MASQUERADE || \
    warn "could not install the MASQUERADE rule for ${VETH_SUBNET}; the WireGuard handshake will not reach the VPN server"

# IPv6 leak prevention. Guarded as a whole: on a host booted with ipv6.disable=1 none of
# /proc/sys/net/ipv6 exists, every sysctl below returns non-zero, and under `set -e` this
# script would abort — failing vpntorrent-netns.service and, through Requires=, TorrServer
# too. There is also nothing to protect against on such a host.
if [ -d /proc/sys/net/ipv6 ]; then
    # accept_ra=0 on the host veth so the namespace cannot inject IPv6 routes into the host.
    sysctl -qw "net.ipv6.conf.${VETH_HOST}.accept_ra=0" \
        || warn "could not set accept_ra=0 on ${VETH_HOST}"

    # Disable IPv6 on the veth — the only non-VPN interface in the namespace. The tunnel
    # carries ::/0 when the peer has an IPv6 address; this is the guarantee that no IPv6
    # can exit through the host instead.
    ip netns exec "$NETNS" sysctl -qw "net.ipv6.conf.${VETH_VPN}.disable_ipv6=1" \
        || warn "could not disable IPv6 on ${VETH_VPN} inside the namespace"

    # ip6tables backstop: only loopback and the tunnel may emit IPv6.
    ip netns exec "$NETNS" ip6tables -w 5 -F OUTPUT 2>/dev/null || true
    ip netns exec "$NETNS" ip6tables -w 5 -P OUTPUT DROP 2>/dev/null || true
    ip netns exec "$NETNS" ip6tables -w 5 -A OUTPUT -o lo -j ACCEPT 2>/dev/null || true
    ip netns exec "$NETNS" ip6tables -w 5 -A OUTPUT -o wg0-vpntorrent -j ACCEPT 2>/dev/null || true
    IPV6_NOTE="IPv6 leak prevention: veth IPv6 disabled, ip6tables OUTPUT drop-by-default."
else
    IPV6_NOTE="IPv6 leak prevention: not needed (this host booted without IPv6)."
fi

# ---------------------------------------------------------------------------
# Per-netns DNS for `ip netns exec` clients. These public resolvers exit via the tunnel
# (AllowedIPs 0.0.0.0/0) so queries go through whatever VPN is active — works with ANY
# provider, no real-IP leak. Without this the namespace inherits the host's 127.0.0.53 stub,
# which nothing answers inside the netns -> tracker/DHT hostname lookups fail -> zero peers.
# (Override with VPNTORRENT_DNS="1.2.3.4 5.6.7.8" to use a provider's own DNS, e.g. 10.2.0.1.)
# ---------------------------------------------------------------------------
mkdir -p "/etc/netns/${NETNS}"
: > "/etc/netns/${NETNS}/resolv.conf"
# shellcheck disable=SC2086 # word splitting is the point: VPNTORRENT_DNS is a space-separated list
for ns in ${VPNTORRENT_DNS:-1.1.1.1 9.9.9.9}; do
    echo "nameserver $ns" >> "/etc/netns/${NETNS}/resolv.conf"
done

# ---------------------------------------------------------------------------
# v4 filter backstop (layer 2 of the kill switch).
#
# Applied BEFORE the tunnel comes up and regardless of whether a config exists, so a fresh
# install with no VPN config yet is closed by rule and not merely by the absence of a route.
# ACCEPT rules go in first and the DROP policy last, so a re-run never opens a window where
# a live stream's packets are dropped.
# ---------------------------------------------------------------------------
ipt() { ip netns exec "$NETNS" iptables -w 5 "$@"; }
ipt_ensure() {  # append the rule only if an identical one is not already there
    ipt -C OUTPUT "$@" 2>/dev/null || ipt -A OUTPUT "$@"
}

if ipt -S OUTPUT >/dev/null 2>&1; then
    # lo: TorrServer's own internal traffic.
    ipt_ensure -o lo -j ACCEPT || warn "could not allow loopback in the namespace"
    # The tunnel: everything that is supposed to leave.
    ipt_ensure -o wg0-vpntorrent -j ACCEPT || warn "could not allow the tunnel interface"
    # The point-to-point link to the host: this is how the dashboard and Jellyfin reach
    # TorrServer's HTTP API. It terminates on the host, so it is not an exit path.
    ipt_ensure -o "$VETH_VPN" -d "$VETH_SUBNET" -j ACCEPT || warn "could not allow host<->namespace traffic"
    # Replies to connections opened FROM the host (e.g. a DNAT'd stream). A reply is not a
    # leak — the namespace never initiates it. Namespace-initiated traffic out the veth is
    # still dropped, which is the only thing we actually care about.
    ipt_ensure -o "$VETH_VPN" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT \
        || warn "could not install the conntrack reply rule (is xt_conntrack available?); host-initiated connections into the namespace may stall"
    # Everything else stays in.
    ipt -P OUTPUT DROP || warn "could not set the OUTPUT DROP policy; only the routing kill switch is active"
    V4_NOTE="v4 kill switch: OUTPUT DROP, allowing lo + tunnel + ${VETH_SUBNET} + the WireGuard handshake."
else
    warn "iptables is not usable inside the namespace; the v4 filter backstop is NOT active. Only the routing kill switch protects you. Install the 'iptables' package and restart vpntorrent-netns.service."
    V4_NOTE="v4 kill switch: routing only (iptables unavailable)."
fi

# ---------------------------------------------------------------------------
# Tunnel. Everything privileged and config-derived lives in the helper: it is the ONE place
# that sanitises the config before root's wg-quick parses it, so there is no second
# implementation to drift out of sync.
# ---------------------------------------------------------------------------
[ -x "$HELPER" ] || die "privileged helper not found at ${HELPER}. Reinstall JellyFreedom (release/install.sh installs it as root:root 0755)."

set +e
"$HELPER" vpn-up
helper_rc=$?
set -e
case "$helper_rc" in
    0) echo "WireGuard up inside ${NETNS}. Kill switch active." ;;
    3) warn "no WireGuard config activated yet. The namespace is up and fail-closed: TorrServer has no internet until you upload and activate a config in the dashboard." ;;
    *) warn "the VPN did not come up (helper exit ${helper_rc}; see the message above). The namespace is up and fail-closed — no traffic can leak while it is down. Fix the config in the dashboard, then: systemctl restart vpntorrent-netns.service" ;;
esac

echo "vpntorrent netns ready. ${VETH_HOST_IP} (host) <-> ${VETH_VPN_IP} (vpntorrent)"
echo "$V4_NOTE"
echo "$IPV6_NOTE"
