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
