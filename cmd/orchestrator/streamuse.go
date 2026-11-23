package main

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"jellyfreedom/internal/torrserver"
)

// ── Torrents that are in use ──────────────────────────────────────────────────
//
// Several background paths drop torrents from TorrServer: the queue worker after it has
// validated a release, the worker when a re-resolve supersedes the cached one, the
// resolver when a candidate fails a check, the orphan cleaner, and the PlaybackStop
// webhook. None of them asked whether somebody was watching that torrent, and on the live
// box that is how a stop-and-retry after buffering lost its playback. At 08:26:53 on
// 2026-11-23 the stop of one attempt dropped S01E01 in the same second the next attempt
// started reading it.
//
// Every byte a player gets passes through proxyTorrentStream, so this process knows which
// torrents are being read and when they were last read. A drop that is not the user's
// own decision now leaves a torrent alone while it is in use, or was within inUseGrace.
// Nothing leaks by keeping it: TorrServer closes a torrent with no readers on its own
// after its disconnect timeout (torrserver.cache.disconnect_timeout_s, default 90s).
//
// Removing a title from the library is the exception. That drop is the user's decision,
// and it goes through ts.Drop directly.

// inUseGrace is how long after its last reader a torrent still counts as in use. It
// covers a player reconnecting between range requests, and the stop-then-retry a user
// does when playback stalls. Keeping the torrent across that gap keeps its buffered
// pieces in TorrServer's cache, so the retry does not start again from zero.
const inUseGrace = 2 * time.Minute

type torrentUse struct {
	open int       // readers right now
	last time.Time // when a reader last started or finished
}

var (
	torrentUseMu sync.Mutex
	torrentUses  = map[string]*torrentUse{}
)

// useTorrent marks a torrent as being read until the returned func is called.
func useTorrent(hash string) (done func()) {
	hash = strings.ToLower(hash)
	if hash == "" {
		return func() {}
	}
	torrentUseMu.Lock()
	u := torrentUses[hash]
	if u == nil {
		// Opportunistic sweep, so a long-running process does not keep one entry per
		// torrent it has ever streamed. Only idle entries past the grace window go.
		if len(torrentUses) >= 256 {
			for h, old := range torrentUses {
				if old.open == 0 && time.Since(old.last) >= inUseGrace {
					delete(torrentUses, h)
				}
			}
		}
		u = &torrentUse{}
		torrentUses[hash] = u
	}
	u.open++
	u.last = time.Now()
	torrentUseMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			torrentUseMu.Lock()
			u.open--
			u.last = time.Now()
			torrentUseMu.Unlock()
		})
	}
}

// torrentInUse reports whether a torrent is being read, or was within grace.
func torrentInUse(hash string, grace time.Duration) bool {
	hash = strings.ToLower(hash)
	torrentUseMu.Lock()
	defer torrentUseMu.Unlock()
	u := torrentUses[hash]
	if u == nil {
		return false
	}
	if u.open > 0 || time.Since(u.last) < grace {
		return true
	}
	// Idle past the grace window: forget it, so the map holds only recent torrents.
	delete(torrentUses, hash)
	return false
}

// dropIfIdle drops a torrent unless it is in use. Every drop that is not a direct user
// decision goes through here. Reports whether it dropped.
func dropIfIdle(ts *torrserver.Client, hash, why string) bool {
	if hash == "" {
		return false
	}
	if torrentInUse(hash, inUseGrace) {
		slog.Info("keeping a torrent that is being played", "hash", hash, "would_have_dropped_for", why)
		return false
	}
	if err := ts.Drop(hash); err != nil {
		slog.Warn("could not drop torrent", "hash", hash, "reason", why, "err", err)
		return false
	}
	return true
}
