package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"jellyfreedom/internal/netnsproxy"
)

// netnsEnvPath is written by vpntorrent/setup-netns.sh every time the namespace is
// created. Reading the addressing back out of it is what keeps this from hardcoding
// 10.42.0.0/30 — an operator who needs a different subnet sets VPNTORRENT_VETH_SUBNET
// once and every consumer follows, this one included.
const netnsEnvPath = "/run/vpntorrent/netns.env"

// defaultProxyPort is the SOCKS port inside the namespace. 1080 is the registered SOCKS
// port and there is nothing to collide with: the namespace contains TorrServer on 8090
// and this, and nothing else.
const defaultProxyPort = "1080"

// runNetnsProxy is the `orchestrator netns-proxy` subcommand — the in-namespace half of
// internal/netnsproxy.
//
// It is a subcommand of the same binary rather than a second program because the project
// deploys as one static Go binary; a separate executable would be a second thing to
// build, ship, version and keep in step. systemd starts it with NetworkNamespacePath=,
// exactly as it starts TorrServer, so this process is inside the tunnel and behind the
// kill switch without ever holding a privilege of its own.
func runNetnsProxy(args []string) int {
	fs := flag.NewFlagSet("netns-proxy", flag.ExitOnError)
	listen := fs.String("listen", "", "address to listen on (default: the namespace's veth address from "+netnsEnvPath+", port "+defaultProxyPort+")")
	allow := fs.String("allow-from", "", "comma-separated client addresses permitted to use the proxy (default: the host's veth address)")
	fs.Parse(args)

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// Where the addressing comes from, in order. Flags win; then the file
	// setup-netns.sh publishes; then the veth interface itself.
	//
	// The third source is the one that actually runs in production, and it is here
	// because of a permission fact the file's own mode hides: setup-netns.sh chmods
	// netns.env to 0644, but /run/vpntorrent is 0700 root:root — it also holds the
	// sanitised WireGuard config, private key and all — so a service user cannot
	// traverse into the directory to reach the file however readable the file is. The
	// alternative was loosening that directory, which is not a trade worth making to
	// avoid reading an interface address.
	//
	// Deriving it is arguably the better source anyway: this process is INSIDE the
	// namespace, so the veth is right there, and an operator who overrides
	// VPNTORRENT_VETH_SUBNET is followed automatically with nothing to keep in sync.
	env := readNetnsEnv(netnsEnvPath)
	vethIP, vethSubnet := env["VETH_VPN_IP"], env["VETH_SUBNET"]
	if vethIP == "" || vethSubnet == "" {
		if p, err := vethPrefix(); err == nil {
			if vethIP == "" {
				vethIP = p.Addr().String()
			}
			if vethSubnet == "" {
				vethSubnet = p.Masked().String()
			}
		} else {
			slog.Warn("netns-proxy: could not determine the veth addressing", "err", err)
		}
	}

	listenAddr := *listen
	if listenAddr == "" {
		if vethIP == "" {
			// Guessing here would be worse than failing. Binding 0.0.0.0 as a fallback
			// would put an unauthenticated proxy on the tunnel interface, reachable by
			// the VPN provider's network — the one place this must never listen.
			fmt.Fprintf(os.Stderr, "netns-proxy: could not find the namespace's veth address, and no --listen given.\n"+
				"Start vpntorrent-netns.service first, or pass --listen explicitly.\n")
			return 1
		}
		listenAddr = net.JoinHostPort(vethIP, defaultProxyPort)
	}
	allowed := splitList(*allow)
	if len(allowed) == 0 {
		// The whole veth subnet, which on the default /30 is exactly one other host: the
		// host side of the link. It is the same rule the namespace's kill switch uses for
		// traffic in the other direction, so the two cannot drift apart.
		if vethSubnet != "" {
			allowed = []string{vethSubnet}
		} else {
			fmt.Fprintf(os.Stderr, "netns-proxy: could not determine the veth subnet, and no --allow-from given.\n"+
				"Refusing to start an unauthenticated proxy with no client allow-list.\n")
			return 1
		}
	}

	srv, err := netnsproxy.New(allowed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "netns-proxy: %v\n", err)
		return 1
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "netns-proxy: cannot listen on %s: %v\n", listenAddr, err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("netns-proxy listening", "addr", listenAddr, "allow_from", allowed, "version", version)
	if err := srv.Serve(ctx, ln); err != nil {
		slog.Error("netns-proxy stopped", "err", err)
		return 1
	}
	return 0
}

// readNetnsEnv parses the KEY=value file setup-netns.sh publishes. A missing file is not
// an error here — the caller decides what to do about a missing key, and its message is
// more useful than one from this level.
func readNetnsEnv(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// link is one interface, reduced to the facts pickHostLink needs. It exists so the
// selection can be tested without a network namespace — which on Ubuntu 24.04 and later an
// unprivileged process cannot create at all, AppArmor having restricted unprivileged user
// namespaces (the same restriction behind decision D19).
type link struct {
	name     string
	flags    net.Flags
	prefixes []netip.Prefix
}

// vethPrefix finds this namespace's end of the host link.
func vethPrefix() (netip.Prefix, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("listing interfaces: %w", err)
	}
	links := make([]link, 0, len(ifaces))
	for _, ifc := range ifaces {
		l := link{name: ifc.Name, flags: ifc.Flags}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			ones, _ := n.Mask.Size()
			l.prefixes = append(l.prefixes, netip.PrefixFrom(ip.Unmap(), ones))
		}
		links = append(links, l)
	}
	return pickHostLink(links)
}

// pickHostLink returns the ONE interface that is neither loopback nor the tunnel. Inside
// `vpntorrent` that is the veth and nothing else.
//
// Matching by ROLE rather than by the name "veth-vpn" keeps it working if the link is ever
// renamed. Refusing when there is more than one candidate is equally deliberate: a
// namespace with two host links is not the topology this assumes, and picking one
// arbitrarily could bind an unauthenticated proxy to the wrong interface.
func pickHostLink(links []link) (netip.Prefix, error) {
	var found []netip.Prefix
	for _, l := range links {
		if l.flags&net.FlagLoopback != 0 || l.flags&net.FlagUp == 0 {
			continue
		}
		// The tunnel is not a host link, and binding a proxy to it would expose it to the
		// VPN provider's network — the one place this must never listen.
		if strings.HasPrefix(l.name, "wg") || l.flags&net.FlagPointToPoint != 0 {
			continue
		}
		for _, p := range l.prefixes {
			// IPv4 only: setup-netns.sh disables IPv6 on the veth outright.
			if p.Addr().Is4() {
				found = append(found, p)
			}
		}
	}
	switch len(found) {
	case 0:
		return netip.Prefix{}, errors.New("no non-loopback, non-tunnel IPv4 interface in this namespace")
	case 1:
		return found[0], nil
	default:
		return netip.Prefix{}, fmt.Errorf("%d candidate host links in this namespace; pass --listen and --allow-from explicitly", len(found))
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
