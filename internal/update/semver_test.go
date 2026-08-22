package update

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// The case string comparison gets backwards.
		{"0.10.0", "0.9.0", 1},
		{"0.9.0", "0.10.0", -1},
		{"1.0.0", "0.99.99", 1},
		{"2.0.0", "10.0.0", -1},
		{"0.0.10", "0.0.9", 1},

		// Equality, including through the noise a tag carries.
		{"0.3.0", "0.3.0", 0},
		{"v0.3.0", "0.3.0", 0},
		{"  v0.3.0\n", "0.3.0", 0},
		{"V0.3.0", "v0.3.0", 0},
		{"0.3", "0.3.0", 0},
		{"1", "1.0.0", 0},
		{"1.2.3+abc123", "1.2.3", 0}, // build metadata is not part of precedence

		// Pre-releases rank BELOW the matching final release.
		{"0.4.0-rc1", "0.4.0", -1},
		{"0.4.0", "0.4.0-rc1", 1},
		{"0.4.0-rc1", "0.4.0-rc2", -1},
		// "rc10" and "rc2" are single ALPHANUMERIC identifiers, so semver compares them
		// as ASCII, not as numbers: "rc10" sorts before "rc2".
		{"0.4.0-rc2", "0.4.0-rc10", 1},
		{"0.4.0-1", "0.4.0-2", -1},
		{"0.4.0-1", "0.4.0-alpha", -1}, // numeric identifiers rank below alphanumeric
		{"0.4.0-alpha", "0.4.0-alpha.1", -1},
		{"0.4.0-rc1", "0.5.0", -1},
		{"0.4.0-rc1", "0.3.9", 1},
	}
	for _, c := range cases {
		got, err := CompareVersions(c.a, c.b)
		if err != nil {
			t.Errorf("CompareVersions(%q,%q) unexpected error: %v", c.a, c.b, err)
			continue
		}
		if got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseVersionRejectsGarbage(t *testing.T) {
	bad := []string{
		"", "   ", "dev", "v", "vv1.0.0", "1.2.3.4", "one.two.three", "1.x.0",
		"-1.0.0", "1..0", "1.2.3-", "latest", "🙂", "0x10.0.0", "1.2.3 4",
	}
	for _, s := range bad {
		if v, err := ParseVersion(s); err == nil {
			t.Errorf("ParseVersion(%q) = %+v, want an error", s, v)
		}
	}
}

// A malformed version must never panic and must never be read as "an update exists".
func TestIsNewerFailsClosed(t *testing.T) {
	cases := []struct {
		name            string
		current, latest string
		want            bool
	}{
		{"real upgrade", "0.3.0", "0.4.0", true},
		{"real upgrade, ten beats nine", "0.9.0", "0.10.0", true},
		{"tagged with v", "0.3.0", "v0.4.0", true},
		{"same version", "0.4.0", "0.4.0", false},
		{"downgrade", "0.5.0", "0.4.0", false},
		{"prerelease is not an upgrade over the final", "0.4.0", "0.4.0-rc1", false},
		{"final IS an upgrade over the rc", "0.4.0-rc1", "0.4.0", true},
		{"dev build is never offered an update", devVersion, "9.9.9", false},
		{"stamped dev build is never offered an update", "dev-a1b2c3", "9.9.9", false},
		{"empty current", "", "0.4.0", false},
		{"empty latest", "0.3.0", "", false},
		{"garbage latest", "0.3.0", "not-a-version", false},
		{"garbage current", "banana", "0.4.0", false},
		{"both garbage", "banana", "kiwi", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsNewer(c.current, c.latest); got != c.want {
				t.Errorf("IsNewer(%q,%q) = %v, want %v", c.current, c.latest, got, c.want)
			}
		})
	}
}

func TestIsDev(t *testing.T) {
	for _, s := range []string{"dev", "DEV", " dev ", "dev-a1b2c3"} {
		if !IsDev(s) {
			t.Errorf("IsDev(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"0.4.0", "v0.4.0", "", "0.4.0-dev"} {
		if IsDev(s) {
			t.Errorf("IsDev(%q) = true, want false", s)
		}
	}
}
