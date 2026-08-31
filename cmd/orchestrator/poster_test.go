package main

import "testing"

// A poster is rendered as <img src> to OTHER viewers, so an unvalidated one is a stored
// beacon: a low-privileged account submits a URL on a host it controls and learns when an
// admin looks at the queue, from that admin's real address, outside the VPN. It is not XSS —
// esc() escapes the quote and javascript: is inert in an img src — which is exactly why it
// survived review as harmless.
func TestSafePosterURL(t *testing.T) {
	keep := []string{
		"https://image.tmdb.org/t/p/w500/abc.jpg",
		"http://192.168.1.5/local-art.png",
	}
	for _, u := range keep {
		if safePosterURL(u) != u {
			t.Errorf("dropped a legitimate poster: %q", u)
		}
	}

	drop := []string{
		"",
		"javascript:alert(1)",
		"data:image/svg+xml,<svg onload=alert(1)>",
		"//attacker.example/beacon.png", // scheme-relative: no scheme, so no host check applies
		"https://attacker.example/b.png\nX-Injected: 1",
		"ftp://attacker.example/b.png",
		"https://",
	}
	for _, u := range drop {
		if got := safePosterURL(u); got != "" {
			t.Errorf("kept a poster it should have dropped: %q -> %q", u, got)
		}
	}
}
