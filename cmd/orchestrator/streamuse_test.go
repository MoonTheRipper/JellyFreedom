package main

import (
	"testing"
	"time"

	"jellyfreedom/internal/jellyfin"
	"jellyfreedom/internal/torrserver"
)

// The in-use table is package state. Tests use their own hashes, and a test that marks a
// shared hash (packHash) in use must forget it again, or the grace window leaks into
// every test after it.
func forgetTorrentUse(t *testing.T, hash string) {
	t.Cleanup(func() {
		torrentUseMu.Lock()
		delete(torrentUses, hash)
		torrentUseMu.Unlock()
	})
}

func TestTorrentInUseWhileOpenAndForTheGraceAfter(t *testing.T) {
	const h = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0001"
	if torrentInUse(h, time.Minute) {
		t.Fatal("a torrent nobody has read counts as in use")
	}
	done := useTorrent(h)
	if !torrentInUse(h, 0) {
		t.Fatal("an open reader does not count as in use")
	}
	done()
	done() // idempotent: a double release must not drive the count negative
	if !torrentInUse(h, time.Minute) {
		t.Fatal("a reader that just finished is not inside the grace window")
	}
	if torrentInUse(h, 0) {
		t.Fatal("still in use with no reader and no grace")
	}
}

func TestTorrentInUseIgnoresHashCase(t *testing.T) {
	defer useTorrent("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB0002")()
	if !torrentInUse("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb0002", 0) {
		t.Fatal("TorrServer and the library spell hashes in different cases; both must match")
	}
}

// The queue worker, the resolver and the orphan cleaner all drop through dropIfIdle. A
// torrent that is being streamed must survive every one of them.
func TestDropIfIdleSparesATorrentBeingPlayed(t *testing.T) {
	const h = "cccccccccccccccccccccccccccccccccccc0003"
	fake := &fakeTorrServer{}
	ts := torrserver.New(fake.serve(t).URL)

	done := useTorrent(h)
	if dropIfIdle(ts, h, "test") {
		t.Fatal("dropped a torrent with an open reader")
	}
	done()
	if dropIfIdle(ts, h, "test") {
		t.Fatal("dropped a torrent inside the grace window after its reader stopped")
	}
	if d := fake.drops(); len(d) != 0 {
		t.Fatalf("TorrServer was told to drop %v", d)
	}
	const idle = "dddddddddddddddddddddddddddddddddddd0004"
	if !dropIfIdle(ts, idle, "test") {
		t.Fatal("refused to drop a torrent nobody is using")
	}
	if d := fake.drops(); len(d) != 1 || d[0] != idle {
		t.Fatalf("drops = %v, want [%s]", d, idle)
	}
}

// The live failure: the stop of one playback attempt dropped the torrent in the same
// second the next attempt started reading it.
func TestPlaybackStopKeepsATorrentTheNextAttemptIsReading(t *testing.T) {
	jf := &fakeJellyfin{paths: map[string]string{stoppedAPI: e1}}
	db := libraryWith(t, e1)
	forgetTorrentUse(t, packHash)
	defer useTorrent(packHash)() // the retry is already streaming
	if got := runStop(t, db, jf); len(got) != 0 {
		t.Fatalf("dropped %v while the retry was reading it", got)
	}
}

func TestPlaybackStopDropIsDeferred(t *testing.T) {
	old := stopDropDelay
	stopDropDelay = 150 * time.Millisecond
	defer func() { stopDropDelay = old }()

	fake := &fakeTorrServer{}
	dropAfterPlaybackStop(libraryWith(t, e1), jellyfin.New((&fakeJellyfin{paths: map[string]string{stoppedAPI: e1}}).serve(t).URL, "key"),
		torrserver.New(fake.serve(t).URL), stoppedID)
	if d := fake.drops(); len(d) != 0 {
		t.Fatalf("dropped %v at the moment of the stop; it must wait for a retry", d)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(fake.drops()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if d := fake.drops(); len(d) != 1 || d[0] != packHash {
		t.Fatalf("after the delay, drops = %v, want [%s]", d, packHash)
	}
}
