package main

import (
	"log/slog"
	"net/http"
	"strings"
)

// Browser-facing hardening applied to every response and every request.
//
// The UI is ~11,500 lines that build HTML with template strings and innerHTML. The escaping
// discipline in it is, as far as five passes could tell, currently perfect — but it is
// enforced by convention alone: no linter, no DOM construction, no Trusted Types. One missed
// esc() on a release title or a yt-dlp video title (both attacker-controlled, both arriving
// from third parties) is full admin compromise, and from there `POST /api/update/apply`
// reaches the root-owned updater.
//
// A CSP does not fix a missed escape. It changes what a missed escape COSTS.

// contentSecurityPolicy is deliberately strict everywhere it can afford to be.
//
//   - script-src 'self': there is not one inline <script> in the tree — both pages load a
//     single ES module — so no 'unsafe-inline' is needed and injected script cannot run.
//   - style-src allows 'unsafe-inline' because the UI sets style="" attributes throughout
//     (skeleton heights, progress widths). That is a real weakening and it is why script-src
//     matters: style injection alone is a defacement, script injection is the box.
//   - img-src permits any https origin. TMDB artwork and web-source thumbnails are both
//     third-party by design. Tightening this is the right long-term move but it belongs with
//     proxying those images, not here, where it would silently blank the library.
//   - connect-src 'self': the API is same-origin, so an injected script cannot exfiltrate to
//     an attacker's host with fetch().
//   - frame-ancestors 'none' also covers clickjacking, replacing X-Frame-Options for anything
//     modern; the header is sent as well for old TV browsers, which this project targets.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: https:; " +
	"media-src 'self' data: blob: https:; " +
	"connect-src 'self'; " +
	"font-src 'self' data:; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// securityHeaders sets the response headers on every route.
//
// Deliberately NOT set: Strict-Transport-Security. The primary deployment is plain HTTP on a
// LAN, and an HSTS header served over HTTP is either ignored or, once a TLS proxy appears in
// front, pins a host into HTTPS-only in a way the operator did not choose.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// crossOriginGuard rejects state-changing requests that a browser has told us came from
// somewhere else.
//
// The session cookie is SameSite=Lax, which does carry the mainstream case: a cross-site
// fetch, XHR or form POST does not send it. Two gaps remain, and both are live here.
//
//  1. SameSite is SITE-scoped, not origin-scoped. Jellyfin (:8096), Prowlarr (:9696) and
//     TorrServer (:8090) run on the same host over the same scheme, so they are the SAME SITE
//     as the dashboard. An XSS or open redirect in any of those three — all third-party code,
//     two with CVE history for exactly this — bypasses Lax entirely and can drive the whole
//     admin API: upload and activate a WireGuard config, repoint Prowlarr at an attacker's
//     server, create an admin, trigger the updater.
//  2. This project targets TV browsers. Pre-2020 WebKit either ignores SameSite or ships the
//     buggy Safari 12 interpretation.
//
// The check is on Origin and Sec-Fetch-Site, and ONLY when the browser sent them. A missing
// Origin is not treated as hostile: the Jellyfin webhook and the provider ingest API are
// server-to-server callers that send none, and both authenticate with their own shared
// secret. This closes browser-driven CSRF without touching machine callers.
func crossOriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		// Sec-Fetch-Site is the browser's own answer and cannot be forged by page script.
		switch r.Header.Get("Sec-Fetch-Site") {
		case "same-origin", "none":
			next.ServeHTTP(w, r)
			return
		case "same-site", "cross-site":
			slog.Warn("rejected a cross-origin state-changing request",
				"method", r.Method, "path", r.URL.Path,
				"site", r.Header.Get("Sec-Fetch-Site"), "remote", clientIPOf(r))
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		// Older browsers send Origin but not Sec-Fetch-Site.
		origin := r.Header.Get("Origin")
		if origin == "" || origin == "null" {
			next.ServeHTTP(w, r)
			return
		}
		if !originMatchesHost(origin, r.Host) {
			slog.Warn("rejected a state-changing request from another origin",
				"method", r.Method, "path", r.URL.Path, "origin", origin, "remote", clientIPOf(r))
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originMatchesHost compares an Origin header against the Host the request arrived on.
//
// Host-and-port, not host alone: :8096 and :1990 on one machine are the same SITE but
// different ORIGINS, and telling them apart is the entire point of this check.
func originMatchesHost(origin, host string) bool {
	i := strings.Index(origin, "://")
	if i < 0 {
		return false
	}
	return strings.EqualFold(origin[i+3:], host)
}
