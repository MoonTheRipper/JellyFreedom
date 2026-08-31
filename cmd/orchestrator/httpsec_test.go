package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestSecurityHeadersAreSetOnEveryResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	securityHeaders(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard/", nil))

	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"script-src 'self'", // no 'unsafe-inline': injected script must not run
		"object-src 'none'", // no plugin content
		"base-uri 'none'",   // an injected <base> cannot repoint every relative URL
		"frame-ancestors 'none'",
		"connect-src 'self'", // injected script cannot exfiltrate with fetch()
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP is missing %q:\n  %s", want, csp)
		}
	}
	// 'unsafe-inline' in script-src would make the whole policy decorative.
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Error("script-src allows 'unsafe-inline' — the policy would not stop an injected script")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", got)
	}
}

func TestCrossOriginGuard(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		// The dashboard's own requests.
		{"same-origin POST", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"origin matches host", http.MethodPost, map[string]string{"Origin": "http://jelly.local:1990"}, http.StatusOK},

		// Machine callers: the Jellyfin webhook and the provider ingest API send neither
		// header and authenticate with their own shared secret. They must not be blocked.
		{"no browser headers at all", http.MethodPost, nil, http.StatusOK},

		// The gap SameSite=Lax leaves open: same site, different port. Jellyfin on :8096 is
		// the same SITE as the dashboard on :1990, so Lax sends the cookie.
		{"same-site different port", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"origin differs by port only", http.MethodPost, map[string]string{"Origin": "http://jelly.local:8096"}, http.StatusForbidden},

		{"cross-site POST", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"cross-site DELETE", http.MethodDelete, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"cross-origin PUT", http.MethodPut, map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},

		// Reads are not state-changing; /play and the media UI must keep working.
		{"cross-site GET is allowed", http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/api/settings", nil)
			r.Host = "jelly.local:1990"
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			crossOriginGuard(okHandler()).ServeHTTP(rec, r)
			if rec.Code != tc.want {
				t.Errorf("got %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestOriginMatchesHostComparesThePort(t *testing.T) {
	if !originMatchesHost("http://box:1990", "box:1990") {
		t.Error("identical origin and host did not match")
	}
	// The whole point: same site, different origin.
	if originMatchesHost("http://box:8096", "box:1990") {
		t.Error("a different port was treated as the same origin")
	}
	if originMatchesHost("http://evil.example", "box:1990") {
		t.Error("a different host matched")
	}
	if originMatchesHost("box:1990", "box:1990") {
		t.Error("a value with no scheme was accepted as an origin")
	}
}
