package api

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ── Privileged operations ─────────────────────────────────────────────────────
//
// Everything that needs root goes through ONE root-owned helper binary with a CLOSED
// verb set and NO free-form arguments (see coord/privilege-contract.md).
//
// What this replaces: the service user previously held
//     NOPASSWD: /usr/sbin/ip netns exec vpntorrent /usr/bin/curl *
//     NOPASSWD: /usr/sbin/ip netns exec vpntorrent wg show *
// `curl *` as root is arbitrary root file write (`-o /etc/sudoers.d/x`) and arbitrary
// root file read (`file:///etc/shadow`), so any RCE in this process was instantly host
// root. `wg show <if> private-key` also leaks the tunnel's private key.
//
// Fixed-argument sudoers rules were impossible while the Go side passed a dynamic
// --resolve value and a dynamic service name; the helper removes both.

// helperPath is the root-owned helper. Root-owned directory, root:root 0755 — the
// service user must not be able to modify it.
const helperPath = "/opt/vpntorrent/jf-netns-helper"

// systemctlPath is deliberately absolute AND deliberately /usr/bin.
//
// sudo matches the RESOLVED command path and does NOT follow the merged-usr
// /bin -> /usr/bin symlink when matching a sudoers rule. A rule written for
// /bin/systemctl therefore never matches and every restart is denied, while a bare
// "systemctl" resolves through sudo's secure_path and may not match either.
const systemctlPath = "/usr/bin/systemctl"

// Helper verbs — the complete set. Anything not here cannot be run as root.
const (
	verbStatus    = "status"    // wg show, private/preshared keys filtered out
	verbExitIP    = "exit-ip"   // resolves + fetches the exit IP inside the netns; prints the IP only
	verbLeakCheck = "leakcheck" // ip6tables OUTPUT policy + v4 default route
	verbVPNUp     = "vpn-up"    // sanitises the active conf, then wg-quick up (fixed path, no argument)
	verbVPNDown   = "vpn-down"  // wg-quick down
	verbRoutes    = "routes"    // ip route show / ip link show inside the netns
)

// restartableUnits is a HARDCODED allowlist. A unit name must never be interpolated
// from a request path into a privileged command.
//
// jellyfreedom.service is intentionally absent: a service must not be able to bounce
// itself through root. The orchestrator restarts itself by exiting non-zero and letting
// systemd's Restart=on-failure do it.
var restartableUnits = map[string]string{
	"jellyfin":         "jellyfin.service",
	"torrserver-netns": "torrserver-netns.service",
	"vpntorrent-netns": "vpntorrent-netns.service",
	"prowlarr":         "prowlarr.service",
	"flaresolverr":     "flaresolverr.service",
}

// RestartableUnit resolves a UI service name to its allowlisted unit, or "" if the
// name is not restartable.
func RestartableUnit(name string) string { return restartableUnits[name] }

// ErrNoVPNConfig is vpn-up's exit code 3: no config has been activated yet. That is a
// neutral "not configured" state for the UI, not an error to alarm the user with.
var ErrNoVPNConfig = errors.New("no VPN config has been activated yet")

// runHelper executes one helper verb under sudo and returns its combined output.
//
// verb is validated against the closed set; NO other argument is ever passed — sudoers
// grants exactly `<helper> <verb>` and would deny anything more. The helper's contract:
// exit 0 = success, non-zero = failure with a human-readable reason on stderr, which is
// why CombinedOutput is used rather than Output.
func runHelper(ctx context.Context, verb string) (string, error) {
	switch verb {
	case verbStatus, verbExitIP, verbLeakCheck, verbVPNUp, verbVPNDown, verbRoutes:
	default:
		return "", fmt.Errorf("refusing unknown privileged verb %q", verb)
	}
	cmd := exec.CommandContext(ctx, "sudo", "-n", helperPath, verb)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == exitNoVPNConfig {
			return string(out), ErrNoVPNConfig
		}
		return string(out), fmt.Errorf("%s %s: %w: %s", helperPath, verb, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// exitNoVPNConfig is the helper's reserved exit code for "no config activated yet".
const exitNoVPNConfig = 3

// ParseKeyValues parses the helper's stable `key=value` output (one pair per line).
// Unknown keys are kept rather than rejected, so the helper can add new ones without
// breaking this side.
func ParseKeyValues(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		m[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return m
}

// LeakCheck runs the leakcheck verb and returns its parsed key=value report.
func LeakCheck(ctx context.Context) (map[string]string, error) {
	out, err := runHelper(ctx, verbLeakCheck)
	if err != nil {
		return nil, err
	}
	return ParseKeyValues(out), nil
}

// Routes returns the helper's human-readable routing diagnostics. Render verbatim; do
// not parse it.
func Routes(ctx context.Context) (string, error) { return runHelper(ctx, verbRoutes) }

// restartUnit restarts an allowlisted systemd unit via the absolute systemctl path.
func restartUnit(ctx context.Context, name string) (string, error) {
	unit := RestartableUnit(name)
	if unit == "" {
		return "", fmt.Errorf("service %q is not restartable", name)
	}
	cmd := exec.CommandContext(ctx, "sudo", "-n", systemctlPath, "restart", unit)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// RestartUnit is the exported entry point for callers outside this package
// (the VPN activate handler in cmd/orchestrator).
func RestartUnit(ctx context.Context, name string) (string, error) {
	return restartUnit(ctx, name)
}

// VPNUp / VPNDown bring the tunnel up/down through the helper. The helper re-strips
// PostUp/PostDown/PreUp/PreDown/Table/SaveConfig/DNS into a root-owned copy under /run
// before handing it to wg-quick, so a config the service user can write cannot execute
// code as root even if the Go-side validator is ever bypassed.
func VPNUp(ctx context.Context) (string, error)   { return runHelper(ctx, verbVPNUp) }
func VPNDown(ctx context.Context) (string, error) { return runHelper(ctx, verbVPNDown) }

// helperTimeout bounds every privileged call so a hung netns operation cannot pin an
// HTTP handler (or the health cache refresh) indefinitely.
const helperTimeout = 12 * time.Second

func helperCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, helperTimeout)
}
