package api

import (
	"context"
	"strings"
	"testing"
)

// TestRestartableUnitAllowlist: a unit name must never be interpolated from a request
// path into a privileged command.
func TestRestartableUnitAllowlist(t *testing.T) {
	allowed := map[string]string{
		"jellyfin":         "jellyfin.service",
		"torrserver-netns": "torrserver-netns.service",
		"vpntorrent-netns": "vpntorrent-netns.service",
		"prowlarr":         "prowlarr.service",
		"flaresolverr":     "flaresolverr.service",
	}
	for name, unit := range allowed {
		if got := RestartableUnit(name); got != unit {
			t.Errorf("RestartableUnit(%q) = %q, want %q", name, got, unit)
		}
	}

	refused := []string{
		"",
		// A service must not be able to bounce itself through root.
		"jellyfreedom",
		"jellyfreedom.service",
		// Injection shapes that a path segment could carry.
		"jellyfin.service; rm -rf /",
		"../../../etc/systemd/system/evil",
		"sshd",
		"jellyfin ",
		" jellyfin",
		"JELLYFIN",
		"jellyfin\n",
	}
	for _, name := range refused {
		if got := RestartableUnit(name); got != "" {
			t.Errorf("RestartableUnit(%q) = %q, want it refused", name, got)
		}
	}
}

// TestRunHelperRejectsUnknownVerbs: the helper accepts a CLOSED verb set and no
// free-form arguments, and the Go side must refuse anything outside it before ever
// invoking sudo.
func TestRunHelperRejectsUnknownVerbs(t *testing.T) {
	for _, verb := range []string{
		"", "shell", "exec", "status; id", "--help", "vpn-up extra", "STATUS",
	} {
		out, err := runHelper(context.Background(), verb)
		if err == nil {
			t.Errorf("runHelper(%q) was permitted (output %q)", verb, out)
			continue
		}
		if !strings.Contains(err.Error(), "unknown privileged verb") {
			t.Errorf("runHelper(%q) failed for the wrong reason: %v", verb, err)
		}
	}
}

func TestPrivilegedPathsAreAbsoluteAndCorrect(t *testing.T) {
	if !strings.HasPrefix(helperPath, "/") {
		t.Errorf("helperPath %q must be absolute", helperPath)
	}
	// sudo matches the RESOLVED path and does NOT follow the merged-usr /bin -> /usr/bin
	// symlink, so a rule written for /bin/systemctl never matches and every restart is
	// denied. This must stay /usr/bin.
	if systemctlPath != "/usr/bin/systemctl" {
		t.Errorf("systemctlPath = %q, want /usr/bin/systemctl", systemctlPath)
	}
}
