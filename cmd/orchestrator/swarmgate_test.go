package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyfreedom/internal/config"
	"jellyfreedom/internal/indexer"
	"jellyfreedom/internal/tmdb"
	"jellyfreedom/internal/torrserver"
)

// ── The gate on its own ───────────────────────────────────────────────────────

func gateRes(hash string) *resolveResult { return &resolveResult{hash: hash} }

func TestSwarmGate(t *testing.T) {
	t.Run("a strong release is accepted and the held fallback released", func(t *testing.T) {
		g := swarmGate{want: 10}
		if acc, rel := g.offer(gateRes("a"), 3); acc != nil || len(rel) != 0 {
			t.Fatalf("weak first offer = %v, %v; want held, nothing released", acc, rel)
		}
		acc, rel := g.offer(gateRes("b"), 12)
		if acc == nil || acc.hash != "b" || len(rel) != 1 || rel[0] != "a" {
			t.Fatalf("strong offer = %v, %v; want b accepted and a released", acc, rel)
		}
		if held, _ := g.take(); held != nil {
			t.Fatalf("gate still holds %q after accepting", held.hash)
		}
	})
	t.Run("between weak releases the stronger one is kept", func(t *testing.T) {
		g := swarmGate{want: 10}
		g.offer(gateRes("a"), 2)
		if _, rel := g.offer(gateRes("b"), 7); len(rel) != 1 || rel[0] != "a" {
			t.Fatalf("stronger weak offer released %v, want [a]", rel)
		}
		if _, rel := g.offer(gateRes("c"), 4); len(rel) != 1 || rel[0] != "c" {
			t.Fatalf("weaker weak offer released %v, want [c]", rel)
		}
		if held, live := g.take(); held == nil || held.hash != "b" || live != 7 {
			t.Fatalf("take = %v (%d), want b with 7", held, live)
		}
	})
	t.Run("the same torrent listed twice is never released", func(t *testing.T) {
		g := swarmGate{want: 10}
		g.offer(gateRes("a"), 3)
		if _, rel := g.offer(gateRes("a"), 2); len(rel) != 0 {
			t.Fatalf("a second listing of the held torrent released %v", rel)
		}
		if !g.holds("a") {
			t.Fatal("the held torrent was lost")
		}
	})
}

// ── resolvePlayable against fake TMDB, Prowlarr and TorrServer ────────────────

type fakeRelease struct {
	hash   string
	listed int // seeders the indexer reports
	live   int // seeders TorrServer actually connects to
}

type swarmFake struct {
	mu      sync.Mutex
	live    map[string]int
	added   []string
	dropped []string
	onAdd   func(hash string)
}

func (f *swarmFake) record(list *[]string, h string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*list = append(*list, h)
}

func (f *swarmFake) snapshot() (added, dropped []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.added...), append([]string(nil), f.dropped...)
}

var btih = regexp.MustCompile(`(?i)btih:([0-9a-f]{40})`)

// swarmWorker builds a queueWorker whose resolver sees releases in the order given: the
// listed seeder counts are chosen so the picker ranks them that way.
func swarmWorker(t *testing.T, releases ...fakeRelease) (*queueWorker, *swarmFake) {
	t.Helper()
	swarmSeenMu.Lock()
	swarmSeen = map[string]swarmReading{}
	swarmSeenMu.Unlock()

	tm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":77,"title":"Film","release_date":"2018-05-04","runtime":95}`))
	}))
	t.Cleanup(tm.Close)
	tc := tmdb.New("test-key")
	tc.SetBaseURL(tm.URL)

	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		for i, rel := range releases {
			out = append(out, map[string]any{
				"title": fmt.Sprintf("Film 2018 1080p WEB-DL x264 R%d", i), "infoHash": rel.hash,
				"size": 2_000_000_000, "seeders": rel.listed, "leechers": 0, "indexer": "fake",
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(px.Close)

	f := &swarmFake{live: map[string]int{}}
	for _, rel := range releases {
		f.live[strings.ToLower(rel.hash)] = rel.live
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Action, Link, Hash string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.Action {
		case "add":
			m := btih.FindStringSubmatch(body.Link)
			if m == nil {
				http.Error(w, "bad link", http.StatusBadRequest)
				return
			}
			h := strings.ToLower(m[1])
			f.record(&f.added, h)
			if f.onAdd != nil {
				f.onAdd(h)
			}
			_, _ = fmt.Fprintf(w, `{"hash":%q}`, h)
		case "get":
			f.mu.Lock()
			live := f.live[body.Hash]
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"hash":%q,"file_stats":[{"id":1,"path":"Film.2018.1080p.mkv","length":2000000000}],`+
				`"connected_seeders":%d,"active_peers":1}`, body.Hash, live)
		case "rem":
			f.record(&f.dropped, body.Hash)
		}
	}))
	t.Cleanup(ts.Close)

	cfg := &config.Config{}
	cfg.Libraries = []config.Library{{Name: "Movies", Type: "movie", Path: t.TempDir(), Default: true}}
	w := &queueWorker{tmdb: tc, indexer: indexer.New(px.URL, "test-key"), ts: torrserver.New(ts.URL), cfg: cfg}
	return w, f
}

// fastSwarmWindow shortens the live-seeder wait for a test.
func fastSwarmWindow(t *testing.T, window time.Duration) {
	t.Helper()
	oldWant, oldWindow, oldEvery := liveSeedersWanted, liveSeedersWindow, liveSeedersEvery
	liveSeedersWanted, liveSeedersWindow, liveSeedersEvery = 10, window, 10*time.Millisecond
	t.Cleanup(func() { liveSeedersWanted, liveSeedersWindow, liveSeedersEvery = oldWant, oldWindow, oldEvery })
}

func hashN(n int) string { return strings.Repeat(fmt.Sprintf("%x", n), 40) }

func contains(list []string, h string) bool {
	for _, x := range list {
		if x == h {
			return true
		}
	}
	return false
}

// The Bohemian Rhapsody case: the top-ranked release is listed with plenty of seeders but
// connects to two. The resolver must move on to a release that can actually stream.
func TestResolveSkipsAReleaseWhoseLiveSwarmIsWeak(t *testing.T) {
	fastSwarmWindow(t, 200*time.Millisecond)
	weak, strong := hashN(1), hashN(2)
	w, f := swarmWorker(t, fakeRelease{weak, 600, 2}, fakeRelease{strong, 300, 25})

	res, err := w.resolvePlayable(context.Background(), "movie", "Movies", 77, 0, 0, "", "", nil)
	if err != nil {
		t.Fatalf("resolvePlayable: %v", err)
	}
	if res.hash != strong {
		t.Fatalf("picked %s, want the release with 25 live seeders %s", res.hash, strong)
	}
	_, dropped := f.snapshot()
	if !contains(dropped, weak) || contains(dropped, strong) {
		t.Fatalf("dropped = %v, want the weak release dropped and the pick kept", dropped)
	}
}

// When nothing reaches the bar, the strongest swarm seen is used: this must never make a
// title unplayable that resolved before.
func TestResolveFallsBackToTheStrongestWeakSwarm(t *testing.T) {
	fastSwarmWindow(t, 100*time.Millisecond)
	a, b, c, d := hashN(1), hashN(2), hashN(3), hashN(4)
	w, f := swarmWorker(t, fakeRelease{a, 600, 2}, fakeRelease{b, 300, 7}, fakeRelease{c, 150, 4}, fakeRelease{d, 60, 1})

	res, err := w.resolvePlayable(context.Background(), "movie", "Movies", 77, 0, 0, "", "", nil)
	if err != nil {
		t.Fatalf("resolvePlayable: %v", err)
	}
	if res.hash != b {
		t.Fatalf("picked %s, want the strongest weak swarm %s (7 live seeders)", res.hash, b)
	}
	added, dropped := f.snapshot()
	if len(added) != 4 {
		t.Fatalf("added %v, want all four candidates tried", added)
	}
	for _, h := range []string{a, c, d} {
		if !contains(dropped, h) {
			t.Errorf("%s was not dropped; dropped = %v", h, dropped)
		}
	}
	if contains(dropped, b) {
		t.Fatalf("the chosen fallback %s was dropped", b)
	}
}

func TestResolveAcceptsAStrongFirstReleaseWithoutWaiting(t *testing.T) {
	fastSwarmWindow(t, 5*time.Second)
	strong := hashN(1)
	w, f := swarmWorker(t, fakeRelease{strong, 600, 30}, fakeRelease{hashN(2), 300, 40})

	start := time.Now()
	res, err := w.resolvePlayable(context.Background(), "movie", "Movies", 77, 0, 0, "", "", nil)
	if err != nil || res.hash != strong {
		t.Fatalf("resolvePlayable = %v, %v; want %s", res, err, strong)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("a strong first release took %v: it waited out the window", el)
	}
	if added, _ := f.snapshot(); len(added) != 1 {
		t.Fatalf("added %v, want only the first release", added)
	}
}

// A release the user picked by hand is not second-guessed or kept waiting.
func TestResolveTrustsAHandPickedRelease(t *testing.T) {
	fastSwarmWindow(t, 5*time.Second)
	picked := hashN(5)
	w, f := swarmWorker(t)
	f.live[picked] = 1

	start := time.Now()
	res, err := w.resolvePlayable(context.Background(), "movie", "Movies", 77, 0, 0,
		"magnet:?xt=urn:btih:"+picked, "", nil)
	if err != nil || res.hash != picked {
		t.Fatalf("resolvePlayable = %v, %v; want the hand-picked %s", res, err, picked)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("a hand-picked release waited %v for its swarm", el)
	}
}

// /play has a deadline. Running out of it after a weak release was found plays that
// release rather than failing.
func TestResolveOutOfTimeUsesTheStrongestReleaseFound(t *testing.T) {
	fastSwarmWindow(t, 400*time.Millisecond)
	a, b := hashN(1), hashN(2)
	w, f := swarmWorker(t, fakeRelease{a, 600, 3}, fakeRelease{b, 300, 1})

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	res, err := w.resolvePlayable(ctx, "movie", "Movies", 77, 0, 0, "", "", nil)
	if err != nil || res == nil || res.hash != a {
		t.Fatalf("resolvePlayable = %v, %v; want the weak release %s found before the deadline", res, err, a)
	}
	if _, dropped := f.snapshot(); contains(dropped, a) {
		t.Fatalf("the release being returned was dropped: %v", dropped)
	}
}

// A cancelled request (the user cancelled it, or removed the title) gets nothing, and
// nothing it loaded is left behind in TorrServer.
func TestResolveCancelledDropsWhatItHeld(t *testing.T) {
	fastSwarmWindow(t, 100*time.Millisecond)
	a, b := hashN(1), hashN(2)
	w, f := swarmWorker(t, fakeRelease{a, 600, 3}, fakeRelease{b, 300, 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onAdd = func(h string) {
		if h == b {
			cancel()
		}
	}

	res, err := w.resolvePlayable(ctx, "movie", "Movies", 77, 0, 0, "", "", nil)
	if !errors.Is(err, context.Canceled) || res != nil {
		t.Fatalf("resolvePlayable = %v, %v; want context.Canceled", res, err)
	}
	_, dropped := f.snapshot()
	if !contains(dropped, a) || !contains(dropped, b) {
		t.Fatalf("dropped = %v, want both the held fallback %s and %s", dropped, a, b)
	}
}

// A weak reading expires sooner than a strong one, so a measurement taken while
// TorrServer was still warming up does not hold a good release back for half an hour.
func TestSwarmReadingTTL(t *testing.T) {
	if got := (swarmReading{live: liveSeedersWanted}).ttl(); got != swarmSeenTTL {
		t.Errorf("strong reading TTL = %v, want %v", got, swarmSeenTTL)
	}
	if got := (swarmReading{live: liveSeedersWanted - 1}).ttl(); got != weakSwarmTTL {
		t.Errorf("weak reading TTL = %v, want %v", got, weakSwarmTTL)
	}
}

// The cache is what keeps a season of episodes on one weak pack from paying the full wait
// per episode.
func TestMeasureSwarmReusesARecentReading(t *testing.T) {
	fastSwarmWindow(t, 300*time.Millisecond)
	h := hashN(7)
	ts := torrserver.New("http://127.0.0.1:1") // unreachable: a cache miss would read 0
	swarmSeenMu.Lock()
	swarmSeen = map[string]swarmReading{h: {live: 4, at: time.Now()}}
	swarmSeenMu.Unlock()
	start := time.Now()
	if got := measureSwarm(context.Background(), ts, h); got != 4 {
		t.Fatalf("measureSwarm = %d, want the cached 4", got)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("a cached reading still waited for the swarm")
	}
	swarmSeenMu.Lock()
	swarmSeen[h] = swarmReading{live: 4, at: time.Now().Add(-weakSwarmTTL - time.Second)}
	swarmSeenMu.Unlock()
	if got := measureSwarm(context.Background(), ts, h); got != 0 {
		t.Fatalf("an expired weak reading was reused: got %d, want a fresh (unreachable) 0", got)
	}
}
