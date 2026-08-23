package indexer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestParseResolution covers the field the picker's headline fix depends on. Resolution
// was not parsed at all, so nothing could score it — and a 480p rip with a big swarm beat
// everything else in the list.
func TestParseResolution(t *testing.T) {
	cases := map[string]string{
		// Explicit height tags, in the shapes real release titles use.
		"Movie.2024.2160p.WEB-DL.x265": Res2160p,
		"Movie.2024.1080p.BluRay.x264": Res1080p,
		"Movie.2024.720p.WEBRip.x264":  Res720p,
		"Movie.2024.576p.PAL.DVDRip":   Res576p,
		"Movie.2024.480p.DVDRip.XviD":  Res480p,
		"Movie.2024.360p.WEBRip":       Res360p,
		"Show.S01E01.1080i.HDTV.MPEG2": Res1080p,
		"Movie.2024.576i.DVD":          Res576p,
		// 1440p and 4320p are folded onto the nearest common rung rather than given
		// rungs of their own — inventing rungs would shift every "one step below the
		// target" decision.
		"Movie.2024.1440p.WEB-DL":    Res1080p,
		"Movie.2024.4320p.8K.WEB-DL": Res2160p,
		// Marketing spellings of 2160p.
		"Movie 2024 4K UHD BluRay REMUX": Res2160p,
		"Movie.2024.UHD.BluRay.x265":     Res2160p,
		// The WIDTHxHEIGHT form.
		"Movie.2024.1920x1080.x264": Res1080p,
		"Movie.2024.3840x2160.HEVC": Res2160p,
		"Movie.2024.1280x720.x264":  Res720p,
		"Movie 2024 854x480 XviD":   Res480p,
		// A cropped 2.39:1 encode is shorter than its nominal height.
		"Movie.2024.1920x800.x264": Res1080p,
		// Nothing to go on. "" means unknown, which the picker treats as "no evidence",
		// not as "bad".
		"Movie.2024.BluRay.x264": "",
		"Show.S01E05.HDTV.XviD":  "",
		// Episode numbering must not be read as a dimension pair.
		"Show.1x05.HDTV.XviD": "",
	}
	for title, want := range cases {
		if got := ParseResolution(title); got != want {
			t.Errorf("ParseResolution(%q) = %q, want %q", title, got, want)
		}
	}
}

// parsePublishDate must never cost us a result: several indexers send "" or a
// non-RFC3339 stamp, and decoding straight into a time.Time would fail the whole
// json.Decode and throw away every release in the response.
func TestParsePublishDate(t *testing.T) {
	cases := []struct {
		in       string
		wantZero bool
		wantYear int
	}{
		{"2024-03-15T10:30:00Z", false, 2024},
		{"2024-03-15T10:30:00.123456Z", false, 2024},
		{"2024-03-15T10:30:00+02:00", false, 2024},
		{"2024-03-15T10:30:00", false, 2024},
		{"2024-03-15 10:30:00", false, 2024},
		{"2024-03-15", false, 2024},
		{"  2024-03-15T10:30:00Z  ", false, 2024},
		{"", true, 0},
		{"yesterday", true, 0},
		{"0", true, 0},
	}
	for _, tc := range cases {
		got := parsePublishDate(tc.in)
		if got.IsZero() != tc.wantZero {
			t.Errorf("parsePublishDate(%q).IsZero() = %v, want %v", tc.in, got.IsZero(), tc.wantZero)
			continue
		}
		if !tc.wantZero && got.Year() != tc.wantYear {
			t.Errorf("parsePublishDate(%q).Year() = %d, want %d", tc.in, got.Year(), tc.wantYear)
		}
	}
}

func TestAgeDays(t *testing.T) {
	cases := []struct {
		name string
		rel  Release
		want int
	}{
		{"unknown publish date", Release{}, -1},
		{"today", Release{PublishDate: time.Now()}, 0},
		{"ten days ago", Release{PublishDate: time.Now().Add(-10 * 24 * time.Hour)}, 10},
		// A clock-skewed "future" release is 0 days old, never negative.
		{"clock skew", Release{PublishDate: time.Now().Add(48 * time.Hour)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rel.AgeDays(); got != tc.want {
				t.Errorf("AgeDays() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSearchParsesEveryField walks a realistic Prowlarr response through search() and
// checks what lands on the Release. The publish date and indexer name were simply not
// decoded before, and the resolution was never derived at all.
func TestSearchParsesEveryField(t *testing.T) {
	const hash = "aabbccddeeff00112233445566778899aabbccdd"
	body := []map[string]any{
		{
			"title":       "Movie.2024.2160p.WEB-DL.DDP5.1.HEVC-NTb.mkv",
			"infoHash":    hash,
			"size":        int64(18) * 1024 * 1024 * 1024,
			"seeders":     210,
			"leechers":    17,
			"indexer":     "TorrentLeech",
			"publishDate": "2024-03-15T10:30:00Z",
		},
		{
			// No usable hash anywhere: must be dropped rather than turned into a
			// magnet nobody can resolve.
			"title":   "Movie.2024.1080p.WEB-DL.x264",
			"seeders": 900,
		},
		{
			// An unreadable publish date must cost the date, not the release.
			"title":       "Movie.2024.720p.WEBRip.x264",
			"infoHash":    "00112233445566778899aabbccddeeff00112233",
			"seeders":     40,
			"indexer":     "1337x",
			"publishDate": "not a date",
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "secret" {
			t.Errorf("X-Api-Key header = %q; the key must travel in the header, never the query", got)
		}
		if r.URL.Query().Get("apikey") != "" {
			t.Error("the API key must never appear in the query string")
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	releases, err := New(srv.URL, "secret").Search("movie", []int{CatMovies})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(releases) != 2 {
		t.Fatalf("Search returned %d releases, want 2 (the hashless result is dropped)", len(releases))
	}

	first := releases[0]
	if first.Resolution != Res2160p {
		t.Errorf("Resolution = %q, want %q", first.Resolution, Res2160p)
	}
	if first.VideoCodec != "h265" {
		t.Errorf("VideoCodec = %q, want h265 (HEVC is folded onto h265)", first.VideoCodec)
	}
	if first.AudioCodec != "eac3" {
		t.Errorf("AudioCodec = %q, want eac3 (DDP)", first.AudioCodec)
	}
	if first.Container != "mkv" {
		t.Errorf("Container = %q, want mkv", first.Container)
	}
	if first.Leechers != 17 {
		t.Errorf("Leechers = %d, want 17", first.Leechers)
	}
	if first.Indexer != "TorrentLeech" {
		t.Errorf("Indexer = %q, want TorrentLeech", first.Indexer)
	}
	if first.PublishDate.IsZero() || first.PublishDate.Year() != 2024 {
		t.Errorf("PublishDate = %v, want 2024-03-15", first.PublishDate)
	}

	second := releases[1]
	if second.Resolution != Res720p {
		t.Errorf("Resolution = %q, want %q", second.Resolution, Res720p)
	}
	if !second.PublishDate.IsZero() {
		t.Errorf("PublishDate = %v, want the zero time for an unreadable date", second.PublishDate)
	}
	if second.Indexer != "1337x" {
		t.Errorf("Indexer = %q, want 1337x", second.Indexer)
	}
}
