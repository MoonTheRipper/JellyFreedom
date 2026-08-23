package torrserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestConfigureBaseURLRace is the regression test for the data race in post().
//
// post() read c.baseURL directly while Configure() wrote it under a write lock from the
// Settings handler. Every other accessor went through c.base(). Run with -race: without
// the fix this reports a write/read data race on Client.baseURL.
func TestConfigureBaseURLRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hash":"aa","file_stats":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: the Settings → Connections handler reconfiguring live.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				c.Configure(srv.URL)
			} else {
				c.Configure(srv.URL + "/")
			}
		}
	}()

	// Readers: anything that issues a request.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = c.Stat("aa")
				_ = c.Configured()
				_ = c.BaseURL()
				_ = c.StreamURL("aabbccddeeff00112233445566778899aabbccdd", 1)
			}
		}()
	}

	time.Sleep(250 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestValidInfoHash(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"aabbccddeeff00112233445566778899aabbccdd", true},
		{"AABBCCDDEEFF00112233445566778899AABBCCDD", true},
		{"", false},
		{"aabbccdd", false}, // too short
		{"aabbccddeeff00112233445566778899aabbccddee", false},        // too long
		{"aabbccddeeff00112233445566778899aabbccdg", false},          // non-hex
		{"aabbccddeeff00112233445566778899aabbccd&link=evil", false}, // parameter smuggling
		{"../../etc/passwd", false},
		{"aabbccddeeff0011223344556677 899aabbccdd", false}, // embedded space
	}
	for _, tc := range cases {
		if got := ValidInfoHash(tc.in); got != tc.want {
			t.Errorf("ValidInfoHash(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestStreamURLEscapesParameters guards against raw interpolation into the upstream URL.
func TestStreamURLEscapesParameters(t *testing.T) {
	c := New("http://ts.local")
	got := c.StreamURL("aabbccddeeff00112233445566778899aabbccdd", 3)
	want := "http://ts.local/stream?index=3&link=aabbccddeeff00112233445566778899aabbccdd&play"
	if got != want {
		t.Fatalf("StreamURL = %q, want %q", got, want)
	}
}

func TestPostRefusesUnconfiguredClient(t *testing.T) {
	c := New("")
	if _, err := c.Stat("aa"); err == nil {
		t.Fatal("an unconfigured client must not attempt a request")
	}
}

// TestEnsureLoadedHonoursContext: the waits used to be bare time.Sleep, so a cancelled
// request still burned the whole window.
func TestEnsureLoadedHonoursContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always "not loaded", so EnsureLoaded keeps waiting.
		_, _ = w.Write([]byte(`{"hash":"aa","file_stats":[]}`))
	}))
	defer srv.Close()
	c := New(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	start := time.Now()
	if c.EnsureLoaded(ctx, "aa", "magnet:?xt=urn:btih:aa", "t", 30) {
		t.Fatal("EnsureLoaded should report false when the context is done")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("EnsureLoaded ignored the cancelled context and waited %v", elapsed)
	}
}

func TestWaitConnectableHonoursContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hash":"aa","connected_seeders":0,"active_peers":0}`))
	}))
	defer srv.Close()
	c := New(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if c.WaitConnectable(ctx, "aa", 30) {
		t.Fatal("WaitConnectable should be false with no peers")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("WaitConnectable ignored the context deadline and waited %v", elapsed)
	}
}

// ── Episode / file selection ──────────────────────────────────────────────────

func TestEpisodeFileIndex(t *testing.T) {
	const mb = int64(1024 * 1024)
	cases := []struct {
		name        string
		files       []FileInfo
		season, ep  int
		wantIndex   int
		wantMatched bool
	}{
		{
			name: "confident SxxEyy filename match in a season pack",
			files: []FileInfo{
				{ID: 1, Path: "Show/Show.S01E01.1080p.mkv", Length: 900 * mb},
				{ID: 2, Path: "Show/Show.S01E02.1080p.mkv", Length: 950 * mb},
				{ID: 3, Path: "Show/Show.S01E03.1080p.mkv", Length: 910 * mb},
			},
			season: 1, ep: 2, wantIndex: 2, wantMatched: true,
		},
		{
			name: "alternate 1x05 numbering",
			files: []FileInfo{
				{ID: 1, Path: "Show - 1x04.mkv", Length: 800 * mb},
				{ID: 2, Path: "Show - 1x05.mkv", Length: 810 * mb},
			},
			season: 1, ep: 5, wantIndex: 2, wantMatched: true,
		},
		{
			name: "lowercase s1e5 shorthand",
			files: []FileInfo{
				{ID: 7, Path: "show.s1e5.web.mp4", Length: 700 * mb},
			},
			season: 1, ep: 5, wantIndex: 7, wantMatched: true,
		},
		{
			name: "a single video file with no token is trusted",
			files: []FileInfo{
				{ID: 1, Path: "readme.txt", Length: 1024},
				{ID: 2, Path: "episode.mkv", Length: 900 * mb},
			},
			season: 3, ep: 4, wantIndex: 2, wantMatched: true,
		},
		{
			name: "multiple videos with no match is NOT confident",
			files: []FileInfo{
				{ID: 1, Path: "Show.S02E01.mkv", Length: 900 * mb},
				{ID: 2, Path: "Show.S02E02.mkv", Length: 950 * mb},
			},
			// Asking for an episode that is not in the pack: caller must reject it
			// rather than stream the largest file and hope.
			season: 2, ep: 9, wantIndex: 2, wantMatched: false,
		},
		{
			name: "anime dash numbering must not match a SxxEyy request",
			files: []FileInfo{
				{ID: 1, Path: "[Subs] Show - 05 [1080p].mkv", Length: 500 * mb},
				{ID: 2, Path: "[Subs] Show - 06 [1080p].mkv", Length: 510 * mb},
			},
			season: 1, ep: 5, wantIndex: 2, wantMatched: false,
		},
		{
			name: "the largest matching file wins when a sample shares the token",
			files: []FileInfo{
				{ID: 1, Path: "Show.S01E01.sample.mkv", Length: 20 * mb},
				{ID: 2, Path: "Show.S01E01.1080p.mkv", Length: 900 * mb},
			},
			season: 1, ep: 1, wantIndex: 2, wantMatched: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx, matched := EpisodeFileIndex(tc.files, tc.season, tc.ep)
			if idx != tc.wantIndex || matched != tc.wantMatched {
				t.Errorf("EpisodeFileIndex = (%d, %v), want (%d, %v)", idx, matched, tc.wantIndex, tc.wantMatched)
			}
		})
	}
}

func TestBestFileIndex(t *testing.T) {
	const mb = int64(1024 * 1024)
	files := []FileInfo{
		{ID: 1, Path: "sample.mkv", Length: 20 * mb},
		{ID: 2, Path: "movie.mkv", Length: 4000 * mb},
		{ID: 3, Path: "subs.srt", Length: 40},
	}
	if got := BestFileIndex(files); got != 2 {
		t.Errorf("BestFileIndex = %d, want 2", got)
	}
	if got := BestFileIndex(nil); got != 0 {
		t.Errorf("BestFileIndex(nil) = %d, want 0", got)
	}
}

func TestMatchesEpisode(t *testing.T) {
	cases := []struct {
		name       string
		season, ep int
		want       bool
	}{
		{"Show.S01E05.1080p.WEB.mkv", 1, 5, true},
		{"Show 1x05.mkv", 1, 5, true},
		{"Show.s1e5.mkv", 1, 5, true},
		{"Show.S01.E05.mkv", 1, 5, true},
		{"Show S01 E05.mkv", 1, 5, true},
		{"Show.S01E05.mkv", 1, 6, false},
		{"[Subs] Show - 05.mkv", 1, 5, false},
		{"Show.S01E15.mkv", 1, 5, false},
	}
	for _, tc := range cases {
		if got := MatchesEpisode(tc.name, tc.season, tc.ep); got != tc.want {
			t.Errorf("MatchesEpisode(%q, %d, %d) = %v, want %v", tc.name, tc.season, tc.ep, got, tc.want)
		}
	}
}

// TestMatchesEpisodeShortTokenCollision is the regression test for the wrong-episode bug.
//
// The matcher used to be strings.Contains over fmt.Sprintf'd patterns, and "s1e5" is a
// prefix of "S1E50".."S1E59" (likewise "s1e1" of "S1E10".."S1E19"). Because the same
// function filters candidate RELEASES and selects the FILE inside a season pack, asking
// for episode 5 could hand back episode 50 and report it as a confident match — the
// viewer just gets the wrong episode with nothing logged.
func TestMatchesEpisodeShortTokenCollision(t *testing.T) {
	cases := []struct {
		name       string
		season, ep int
		want       bool
	}{
		// The collision itself.
		{"Show.S1E50.1080p.mkv", 1, 5, false},
		{"Show.S1E59.1080p.mkv", 1, 5, false},
		{"Show.S1E5.1080p.mkv", 1, 5, true},
		{"Show.S1E10.mkv", 1, 1, false},
		{"Show.S1E19.mkv", 1, 1, false},
		{"Show.S1E1.mkv", 1, 1, true},
		// Zero padding on either side is still equivalent.
		{"Show.S01E05.mkv", 1, 5, true},
		{"Show.s001e005.mkv", 1, 5, true},
		{"Show.S1E05.mkv", 1, 5, true},
		// A digit before the season number must not be swallowed: "1920x1080" contains
		// the substring "20x108", which is season 20 episode 108 to a naive matcher.
		{"Show.1920x1080.mkv", 20, 108, false},
		{"Show.1x05.mkv", 1, 5, true},
		{"Show.11x05.mkv", 1, 5, false},
		{"Show.S11E05.mkv", 1, 5, false},
		{"Show.S21E05.mkv", 1, 5, false},
		// Letters after the token are still allowed on purpose: these are real forms
		// that genuinely do contain the requested episode.
		{"Show.S01E05v2.1080p.mkv", 1, 5, true},
		{"Show.S01E05E06.1080p.mkv", 1, 5, true},
		{"Show.S01E05-E06.1080p.mkv", 1, 5, true},
		// The double-episode above must not make episode 5 match a request for 50.
		{"Show.S01E05E06.1080p.mkv", 1, 50, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesEpisode(tc.name, tc.season, tc.ep); got != tc.want {
				t.Errorf("MatchesEpisode(%q, %d, %d) = %v, want %v",
					tc.name, tc.season, tc.ep, got, tc.want)
			}
		})
	}
}

// TestEpisodeFileIndexIgnoresPackFolder is the regression test for the pack-folder
// poisoning: the episode token in a DIRECTORY component used to satisfy the match for
// every file underneath it, so a request for E01 in "Show.S01E01-E10.COMPLETE/" matched
// all ten files, took the largest, and returned matched=true.
func TestEpisodeFileIndexIgnoresPackFolder(t *testing.T) {
	const mb = int64(1024 * 1024)
	pack := []FileInfo{
		{ID: 1, Path: "Show.S01E01-E10.COMPLETE.1080p/Show.S01E07.1080p.mkv", Length: 1400 * mb},
		{ID: 2, Path: "Show.S01E01-E10.COMPLETE.1080p/Show.S01E08.1080p.mkv", Length: 900 * mb},
		{ID: 3, Path: "Show.S01E01-E10.COMPLETE.1080p/Show.S01E09.1080p.mkv", Length: 950 * mb},
	}
	// Episode 1 is not actually in this pack, whatever the folder claims.
	if idx, matched := EpisodeFileIndex(pack, 1, 1); matched {
		t.Errorf("EpisodeFileIndex = (%d, true); the folder name must not make every "+
			"file a confident match for an episode the pack does not contain", idx)
	}
	// An episode that IS present still resolves to its own file.
	if idx, matched := EpisodeFileIndex(pack, 1, 8); idx != 2 || !matched {
		t.Errorf("EpisodeFileIndex = (%d, %v), want (2, true)", idx, matched)
	}
}

// The directory is still consulted when the filenames say nothing — but only when it
// points at exactly one file. More than one file matching on its path alone means the
// token came from a shared ancestor, which carries no information about which to play.
func TestEpisodeFileIndexDirectoryFallback(t *testing.T) {
	const mb = int64(1024 * 1024)

	t.Run("token lives in the directory and identifies one file", func(t *testing.T) {
		files := []FileInfo{
			{ID: 1, Path: "Show S01/E04.mkv", Length: 800 * mb},
			{ID: 2, Path: "Show S01/E05.mkv", Length: 810 * mb},
		}
		if idx, matched := EpisodeFileIndex(files, 1, 5); idx != 2 || !matched {
			t.Errorf("EpisodeFileIndex = (%d, %v), want (2, true)", idx, matched)
		}
	})

	t.Run("an ambiguous directory token is not an answer", func(t *testing.T) {
		files := []FileInfo{
			{ID: 1, Path: "Show.S01E05.Complete/part1.mkv", Length: 800 * mb},
			{ID: 2, Path: "Show.S01E05.Complete/part2.mkv", Length: 810 * mb},
		}
		if idx, matched := EpisodeFileIndex(files, 1, 5); matched {
			t.Errorf("EpisodeFileIndex = (%d, true); two files matching only via a "+
				"shared folder is not a confident answer", idx)
		}
	})
}

// TestEpisodeFileIndexFileZero: file IDs start at 0 in some torrents, and the old code
// used `bestID != 0` as its "did we match anything" test — so a correct match on the
// first file was reported as no match, and the caller fell through to a guess.
func TestEpisodeFileIndexFileZero(t *testing.T) {
	const mb = int64(1024 * 1024)
	files := []FileInfo{
		{ID: 0, Path: "Show.S01E05.1080p.mkv", Length: 900 * mb},
		{ID: 1, Path: "Show.S01E06.1080p.mkv", Length: 950 * mb},
	}
	idx, matched := EpisodeFileIndex(files, 1, 5)
	if idx != 0 || !matched {
		t.Errorf("EpisodeFileIndex = (%d, %v), want (0, true) — file ID 0 is a real file",
			idx, matched)
	}
}
