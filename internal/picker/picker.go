package picker

import (
	"regexp"
	"strings"

	"jellyfreedom/internal/indexer"
)

type Config struct {
	MinSeeders        int
	PreferVideoCodecs []string
	PreferAudioCodecs []string
	PreferContainers  []string
	MaxSizeGB         int
	RejectCAM         bool // when true, camera/telesync rips are never auto-picked
}

// ScoredRelease is a release with its computed score and title-match flag,
// returned by the /api/releases endpoint so the UI can show the full list.
type ScoredRelease struct {
	indexer.Release
	Score      int    `json:"score"`
	TitleMatch bool   `json:"title_match"`
	IsBest     bool   `json:"is_best"`
	Quality    string `json:"quality"` // cam|bluray|webdl|webrip|hdtv|dvd|screener|""
	IsCAM      bool   `json:"is_cam"`  // camera/telesync rip — low quality
}

// camRe matches camera-copy / telesync / telecine source tags in a release title.
// These are filmed-in-cinema or analog-captured rips — the low-quality "camera
// copies" we never want to auto-pick.
var camRe = regexp.MustCompile(`(?i)\b(cam|cam-?rip|hd-?cam|hq-?cam|ts|hd-?ts|tele-?sync|tc|tele-?cine|pdvd|pre-?dvd-?rip|dvd-?scr|scr(eener)?)\b`)

// IsCAM reports whether a release title looks like a camera/telesync/screener rip.
func IsCAM(title string) bool { return camRe.MatchString(title) }

// ReleaseQuality returns a coarse source-quality label for display.
func ReleaseQuality(title string) string {
	t := strings.ToLower(title)
	switch {
	case camRe.MatchString(title):
		return "cam"
	case strings.Contains(t, "remux"):
		return "remux"
	case strings.Contains(t, "bluray"), strings.Contains(t, "blu-ray"),
		strings.Contains(t, "bdrip"), strings.Contains(t, "brrip"), strings.Contains(t, "brip"):
		return "bluray"
	case strings.Contains(t, "web-dl"), strings.Contains(t, "webdl"), strings.Contains(t, "web.dl"):
		return "webdl"
	case strings.Contains(t, "webrip"), strings.Contains(t, "web-rip"), strings.Contains(t, "web"):
		return "webrip"
	case strings.Contains(t, "hdtv"), strings.Contains(t, "hdrip"):
		return "hdtv"
	case strings.Contains(t, "dvdrip"), strings.Contains(t, "dvd"):
		return "dvd"
	default:
		return ""
	}
}

// Best picks the highest-scoring release that also passes the title match check.
// Falls back to best title-matched release without codec preference if needed.
// Returns nil if no release meets the minimum bar.
func Best(releases []indexer.Release, cfg Config) *indexer.Release {
	scored := Score(releases, cfg, "", "")
	for i := range scored {
		if scored[i].IsBest {
			return &scored[i].Release
		}
	}
	return nil
}

// BestForTitle is like Best but filters strongly on title similarity.
func BestForTitle(releases []indexer.Release, cfg Config, title, year string) *indexer.Release {
	scored := Score(releases, cfg, title, year)
	for i := range scored {
		if scored[i].IsBest {
			return &scored[i].Release
		}
	}
	return nil
}

// Score returns all releases sorted best-first with scores and title-match flags.
// title and year are used for TitleMatch; pass empty strings to skip that check.
func Score(releases []indexer.Release, cfg Config, title, year string) []ScoredRelease {
	maxBytes := int64(cfg.MaxSizeGB) * 1024 * 1024 * 1024

	var out []ScoredRelease
	bestIdx := -1
	bestScore := -1

	for _, r := range releases {
		if r.Seeders < cfg.MinSeeders {
			continue
		}
		if maxBytes > 0 && r.SizeBytes > maxBytes {
			continue
		}
		match := title == "" || TitleMatch(r.Title, title, year)
		quality := ReleaseQuality(r.Title)
		cam := quality == "cam"
		s := scoreRelease(&r, cfg)
		if match {
			s += 500 // strongly prefer title-matched releases
		}
		if cam {
			s -= 10000 // sink camera copies to the bottom of the list
		}
		out = append(out, ScoredRelease{Release: r, Score: s, TitleMatch: match, Quality: quality, IsCAM: cam})
		// A release is eligible for auto-pick unless it's a camera copy and we're
		// rejecting those — so the worker never lands a CAM, but the picker still
		// lists it (flagged) for a conscious manual override.
		eligible := !(cfg.RejectCAM && cam)
		if eligible && s > bestScore {
			bestScore = s
			bestIdx = len(out) - 1
		}
	}
	if bestIdx >= 0 {
		out[bestIdx].IsBest = true
	}

	// Sort: best first (simple insertion — lists are small)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Score > out[j-1].Score; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TitleMatch returns true if the release title plausibly matches the given
// movie/show title. Used to filter out mislabelled torrents (e.g. a WWE
// stream uploaded with a movie's name and year).
func TitleMatch(releaseTitle, movieTitle, year string) bool {
	rel := normalise(releaseTitle)
	mov := normalise(movieTitle)

	words := significantWords(mov)
	if len(words) == 0 {
		return true
	}
	matched := 0
	for _, w := range words {
		if strings.Contains(rel, w) {
			matched++
		}
	}
	ratio := float64(matched) / float64(len(words))

	// Year check: if release doesn't contain the year, require a higher word-match ratio
	if year != "" && !strings.Contains(rel, year) {
		return ratio >= 0.9
	}
	return ratio >= 0.7
}

func normalise(s string) string {
	s = strings.ToLower(s)
	noise := []string{
		"1080p", "720p", "480p", "2160p", "4k", "uhd",
		"bluray", "blu-ray", "bdrip", "brrip", "webrip", "web-dl", "webdl",
		"hdtv", "dvdrip", "dvdscr", "cam", "ts", "hdrip",
		"x264", "x265", "h264", "h265", "hevc", "avc", "xvid", "divx",
		"aac", "ac3", "dts", "mp3", "flac", "truehd", "atmos",
		"hdr", "hdr10", "dolby", "sdr", "remux", "proper", "repack",
		"extended", "theatrical", "directors", "cut", "remastered",
		"yify", "yts", "rarbg", "eztv", "ettv",
	}
	for _, n := range noise {
		s = strings.ReplaceAll(s, n, " ")
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == ' ' {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func significantWords(s string) []string {
	stop := map[string]bool{
		"the": true, "a": true, "an": true, "of": true, "in": true,
		"on": true, "at": true, "to": true, "and": true, "or": true,
		"for": true, "is": true, "it": true, "be": true,
	}
	var words []string
	for _, w := range strings.Fields(s) {
		if len(w) > 1 && !stop[w] {
			words = append(words, w)
		}
	}
	return words
}

func scoreRelease(r *indexer.Release, cfg Config) int {
	s := 0
	// Seeders DOMINATE. For streaming, swarm health is the single biggest predictor of
	// "plays smoothly" vs "stalls / is dead" — indexer seeder counts are also a rough
	// liveness signal, so a well-seeded release is far more likely to survive validation
	// and actual playback. Codec/audio/container below are tie-breakers among healthy ones.
	switch {
	case r.Seeders >= 500:
		s += 500
	case r.Seeders >= 200:
		s += 380
	case r.Seeders >= 100:
		s += 280
	case r.Seeders >= 50:
		s += 180
	case r.Seeders >= 20:
		s += 100
	case r.Seeders >= 10:
		s += 50
	case r.Seeders >= 5:
		s += 25
	default:
		s += r.Seeders
	}
	// Tie-breakers: prefer direct-play-friendly codecs/containers, but never enough to
	// outweigh a meaningfully better-seeded release.
	for i, codec := range cfg.PreferVideoCodecs {
		if r.VideoCodec == codec {
			s += (len(cfg.PreferVideoCodecs) - i) * 40
			break
		}
	}
	for i, codec := range cfg.PreferAudioCodecs {
		if r.AudioCodec == codec {
			s += (len(cfg.PreferAudioCodecs) - i) * 15
			break
		}
	}
	for i, container := range cfg.PreferContainers {
		if r.Container == container {
			s += (len(cfg.PreferContainers) - i) * 10
			break
		}
	}
	return s
}
