package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
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

	env := readNetnsEnv(netnsEnvPath)
	listenAddr := *listen
	if listenAddr == "" {
		ip := env["VETH_VPN_IP"]
		if ip == "" {
			// Guessing here would be worse than failing. Binding 0.0.0.0 as a fallback
			// would put an unauthenticated proxy on the tunnel interface, reachable by
			// the VPN provider's network — the one place this must never listen.
			fmt.Fprintf(os.Stderr, "netns-proxy: no VETH_VPN_IP in %s and no --listen given.\n"+
				"Start vpntorrent-netns.service first, or pass --listen explicitly.\n", netnsEnvPath)
			return 1
		}
		listenAddr = net.JoinHostPort(ip, defaultProxyPort)
	}
	allowed := splitList(*allow)
	if len(allowed) == 0 {
		if ip := env["VETH_HOST_IP"]; ip != "" {
			allowed = []string{ip}
		} else {
			fmt.Fprintf(os.Stderr, "netns-proxy: no VETH_HOST_IP in %s and no --allow-from given.\n"+
				"Refusing to start an unauthenticated proxy with no client allow-list.\n", netnsEnvPath)
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

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
