package indexer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client talks to Prowlarr's JSON search API.
type Client struct {
	mu         sync.RWMutex
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		// Generous: the FIRST Prowlarr search after idle wakes FlareSolverr +
		// indexers and can take 90s+; warm searches return in well under a second.
		httpClient: &http.Client{Timeout: 150 * time.Second},
	}
}

// Configure updates the base URL and API key at runtime under a write lock.
func (c *Client) Configure(baseURL, apiKey string) {
	c.mu.Lock()
	c.baseURL = strings.TrimRight(baseURL, "/")
	c.apiKey = apiKey
	c.mu.Unlock()
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
func (c *Client) Search(query string, categories []int) ([]Release, error) {
	catParams := make([]string, len(categories))
	for i, cat := range categories {
		catParams[i] = "categories=" + strconv.Itoa(cat)
	}
	u := fmt.Sprintf("%s/api/v1/search?apikey=%s&query=%s&%s",
		c.base(),
		c.key(),
		url.QueryEscape(query),
		strings.Join(catParams, "&"),
	)

	resp, err := c.httpClient.Get(u)
	if err != nil {
		return nil, fmt.Errorf("indexer search: %w", err)
	}
	defer resp.Body.Close()

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
			Title:     r.Title,
			InfoHash:  hash,
			SizeBytes: r.Size,
			Seeders:   r.Seeders,
			Leechers:  r.Leechers,
			Magnet:    buildMagnet(r),
		}
		parseCodecs(&rel)
		releases = append(releases, rel)
	}
	return releases, nil
}

// SearchCount runs a search (used to warm the indexers) and returns the result count.
func (c *Client) SearchCount(query string, categories []int) (int, error) {
	r, err := c.Search(query, categories)
	return len(r), err
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
	case strings.Contains(t, "eac3"), strings.Contains(t, "e-ac-3"), strings.Contains(t, "dd+"):
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
