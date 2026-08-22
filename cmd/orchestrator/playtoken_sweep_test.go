package main

import "testing"

// The three real-world shapes found on a live install after the capability-token migration:
// two orphaned /play URLs whose library rows were gone, and one legacy /proxy/stream file
// written before resolve-at-play existed. All three 403'd because nothing re-signed them.
func TestParsePlayURL(t *testing.T) {
	cases := []struct {
		name                 string
		raw                  string
		wantType             string
		wantID, wantS, wantE int
		wantOK               bool
	}{
		{"tv orphan", "http://192.168.178.2:1990/play/tv/76479/1/6", "tv", 76479, 1, 6, true},
		{"tv orphan s4", "http://192.168.178.2:1990/play/tv/76479/4/2", "tv", 76479, 4, 2, true},
		{"movie", "http://host:1990/play/movie/27205", "movie", 27205, 0, 0, true},
		{"already tokenised", "http://host:1990/play/tv/1622/14/1?t=abc", "tv", 1622, 14, 1, true},
		{"trailing slash", "http://host:1990/play/movie/27205/", "movie", 27205, 0, 0, true},
		{"legacy proxy form", "http://host:1990/proxy/stream?link=deadbeef&index=0", "", 0, 0, 0, false},
		{"tv missing episode", "http://host:1990/play/tv/76479/1", "", 0, 0, 0, false},
		{"non-numeric id", "http://host:1990/play/movie/abc", "", 0, 0, 0, false},
		{"unrelated url", "http://host:1990/api/library", "", 0, 0, 0, false},
		{"empty", "", "", 0, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mt, id, sn, ep, ok := parsePlayURL(c.raw)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (%q)", ok, c.wantOK, c.raw)
			}
			if !ok {
				return
			}
			if mt != c.wantType || id != c.wantID || sn != c.wantS || ep != c.wantE {
				t.Fatalf("got (%s,%d,%d,%d), want (%s,%d,%d,%d)",
					mt, id, sn, ep, c.wantType, c.wantID, c.wantS, c.wantE)
			}
		})
	}
}

func TestParseLegacyStreamHash(t *testing.T) {
	const h = "2ef50cc10f6eb563314c2db47d6a4e04734312e1"
	if got := parseLegacyStreamHash("http://192.168.178.2:1990/proxy/stream?link=" + h + "&index=0"); got != h {
		t.Fatalf("legacy hash = %q, want %q", got, h)
	}
	for _, raw := range []string{
		"http://host:1990/play/movie/27205",
		"http://host:1990/proxy/stream?index=0",
		"",
	} {
		if got := parseLegacyStreamHash(raw); got != "" {
			t.Fatalf("expected no hash for %q, got %q", raw, got)
		}
	}
}
