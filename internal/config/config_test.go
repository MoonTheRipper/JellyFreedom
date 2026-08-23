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

// ── Picker quality policy ─────────────────────────────────────────────────────

func writeConfig(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

const minimalLibraries = `
libraries:
  - name: Movies
    type: movie
    path: /tmp/movies
    default: true
`

func TestPickerQualityDefaults(t *testing.T) {
	cfg := writeConfig(t, minimalLibraries)

	if cfg.Picker.TargetResolution != "1080p" {
		t.Errorf("target_resolution default = %q, want 1080p", cfg.Picker.TargetResolution)
	}
	// Default FALSE on purpose: turning direct-play into a hard filter for an existing
	// install would empty release lists that work fine today.
	if cfg.Picker.RequireDirectPlayValue() {
		t.Error("require_direct_play must default to false")
	}
	if cfg.Picker.MaxMbps != 0 {
		t.Errorf("max_mbps default = %d, want 0 (no cap)", cfg.Picker.MaxMbps)
	}
	// "hevc" is unreachable: the indexer folds x265/HEVC/H265 into "h265" while parsing
	// the title, so the picker never sees that string on a release. Shipping it in the
	// default also inflated the codec weights with a value that matched nothing.
	for _, c := range cfg.Picker.PreferVideoCodecs {
		if c == "hevc" {
			t.Errorf("prefer_video_codecs default still ships the unreachable %q entry: %v",
				c, cfg.Picker.PreferVideoCodecs)
		}
	}
}

func TestPickerTargetResolutionIsCanonicalised(t *testing.T) {
	for _, tc := range []struct{ given, want string }{
		{"2160p", "2160p"},
		{"4k", "2160p"},
		{"UHD", "2160p"},
		{"1080p", "1080p"},
		{"720p", "720p"},
	} {
		cfg := writeConfig(t, minimalLibraries+"picker:\n  target_resolution: \""+tc.given+"\"\n")
		if cfg.Picker.TargetResolution != tc.want {
			t.Errorf("target_resolution %q loaded as %q, want %q",
				tc.given, cfg.Picker.TargetResolution, tc.want)
		}
	}
}

// A typo'd target resolution must be a hard failure, not a silent fallback: the symptom
// of a silent fallback is picks that feel wrong months later, with the config on screen
// saying exactly what the user thought they asked for.
func TestPickerRejectsInvalidQualitySettings(t *testing.T) {
	cases := map[string]string{
		"a bare number is not a rung": "picker:\n  target_resolution: \"1085p\"\n",
		"nonsense target":             "picker:\n  target_resolution: potato\n",
		"8k is not on the ladder":     "picker:\n  target_resolution: 8k\n",
		"negative max_mbps":           "picker:\n  max_mbps: -5\n",
		"negative min_seeders":        "picker:\n  min_seeders: -1\n",
		"negative max_size_gb":        "picker:\n  max_size_gb: -1\n",
		"bad target in a library override": `
libraries:
  - name: TV
    type: tv
    path: /tmp/tv
    picker:
      target_resolution: banana
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			full := minimalLibraries + body
			if strings.Contains(body, "libraries:") {
				full = body
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(full), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Errorf("Load accepted an invalid picker setting:\n%s", full)
			}
		})
	}
}

// TestPickerForMergesEveryOverride guards the per-library overrides livePickerFor
// depends on. reject_cam was silently NOT merged: a library that set `reject_cam: false`
// still had the global `true` applied, and got "no suitable release found" with nothing
// to indicate its own setting had been discarded.
func TestPickerForMergesEveryOverride(t *testing.T) {
	cfg := writeConfig(t, `
libraries:
  - name: Movies
    type: movie
    path: /tmp/movies
    default: true
  - name: Obscure
    type: movie
    path: /tmp/obscure
    picker:
      min_seeders: 1
      max_size_gb: 60
      target_resolution: 4k
      max_mbps: 25
      reject_cam: false
      require_direct_play: true
      prefer_video_codecs: ["h265"]
      prefer_audio_codecs: ["eac3"]
      prefer_containers: ["mkv"]
picker:
  min_seeders: 20
  max_size_gb: 20
  target_resolution: 720p
  reject_cam: true
`)

	global := cfg.PickerFor(cfg.FindLibrary("Movies"))
	if global.MinSeeders != 20 || global.TargetResolution != "720p" || !global.RejectCAMValue() {
		t.Fatalf("a library with no overrides must get the global picker verbatim: %+v", global)
	}
	if global.RequireDirectPlayValue() {
		t.Error("the global picker did not set require_direct_play; it must stay false")
	}

	merged := cfg.PickerFor(cfg.FindLibrary("Obscure"))
	checks := []struct {
		name      string
		got, want any
	}{
		{"min_seeders", merged.MinSeeders, 1},
		{"max_size_gb", merged.MaxSizeGB, 60},
		{"target_resolution", merged.TargetResolution, "2160p"}, // canonicalised on load
		{"max_mbps", merged.MaxMbps, 25},
		{"reject_cam", merged.RejectCAMValue(), false},
		{"require_direct_play", merged.RequireDirectPlayValue(), true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("override %s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if len(merged.PreferVideoCodecs) != 1 || merged.PreferVideoCodecs[0] != "h265" {
		t.Errorf("prefer_video_codecs override = %v", merged.PreferVideoCodecs)
	}

	// The merge must not mutate the global config: PickerFor is called per request.
	if cfg.Picker.MinSeeders != 20 || cfg.Picker.TargetResolution != "720p" || !cfg.Picker.RejectCAMValue() {
		t.Errorf("PickerFor mutated the global picker: %+v", cfg.Picker)
	}
}

// A library override that omits a key must inherit it, not zero it.
func TestPickerForInheritsUnsetKeys(t *testing.T) {
	cfg := writeConfig(t, `
libraries:
  - name: TV
    type: tv
    path: /tmp/tv
    default: true
    picker:
      min_seeders: 3
picker:
  min_seeders: 20
  target_resolution: 2160p
  max_mbps: 30
  require_direct_play: true
`)
	merged := cfg.PickerFor(cfg.FindLibrary("TV"))
	if merged.MinSeeders != 3 {
		t.Errorf("min_seeders = %d, want the library's 3", merged.MinSeeders)
	}
	if merged.TargetResolution != "2160p" {
		t.Errorf("target_resolution = %q, want the inherited 2160p", merged.TargetResolution)
	}
	if merged.MaxMbps != 30 {
		t.Errorf("max_mbps = %d, want the inherited 30", merged.MaxMbps)
	}
	if !merged.RequireDirectPlayValue() {
		t.Error("require_direct_play must be inherited from the global picker")
	}
}
