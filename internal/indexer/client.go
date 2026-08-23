package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"jellyfreedom/internal/redact"
)

// Sentinel errors so callers can render a SPECIFIC, actionable message instead of
// leaking transport detail. Neither ever contains a URL or a key.
var (
	ErrNotConfigured = errors.New("Prowlarr is not configured — set its URL and API key in Settings → Connections")
	ErrBadKey        = errors.New("Prowlarr rejected the API key — re-enter it in Settings → Connections")
)

// Client talks to Prowlarr's JSON search API.
type Client struct {
	mu         sync.RWMutex
	baseURL    string
	apiKey     string
	httpClient *http.Client
	// ctlClient handles short control-plane calls (ping, indexer list). The search
	// client's 150s timeout is right for a cold FlareSolverr wake-up and badly wrong
	// for a pre-flight health probe that must answer fast or not at all.
	ctlClient *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		// Generous: the FIRST Prowlarr search after idle wakes FlareSolverr +
		// indexers and can take 90s+; warm searches return in well under a second.
		httpClient: &http.Client{Timeout: 150 * time.Second},
		ctlClient:  &http.Client{Timeout: 8 * time.Second},
	}
}

// Configure updates the base URL and API key at runtime under a write lock.
func (c *Client) Configure(baseURL, apiKey string) {
	c.mu.Lock()
	c.baseURL = strings.TrimRight(baseURL, "/")
	c.apiKey = apiKey
	c.mu.Unlock()
	// Backstop: if this key ever reaches a log line or an error string, mask it.
	redact.Register(apiKey)
}

// Configured reports whether both base URL and API key are set.
func (c *Client) Configured() bool {
	return c.base() != "" && c.key() != ""
}

// base returns the current base URL under a read lock.
func (c *Client) base() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL
}

// key returns the current API key under a read lock.
func (c *Client) key() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.apiKey
}

type Release struct {
	Title      string `json:"title"`
	Magnet     string `json:"magnet"`
	InfoHash   string `json:"info_hash"`
	Seeders    int    `json:"seeders"`
	Leechers   int    `json:"leechers"`
	SizeBytes  int64  `json:"size_bytes"`
	VideoCodec string `json:"video_codec"`
	AudioCodec string `json:"audio_codec"`
	Container  string `json:"container"`
	// Resolution is the canonical vertical-resolution rung parsed from the title
	// ("2160p"/"1080p"/"720p"/"576p"/"480p"/"360p"), or "" when the title does not say.
	// The picker ranks distance from the user's target rung, which is the single
	// biggest lever on "did the auto-pick feel right".
	Resolution string `json:"resolution"`
	// Indexer is the Prowlarr indexer that returned the result, kept so the UI can show
	// where a release came from and so a consistently bad indexer is identifiable.
	Indexer string `json:"indexer"`
	// PublishDate is when the indexer says the release was posted. Zero when unknown.
	// Exposed for display/age, deliberately NOT scored: age correlates far too weakly
	// with stream quality to be worth a ranking weight, and penalising old releases
	// would bury the best-seeded catalogue titles.
	PublishDate time.Time `json:"publish_date,omitzero"`
}

// AgeDays reports how many days ago the release was published, or -1 when the indexer
// did not supply a publish date. For display only — see the PublishDate comment.
func (r Release) AgeDays() int {
	if r.PublishDate.IsZero() {
		return -1
	}
	d := int(time.Since(r.PublishDate).Hours() / 24)
	if d < 0 {
		return 0 // a clock-skewed "future" release is 0 days old, never negative
	}
	return d
}

type prowlarrResult struct {
	Title       string `json:"title"`
	InfoHash    string `json:"infoHash"`
	MagnetURL   string `json:"magnetUrl"`
	DownloadURL string `json:"downloadUrl"`
	Guid        string `json:"guid"`
	InfoURL     string `json:"infoUrl"`
	Size        int64  `json:"size"`
	Seeders     int    `json:"seeders"`
	Leechers    int    `json:"leechers"`
	Indexer     string `json:"indexer"`
	// PublishDate is decoded as a STRING and parsed by hand rather than into a
	// time.Time. Prowlarr passes the indexer's own value straight through, and several
	// indexers send "" or a non-RFC3339 stamp — decoding into time.Time makes the whole
	// json.Decode fail on those, which would throw away every result in the response.
	PublishDate string `json:"publishDate"`
}

// publishDateLayouts are tried in order. RFC3339 is what Prowlarr normally emits; the
// rest are the shapes real indexers have been seen to send instead.
var publishDateLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parsePublishDate returns the zero time for anything it cannot read. An unparseable
// date is a display detail; it must never cost us a result.
func parsePublishDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range publishDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// infoHashRe matches a bare 40-char hex BitTorrent v1 info hash. The word boundaries keep
// it from matching a 40-char slice of a longer hex run (e.g. a 64-char SHA-256 in a URL).
var infoHashRe = regexp.MustCompile(`\b([A-Fa-f0-9]{40})\b`)

// extractHash returns the torrent info hash for a Prowlarr result. It prefers the explicit
// infoHash field, then falls back to a 40-hex hash embedded in the magnet/guid/URL fields.
// Many indexers (torrentdownload.info, eztv, rarbg mirrors) leave infoHash empty but expose
// the hash in the guid — without this fallback we discard the majority of results, often
// the best-seeded ones, leaving the library with weak/dead magnets.
func extractHash(r prowlarrResult) string {
	if r.InfoHash != "" {
		return strings.ToLower(r.InfoHash)
	}
	for _, f := range []string{r.MagnetURL, r.Guid, r.DownloadURL, r.InfoURL} {
		if m := infoHashRe.FindString(f); m != "" {
			return strings.ToLower(m)
		}
	}
	return ""
}

// CatMovies and CatTV are Prowlarr category IDs.
const (
	CatMovies = 2000
	CatTV     = 5000
)

// Search queries Prowlarr's /api/v1/search endpoint.
//
// The API key travels in the X-Api-Key HEADER, never in the query string. As a query
// parameter it ended up inside *url.Error (which embeds the whole URL), and those error
// strings were being returned verbatim to unauthenticated HTTP callers and written to
// the log — a verified key disclosure.
func (c *Client) Search(query string, categories []int) ([]Release, error) {
	return c.search(context.Background(), query, categories)
}

// SearchContext is Search with caller-controlled cancellation, so an abandoned HTTP
// request does not keep a 150s indexer call alive behind it.
func (c *Client) SearchContext(ctx context.Context, query string, categories []int) ([]Release, error) {
	return c.search(ctx, query, categories)
}

func (c *Client) search(ctx context.Context, query string, categories []int) ([]Release, error) {
	base, key := c.base(), c.key()
	if base == "" {
		return nil, ErrNotConfigured
	}
	catParams := make([]string, len(categories))
	for i, cat := range categories {
		catParams[i] = "categories=" + strconv.Itoa(cat)
	}
	u := fmt.Sprintf("%s/api/v1/search?query=%s&%s",
		base,
		url.QueryEscape(query),
		strings.Join(catParams, "&"),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("indexer search: %w", err)
	}
	req.Header.Set("X-Api-Key", key)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("indexer search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrBadKey
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("indexer search: status %d", resp.StatusCode)
	}

	var results []prowlarrResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, fmt.Errorf("indexer parse: %w", err)
	}

	releases := make([]Release, 0, len(results))
	for _, r := range results {
		hash := extractHash(r)
		if hash == "" {
			continue // need a hash to add to TorrServer
		}
		r.InfoHash = hash // so buildMagnet uses it when magnetUrl is absent
		rel := Release{
			Title:       r.Title,
			InfoHash:    hash,
			SizeBytes:   r.Size,
			Seeders:     r.Seeders,
			Leechers:    r.Leechers,
			Magnet:      buildMagnet(r),
			Indexer:     r.Indexer,
			PublishDate: parsePublishDate(r.PublishDate),
		}
		parseCodecs(&rel)
		rel.Resolution = ParseResolution(rel.Title)
		releases = append(releases, rel)
	}
	return releases, nil
}

// publicTrackers are reliable public trackers appended to every magnet so peer
// discovery is never DHT-only. Trackerless magnets find few/slow peers, which
// starves streaming — seeking to a not-yet-downloaded region then stalls and the
// player resets to the start.
var publicTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.tracker.cl:1337/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.dler.org:6969/announce",
	"udp://tracker.0x7c0.com:6969/announce",
}

// buildMagnet prefers Prowlarr's real magnet (which carries the torrent's own
// trackers) and falls back to a hash-only magnet, then appends the curated
// public-tracker list for fast, robust peer discovery.
func buildMagnet(r prowlarrResult) string {
	// Only trust magnetUrl if it's an actual magnet link. Prowlarr often returns a
	// download-proxy HTTP URL there instead (which TorrServer, in the VPN netns,
	// can't reach) — in that case build the magnet from the info hash.
	base := r.MagnetURL
	if !strings.HasPrefix(base, "magnet:") {
		base = fmt.Sprintf("magnet:?xt=urn:btih:%s&dn=%s",
			strings.ToUpper(r.InfoHash), url.QueryEscape(r.Title))
	}
	var b strings.Builder
	b.WriteString(base)
	for _, tr := range publicTrackers {
		b.WriteString("&tr=")
		b.WriteString(url.QueryEscape(tr))
	}
	return b.String()
}

// parseCodecs does best-effort extraction of codec/container from the release title.
func parseCodecs(r *Release) {
	t := strings.ToLower(r.Title)

	switch {
	case strings.Contains(t, "x265"), strings.Contains(t, "hevc"), strings.Contains(t, "h265"):
		r.VideoCodec = "h265"
	case strings.Contains(t, "x264"), strings.Contains(t, "h264"), strings.Contains(t, "avc"):
		r.VideoCodec = "h264"
	case strings.Contains(t, "av1"):
		r.VideoCodec = "av1"
	}

	switch {
	case strings.Contains(t, "truehd"), strings.Contains(t, "atmos"):
		r.AudioCodec = "truehd"
	case strings.Contains(t, "dts-hd"), strings.Contains(t, "dts hd"):
		r.AudioCodec = "dts-hd"
	// "ddp" is the spelling almost every modern WEB-DL uses (DDP5.1). Without it the
	// most common direct-play-capable audio track on the whole indexer parsed as
	// unknown, which cost those releases the direct-play bonus outright.
	case strings.Contains(t, "eac3"), strings.Contains(t, "e-ac-3"),
		strings.Contains(t, "dd+"), strings.Contains(t, "ddp"):
		r.AudioCodec = "eac3"
	case strings.Contains(t, "ac3"), strings.Contains(t, "dolby digital"):
		r.AudioCodec = "ac3"
	case strings.Contains(t, "aac"):
		r.AudioCodec = "aac"
	case strings.Contains(t, "dts"):
		r.AudioCodec = "dts"
	}

	switch {
	case strings.Contains(t, ".mkv"), strings.Contains(t, "mkv"):
		r.Container = "mkv"
	case strings.Contains(t, ".mp4"), strings.Contains(t, "mp4"):
		r.Container = "mp4"
	}
}

// The resolution ladder, lowest rung first. Everything a release title can say about
// picture size is normalised onto one of these so the picker can talk about "one rung
// below the target" instead of comparing free-form strings.
const (
	Res360p  = "360p"
	Res480p  = "480p"
	Res576p  = "576p"
	Res720p  = "720p"
	Res1080p = "1080p"
	Res2160p = "2160p"
)

var (
	// resolutionTagRe matches an explicit height tag. Word boundaries matter: without
	// them "1080p" inside a group name or a "x264-1080" suffix would be indistinguishable
	// from the real tag.
	resolutionTagRe = regexp.MustCompile(`(?i)\b(4320p|2160p|1440p|1080[pi]|720p|576[pi]|480[pi]|360p)\b`)
	// uhdRe covers the marketing spellings of 2160p.
	uhdRe = regexp.MustCompile(`(?i)\b(4k|uhd)\b`)
	// dimensionsRe matches the WIDTHxHEIGHT form some indexers use ("1920x1080"). The
	// 3-4 digit floor keeps it away from episode numbering like "1x05".
	dimensionsRe = regexp.MustCompile(`\b(\d{3,4})\s?[xX\x{00D7}]\s?(\d{3,4})\b`)
)

// ParseResolution does best-effort extraction of the picture resolution from a release
// title, normalised onto the ladder above. It returns "" when the title does not say —
// which the picker treats as "unknown", not as "bad".
//
// Exported because resolution is not only a scoring input: the dashboard shows it, and
// a caller holding a bare title (a manual magnet override, for instance) has no Release
// to read it from.
func ParseResolution(title string) string {
	if m := resolutionTagRe.FindStringSubmatch(title); m != nil {
		switch strings.ToLower(m[1]) {
		case "4320p", "2160p":
			return Res2160p
		// 1440p sits between the two common rungs. It is treated as 1080p rather than
		// given a rung of its own: it direct-plays and looks like 1080p-or-better, and
		// inventing a rung would silently shift every "one step below target" decision.
		case "1440p", "1080p", "1080i":
			return Res1080p
		case "720p":
			return Res720p
		case "576p", "576i":
			return Res576p
		case "480p", "480i":
			return Res480p
		case "360p":
			return Res360p
		}
	}
	if uhdRe.MatchString(title) {
		return Res2160p
	}
	if m := dimensionsRe.FindStringSubmatch(title); m != nil {
		w, werr := strconv.Atoi(m[1])
		h, herr := strconv.Atoi(m[2])
		if werr == nil && herr == nil {
			return resolutionForDimensions(w, h)
		}
	}
	return ""
}

// resolutionForDimensions snaps a WIDTHxHEIGHT pair onto the ladder.
//
// Width is the more reliable of the two. Cropped widescreen encodes lose height while
// keeping the rung's nominal width — a 2.39:1 "1080p" file is 1920x800, which is shorter
// than a full-frame 720p — so ranking on height alone puts HD encodes a rung too low.
// Only in SD does height carry the information, where it is the one thing separating PAL
// 720x576 from NTSC 720x480.
func resolutionForDimensions(w, h int) string {
	switch {
	case w >= 3000:
		return Res2160p
	case w >= 1800:
		return Res1080p
	case w >= 1200:
		return Res720p
	case w >= 640:
		switch {
		case h >= 520:
			return Res576p
		case h >= 400:
			return Res480p
		default:
			return Res360p
		}
	default:
		return ""
	}
}

// IndexerCount returns how many indexers Prowlarr currently has configured, or an
// error if Prowlarr is unreachable / rejects the key.
//
// This exists to surface the invisible "Prowlarr installed with zero indexers" trap:
// a perfectly valid URL and key with no indexers behind it returns an empty result
// set forever, which is indistinguishable in the UI from "nothing matched".
func (c *Client) IndexerCount(ctx context.Context) (int, error) {
	base, key := c.base(), c.key()
	if base == "" || key == "" {
		return 0, ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/indexer", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Api-Key", key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.ctlClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("prowlarr unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return 0, ErrBadKey
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("prowlarr returned HTTP %d", resp.StatusCode)
	}
	var list []struct {
		ID     int  `json:"id"`
		Enable bool `json:"enable"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return 0, fmt.Errorf("prowlarr sent an unreadable indexer list")
	}
	n := 0
	for _, ix := range list {
		if ix.Enable {
			n++
		}
	}
	return n, nil
}

// Ping reports whether Prowlarr is reachable and the key is accepted. Used by the
// pre-flight dependency gate so /request and /play fail fast with a specific message
// instead of discovering the problem 150 seconds into a search.
func (c *Client) Ping(ctx context.Context) error {
	base, key := c.base(), c.key()
	if base == "" || key == "" {
		return ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/system/status", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", key)
	resp, err := c.ctlClient.Do(req)
	if err != nil {
		return fmt.Errorf("Prowlarr is unreachable — check it is running and the URL in Settings → Connections")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrBadKey
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Prowlarr returned HTTP %d — see Settings → Connections → Test", resp.StatusCode)
	}
	return nil
}
