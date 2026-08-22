package picker

import (
	"testing"

	"jellyfreedom/internal/indexer"
)

var testCfg = Config{
	MinSeeders:        5,
	PreferVideoCodecs: []string{"h264", "h265", "hevc"},
	PreferAudioCodecs: []string{"aac", "ac3", "eac3"},
	PreferContainers:  []string{"mp4", "mkv"},
	MaxSizeGB:         20,
}

func TestBest_RejectsLowSeeders(t *testing.T) {
	releases := []indexer.Release{
		{Title: "Movie x264", Seeders: 2, VideoCodec: "h264"},
	}
	if got := Best(releases, testCfg); got != nil {
		t.Errorf("expected nil for low seeders, got %+v", got)
	}
}

func TestBest_RejectsOversized(t *testing.T) {
	releases := []indexer.Release{
		{Title: "Movie x264", Seeders: 50, VideoCodec: "h264", SizeBytes: 25 * 1024 * 1024 * 1024},
	}
	if got := Best(releases, testCfg); got != nil {
		t.Errorf("expected nil for oversized release, got %+v", got)
	}
}

func TestBest_PrefersH264OverUnknown(t *testing.T) {
	releases := []indexer.Release{
		{Title: "Movie", Seeders: 20},
		{Title: "Movie x264", Seeders: 20, VideoCodec: "h264"},
	}
	got := Best(releases, testCfg)
	if got == nil || got.VideoCodec != "h264" {
		t.Errorf("expected h264 release, got %+v", got)
	}
}

func TestBest_MoreSeedersBreaksTie(t *testing.T) {
	releases := []indexer.Release{
		{Title: "Movie x264 A", Seeders: 15, VideoCodec: "h264", Container: "mkv"},
		{Title: "Movie x264 B", Seeders: 60, VideoCodec: "h264", Container: "mkv"},
	}
	got := Best(releases, testCfg)
	if got == nil || got.Title != "Movie x264 B" {
		t.Errorf("expected release B with more seeders, got %+v", got)
	}
}

func TestBest_PreferredCodecBeatsMoreSeeders(t *testing.T) {
	releases := []indexer.Release{
		// Many seeders but unknown codec
		{Title: "Movie CAM", Seeders: 200},
		// Fewer seeders but preferred codec
		{Title: "Movie x264", Seeders: 10, VideoCodec: "h264"},
	}
	got := Best(releases, testCfg)
	if got == nil || got.VideoCodec != "h264" {
		t.Errorf("expected h264 release to win, got %+v", got)
	}
}

// ── CAM handling ──────────────────────────────────────────────────────────────

var camCfg = Config{
	MinSeeders:        5,
	PreferVideoCodecs: []string{"h264", "h265"},
	MaxSizeGB:         20,
	RejectCAM:         true,
}

// TestBest_AllCAMWithRejectOff is the regression test for the bestScore seed.
//
// bestScore started at -1 while the CAM penalty is -10000, so when EVERY candidate was a
// camera rip, no release could ever beat the seed and Best returned nil — even with
// RejectCAM off, i.e. even when the user had explicitly said camera rips were acceptable.
func TestBest_AllCAMWithRejectOff(t *testing.T) {
	cfg := camCfg
	cfg.RejectCAM = false
	releases := []indexer.Release{
		{Title: "Movie 2024 HDCAM x264", Seeders: 300, VideoCodec: "h264"},
		{Title: "Movie 2024 TS x264", Seeders: 100, VideoCodec: "h264"},
	}
	got := Best(releases, cfg)
	if got == nil {
		t.Fatal("with RejectCAM off and only CAM releases available, Best must still pick one")
	}
	if got.Seeders != 300 {
		t.Errorf("picked %q (%d seeders), want the best-seeded CAM", got.Title, got.Seeders)
	}
}

// With RejectCAM ON, an all-CAM result set must yield nothing to auto-pick.
func TestBest_AllCAMWithRejectOn(t *testing.T) {
	releases := []indexer.Release{
		{Title: "Movie 2024 HDCAM x264", Seeders: 300, VideoCodec: "h264"},
		{Title: "Movie 2024 TELESYNC", Seeders: 100},
	}
	if got := Best(releases, camCfg); got != nil {
		t.Fatalf("RejectCAM is on but Best picked a camera rip: %+v", got)
	}
}

// A CAM must still be LISTED (flagged) so a user can consciously override — it just
// must never be the auto-pick.
func TestScore_ListsCAMButNeverPicksIt(t *testing.T) {
	releases := []indexer.Release{
		{Title: "Movie 2024 HDCAM x264", Seeders: 900, VideoCodec: "h264"},
		{Title: "Movie 2024 1080p WEB-DL x264", Seeders: 10, VideoCodec: "h264"},
	}
	scored := Score(releases, camCfg, "", "")
	if len(scored) != 2 {
		t.Fatalf("Score returned %d releases, want both listed", len(scored))
	}
	for _, sr := range scored {
		if sr.IsCAM && sr.IsBest {
			t.Errorf("a camera rip was marked best under RejectCAM: %q", sr.Title)
		}
		if sr.IsCAM && sr.Quality != "cam" {
			t.Errorf("%q flagged IsCAM but quality=%q", sr.Title, sr.Quality)
		}
	}
	// The non-CAM release, despite having 90x fewer seeders, must be the pick.
	best := Best(releases, camCfg)
	if best == nil || best.Seeders != 10 {
		t.Fatalf("best = %+v, want the WEB-DL release", best)
	}
}

// ── Boundaries ────────────────────────────────────────────────────────────────

func TestMinSeedersBoundary(t *testing.T) {
	cfg := Config{MinSeeders: 10, MaxSizeGB: 20}
	cases := []struct {
		seeders int
		want    bool
	}{
		{9, false}, // below the floor — excluded
		{10, true}, // exactly the floor — INCLUDED (the check is `<`, not `<=`)
		{11, true},
		{0, false},
	}
	for _, tc := range cases {
		got := Best([]indexer.Release{{Title: "M", Seeders: tc.seeders}}, cfg)
		if (got != nil) != tc.want {
			t.Errorf("seeders=%d: picked=%v, want %v", tc.seeders, got != nil, tc.want)
		}
	}
}

func TestMaxSizeBoundary(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)
	cfg := Config{MinSeeders: 1, MaxSizeGB: 20}
	cases := []struct {
		name  string
		bytes int64
		want  bool
	}{
		{"just under the limit", 20*gb - 1, true},
		{"exactly the limit", 20 * gb, true}, // the check is `>`, so equal passes
		{"one byte over", 20*gb + 1, false},
		{"size unknown (zero)", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Best([]indexer.Release{{Title: "M", Seeders: 50, SizeBytes: tc.bytes}}, cfg)
			if (got != nil) != tc.want {
				t.Errorf("picked=%v, want %v", got != nil, tc.want)
			}
		})
	}
}

func TestMaxSizeUnlimitedWhenZero(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)
	cfg := Config{MinSeeders: 1, MaxSizeGB: 0} // 0 = no limit
	if got := Best([]indexer.Release{{Title: "M", Seeders: 50, SizeBytes: 500 * gb}}, cfg); got == nil {
		t.Fatal("MaxSizeGB=0 must mean unlimited")
	}
}

// ── Title matching ────────────────────────────────────────────────────────────

func TestTitleMatch(t *testing.T) {
	cases := []struct {
		name        string
		release     string
		title, year string
		want        bool
	}{
		{"exact with year", "Inception.2010.1080p.BluRay.x264", "Inception", "2010", true},
		{"exact without the year in the release", "Inception.1080p.BluRay.x264", "Inception", "2010", true},
		{"wrong film with the right year", "WWE.Raw.2010.HDTV", "Inception", "2010", false},
		{"multiword title, all words present", "The.Dark.Knight.Rises.2012.1080p", "The Dark Knight Rises", "2012", true},
		{"multiword title, a word missing and no year", "The.Dark.Knight.1080p", "The Dark Knight Rises", "2012", false},
		{"noise words are stripped before comparing", "Inception.2010.REMUX.HDR.ATMOS.x265", "Inception", "2010", true},
		{"punctuation differences do not matter", "Spider-Man.No.Way.Home.2021", "Spider Man: No Way Home", "2021", true},
		{"empty movie title matches anything", "Whatever.2020", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TitleMatch(tc.release, tc.title, tc.year); got != tc.want {
				t.Errorf("TitleMatch(%q, %q, %q) = %v, want %v", tc.release, tc.title, tc.year, got, tc.want)
			}
		})
	}
}

// Score's title argument is optional: passing "" disables the check entirely, which is
// exactly the mistake the library health check was making.
func TestScore_EmptyTitleDisablesMatching(t *testing.T) {
	releases := []indexer.Release{{Title: "Completely Unrelated Thing", Seeders: 100}}

	withTitle := Score(releases, Config{MinSeeders: 1}, "Inception", "2010")
	if len(withTitle) != 1 || withTitle[0].TitleMatch {
		t.Errorf("with a title, an unrelated release must have TitleMatch=false")
	}
	withoutTitle := Score(releases, Config{MinSeeders: 1}, "", "")
	if len(withoutTitle) != 1 || !withoutTitle[0].TitleMatch {
		t.Errorf("with no title, TitleMatch must default to true (matching is skipped)")
	}
}

func TestScoreSortsBestFirst(t *testing.T) {
	releases := []indexer.Release{
		{Title: "A", Seeders: 10},
		{Title: "B", Seeders: 900},
		{Title: "C", Seeders: 100},
	}
	scored := Score(releases, Config{MinSeeders: 1}, "", "")
	for i := 1; i < len(scored); i++ {
		if scored[i].Score > scored[i-1].Score {
			t.Fatalf("Score is not sorted best-first: %+v", scored)
		}
	}
	if scored[0].Title != "B" {
		t.Errorf("first = %q, want B (most seeders)", scored[0].Title)
	}
}

// ── Rejection reasons (feeds the queue diagnosis endpoint) ────────────────────

func TestRejectedBy(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)
	cfg := Config{MinSeeders: 10, MaxSizeGB: 20, RejectCAM: true}
	cases := []struct {
		name     string
		rel      indexer.Release
		reqTitle bool
		want     string
	}{
		{"passes everything", indexer.Release{Title: "Inception 2010 1080p", Seeders: 50, SizeBytes: 5 * gb}, true, ""},
		{"too few seeders", indexer.Release{Title: "Inception 2010", Seeders: 2}, true, RejectMinSeeders},
		{"too large", indexer.Release{Title: "Inception 2010", Seeders: 50, SizeBytes: 50 * gb}, true, RejectMaxSize},
		{"camera rip", indexer.Release{Title: "Inception 2010 HDCAM", Seeders: 50}, true, RejectCAMRule},
		{"wrong title, and the caller requires a match", indexer.Release{Title: "WWE Raw 2010", Seeders: 50}, true, RejectTitle},
		{"wrong title, but the caller does NOT require a match", indexer.Release{Title: "WWE Raw 2010", Seeders: 50}, false, ""},
		// Ordering: seeders is checked before size, so the first failure is reported.
		{"reports the FIRST failing rule", indexer.Release{Title: "Inception 2010 HDCAM", Seeders: 1, SizeBytes: 90 * gb}, true, RejectMinSeeders},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RejectedBy(tc.rel, cfg, "Inception", "2010", tc.reqTitle)
			if got != tc.want {
				t.Errorf("RejectedBy = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReleaseQuality(t *testing.T) {
	cases := map[string]string{
		"Movie.2024.HDCAM.x264":      "cam",
		"Movie.2024.TELESYNC":        "cam",
		"Movie.2024.1080p.REMUX":     "remux",
		"Movie.2024.1080p.BluRay":    "bluray",
		"Movie.2024.1080p.WEB-DL":    "webdl",
		"Movie.2024.1080p.WEBRip":    "webrip",
		"Movie.2024.720p.HDTV":       "hdtv",
		"Movie.2024.DVDRip":          "dvd",
		"Movie.2024.Something.Weird": "",
	}
	for title, want := range cases {
		if got := ReleaseQuality(title); got != want {
			t.Errorf("ReleaseQuality(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestScore_SkipsNothingWhenAllPass(t *testing.T) {
	releases := []indexer.Release{
		{Title: "A", Seeders: 20}, {Title: "B", Seeders: 30}, {Title: "C", Seeders: 40},
	}
	if got := Score(releases, Config{MinSeeders: 1}, "", ""); len(got) != 3 {
		t.Fatalf("Score dropped releases: got %d, want 3", len(got))
	}
}

func TestScore_EmptyInput(t *testing.T) {
	if got := Score(nil, Config{MinSeeders: 1}, "", ""); len(got) != 0 {
		t.Fatalf("Score(nil) = %v, want empty", got)
	}
	if got := Best(nil, Config{MinSeeders: 1}); got != nil {
		t.Fatalf("Best(nil) = %+v, want nil", got)
	}
}
