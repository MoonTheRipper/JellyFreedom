package picker

import (
	"math"
	"regexp"
	"strings"

	"jellyfreedom/internal/indexer"
)

// Rejection reason tokens returned by RejectedBy. Closed set — the UI renders them.
const (
	RejectMinSeeders = "min_seeders"
	RejectMaxSize    = "max_size_gb"
	RejectCAMRule    = "reject_cam"
	RejectJunk       = "junk_release"
	RejectDirectPlay = "require_direct_play"
	RejectTitle      = "title_mismatch"
)

// RejectedBy reports which single filter rule excluded a release from auto-pick, or
// "" if it was eligible. Rules are evaluated in the order Score applies them, so the
// answer matches what actually happened. This is what turns "no suitable release found"
// into "these 12 releases were found and here is exactly which rule rejected each".
//
// requireTitleMatch mirrors the caller's policy: the resolve pipeline only *prefers* a
// title match (Score adds +500), while the library health check treats a mismatch as
// disqualifying. Passing false therefore never reports RejectTitle.
func RejectedBy(r indexer.Release, cfg Config, title, year string, requireTitleMatch bool) string {
	if r.Seeders < cfg.MinSeeders {
		return RejectMinSeeders
	}
	if maxBytes := int64(cfg.MaxSizeGB) * 1024 * 1024 * 1024; maxBytes > 0 && r.SizeBytes > maxBytes {
		return RejectMaxSize
	}
	if IsJunkFor(r.Title, title) {
		return RejectJunk
	}
	if cfg.RequireDirectPlay && !IsDirectPlay(r) {
		return RejectDirectPlay
	}
	if cfg.RejectCAM && IsCAMFor(r.Title, title) {
		return RejectCAMRule
	}
	if requireTitleMatch && title != "" && !TitleMatch(r.Title, title, year) {
		return RejectTitle
	}
	return ""
}

type Config struct {
	MinSeeders        int
	PreferVideoCodecs []string
	PreferAudioCodecs []string
	PreferContainers  []string
	MaxSizeGB         int
	RejectCAM         bool // when true, camera/telesync rips are never auto-picked

	// TargetResolution is the rung the user actually wants ("2160p".."480p"). Empty
	// means the DefaultTargetResolution below, so a zero-value Config — which several
	// callers and tests construct — still ranks resolution sensibly instead of not at all.
	TargetResolution string

	// RequireDirectPlay turns the direct-play check from a strong preference into a hard
	// filter. Default false, deliberately: switching it on for existing users would make
	// perfectly good releases vanish from their lists with no explanation.
	RequireDirectPlay bool

	// MaxMbps caps the average video bitrate the swarm has to keep up with. 0 = no cap.
	// Only meaningful together with RuntimeMinutes.
	MaxMbps int

	// RuntimeMinutes is the item's running time, used with SizeBytes to estimate
	// bitrate. 0 = unknown, which disables the bitrate rule entirely rather than
	// guessing — a wrong runtime would penalise exactly the wrong releases.
	RuntimeMinutes int
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
	// DirectPlay reports that Jellyfin can remux-or-passthrough this to an Apple TV
	// instead of transcoding it. Exposed so the UI can badge it — it is the single most
	// useful thing to know about a release on this system. See IsDirectPlay.
	DirectPlay bool `json:"direct_play"`
	// Mbps is the estimated average bitrate, or 0 when the runtime is unknown.
	Mbps float64 `json:"mbps,omitempty"`
}

// ── Source-tag vocabulary ─────────────────────────────────────────────────────
//
// Release titles are "<title> <year> <tags> <group>" by convention, and the tags are
// where the source lives. Several source tags are also ordinary English words, so
// where a token appears matters as much as whether it appears:
//
//   - "Cam" (2018) is a real film. Matching \bcam\b against the whole title scored
//     every one of its releases at -10000, making the film literally unpickable.
//   - "Extras" is a real series, "Sample" and "Preview" are plausible titles.
//   - "Charlotte's Web" is not a WEBRip, and ".ts" is a legal container extension.
//
// So the short, ambiguous tokens are only honoured inside the TAG REGION (see
// tagRegion), while unambiguous multi-character tags are matched anywhere.

var (
	// strongCamRe matches source tags that are never anything but a camera/analog rip,
	// wherever they appear in the title.
	strongCamRe = regexp.MustCompile(`(?i)\b(cam-?rip|hd-?cam|hq-?cam|hd-?ts|tele-?sync|tele-?cine|pdvd|pre-?dvd-?rip|dvd-?scr|screener|workprint|hdtc)\b`)
	// shortCamRe matches the bare abbreviations. Only ever run against the tag region,
	// and only when no legitimate source tag contradicts them.
	shortCamRe = regexp.MustCompile(`(?i)\b(cam|ts|tc|scr)\b`)
	// cleanSourceRe matches tags that state a real digital source. Nobody labels a
	// release both "BluRay" and "TS", so the presence of one of these means a bare "ts"
	// in the same title is a container extension or part of a word — not a telesync.
	// "hdrip" is deliberately absent: it is a vague label that gets slapped on
	// cam-sourced content as often as on a real HD rip, so it must not be allowed to
	// vouch for a title that also says "TS".
	cleanSourceRe = regexp.MustCompile(`(?i)\b(blu-?ray|bd-?rip|br-?rip|bd-?remux|web-?dl|web-?rip|webdl|webrip|hd-?tv|hdtv|dvd-?rip|remux|amzn|nf|dsnp|hmax|atvp|itunes)\b`)
	// bareWebRe is the last-resort WEB check, run against the tag region only.
	bareWebRe = regexp.MustCompile(`(?i)\bweb\b`)
	// junkRe matches releases that are not the feature at all: promotional material and
	// bonus content, which are small, plentiful, and useless to stream.
	junkRe = regexp.MustCompile(`(?i)\b(sample|trailer|teaser|promo|featurette|extras?|preview|bloopers|outtakes|deleted[. _-]scenes|behind[. _-]the[. _-]scenes)\b`)
	// yearRe matches a plausible release year, the usual boundary between the title and
	// the tags.
	yearRe = regexp.MustCompile(`^(19|20)\d{2}$`)
	// tagStartRe matches the tokens that unambiguously begin the tag block when there is
	// no year to anchor on.
	tagStartRe = regexp.MustCompile(`(?i)^(2160p|1440p|1080[pi]|720p|576[pi]|480[pi]|360p|4k|uhd|x26[45]|h ?26[45]|hevc|avc|av1|xvid|divx|complete|s\d{1,3}(e\d{1,3})?|\d{1,2}x\d{2})$`)
)

// tagRegion returns the part of a release title where source tags live, lowercased.
//
// Two ways in, most reliable first:
//
//  1. If the caller knows the real title (it comes from TMDB in the resolve path),
//     strip that many leading tokens off the front. "Cam 2018 1080p WEB" with the known
//     title "Cam" leaves "2018 1080p web" — no camera rip in sight, which is the point.
//  2. Otherwise, start at the first token that is a year or an unmistakable tag. This
//     is weaker: a title whose tags start later than a title word ("Inception Trailer
//     1080p") hides that word from the check. That is the deliberate trade — a missed
//     junk release costs one bad pick, whereas a false positive makes a legitimate film
//     permanently unwatchable.
func tagRegion(releaseTitle, knownTitle string) string {
	tokens := tokenise(releaseTitle)
	if len(tokens) == 0 {
		return ""
	}
	if knownTitle != "" {
		want := map[string]bool{}
		for _, t := range tokenise(knownTitle) {
			want[t] = true
		}
		i := 0
		for i < len(tokens) && want[tokens[i]] {
			i++
		}
		// Only trust the strip if it actually consumed the title. A release whose first
		// token is not part of the title (a scene prefix, a different transliteration)
		// falls through to the positional rule rather than scanning the whole string.
		if i > 0 {
			return strings.Join(tokens[i:], " ")
		}
	}
	for i, t := range tokens {
		if yearRe.MatchString(t) || tagStartRe.MatchString(t) {
			return strings.Join(tokens[i:], " ")
		}
	}
	// Nothing looks like a tag: treat the whole title as the region. A title with no
	// year, no resolution and no codec is almost always all tags anyway ("Movie TS").
	return strings.Join(tokens, " ")
}

// tokenise lowercases and splits on everything that is not a letter or a digit, so
// dots, underscores, brackets and hyphens all become token boundaries.
func tokenise(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
}

// IsCAM reports whether a release title looks like a camera/telesync/screener rip,
// with no knowledge of the real title. Prefer IsCAMFor where the title is known.
func IsCAM(releaseTitle string) bool { return IsCAMFor(releaseTitle, "") }

// IsCAMFor is IsCAM with the real movie/show title supplied, so a film actually called
// "Cam", "Screener" or "TS" is not mistaken for a rip of itself.
func IsCAMFor(releaseTitle, knownTitle string) bool {
	if strongCamRe.MatchString(releaseTitle) {
		return true
	}
	if cleanSourceRe.MatchString(releaseTitle) {
		return false // a stated digital source beats a two-letter abbreviation
	}
	return shortCamRe.MatchString(tagRegion(releaseTitle, knownTitle))
}

// IsJunk reports whether a release is promotional or bonus material rather than the
// feature itself. Prefer IsJunkFor where the real title is known.
func IsJunk(releaseTitle string) bool { return IsJunkFor(releaseTitle, "") }

// IsJunkFor is IsJunk with the real title supplied, so the series "Extras" and a film
// called "Preview" survive their own release names.
//
// These are filtered outright rather than penalised. A 40 MB sample or a two-minute
// trailer is never a thing anyone wanted to watch, so there is no manual override worth
// listing it for — unlike a camera rip, which someone may knowingly accept.
func IsJunkFor(releaseTitle, knownTitle string) bool {
	return junkRe.MatchString(tagRegion(releaseTitle, knownTitle))
}

// ReleaseQuality returns a coarse source-quality label for display.
func ReleaseQuality(releaseTitle string) string { return ReleaseQualityFor(releaseTitle, "") }

// ReleaseQualityFor is ReleaseQuality with the real title supplied for the ambiguous
// tokens. See tagRegion.
func ReleaseQualityFor(releaseTitle, knownTitle string) string {
	t := strings.ToLower(releaseTitle)
	switch {
	case IsCAMFor(releaseTitle, knownTitle):
		return "cam"
	case strings.Contains(t, "remux"):
		return "remux"
	// The explicit web cases MUST be tested before bluray: "webrip" contains the
	// substring "brip", so with bluray first every WEBRip release was mislabelled as a
	// BluRay.
	case strings.Contains(t, "web-dl"), strings.Contains(t, "webdl"), strings.Contains(t, "web.dl"):
		return "webdl"
	case strings.Contains(t, "webrip"), strings.Contains(t, "web-rip"):
		return "webrip"
	case strings.Contains(t, "bluray"), strings.Contains(t, "blu-ray"),
		strings.Contains(t, "bdrip"), strings.Contains(t, "brrip"), strings.Contains(t, "brip"):
		return "bluray"
	case strings.Contains(t, "hdtv"), strings.Contains(t, "hdrip"):
		return "hdtv"
	case strings.Contains(t, "dvdrip"), strings.Contains(t, "dvd"):
		return "dvd"
	// Bare "web" is LAST and is restricted to the tag region. As a plain substring
	// tested before bluray it labelled "Charlotte's Web 1080p BluRay" a webrip — which
	// was cosmetic while nothing scored the source, and stops being cosmetic the moment
	// anything does.
	case bareWebRe.MatchString(tagRegion(releaseTitle, knownTitle)):
		return "webdl"
	default:
		return ""
	}
}

// ── Direct play ───────────────────────────────────────────────────────────────

// directPlayVideo / directPlayAudio / directPlayContainer are what an Apple TV can take
// from Jellyfin without the server re-encoding it.
//
// This matters more here than on a normal Jellyfin box. A transcode reads the source
// far faster than it plays it, and the source is a torrent stream out of TorrServer's
// bounded ring buffer — so ffmpeg races ahead of the swarm, drains the buffer, stalls,
// and the whole playback session collapses. Direct play is not a nicety on this
// architecture; it is the difference between working and not.
//
// The exclusions are as important as the inclusions: DTS, DTS-HD, TrueHD and Atmos all
// force an AUDIO transcode on an Apple TV, and AV1 forces a VIDEO transcode (the worst
// case — a CPU-bound encode fed by a network stream).
var (
	directPlayVideo     = map[string]bool{"h264": true, "h265": true, "hevc": true}
	directPlayAudio     = map[string]bool{"aac": true, "ac3": true, "eac3": true}
	directPlayContainer = map[string]bool{"mp4": true, "mkv": true}
)

// IsDirectPlay reports whether the release's parsed codecs and container are all
// Apple-TV-friendly. Unknown fields count as NOT direct play: the flag is a promise the
// UI badges and the picker pays 400 points for, so it has to mean "we know", not
// "nothing contradicted it".
func IsDirectPlay(r indexer.Release) bool {
	return directPlayVideo[r.VideoCodec] &&
		directPlayAudio[r.AudioCodec] &&
		directPlayContainer[r.Container]
}

// ── Resolution ────────────────────────────────────────────────────────────────

// DefaultTargetResolution is used when the config does not name one. 1080p rather than
// 2160p: 4K quadruples the bitrate the swarm has to sustain in real time, and on a
// streaming-only system that buys stalls far more often than it buys detail.
const DefaultTargetResolution = indexer.Res1080p

// resolutionRank orders the ladder so "distance from target" is a subtraction. Values
// are rungs, not qualities — only the difference between two of them is used.
var resolutionRank = map[string]int{
	indexer.Res360p:  0,
	indexer.Res480p:  1,
	indexer.Res576p:  2,
	indexer.Res720p:  3,
	indexer.Res1080p: 4,
	indexer.Res2160p: 5,
}

// NormaliseTargetResolution maps a user-supplied target onto the ladder, returning ""
// for anything unrecognised so callers can reject it with a useful message.
func NormaliseTargetResolution(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "2160p", "4k", "uhd", "2160":
		return indexer.Res2160p
	case "1080p", "1080", "1080i", "1440p", "fhd":
		return indexer.Res1080p
	case "720p", "720", "hd":
		return indexer.Res720p
	case "576p", "576", "576i":
		return indexer.Res576p
	case "480p", "480", "480i", "sd":
		return indexer.Res480p
	case "360p", "360":
		return indexer.Res360p
	default:
		return ""
	}
}

// scoreResolution ranks a release's resolution against the target.
//
// This is the fix the auto-pick most needed. Resolution was not parsed and not scored
// at all, while one step on the seeder ladder is worth 80-120 points — so a 480p rip
// with 520 seeders beat a 2160p HEVC with 210 seeders by a landslide, every time, and
// the pick looked broken to anyone watching the result.
//
// Above the target scores well below the target itself. On a system that streams from a
// swarm in real time, 4K is not simply "better": it is three to four times the bitrate,
// which is three to four times the chance of running the ring buffer dry.
func scoreResolution(res, target string) int {
	rank, ok := resolutionRank[res]
	if !ok {
		// Unknown resolution. Worth roughly what "above target" is worth: no evidence
		// either way, so neither reward it as a match nor bury it below a 576p rip.
		return 100
	}
	switch d := resolutionRank[target] - rank; {
	case d < 0:
		return 100 // above the target — see above
	case d == 0:
		return 300
	case d == 1:
		return 150
	case d == 2:
		return 0
	default:
		return -150 // three rungs down or worse; still not a hard filter
	}
}

// ── Bitrate ───────────────────────────────────────────────────────────────────

// estimateMbps returns the average bitrate implied by size and runtime, or 0 when
// either is unknown. It is the whole file's bitrate (video plus audio plus subtitles),
// which is the right number here: the swarm has to deliver all of it.
func estimateMbps(sizeBytes int64, runtimeMinutes int) float64 {
	if sizeBytes <= 0 || runtimeMinutes <= 0 {
		return 0
	}
	return float64(sizeBytes) * 8 / (float64(runtimeMinutes) * 60) / 1e6
}

// scoreBitrate penalises a release whose average bitrate exceeds what the user said
// their connection can stream. It is a penalty rather than a filter because the cap is
// a comfort threshold, not a hard capability: a slightly-over release still plays, it
// just buffers more, and with no alternative it is better than nothing.
//
// The penalty scales with how far over the cap it is, and is bounded so that a wildly
// oversized remux is pushed to the bottom of the list without becoming un-pickable when
// it is the only thing available.
func scoreBitrate(mbps float64, maxMbps int) int {
	if maxMbps <= 0 || mbps <= float64(maxMbps) {
		return 0
	}
	over := (mbps - float64(maxMbps)) / float64(maxMbps)
	penalty := int(over * 400)
	if penalty > 800 {
		penalty = 800
	}
	return -penalty
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

// Score returns all releases sorted best-first with scores and title-match flags.
// title and year are used for TitleMatch; pass empty strings to skip that check.
func Score(releases []indexer.Release, cfg Config, title, year string) []ScoredRelease {
	maxBytes := int64(cfg.MaxSizeGB) * 1024 * 1024 * 1024
	target := NormaliseTargetResolution(cfg.TargetResolution)
	if target == "" {
		target = DefaultTargetResolution
	}

	var out []ScoredRelease
	bestIdx := -1
	// Start below every representable score. It used to start at -1, which silently
	// made "all candidates are CAMs" return NOTHING: the CAM penalty is -10000, so
	// every eligible release scored below the seed and none was ever marked IsBest.
	bestScore := math.MinInt

	for _, r := range releases {
		if r.Seeders < cfg.MinSeeders {
			continue
		}
		if maxBytes > 0 && r.SizeBytes > maxBytes {
			continue
		}
		// Samples, trailers and featurettes are dropped rather than listed: see
		// IsJunkFor. Nothing downstream wants a two-minute teaser in the candidate list.
		if IsJunkFor(r.Title, title) {
			continue
		}
		direct := IsDirectPlay(r)
		if cfg.RequireDirectPlay && !direct {
			continue
		}
		match := title == "" || TitleMatch(r.Title, title, year)
		quality := ReleaseQualityFor(r.Title, title)
		cam := quality == "cam"
		mbps := estimateMbps(r.SizeBytes, cfg.RuntimeMinutes)
		s := scoreRelease(&r, cfg, target, direct, mbps)
		if match {
			s += 500 // strongly prefer title-matched releases
		}
		if cam {
			s -= 10000 // sink camera copies to the bottom of the list
		}
		out = append(out, ScoredRelease{
			Release: r, Score: s, TitleMatch: match,
			Quality: quality, IsCAM: cam, DirectPlay: direct, Mbps: mbps,
		})
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

// noiseVocabulary is the release-tag vocabulary stripped out before two titles are
// compared. Multi-word entries ("web dl", "blu ray") are matched as a run of ADJACENT
// tokens, never as separate words, so the film "Ray" keeps its only word.
//
// Bare "web" is deliberately absent. Stripping noise from the RELEASE side is harmless
// (surplus tokens never cost a match), so the vocabulary only really acts on the title
// being searched for — and there, deleting a word is pure loss. "Charlotte's Web" needs
// every word it has.
var noiseVocabulary = []string{
	"1080p", "1080i", "720p", "480p", "576p", "2160p", "1440p", "4k", "uhd",
	"bluray", "blu ray", "bdrip", "brrip", "webrip", "web dl", "webdl",
	"hdtv", "dvdrip", "dvdscr", "cam", "ts", "hdrip",
	"x264", "x265", "h264", "h265", "hevc", "avc", "xvid", "divx",
	"aac", "ac3", "dts", "mp3", "flac", "truehd", "atmos",
	"hdr", "hdr10", "dolby", "sdr", "remux", "proper", "repack",
	"extended", "theatrical", "directors", "cut", "remastered",
	"yify", "yts", "rarbg", "eztv", "ettv",
}

// noiseTokens / noisePhrases are noiseVocabulary split into the single-token fast path
// and the multi-token phrases, built once at startup.
var noiseTokens, noisePhrases = buildNoise()

func buildNoise() (map[string]bool, [][]string) {
	tokens := map[string]bool{}
	var phrases [][]string
	for _, entry := range noiseVocabulary {
		parts := tokenise(entry)
		switch len(parts) {
		case 0:
		case 1:
			tokens[parts[0]] = true
		default:
			phrases = append(phrases, parts)
		}
	}
	return tokens, phrases
}

// normalise lowercases a title, drops release-tag noise and collapses punctuation, so
// two spellings of the same title compare equal.
//
// Noise is removed TOKEN-WISE. It used to be strings.ReplaceAll over the same
// vocabulary, which deleted the tags wherever their letters happened to occur — and the
// vocabulary contains "ts", "cam" and "cut". That silently mangled real titles:
// "Ghosts" became "ghos", "Camelot" became "elot", "Cutthroat Island" became "throat
// island". Every one of those is a title-match failure that reads as "no releases
// found" to the user, with nothing in the logs to explain it.
func normalise(s string) string {
	tokens := tokenise(s)
	out := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); {
		if n := matchNoisePhrase(tokens[i:]); n > 0 {
			i += n
			continue
		}
		if noiseTokens[tokens[i]] {
			i++
			continue
		}
		out = append(out, tokens[i])
		i++
	}
	return strings.Join(out, " ")
}

// matchNoisePhrase returns the length of the longest noise phrase starting at tokens[0],
// or 0 if none does.
func matchNoisePhrase(tokens []string) int {
	best := 0
	for _, ph := range noisePhrases {
		if len(ph) <= best || len(ph) > len(tokens) {
			continue
		}
		ok := true
		for i, w := range ph {
			if tokens[i] != w {
				ok = false
				break
			}
		}
		if ok {
			best = len(ph)
		}
	}
	return best
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

// codecAliases normalises the codec names a user may write in prefer_video_codecs onto
// the names parseCodecs actually emits. "hevc" was in the shipped default list and
// could never match anything, because the indexer folds x265/HEVC/H265 into "h265"
// before the picker ever sees it.
var codecAliases = map[string]string{
	"hevc":     "h265",
	"x265":     "h265",
	"x264":     "h264",
	"avc":      "h264",
	"dd+":      "eac3",
	"ddp":      "eac3",
	"matroska": "mkv",
}

func canonicalCodec(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if c, ok := codecAliases[s]; ok {
		return c
	}
	return s
}

// preferenceBonus scores a value against an ordered preference list: first choice is
// worth the most, anything not listed is worth nothing.
func preferenceBonus(value string, prefs []string, step int) int {
	value = canonicalCodec(value)
	if value == "" {
		return 0
	}
	for i, p := range prefs {
		if canonicalCodec(p) == value {
			return (len(prefs) - i) * step
		}
	}
	return 0
}

func scoreRelease(r *indexer.Release, cfg Config, target string, direct bool, mbps float64) int {
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
	// Direct play outweighs everything except swarm health and the title match, because
	// a transcode off a ring-buffered torrent stream is the one failure mode this whole
	// architecture is built to avoid. See IsDirectPlay.
	if direct {
		s += 400
	}
	s += scoreResolution(r.Resolution, target)
	s += scoreBitrate(mbps, cfg.MaxMbps)
	s += seedRatioBonus(r.Seeders, r.Leechers)
	// Tie-breakers: prefer direct-play-friendly codecs/containers, but never enough to
	// outweigh a meaningfully better-seeded release.
	s += preferenceBonus(r.VideoCodec, cfg.PreferVideoCodecs, 40)
	s += preferenceBonus(r.AudioCodec, cfg.PreferAudioCodecs, 15)
	s += preferenceBonus(r.Container, cfg.PreferContainers, 10)
	return s
}

// seedRatioBonus is a small refinement on top of the raw seeder count. Two releases
// with 200 seeders are not equally healthy if one has 20 leechers and the other has
// 2000: the second is sharing the same upload capacity across ten times the demand, and
// that shows up as slower buffering.
//
// It is deliberately worth far less than a step on the seeder ladder — the ratio is a
// tie-breaker between comparable swarms, not a reason to prefer a small one.
func seedRatioBonus(seeders, leechers int) int {
	if seeders <= 0 {
		return 0
	}
	if leechers <= 0 {
		return 40 // seeders and nobody queued for them
	}
	switch ratio := float64(seeders) / float64(leechers); {
	case ratio >= 5:
		return 40
	case ratio >= 2:
		return 25
	case ratio >= 1:
		return 10
	default:
		return 0
	}
}
