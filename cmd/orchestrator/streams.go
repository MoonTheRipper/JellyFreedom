package main

import (
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"

	"jellyfreedom/internal/torrserver"
)

// ── Open streams ──────────────────────────────────────────────────────────────
//
// Every byte a player receives passes through this process: torrent streams through
// proxyTorrentStream, web sources through streamWebSource. So the orchestrator can count
// what is being watched without asking anyone.
//
// Jellyfin's sessions alone are not enough. A transcode Jellyfin has lost track of — after
// its client disconnected, or across a Jellyfin restart — keeps reading a /play URL with
// no session behind it, and /api/playback/active reported nobody watching. The watchdog
// could then restart TorrServer under it.

var openStreams atomic.Int64

// trackStream counts one stream as open until the returned func is called.
func trackStream() (done func()) {
	openStreams.Add(1)
	return func() { openStreams.Add(-1) }
}

// playbackActiveHandler answers "is anyone watching?" for the VPN watchdog and the
// port-forward keeper, so they can defer non-urgent TorrServer restarts while a stream is
// live.
//
// An open stream settles it without asking Jellyfin. Otherwise it is Jellyfin's sessions,
// which also cover a paused player that has no connection open.
//
// FAILS CLOSED. This used to report {"active":false} on ANY error, so an unreachable
// Jellyfin told the watchdog nobody was watching and it would restart TorrServer in the
// middle of a stream. When the answer is unknown, "someone is watching" is the only safe
// answer.
func playbackActiveHandler(jellyfinPlaying func() (int, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if openStreams.Load() > 0 {
			jsonOK(w, map[string]any{"active": true, "known": true})
			return
		}
		n, err := jellyfinPlaying()
		if err != nil {
			slog.Warn("playback/active: assuming active because Jellyfin could not be queried", "err", err)
			jsonOK(w, map[string]any{"active": true, "known": false})
			return
		}
		jsonOK(w, map[string]any{"active": n > 0, "known": true})
	}
}

// proxyTorrentStream forwards a Range request to TorrServer's /stream for hash+index. It
// serves both the legacy /proxy/stream path and Resolve-at-Play (/play), and counts as an
// open stream while it copies.
func proxyTorrentStream(ts *torrserver.Client, w http.ResponseWriter, r *http.Request, hash string, index int) {
	// hash is 40-hex-validated by every caller; StreamURL additionally escapes the
	// query values rather than interpolating them raw.
	if !torrserver.ValidInfoHash(hash) {
		http.Error(w, "bad torrent reference", http.StatusBadRequest)
		return
	}
	target := ts.StreamURL(hash, index)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	// A dedicated client with NO timeout: a film streams for hours on one response,
	// and http.DefaultClient's transport state is shared process-wide.
	resp, err := streamClient.Do(req)
	if err != nil {
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	defer trackStream()()
	if _, err := io.Copy(w, resp.Body); err != nil {
		// Almost always the player seeking or closing the connection; log at debug
		// level so a normal seek does not look like an error.
		slog.Debug("stream copy ended", "hash", hash, "err", err)
	}
}
