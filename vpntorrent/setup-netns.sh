#!/bin/bash
# Creates the vpntorrent network namespace with a veth bridge to the main namespace.
# TorrServer runs inside this netns. Outbound torrent traffic goes via wg0 (the WireGuard VPN).
# Kill switch: if wg0 is down, vpntorrent has no default route -> torrent traffic blocked.

set -e
NETNS=vpntorrent
VETH_HOST=veth-host
VETH_VPN=veth-vpn
HOST_IP=10.42.0.1/30
VPN_IP=10.42.0.2/30
WG_CONF="${VPNTORRENT_CONFIG_DIR:-/var/lib/jellyfreedom/vpnconfigs}/wg0-vpntorrent.conf"

# Idempotent
if ip netns list | grep -q "^${NETNS}"; then
    echo "netns ${NETNS} already exists"
else
    ip netns add ${NETNS}
    echo "created netns ${NETNS}"
fi

ip netns exec ${NETNS} ip link set lo up

# Recreate the veth pair cleanly. A deleted netns (or a partial prior run) can leave an
# orphaned veth-host whose peer is gone — the old "if not exists" check would then skip
# creation and the later veth-vpn setup would fail ("Cannot find device veth-vpn").
# Delete-first is robust and idempotent (setup only runs when the netns is being (re)built).
ip link del ${VETH_HOST} 2>/dev/null || true
ip link add ${VETH_HOST} type veth peer name ${VETH_VPN}
ip link set ${VETH_VPN} netns ${NETNS}

ip addr replace ${HOST_IP} dev ${VETH_HOST} 2>/dev/null || true
ip link set ${VETH_HOST} up
ip netns exec ${NETNS} ip addr replace ${VPN_IP} dev ${VETH_VPN} 2>/dev/null || true
ip netns exec ${NETNS} ip link set ${VETH_VPN} up

sysctl -qw net.ipv4.ip_forward=1

# Disable accept_ra on the host veth so the namespace cannot inject IPv6 routes into the host
sysctl -qw net.ipv6.conf.${VETH_HOST}.accept_ra=0

# NAT: allow vpntorrent traffic through main ns (needed for WireGuard handshake packets)
iptables -t nat -C POSTROUTING -s 10.42.0.0/30 -j MASQUERADE 2>/dev/null || \
    iptables -t nat -A POSTROUTING -s 10.42.0.0/30 -j MASQUERADE

# Per-netns DNS for `ip netns exec` clients. These public resolvers exit via wg0
# (AllowedIPs 0.0.0.0/0) so queries go through whatever VPN is active — works with ANY
# provider, no real-IP leak. Without this the namespace inherits the host's 127.0.0.53 stub,
# which nothing answers inside the netns -> tracker/DHT hostname lookups fail -> zero peers.
# (Override with VPNTORRENT_DNS="1.2.3.4 5.6.7.8" to use a provider's own DNS, e.g. 10.2.0.1.)
mkdir -p /etc/netns/${NETNS}
: > /etc/netns/${NETNS}/resolv.conf
for ns in ${VPNTORRENT_DNS:-1.1.1.1 9.9.9.9}; do echo "nameserver $ns" >> /etc/netns/${NETNS}/resolv.conf; done

# Bring up WireGuard inside the netns (if config exists)
if [ -f "${WG_CONF}" ]; then
    # Derive the WireGuard endpoint IP from the conf so swapping servers only needs a new conf.
    WG_ENDPOINT=$(grep -iE '^[[:space:]]*Endpoint' "${WG_CONF}" | head -1 | sed -E 's/.*=[[:space:]]*//; s/:[0-9]+.*$//' | tr -d '[:space:]')
    if [ -z "${WG_ENDPOINT}" ]; then
        echo "ERROR: could not parse Endpoint from ${WG_CONF}"; exit 1
    fi
    if ! ip netns exec ${NETNS} ip link show wg0-vpntorrent &>/dev/null; then
        ip netns exec ${NETNS} wg-quick up ${WG_CONF}
    fi
    # Route: WireGuard endpoint via veth (so the handshake can reach the VPN endpoint through host NAT)
    ip netns exec ${NETNS} ip route replace ${WG_ENDPOINT}/32 via 10.42.0.1 dev veth-vpn
    # Kill switch: default route via wg0 (no wg0 = no internet for TorrServer)
    ip netns exec ${NETNS} ip route replace default dev wg0-vpntorrent
    echo "WireGuard up inside vpntorrent (endpoint ${WG_ENDPOINT}). Kill switch active."
else
    echo "WARNING: ${WG_CONF} not found. TorrServer will have no internet until WireGuard is configured."
fi

# IPv6 leak prevention: disable IPv6 on the veth (the only non-VPN interface in the netns).
# wg0-vpntorrent has AllowedIPs ::/0 so IPv6 routes via the tunnel when the VPN peer has
# an IPv6 address. Without that, disabling IPv6 on veth is the guarantee that no IPv6 can
# exit through the host.
ip netns exec ${NETNS} sysctl -qw net.ipv6.conf.${VETH_VPN}.disable_ipv6=1

# ip6tables kill switch: only allow IPv6 out via wg0 or loopback.
# Belt-and-suspenders in case kernel routing sends IPv6 to veth despite the sysctl above.
ip netns exec ${NETNS} ip6tables -F OUTPUT 2>/dev/null || true
ip netns exec ${NETNS} ip6tables -P OUTPUT DROP 2>/dev/null || true
ip netns exec ${NETNS} ip6tables -A OUTPUT -o lo -j ACCEPT 2>/dev/null || true
ip netns exec ${NETNS} ip6tables -A OUTPUT -o wg0-vpntorrent -j ACCEPT 2>/dev/null || true

echo "vpntorrent netns ready. 10.42.0.1 (host) <-> 10.42.0.2 (vpntorrent)"
echo "IPv6 leak prevention: veth IPv6 disabled, ip6tables OUTPUT drop-by-default."
