package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidatePublicURLRejectsPlaceholder guards the cause of "Jellyfin shows it as ready
// but it won't play": the installer shipped server.public_url as the literal
// CHANGE-ME-LAN-IP, and that string is baked verbatim into every .strm file. The failure
// is invisible until someone actually tries to watch something.
func TestValidatePublicURLRejectsPlaceholder(t *testing.T) {
	bad := []string{
		"http://CHANGE-ME-LAN-IP:1990",
		"http://change-me-lan-ip:1990",
		"http://YOUR-LAN-IP:1990",
		"http://changeme:1990",
		"http://lan-ip-here:1990",
	}
	for _, raw := range bad {
		err := validatePublicURL(raw)
		if err == nil {
			t.Errorf("validatePublicURL(%q) accepted the installer placeholder", raw)
			continue
		}
		// The message must name the fix, not just complain.
		if !strings.Contains(err.Error(), "LAN address") {
			t.Errorf("validatePublicURL(%q) error does not tell the user what to do: %v", raw, err)
		}
	}
}

func TestValidatePublicURLRejectsMalformed(t *testing.T) {
	for _, raw := range []string{
		"192.168.1.50:1990",  // no scheme — would produce "192.168.1.50:1990/play/..."
		"ftp://192.168.1.50", // wrong scheme
		"http://",            // no host
		"not a url at all",
	} {
		if err := validatePublicURL(raw); err == nil {
			t.Errorf("validatePublicURL(%q) accepted a URL that cannot produce a working .strm", raw)
		}
	}
}

func TestValidatePublicURLAcceptsRealValues(t *testing.T) {
	for _, raw := range []string{
		"http://192.168.1.50:1990",
		"http://jellyfreedom:1990",
		"https://media.example.com",
		"http://100.68.44.59:1990", // Tailscale
		"http://localhost:1990",    // legal; warned about separately
	} {
		if err := validatePublicURL(raw); err != nil {
			t.Errorf("validatePublicURL(%q) rejected a valid value: %v", raw, err)
		}
	}
}

// A localhost public_url is legal (single-box setups work) but no other device can reach
// it, so it must produce a warning rather than silent breakage on an Apple TV.
func TestWarnIfPublicURLNotReachableFromLAN(t *testing.T) {
	for _, tc := range []struct {
		url      string
		wantWarn bool
	}{
		{"http://localhost:1990", true},
		{"http://127.0.0.1:1990", true},
		{"http://0.0.0.0:1990", true},
		{"http://[::1]:1990", true},
		{"http://192.168.1.50:1990", false},
		{"https://media.example.com", false},
	} {
		c := &Config{}
		c.Server.PublicURL = tc.url
		got := c.WarnIfPublicURLNotReachableFromLAN() != ""
		if got != tc.wantWarn {
			t.Errorf("%s: warned=%v, want %v", tc.url, got, tc.wantWarn)
		}
	}
}

// The placeholder must be caught by Load(), not just by the internal helper — that is the
// path the service actually takes at startup.
func TestLoadRejectsPlaceholderPublicURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  listen: "127.0.0.1:1990"
  public_url: "http://CHANGE-ME-LAN-IP:1990"
libraries:
  - name: Movies
    type: movie
    path: /tmp/movies
    default: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a config with the installer placeholder still in public_url")
	}
}

func TestLoadAppliesDefaultsAndValidatesLibraries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
libraries:
  - name: Movies
    type: movie
    path: /tmp/movies
    default: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.PublicURL != "http://localhost:1990" {
		t.Errorf("public_url default = %q", cfg.Server.PublicURL)
	}
	if cfg.Picker.MinSeeders != 5 || cfg.Picker.MaxSizeGB != 20 {
		t.Errorf("picker defaults not applied: %+v", cfg.Picker)
	}
	if !cfg.Picker.RejectCAMValue() {
		t.Error("reject_cam must default to true")
	}
	// Secure cookies default OFF: the primary deployment is plain HTTP on a LAN, where
	// Secure would stop the cookie being sent and log everyone out.
	if cfg.Server.SecureCookies {
		t.Error("secure_cookies must default to false")
	}
}

func TestLoadRejectsBadLibraries(t *testing.T) {
	for name, body := range map[string]string{
		"no libraries at all": "server:\n  listen: x\n",
		"missing type":        "libraries:\n  - name: M\n    path: /tmp/m\n",
		"missing path":        "libraries:\n  - name: M\n    type: movie\n",
		"bad type":            "libraries:\n  - name: M\n    type: music\n    path: /tmp/m\n",
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: Load accepted an invalid config", name)
		}
	}
}
