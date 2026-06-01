package torrserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Client struct {
	mu         sync.RWMutex
	baseURL    string
	httpClient *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Configure updates the base URL at runtime under a write lock.
func (c *Client) Configure(baseURL string) {
	c.mu.Lock()
	c.baseURL = strings.TrimRight(baseURL, "/")
	c.mu.Unlock()
}

// Configured reports whether a base URL is set.
func (c *Client) Configured() bool {
	return c.base() != ""
}

// base returns the current base URL under a read lock.
func (c *Client) base() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL
}

// BaseURL exposes the current (live) base URL for callers like the stream proxy.
func (c *Client) BaseURL() string { return c.base() }

// EnsureLoaded re-adds a torrent (via the supplied full magnet, which carries
// trackers) if it isn't currently loaded, then waits up to waitSecs for its file
// list to resolve. Returns true once the torrent has a file list (i.e. it's alive
// and streamable). Used by the stream proxy and the Resolve-at-Play path so a
// dropped torrent comes back on play — and so a dead cached release can be detected.
func (c *Client) EnsureLoaded(hash, magnet, title string, waitSecs int) bool {
	if info, err := c.Stat(hash); err == nil && len(info.Files) > 0 {
		return true // already loaded
	}
	if magnet == "" {
		magnet = "magnet:?xt=urn:btih:" + hash
	}
	c.Add(magnet, title)
	for i := 0; i < waitSecs; i++ {
		if info, err := c.Stat(hash); err == nil && len(info.Files) > 0 {
			return true
		}
		time.Sleep(1 * time.Second)
	}
	return false
}

// CacheSettings is the subset of TorrServer settings the orchestrator manages so
// the same binary adapts to the host (RAM-rich server vs low-RAM device + SSD).
type CacheSettings struct {
	Mode               string // "ram" | "disk" | "" (leave as-is)
	SizeMB             int
	Path               string
	DisconnectTimeoutS int
	ConnectionsLimit   int
	RetrackersMode     *int
	UploadRateLimitKB  int
}

// ApplyCacheSettings reads TorrServer's current settings, overlays the managed
// cache fields, and writes them back. Get-modify-set preserves every field we
// don't touch (API keys, DHT toggles, etc.).
func (c *Client) ApplyCacheSettings(s CacheSettings) error {
	var cur map[string]any
	if err := c.post("/settings", map[string]any{"action": "get"}, &cur); err != nil {
		return fmt.Errorf("get settings: %w", err)
	}
	if cur == nil {
		return fmt.Errorf("torrserver returned empty settings")
	}

	switch s.Mode {
	case "ram":
		cur["UseDisk"] = false
		cur["RemoveCacheOnDrop"] = false // RAM frees automatically on drop
	case "disk":
		cur["UseDisk"] = true
		cur["RemoveCacheOnDrop"] = true // MANDATORY: delete cache file on drop so disk stays flat
		if s.Path != "" {
			cur["TorrentsSavePath"] = s.Path
		}
	}
	if s.SizeMB > 0 {
		cur["CacheSize"] = int64(s.SizeMB) * 1024 * 1024
	}
	if s.DisconnectTimeoutS > 0 {
		cur["TorrentDisconnectTimeout"] = s.DisconnectTimeoutS
	}
	if s.ConnectionsLimit > 0 {
		cur["ConnectionsLimit"] = s.ConnectionsLimit
	}
	if s.RetrackersMode != nil {
		cur["RetrackersMode"] = *s.RetrackersMode
	}
	if s.UploadRateLimitKB > 0 {
		cur["UploadRateLimit"] = s.UploadRateLimitKB * 1024
	}

	if err := c.post("/settings", map[string]any{"action": "set", "sets": cur}, nil); err != nil {
		return fmt.Errorf("set settings: %w", err)
	}
	return nil
}

type FileInfo struct {
	ID     int    `json:"id"`
	Path   string `json:"path"`
	Length int64  `json:"length"`
}

type TorrentInfo struct {
	Hash             string     `json:"hash"`
	Title            string     `json:"title"`
	Stat             int        `json:"stat"`
	StatStr          string     `json:"stat_string"`
	Files            []FileInfo `json:"file_stats"`
	ConnectedSeeders int        `json:"connected_seeders"`
	ActivePeers      int        `json:"active_peers"`
	TotalPeers       int        `json:"total_peers"`
}

// WaitConnectable reports whether a loaded torrent can actually reach the swarm — guards
// against "ghost" releases whose indexer scrape count is high but have no reachable peers.
// It requires a REAL connection (a connected seeder or active peer) within the window — not
// merely peer DISCOVERY: a ghost can discover peers (total_peers>0) yet never connect to any,
// which still won't stream, so discovery alone is not enough.
func (c *Client) WaitConnectable(hash string, secs int) bool {
	for i := 0; i < secs; i++ {
		if info, err := c.Stat(hash); err == nil && (info.ConnectedSeeders > 0 || info.ActivePeers > 0) {
			return true
		}
		time.Sleep(1 * time.Second)
	}
	return false
}

// Add adds a magnet to TorrServer and returns its info hash.
func (c *Client) Add(magnet, title string) (string, error) {
	payload := map[string]any{
		"action": "add",
		"link":   magnet,
		"title":  title,
	}
	var result TorrentInfo
	if err := c.post("/torrents", payload, &result); err != nil {
		return "", fmt.Errorf("torrserver add: %w", err)
	}
	if result.Hash == "" {
		return "", fmt.Errorf("torrserver add: empty hash in response")
	}
	return result.Hash, nil
}

// Stat returns the current status of a torrent including its file list.
func (c *Client) Stat(hash string) (*TorrentInfo, error) {
	payload := map[string]any{"action": "get", "hash": hash}
	var result TorrentInfo
	if err := c.post("/torrents", payload, &result); err != nil {
		return nil, fmt.Errorf("torrserver stat: %w", err)
	}
	return &result, nil
}

// Drop removes a torrent from TorrServer.
func (c *Client) Drop(hash string) error {
	payload := map[string]any{"action": "rem", "hash": hash}
	if err := c.post("/torrents", payload, nil); err != nil {
		return fmt.Errorf("torrserver drop: %w", err)
	}
	return nil
}

// List returns all torrent hashes currently in TorrServer.
func (c *Client) List() ([]string, error) {
	payload := map[string]any{"action": "list"}
	var results []TorrentInfo
	if err := c.post("/torrents", payload, &results); err != nil {
		return nil, fmt.Errorf("torrserver list: %w", err)
	}
	hashes := make([]string, len(results))
	for i, r := range results {
		hashes[i] = r.Hash
	}
	return hashes, nil
}

// StreamURL returns the HTTP stream URL for a file within a torrent.
// TorrServer MatriX requires the &play parameter to serve actual bytes.
func (c *Client) StreamURL(hash string, fileIndex int) string {
	return fmt.Sprintf("%s/stream?link=%s&index=%d&play", c.base(), hash, fileIndex)
}

// BestFileIndex picks the largest file in the torrent (the main video file).
// Use for movies or when episode matching is not needed.
func BestFileIndex(files []FileInfo) int {
	best := 0
	var bestSize int64
	for _, f := range files {
		if f.Length > bestSize {
			bestSize = f.Length
			best = f.ID
		}
	}
	return best
}

var videoExtSet = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".m4v": true,
	".mov": true, ".ts": true, ".wmv": true, ".flv": true,
}

// episodePatterns returns the SxxEyy / NxNN tokens that identify a specific episode.
func episodePatterns(season, episode int) []string {
	return []string{
		fmt.Sprintf("s%02de%02d", season, episode), // s01e05
		fmt.Sprintf("s%de%d", season, episode),     // s1e5
		fmt.Sprintf("%dx%02d", season, episode),    // 1x05
		fmt.Sprintf("s%02d.e%02d", season, episode),
		fmt.Sprintf("s%02d e%02d", season, episode),
		fmt.Sprintf("s%02dxe%02d", season, episode),
	}
}

// MatchesEpisode reports whether a name (release title or file path) contains a
// token clearly identifying the given season+episode. This is the reliable
// discriminator between Western episodic TV (SxxEyy) and anime "- NN" numbering,
// so a same-named anime never gets grabbed for a TV episode request.
func MatchesEpisode(name string, season, episode int) bool {
	lower := strings.ToLower(name)
	for _, p := range episodePatterns(season, episode) {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func videoFiles(files []FileInfo) []FileInfo {
	var out []FileInfo
	for _, f := range files {
		ext := strings.ToLower(f.Path)
		if i := strings.LastIndex(ext, "."); i >= 0 && videoExtSet[ext[i:]] {
			out = append(out, f)
		}
	}
	return out
}

// EpisodeFileIndex finds the file matching a specific S##E## episode and reports
// whether the match is trustworthy. matched=false means the torrent is a
// multi-file set with NO file for the requested episode — i.e. a mislabeled or
// wrong-show pack — and the caller should reject it rather than guess.
func EpisodeFileIndex(files []FileInfo, season, episode int) (index int, matched bool) {
	var bestID int
	var bestSize int64
	for _, f := range files {
		if MatchesEpisode(f.Path, season, episode) && f.Length > bestSize {
			bestSize = f.Length
			bestID = f.ID
		}
	}
	if bestID != 0 {
		return bestID, true // confident SxxEyy filename match
	}
	// No episode token. Only trust a fallback for an unambiguous single-video
	// torrent (a lone episode file). Multiple files with no match = not confident.
	if vids := videoFiles(files); len(vids) == 1 {
		return vids[0].ID, true
	}
	return BestFileIndex(files), false
}

func (c *Client) post(path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Post(c.baseURL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
