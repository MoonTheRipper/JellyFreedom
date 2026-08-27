package main

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mk(name string, flags net.Flags, prefixes ...string) link {
	l := link{name: name, flags: flags}
	for _, p := range prefixes {
		l.prefixes = append(l.prefixes, netip.MustParsePrefix(p))
	}
	return l
}

const (
	up   = net.FlagUp
	loop = net.FlagUp | net.FlagLoopback
	p2p  = net.FlagUp | net.FlagPointToPoint
)

// The production shape: inside `vpntorrent` there is loopback, the WireGuard tunnel, and
// the veth to the host. Exactly one of those is a host link.
func TestPickHostLinkInsideTheNamespace(t *testing.T) {
	got, err := pickHostLink([]link{
		mk("lo", loop, "127.0.0.1/8"),
		mk("wg0-vpntorrent", p2p, "10.2.0.2/32"),
		mk("veth-vpn", up, "10.42.0.2/30"),
	})
	if err != nil {
		t.Fatalf("pickHostLink: %v", err)
	}
	if got.String() != "10.42.0.2/30" {
		t.Fatalf("picked %s, want 10.42.0.2/30", got)
	}
	// And the two values derived from it, which are what the proxy actually binds and
	// trusts: its own address, and the link subnet — one other host on a /30.
	if a := net.JoinHostPort(got.Addr().String(), defaultProxyPort); a != "10.42.0.2:1080" {
		t.Errorf("listen address = %s", a)
	}
	if got.Masked().String() != "10.42.0.0/30" {
		t.Errorf("allow-list = %s", got.Masked())
	}
}

// An operator who overrides VPNTORRENT_VETH_SUBNET is followed automatically — that is the
// reason this derives the addressing rather than hardcoding it.
func TestPickHostLinkFollowsACustomSubnet(t *testing.T) {
	got, err := pickHostLink([]link{
		mk("lo", loop, "127.0.0.1/8"),
		mk("wg0-vpntorrent", p2p, "10.2.0.2/32"),
		mk("veth-vpn", up, "172.20.5.2/30"),
	})
	if err != nil || got.String() != "172.20.5.2/30" {
		t.Fatalf("got %s, %v", got, err)
	}
}

// The tunnel must never be chosen: binding an unauthenticated proxy there would expose it
// to the VPN provider's network. It is excluded by role, so it is excluded whether it is
// recognised by name or by being point-to-point.
func TestTheTunnelIsNeverAHostLink(t *testing.T) {
	if _, err := pickHostLink([]link{
		mk("lo", loop, "127.0.0.1/8"),
		mk("wg0-vpntorrent", p2p, "10.2.0.2/32"),
	}); err == nil {
		t.Fatal("the tunnel was accepted as a host link")
	}
	// Same interface without the point-to-point flag: the name still rules it out.
	if _, err := pickHostLink([]link{
		mk("lo", loop, "127.0.0.1/8"),
		mk("wg0-vpntorrent", up, "10.2.0.2/32"),
	}); err == nil {
		t.Fatal("the tunnel was accepted when it was not flagged point-to-point")
	}
}

// Ambiguity must refuse, not guess. This is the HOST namespace's shape — a LAN interface
// and the other end of the veth — and it is what the binary actually reports if the unit
// is ever started outside the namespace.
func TestAmbiguityRefusesRatherThanGuessing(t *testing.T) {
	_, err := pickHostLink([]link{
		mk("lo", loop, "127.0.0.1/8"),
		mk("enp2s0", up, "192.168.178.2/24"),
		mk("veth-host", up, "10.42.0.1/30"),
	})
	if err == nil {
		t.Fatal("two candidates were resolved to one — it could bind the proxy to the LAN")
	}
	if !strings.Contains(err.Error(), "--listen") {
		t.Errorf("the error does not say how to resolve it: %v", err)
	}
}

func TestDownAndIPv6OnlyInterfacesAreIgnored(t *testing.T) {
	got, err := pickHostLink([]link{
		mk("lo", loop, "127.0.0.1/8"),
		mk("veth-old", 0, "10.42.0.9/30"),                // down: a leftover from a previous run
		mk("veth-vpn", up, "fe80::1/64", "10.42.0.2/30"), // IPv6 is not a candidate
	})
	if err != nil || got.String() != "10.42.0.2/30" {
		t.Fatalf("got %s, %v", got, err)
	}
}

// netns.env is published 0644 but sits in a 0700 root directory, so the service user
// cannot read it. The parser must therefore treat an unreadable file as "no information"
// rather than as an error — the interface fallback is what actually runs in production.
func TestReadNetnsEnvTolerAtesAMissingFile(t *testing.T) {
	if got := readNetnsEnv(filepath.Join(t.TempDir(), "nope.env")); len(got) != 0 {
		t.Fatalf("got %v, want an empty map", got)
	}
}

func TestReadNetnsEnvParsesWhatSetupNetnsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "netns.env")
	// Byte for byte the shape setup-netns.sh emits.
	body := "NETNS=vpntorrent\nVETH_SUBNET=10.42.0.0/30\nVETH_HOST_IP=10.42.0.1\nVETH_VPN_IP=10.42.0.2\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readNetnsEnv(path)
	for k, want := range map[string]string{
		"NETNS": "vpntorrent", "VETH_SUBNET": "10.42.0.0/30",
		"VETH_HOST_IP": "10.42.0.1", "VETH_VPN_IP": "10.42.0.2",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}
