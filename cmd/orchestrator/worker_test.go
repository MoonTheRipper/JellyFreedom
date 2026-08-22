package main

import (
	"strings"
	"testing"

	"jellyfreedom/internal/indexer"
	"jellyfreedom/internal/picker"
	"jellyfreedom/internal/torrserver"
)

func TestParseRangeStart(t *testing.T) {
	cases := []struct {
		header string
		want   int64
	}{
		{"", 0},
		{"bytes=0-", 0},
		{"bytes=1048576-", 1048576},
		{"bytes=500-999", 500},
		{"bytes=-500", 0},    // suffix range: no explicit start
		{"items=100-200", 0}, // wrong unit
		{"bytes=abc-def", 0}, // unparseable
		{"bytes=", 0},
		{"bytes=9223372036854775807-", 9223372036854775807},
	}
	for _, tc := range cases {
		if got := parseRangeStart(tc.header); got != tc.want {
			t.Errorf("parseRangeStart(%q) = %d, want %d", tc.header, got, tc.want)
		}
	}
}

func TestValidateVideoFile(t *testing.T) {
	const mb = int64(1024 * 1024)
	cases := []struct {
		name      string
		file      *torrserver.FileInfo
		mediaType string
		wantErr   bool
		errSubstr string
	}{
		{name: "nil file is accepted (nothing to judge)", file: nil, mediaType: "movie"},
		{name: "empty path is accepted", file: &torrserver.FileInfo{Path: ""}, mediaType: "movie"},
		{
			name: "a normal movie passes",
			file: &torrserver.FileInfo{Path: "Movie.2020.1080p.mkv", Length: 4000 * mb}, mediaType: "movie",
		},
		{
			name:      "an executable disguised as a release is rejected",
			file:      &torrserver.FileInfo{Path: "Movie.2024.1080p.mkv.exe", Length: 4000 * mb},
			mediaType: "movie", wantErr: true, errSubstr: "not a video",
		},
		{
			name:      "an .lnk fake release is rejected",
			file:      &torrserver.FileInfo{Path: "watch-online.lnk", Length: 4000 * mb},
			mediaType: "movie", wantErr: true, errSubstr: "not a video",
		},
		{
			name:      "a movie under the 200 MB floor is rejected as a fake",
			file:      &torrserver.FileInfo{Path: "Movie.mkv", Length: 100 * mb},
			mediaType: "movie", wantErr: true, errSubstr: "too small",
		},
		{
			name: "the SAME size is fine for TV, whose floor is 50 MB",
			file: &torrserver.FileInfo{Path: "Show.S01E01.mkv", Length: 100 * mb}, mediaType: "tv",
		},
		{
			name:      "a TV file under the 50 MB floor is rejected",
			file:      &torrserver.FileInfo{Path: "Show.S01E01.mkv", Length: 10 * mb},
			mediaType: "tv", wantErr: true, errSubstr: "too small",
		},
		{
			name: "an unknown length (0) is not treated as too small",
			file: &torrserver.FileInfo{Path: "Movie.mkv", Length: 0}, mediaType: "movie",
		},
		{
			name: "extension matching is case-insensitive",
			file: &torrserver.FileInfo{Path: "Movie.2020.1080p.MKV", Length: 4000 * mb}, mediaType: "movie",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVideoFile(tc.file, tc.mediaType)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateVideoFile = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("error %q does not mention %q", err, tc.errSubstr)
			}
		})
	}
}

func TestResolvableSeedersRequiresTitleMatch(t *testing.T) {
	cfg := picker.Config{MinSeeders: 5, MaxSizeGB: 100, RejectCAM: true}

	t.Run("an unrelated well-seeded release must NOT keep an item alive", func(t *testing.T) {
		// This is the bug: the health check passed empty title/year, which disables
		// title matching, so ANY result above the seeder floor kept an item "ready"
		// forever — a dead title stayed green as long as the indexer returned anything.
		releases := []indexer.Release{
			{Title: "WWE Monday Night Raw 2024", Seeders: 900},
		}
		if got := resolvableSeeders(releases, cfg, "", "Inception", "2010"); got != -1 {
			t.Fatalf("resolvableSeeders = %d, want -1 (not resolvable)", got)
		}
	})

	t.Run("a matching release keeps it alive", func(t *testing.T) {
		releases := []indexer.Release{
			{Title: "Inception.2010.1080p.BluRay.x264", Seeders: 120},
		}
		if got := resolvableSeeders(releases, cfg, "", "Inception", "2010"); got != 120 {
			t.Fatalf("resolvableSeeders = %d, want 120", got)
		}
	})

	t.Run("the exact cached release's own seeder count wins", func(t *testing.T) {
		releases := []indexer.Release{
			{Title: "Inception.2010.2160p.REMUX", InfoHash: "zzz", Seeders: 400},
			{Title: "Inception.2010.1080p.BluRay.x264", InfoHash: "abc", Seeders: 30},
		}
		if got := resolvableSeeders(releases, cfg, "abc", "Inception", "2010"); got != 30 {
			t.Fatalf("resolvableSeeders = %d, want 30 (the cached release's own count)", got)
		}
	})

	t.Run("a CAM-only result set is not resolvable when CAMs are rejected", func(t *testing.T) {
		releases := []indexer.Release{
			{Title: "Inception.2010.HDCAM", Seeders: 500},
		}
		if got := resolvableSeeders(releases, cfg, "", "Inception", "2010"); got != -1 {
			t.Fatalf("resolvableSeeders = %d, want -1", got)
		}
	})

	t.Run("no releases at all is not resolvable", func(t *testing.T) {
		if got := resolvableSeeders(nil, cfg, "", "Inception", "2010"); got != -1 {
			t.Fatalf("resolvableSeeders = %d, want -1", got)
		}
	})
}

func TestNoReleaseErrorDiagnosis(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)
	cfg := picker.Config{MinSeeders: 10, MaxSizeGB: 20, RejectCAM: true}

	t.Run("no results at all", func(t *testing.T) {
		e := newNoReleaseError(nil, cfg, "Inception", "2010")
		if !strings.Contains(e.Error(), "No releases were found") {
			t.Errorf("message = %q", e.Error())
		}
		if !strings.Contains(e.JSON(), `"reason":"no_release"`) {
			t.Errorf("JSON = %q", e.JSON())
		}
		if !strings.Contains(e.JSON(), `"total_found":0`) {
			t.Errorf("JSON should report total_found: %q", e.JSON())
		}
	})

	t.Run("everything below the seeder floor names the setting", func(t *testing.T) {
		e := newNoReleaseError([]indexer.Release{
			{Title: "Inception 2010 1080p", Seeders: 1},
			{Title: "Inception 2010 720p", Seeders: 3},
		}, cfg, "Inception", "2010")
		msg := e.Error()
		if !strings.Contains(msg, "minimum of 10 seeders") {
			t.Errorf("message should name the threshold: %q", msg)
		}
		j := e.JSON()
		for _, want := range []string{`"min_seeders":10`, `"rejected_by":"min_seeders"`, `"total_found":2`} {
			if !strings.Contains(j, want) {
				t.Errorf("JSON missing %s: %s", want, j)
			}
		}
	})

	t.Run("everything is a camera rip", func(t *testing.T) {
		e := newNoReleaseError([]indexer.Release{
			{Title: "Inception 2010 HDCAM", Seeders: 500},
		}, cfg, "Inception", "2010")
		if !strings.Contains(e.Error(), "camera/telesync") {
			t.Errorf("message = %q", e.Error())
		}
	})

	t.Run("everything is oversized", func(t *testing.T) {
		e := newNoReleaseError([]indexer.Release{
			{Title: "Inception 2010 REMUX", Seeders: 50, SizeBytes: 80 * gb},
		}, cfg, "Inception", "2010")
		if !strings.Contains(e.Error(), "20 GB limit") {
			t.Errorf("message = %q", e.Error())
		}
	})

	t.Run("the candidate list is bounded", func(t *testing.T) {
		var many []indexer.Release
		for i := 0; i < 500; i++ {
			many = append(many, indexer.Release{Title: "Inception 2010", Seeders: 1})
		}
		e := newNoReleaseError(many, cfg, "Inception", "2010")
		if n := strings.Count(e.JSON(), `"rejected_by"`); n > maxDiagCandidates {
			t.Fatalf("stored %d candidates, want at most %d", n, maxDiagCandidates)
		}
		if !strings.Contains(e.JSON(), `"total_found":500`) {
			t.Error("total_found should still report the full count")
		}
	})
}

func TestPlayURLAndToken(t *testing.T) {
	// With no key loaded, playURL must still produce a usable (untokenised) URL.
	playKeyMu.Lock()
	playKey = nil
	playKeyMu.Unlock()
	if got := playURL("http://host:1990", "movie", 27205, 0, 0); got != "http://host:1990/play/movie/27205" {
		t.Fatalf("untokenised movie URL = %q", got)
	}

	playKeyMu.Lock()
	playKey = []byte("0123456789abcdef0123456789abcdef")
	playKeyMu.Unlock()

	t.Run("a movie URL carries a token", func(t *testing.T) {
		got := playURL("http://host:1990/", "movie", 27205, 0, 0)
		if !strings.HasPrefix(got, "http://host:1990/play/movie/27205?t=") {
			t.Fatalf("URL = %q", got)
		}
	})

	t.Run("a tv URL carries a token", func(t *testing.T) {
		got := playURL("http://host:1990", "tv", 1399, 2, 9)
		if !strings.HasPrefix(got, "http://host:1990/play/tv/1399/2/9?t=") {
			t.Fatalf("URL = %q", got)
		}
	})

	t.Run("a token authorises exactly one identity", func(t *testing.T) {
		tok := playToken("tv", 1399, 2, 9)
		if !validPlayToken(tok, "tv", 1399, 2, 9) {
			t.Fatal("the correct token was rejected")
		}
		// Every neighbouring identity must be refused.
		for _, bad := range []struct {
			mt            string
			tmdb, sea, ep int
		}{
			{"tv", 1399, 2, 10},
			{"tv", 1399, 3, 9},
			{"tv", 1400, 2, 9},
			{"movie", 1399, 2, 9},
		} {
			if validPlayToken(tok, bad.mt, bad.tmdb, bad.sea, bad.ep) {
				t.Errorf("a token for tv/1399/2/9 was accepted for %v", bad)
			}
		}
	})

	t.Run("empty and wrong tokens are refused", func(t *testing.T) {
		if validPlayToken("", "movie", 1, 0, 0) {
			t.Error("an empty token was accepted")
		}
		if validPlayToken("not-a-token", "movie", 1, 0, 0) {
			t.Error("a garbage token was accepted")
		}
	})

	t.Run("movie and tv identities never collide", func(t *testing.T) {
		if playIdentity("movie", 5, 0, 0) == playIdentity("tv", 5, 0, 0) {
			t.Fatal("movie and tv identities collide")
		}
	})
}
