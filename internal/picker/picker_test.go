package picker

import (
	"fmt"
	"strings"
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

// ── normalise: whole-token noise removal ──────────────────────────────────────

// TestNormaliseDropsWholeTokensOnly is the regression test for the substring mangling.
//
// normalise used to run strings.ReplaceAll over the noise vocabulary, and that
// vocabulary contains "ts", "cam" and "cut" — so it deleted those letters wherever they
// occurred, not just where they were tags. "Ghosts" became "ghos", "Camelot" became
// "elot", "Cutthroat Island" became "throat island". The user-visible symptom is a
// title that matches the wrong show, or matches nothing at all, with no way to tell why.
func TestNormaliseDropsWholeTokensOnly(t *testing.T) {
	cases := map[string]string{
		// The mangled words, all of which must survive intact.
		"Ghosts":           "ghosts",
		"Camelot":          "camelot",
		"Cutthroat Island": "cutthroat island",
		"The Hurt Locker":  "the hurt locker",
		"Tsunami":          "tsunami",
		"Scream":           "scream",
		// Real tags are still removed, as whole tokens.
		"Inception.2010.1080p.BluRay.x264":    "inception 2010",
		"Inception.2010.REMUX.HDR.ATMOS.x265": "inception 2010",
		"Movie.2024.WEB-DL.DDP5.1":            "movie 2024 ddp5 1",
		"Movie.2024.Blu-Ray.1080p":            "movie 2024",
		"Movie 2024 Directors Cut Extended":   "movie 2024",
		// "blu ray" is a PHRASE, so the film "Ray" keeps its only word — matching the
		// two tokens separately would have left it with nothing to compare on.
		"Ray":            "ray",
		"Ray 2004 1080p": "ray 2004",
		"Blu-Ray":        "",
	}
	for in, want := range cases {
		if got := normalise(in); got != want {
			t.Errorf("normalise(%q) = %q, want %q", in, got, want)
		}
	}
}

// The substring mangling did not just look wrong, it matched the wrong show: both sides
// were mangled the same way, so "Ghosts" collapsed to "ghos" and happily matched a
// release of the unrelated film "Ghost".
func TestTitleMatchNoLongerCollapsesDistinctTitles(t *testing.T) {
	cases := []struct {
		name        string
		release     string
		title, year string
		want        bool
	}{
		{"Ghosts must not match Ghost", "Ghost.1990.1080p.BluRay.x264", "Ghosts", "2019", false},
		{"Ghosts matches its own release", "Ghosts.2019.S01E01.1080p.HDTV.x264", "Ghosts", "2019", true},
		{"Cats must not match Catwoman", "Catwoman.2004.1080p.BluRay", "Cats", "2019", false},
		{"Camelot matches its own release", "Camelot.2011.S01E01.720p.HDTV.x264", "Camelot", "2011", true},
		{"Cutthroat Island matches its own release", "Cutthroat.Island.1995.1080p.BluRay.x264", "Cutthroat Island", "1995", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TitleMatch(tc.release, tc.title, tc.year); got != tc.want {
				t.Errorf("TitleMatch(%q, %q, %q) = %v, want %v",
					tc.release, tc.title, tc.year, got, tc.want)
			}
		})
	}
}

// ── CAM detection must not eat legitimate titles ──────────────────────────────

// TestIsCAMDoesNotFalsePositive covers the 2018 film "Cam", which was literally
// unpickable: \bcam\b matched its own name in every release title, so every release
// scored -10000 and RejectCAM threw all of them away.
func TestIsCAMDoesNotFalsePositive(t *testing.T) {
	cases := []struct {
		release, knownTitle string
		want                bool
	}{
		// "Cam" (2018) — a real film, in the four shapes its releases actually take.
		{"Cam.2018.1080p.WEB.H264-DEFLATE", "Cam", false},
		{"Cam.2018.1080p.NF.WEB-DL.DDP5.1.x264", "Cam", false},
		{"Cam 2018 720p WEBRip x264", "", false},
		{"Cam.2018.1080p.BluRay.x264", "", false},
		// ".ts" is a legal container extension, not a telesync — the stated digital
		// source settles it.
		{"Show.S01E05.1080p.HDTV.x264.ts", "", false},
		{"Movie.2024.1080p.WEB-DL.x264.ts", "Movie", false},
		// Other titles that collide with the short tags.
		{"Scream.2022.1080p.BluRay.x264", "Scream", false},
		{"Camelot.2011.S01E01.720p.HDTV.x264", "Camelot", false},
		// Genuine camera rips must still be caught, with or without a known title.
		{"Movie.2024.HDCAM.x264", "Movie", true},
		{"Movie.2024.CAMRip.XviD", "Movie", true},
		{"Movie.2024.TELESYNC.x264", "Movie", true},
		{"Movie.2024.TS.x264", "Movie", true},
		{"Movie.2024.TC.x264", "", true},
		{"Movie.2024.DVDSCR.XviD", "", true},
		{"Movie 2024 HDTS", "", true},
		// "HDRip" is too vague to vouch for a title that also says TS.
		{"Movie.2024.TS.HDRip.x264", "Movie", true},
		{"Movie.2024.1080p.HDRip.x264", "Movie", false},
		// The film "Cam" in a genuine camera rip is still a camera rip.
		{"Cam.2018.HDCAM.x264", "Cam", true},
	}
	for _, tc := range cases {
		t.Run(tc.release, func(t *testing.T) {
			if got := IsCAMFor(tc.release, tc.knownTitle); got != tc.want {
				t.Errorf("IsCAMFor(%q, %q) = %v, want %v",
					tc.release, tc.knownTitle, got, tc.want)
			}
		})
	}
}

// The end-to-end consequence: with RejectCAM on, the film "Cam" must be pickable.
func TestBest_CamTheFilmIsPickable(t *testing.T) {
	releases := []indexer.Release{
		{Title: "Cam.2018.1080p.WEB-DL.DDP5.1.x264-NTb", Seeders: 60,
			VideoCodec: "h264", AudioCodec: "eac3", Container: "mkv", Resolution: "1080p"},
		{Title: "Cam.2018.720p.WEBRip.x264-GALAXYRG", Seeders: 25,
			VideoCodec: "h264", Container: "mkv", Resolution: "720p"},
	}
	got := Best(releases, camCfg)
	if got == nil {
		t.Fatal("every release of the 2018 film \"Cam\" was rejected as a camera rip")
	}
	if !strings.Contains(got.Title, "WEB-DL") {
		t.Errorf("picked %q, want the WEB-DL", got.Title)
	}
}

// ── Junk (sample / trailer / extras) ──────────────────────────────────────────

func TestIsJunkFor(t *testing.T) {
	cases := []struct {
		release, knownTitle string
		want                bool
	}{
		{"Movie.2024.1080p.WEB-DL.x264-SAMPLE", "Movie", true},
		{"Movie.2024.Official.Trailer.1080p", "Movie", true},
		{"Movie.2024.Teaser.720p", "Movie", true},
		{"Movie.2024.1080p.BluRay.Featurette", "Movie", true},
		{"Movie.2024.Deleted.Scenes.1080p", "Movie", true},
		{"Movie.2024.Behind.The.Scenes.1080p", "Movie", true},
		{"Movie.2024.1080p.BluRay.Extras", "Movie", true},
		{"Movie.2024.1080p.BluRay.x264", "Movie", false},
		// The tag region keeps a real title out of the filter: "Extras" is a series and
		// "Preview" is a plausible film name.
		{"Extras.S01E01.1080p.HDTV.x264", "Extras", false},
		{"Extras.S01E01.1080p.HDTV.x264", "", false},
		{"Preview.2019.1080p.WEB-DL.x264", "Preview", false},
		// EXTENDED is a cut, not an extra.
		{"Movie.2024.EXTENDED.1080p.BluRay.x264", "Movie", false},
	}
	for _, tc := range cases {
		t.Run(tc.release, func(t *testing.T) {
			if got := IsJunkFor(tc.release, tc.knownTitle); got != tc.want {
				t.Errorf("IsJunkFor(%q, %q) = %v, want %v",
					tc.release, tc.knownTitle, got, tc.want)
			}
		})
	}
}

// Junk is dropped from the list entirely rather than listed and flagged: unlike a
// camera rip there is no scenario where a user wants to stream a 40 MB sample.
func TestScore_DropsJunkReleases(t *testing.T) {
	releases := []indexer.Release{
		{Title: "Movie.2024.1080p.WEB-DL.x264-SAMPLE", Seeders: 900, Resolution: "1080p"},
		{Title: "Movie.2024.1080p.WEB-DL.x264-NTb", Seeders: 40, Resolution: "1080p"},
	}
	scored := Score(releases, Config{MinSeeders: 1}, "Movie", "2024")
	if len(scored) != 1 {
		t.Fatalf("Score returned %d releases, want 1 (the sample must be dropped)", len(scored))
	}
	if !strings.Contains(scored[0].Title, "NTb") {
		t.Errorf("kept %q, want the real release", scored[0].Title)
	}
}

// ── Source labelling ──────────────────────────────────────────────────────────

// TestReleaseQualityWebVsBluRay: the bare "web" substring test used to run BEFORE the
// bluray branch, so any title containing the letters "web" was labelled a WEBRip. That
// was cosmetic while nothing scored the source, and stops being cosmetic the moment
// anything does.
func TestReleaseQualityWebVsBluRay(t *testing.T) {
	cases := []struct {
		release, knownTitle, want string
	}{
		{"Charlotte's Web 1080p BluRay x264", "Charlotte's Web", "bluray"},
		{"Charlotte's Web 1080p BluRay x264", "", "bluray"},
		{"Charlotte's Web 2006 1080p BluRay x264", "", "bluray"},
		{"Charlotte's Web 2006 2160p REMUX", "", "remux"},
		{"Charlotte's Web 2006 720p HDTV x264", "", "hdtv"},
		// A genuine bare-WEB release is still WEB.
		{"Movie.2024.1080p.WEB.x264-NTb", "Movie", "webdl"},
		{"Movie.2024.1080p.WEB-DL.x264", "Movie", "webdl"},
		{"Movie.2024.1080p.WEBRip.x264", "Movie", "webrip"},
	}
	for _, tc := range cases {
		t.Run(tc.release, func(t *testing.T) {
			if got := ReleaseQualityFor(tc.release, tc.knownTitle); got != tc.want {
				t.Errorf("ReleaseQualityFor(%q, %q) = %q, want %q",
					tc.release, tc.knownTitle, got, tc.want)
			}
		})
	}
}

// ── Resolution ────────────────────────────────────────────────────────────────

// TestScore_ResolutionBeatsSeederBucket is the headline regression.
//
// Resolution was not parsed and not scored at all, while a single step on the seeder
// ladder is worth 80-120 points — so a 480p rip with 520 seeders outscored a 2160p HEVC
// with 210 by a wide margin, and the auto-pick looked broken to anyone watching it.
func TestScore_ResolutionBeatsSeederBucket(t *testing.T) {
	cfg := Config{MinSeeders: 5, TargetResolution: "1080p"}
	releases := []indexer.Release{
		{Title: "Movie.2024.480p.DVDRip.XviD", Seeders: 520, Resolution: "480p"},
		{Title: "Movie.2024.1080p.WEB-DL.x264", Seeders: 210, Resolution: "1080p"},
		{Title: "Movie.2024.2160p.HEVC.WEB-DL", Seeders: 210, Resolution: "2160p", VideoCodec: "h265"},
	}
	scored := Score(releases, cfg, "", "")
	if !strings.Contains(scored[0].Title, "1080p") {
		t.Fatalf("best = %q (score %d), want the 1080p release; full order: %s",
			scored[0].Title, scored[0].Score, order(scored))
	}
	// 4K ranks above a 480p but below the target: it is three to four times the bitrate
	// the swarm has to sustain in real time.
	if !strings.Contains(scored[1].Title, "2160p") {
		t.Errorf("second = %q, want the 2160p release; full order: %s", scored[1].Title, order(scored))
	}
	if !strings.Contains(scored[2].Title, "480p") {
		t.Errorf("last = %q, want the 480p release; full order: %s", scored[2].Title, order(scored))
	}
}

// The target is a real setting, not a fixed preference: raising it must actually move
// the pick.
func TestScore_TargetResolutionSteersThePick(t *testing.T) {
	releases := []indexer.Release{
		{Title: "Movie.2024.720p.WEB-DL.x264", Seeders: 300, Resolution: "720p"},
		{Title: "Movie.2024.1080p.WEB-DL.x264", Seeders: 300, Resolution: "1080p"},
		{Title: "Movie.2024.2160p.WEB-DL.x265", Seeders: 300, Resolution: "2160p"},
	}
	for _, tc := range []struct{ target, want string }{
		{"720p", "720p"},
		{"1080p", "1080p"},
		{"2160p", "2160p"},
		{"4k", "2160p"}, // accepted spelling
		{"", "1080p"},   // unset falls back to the default rung
	} {
		t.Run("target="+tc.target, func(t *testing.T) {
			best := Best(releases, Config{MinSeeders: 5, TargetResolution: tc.target})
			if best == nil || !strings.Contains(best.Title, tc.want) {
				t.Errorf("target %q picked %+v, want the %s release", tc.target, best, tc.want)
			}
		})
	}
}

func TestScoreResolution(t *testing.T) {
	cases := []struct {
		res, target string
		want        int
	}{
		{"1080p", "1080p", 300},
		{"720p", "1080p", 150},
		{"576p", "1080p", 0},
		{"480p", "1080p", -150},
		{"360p", "1080p", -150},
		{"2160p", "1080p", 100},
		{"2160p", "2160p", 300},
		{"1080p", "2160p", 150},
		{"480p", "480p", 300},
		// Unknown is not the same as bad: no evidence either way, so it lands between
		// one rung down and two rungs down.
		{"", "1080p", 100},
		{"whatever", "1080p", 100},
	}
	for _, tc := range cases {
		if got := scoreResolution(tc.res, tc.target); got != tc.want {
			t.Errorf("scoreResolution(%q, %q) = %d, want %d", tc.res, tc.target, got, tc.want)
		}
	}
}

func TestNormaliseTargetResolution(t *testing.T) {
	cases := map[string]string{
		"1080p": "1080p", "1080": "1080p", "1440p": "1080p",
		"2160p": "2160p", "4k": "2160p", "4K": "2160p", "uhd": "2160p", "UHD": "2160p",
		"720p": "720p", "576p": "576p", "480p": "480p", "360p": "360p",
		" 1080P ": "1080p",
		"":        "",
		"potato":  "",
		"8k":      "",
	}
	for in, want := range cases {
		if got := NormaliseTargetResolution(in); got != want {
			t.Errorf("NormaliseTargetResolution(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── Direct play ───────────────────────────────────────────────────────────────

func TestIsDirectPlay(t *testing.T) {
	cases := []struct {
		name string
		rel  indexer.Release
		want bool
	}{
		{"h264 + aac + mp4", indexer.Release{VideoCodec: "h264", AudioCodec: "aac", Container: "mp4"}, true},
		{"h265 + eac3 + mkv", indexer.Release{VideoCodec: "h265", AudioCodec: "eac3", Container: "mkv"}, true},
		{"h264 + ac3 + mkv", indexer.Release{VideoCodec: "h264", AudioCodec: "ac3", Container: "mkv"}, true},
		// DTS/TrueHD/Atmos force an AUDIO transcode on an Apple TV.
		{"dts forces an audio transcode", indexer.Release{VideoCodec: "h264", AudioCodec: "dts", Container: "mkv"}, false},
		{"truehd/atmos forces an audio transcode", indexer.Release{VideoCodec: "h265", AudioCodec: "truehd", Container: "mkv"}, false},
		// AV1 forces a VIDEO transcode — the worst case: a CPU-bound encode fed by a
		// torrent stream out of a bounded ring buffer.
		{"av1 forces a video transcode", indexer.Release{VideoCodec: "av1", AudioCodec: "aac", Container: "mkv"}, false},
		{"unknown container is not a promise", indexer.Release{VideoCodec: "h264", AudioCodec: "aac"}, false},
		{"unknown codecs are not a promise", indexer.Release{Container: "mkv"}, false},
		{"nothing known at all", indexer.Release{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsDirectPlay(tc.rel); got != tc.want {
				t.Errorf("IsDirectPlay(%+v) = %v, want %v", tc.rel, got, tc.want)
			}
		})
	}
}

// Direct play is worth more than a step on the seeder ladder, because a transcode off a
// ring-buffered torrent stream is the failure mode this architecture exists to avoid.
func TestScore_DirectPlayOutranksSeeders(t *testing.T) {
	releases := []indexer.Release{
		{Title: "Movie.2024.1080p.BluRay.DTS-HD.x264", Seeders: 100, Resolution: "1080p",
			VideoCodec: "h264", AudioCodec: "dts-hd", Container: "mkv"},
		{Title: "Movie.2024.1080p.WEB-DL.DDP5.1.x264", Seeders: 20, Resolution: "1080p",
			VideoCodec: "h264", AudioCodec: "eac3", Container: "mkv"},
	}
	best := Best(releases, Config{MinSeeders: 5})
	if best == nil || !strings.Contains(best.Title, "WEB-DL") {
		t.Fatalf("best = %+v, want the direct-play WEB-DL despite 5x fewer seeders", best)
	}
	scored := Score(releases, Config{MinSeeders: 5}, "", "")
	for _, sr := range scored {
		wantDirect := strings.Contains(sr.Title, "WEB-DL")
		if sr.DirectPlay != wantDirect {
			t.Errorf("%q: DirectPlay = %v, want %v", sr.Title, sr.DirectPlay, wantDirect)
		}
	}
}

// require_direct_play is OFF by default — turning it on for an existing install would
// silently empty release lists that work fine today.
func TestScore_RequireDirectPlayIsAHardFilter(t *testing.T) {
	releases := []indexer.Release{
		{Title: "Movie.2024.2160p.REMUX.TrueHD.Atmos", Seeders: 900, Resolution: "2160p",
			VideoCodec: "h265", AudioCodec: "truehd", Container: "mkv"},
		{Title: "Movie.2024.1080p.WEB-DL.DDP5.1.x264", Seeders: 20, Resolution: "1080p",
			VideoCodec: "h264", AudioCodec: "eac3", Container: "mkv"},
	}
	off := Score(releases, Config{MinSeeders: 5}, "", "")
	if len(off) != 2 {
		t.Fatalf("with require_direct_play off, Score returned %d releases, want 2", len(off))
	}
	on := Score(releases, Config{MinSeeders: 5, RequireDirectPlay: true}, "", "")
	if len(on) != 1 || !strings.Contains(on[0].Title, "WEB-DL") {
		t.Fatalf("with require_direct_play on, Score returned %s, want only the direct-play release",
			order(on))
	}
	got := RejectedBy(releases[0], Config{MinSeeders: 5, RequireDirectPlay: true}, "Movie", "2024", false)
	if got != RejectDirectPlay {
		t.Errorf("RejectedBy = %q, want %q", got, RejectDirectPlay)
	}
}

// ── Bitrate ───────────────────────────────────────────────────────────────────

func TestEstimateMbps(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)
	cases := []struct {
		name     string
		size     int64
		runtime  int
		wantLow  float64
		wantHigh float64
	}{
		{"8 GB over two hours", 8 * gb, 120, 9.4, 9.7},
		{"20 GB over two hours", 20 * gb, 120, 23.7, 24.0},
		{"runtime unknown disables the estimate", 20 * gb, 0, 0, 0},
		{"size unknown disables the estimate", 0, 120, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := estimateMbps(tc.size, tc.runtime)
			if got < tc.wantLow || got > tc.wantHigh {
				t.Errorf("estimateMbps(%d, %d) = %.2f, want %.1f..%.1f",
					tc.size, tc.runtime, got, tc.wantLow, tc.wantHigh)
			}
		})
	}
}

// The bitrate rule only engages when the runtime is known. main.go supplies it from
// TMDB; until then the whole rule stays dormant rather than guessing.
func TestScore_BitrateCapNeedsARuntime(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)
	releases := []indexer.Release{
		{Title: "Movie.2024.2160p.REMUX", Seeders: 200, Resolution: "1080p", SizeBytes: 20 * gb},
		{Title: "Movie.2024.1080p.WEB-DL", Seeders: 20, Resolution: "1080p", SizeBytes: 8 * gb},
	}
	// No runtime: the cap cannot be evaluated, so the better-seeded big file wins.
	noRuntime := Best(releases, Config{MinSeeders: 5, MaxMbps: 10})
	if noRuntime == nil || !strings.Contains(noRuntime.Title, "REMUX") {
		t.Fatalf("without a runtime the cap must be dormant; picked %+v", noRuntime)
	}
	// With a runtime, the 24 Mbps file is penalised past the seeder advantage.
	withRuntime := Best(releases, Config{MinSeeders: 5, MaxMbps: 10, RuntimeMinutes: 120})
	if withRuntime == nil || !strings.Contains(withRuntime.Title, "WEB-DL") {
		t.Fatalf("with a 10 Mbps cap and a 120 minute runtime, picked %+v, want the 8 GB file",
			withRuntime)
	}
	// max_mbps 0 means unlimited, whatever the runtime says.
	unlimited := Best(releases, Config{MinSeeders: 5, MaxMbps: 0, RuntimeMinutes: 120})
	if unlimited == nil || !strings.Contains(unlimited.Title, "REMUX") {
		t.Fatalf("max_mbps 0 must mean unlimited; picked %+v", unlimited)
	}
}

func TestScoreBitrate(t *testing.T) {
	cases := []struct {
		mbps float64
		max  int
		want int
	}{
		{5, 10, 0},  // under the cap
		{10, 10, 0}, // exactly at it
		{12.5, 10, -100},
		{20, 10, -400},
		{500, 10, -800}, // bounded, so an absurd remux is last but not un-pickable
		{100, 0, 0},     // no cap configured
		{0, 10, 0},      // no estimate available
	}
	for _, tc := range cases {
		if got := scoreBitrate(tc.mbps, tc.max); got != tc.want {
			t.Errorf("scoreBitrate(%.1f, %d) = %d, want %d", tc.mbps, tc.max, got, tc.want)
		}
	}
}

// ── Swarm health refinement ───────────────────────────────────────────────────

func TestSeedRatioBonus(t *testing.T) {
	cases := []struct {
		seeders, leechers, want int
	}{
		{100, 0, 40},  // nobody queued for the upload capacity
		{100, 10, 40}, // 10:1
		{100, 40, 25}, // 2.5:1
		{100, 80, 10}, // 1.25:1
		{100, 500, 0}, // heavily contended
		{0, 100, 0},   // dead
	}
	for _, tc := range cases {
		if got := seedRatioBonus(tc.seeders, tc.leechers); got != tc.want {
			t.Errorf("seedRatioBonus(%d, %d) = %d, want %d",
				tc.seeders, tc.leechers, got, tc.want)
		}
	}
}

// The ratio is a tie-breaker between comparable swarms, never a reason to prefer a much
// smaller one — so it must not flip an equal-seeder comparison into the wrong bucket.
func TestScore_SeedRatioIsOnlyATieBreaker(t *testing.T) {
	releases := []indexer.Release{
		{Title: "Movie.2024.1080p.A", Seeders: 210, Leechers: 900, Resolution: "1080p"},
		{Title: "Movie.2024.1080p.B", Seeders: 210, Leechers: 5, Resolution: "1080p"},
		{Title: "Movie.2024.1080p.C", Seeders: 600, Leechers: 5000, Resolution: "1080p"},
	}
	scored := Score(releases, Config{MinSeeders: 5}, "", "")
	if !strings.Contains(scored[0].Title, ".C") {
		t.Errorf("best = %q, want C: a far bigger swarm still wins despite a poor ratio; order: %s",
			scored[0].Title, order(scored))
	}
	if !strings.Contains(scored[1].Title, ".B") {
		t.Errorf("second = %q, want B: at equal seeders the better ratio wins; order: %s",
			scored[1].Title, order(scored))
	}
}

// ── Codec preference aliases ──────────────────────────────────────────────────

// "hevc" shipped in the default prefer_video_codecs and could never match anything: the
// indexer folds x265/HEVC/H265 into "h265" while parsing the title, so the picker never
// sees the string "hevc" on a release. A config that still says "hevc" must keep working.
func TestPreferenceBonusCanonicalisesCodecNames(t *testing.T) {
	cases := []struct {
		value string
		prefs []string
		want  int
	}{
		{"h265", []string{"hevc"}, 40},
		{"h265", []string{"x265"}, 40},
		{"h264", []string{"avc"}, 40},
		{"h264", []string{"h264", "h265"}, 80},
		{"h265", []string{"h264", "h265"}, 40},
		{"h265", []string{"h264"}, 0},
		{"", []string{"h264"}, 0},
		{"av1", []string{"h264", "h265"}, 0},
	}
	for _, tc := range cases {
		if got := preferenceBonus(tc.value, tc.prefs, 40); got != tc.want {
			t.Errorf("preferenceBonus(%q, %v, 40) = %d, want %d", tc.value, tc.prefs, got, tc.want)
		}
	}
}

// order renders a scored list for failure messages.
func order(scored []ScoredRelease) string {
	var b strings.Builder
	for i, sr := range scored {
		if i > 0 {
			b.WriteString(" > ")
		}
		fmt.Fprintf(&b, "%s(%d)", sr.Title, sr.Score)
	}
	return b.String()
}
