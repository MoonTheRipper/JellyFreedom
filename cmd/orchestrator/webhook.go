package main

import (
	"log/slog"

	"jellyfreedom/internal/jellyfin"
	"jellyfreedom/internal/store"
	"jellyfreedom/internal/torrserver"
)

// dropAfterPlaybackStop frees the TorrServer cache behind an item Jellyfin has stopped
// playing, when nothing else still needs it.
//
// Whatever it cannot establish — which library row the item is, whether anyone else is
// still watching it, whether another episode shares the torrent — it settles by KEEPING
// the torrent. TorrServer's own disconnect timeout frees an idle torrent later anyway
// (operations.md §2), while dropping one that is still being streamed cuts a viewer off.
//
// Each of these steps used to return silently. The lookup had failed on every call for
// as long as anyone can tell, and nothing in the log said so.
func dropAfterPlaybackStop(db *store.Store, jf *jellyfin.Client, ts *torrserver.Client, itemID string) {
	strmPath, err := jf.GetItemPath(itemID)
	if err != nil {
		slog.Warn("webhook: could not look the stopped item up in Jellyfin; keeping its torrent",
			"item", itemID, "err", err)
		return
	}
	if strmPath == "" {
		slog.Info("webhook: Jellyfin has no item with this id; nothing to drop", "item", itemID)
		return
	}
	item, err := db.GetByStrmPath(strmPath)
	if err != nil {
		slog.Warn("webhook: could not read the library; keeping the torrent", "path", strmPath, "err", err)
		return
	}
	if item == nil {
		// A local file or a .strm this instance does not track: not ours to drop.
		return
	}
	active, err := jf.ActiveSessionsForItem(itemID)
	if err != nil {
		slog.Warn("webhook: could not ask Jellyfin who is still watching; keeping the torrent",
			"title", item.Title, "err", err)
		return
	}
	if active > 0 {
		slog.Info("playback stopped but other sessions active, not dropping",
			"title", item.Title, "active_sessions", active)
		return
	}
	// Playback ended: free the TorrServer cache, but the item STAYS 'ready'. It's still
	// resolvable, and /play re-adds the torrent on demand next time. Staleness is decided
	// solely by resolvability (the health check) — never by playback ending. Drop only when
	// no OTHER live item (e.g. another episode from the same season pack) needs this hash.
	n, err := db.CountReadyByHashExcept(item.InfoHash, item.StrmPath)
	if err != nil {
		// Unknown reference count: KEEP the torrent. Dropping one that another
		// episode is still streaming would kill someone else's playback.
		slog.Error("webhook: could not count references for this hash; keeping the torrent",
			"hash", item.InfoHash, "err", err)
		return
	}
	if n > 0 {
		slog.Info("torrent kept, other episodes still in library",
			"title", item.Title, "remaining", n)
		return
	}
	if err := ts.Drop(item.InfoHash); err != nil {
		slog.Warn("webhook: could not drop torrent after playback stop", "title", item.Title, "err", err)
		return
	}
	slog.Info("torrent dropped after playback stop", "title", item.Title)
}
