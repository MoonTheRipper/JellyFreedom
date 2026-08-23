package torrserver

import (
	"bytes"
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
)

// infoHashRe matches a canonical 40-character hex BitTorrent v1 info hash.
var infoHashRe = regexp.MustCompile(`^[A-Fa-f0-9]{40}$`)

// ValidInfoHash reports whether s is a well-formed 40-hex info hash.
//
// Every hash that reaches TorrServer or is interpolated into a stream URL must pass
// this first: the stream proxy took `link` straight from the query string and pasted
// it into an upstream URL unescaped, so an attacker-supplied value could both smuggle
// URL parameters and cause an ARBITRARY torrent to be added over the user's VPN.
func ValidInfoHash(s string) bool { return infoHashRe.MatchString(s) }

// ErrNotConfigured is returned instead of dialling an empty base URL.
var ErrNotConfigured = errors.New("TorrServer is not configured — set its URL in Settings → Connections")

type Client struct {
	mu         sync.RWMutex
	baseURL    string
	httpClient *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
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
func (c *Client) EnsureLoaded(ctx context.Context, hash, magnet, title string, waitSecs int) bool {
	if info, err := c.Stat(hash); err == nil && len(info.Files) > 0 {
		return true // already loaded
	}
	if magnet == "" {
		magnet = "magnet:?xt=urn:btih:" + hash
	}
	if _, err := c.Add(magnet, title); err != nil {
		return false
	}
	for i := 0; i < waitSecs; i++ {
		if info, err := c.Stat(hash); err == nil && len(info.Files) > 0 {
			return true
		}
		if !sleepCtx(ctx, time.Second) {
			return false // caller went away / deadline hit — stop burning the wait
		}
	}
	return false
}

// sleepCtx waits d, or returns false as soon as ctx is done.
//
// These waits used to be bare time.Sleep in a loop that ignored the request context
// entirely, so a client that hung up still held the handler for the full window — and
// /play could stack EnsureLoaded + WaitConnectable + a 150s indexer search + four
// candidate attempts into roughly five minutes of unbreakable work.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
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
func (c *Client) WaitConnectable(ctx context.Context, hash string, secs int) bool {
	for i := 0; i < secs; i++ {
		if info, err := c.Stat(hash); err == nil && (info.ConnectedSeeders > 0 || info.ActivePeers > 0) {
			return true
		}
		if !sleepCtx(ctx, time.Second) {
			return false
		}
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
	// Query values are escaped rather than interpolated raw; hash is additionally
	// constrained to 40-hex by ValidInfoHash at every entry point.
	q := url.Values{}
	q.Set("link", hash)
	q.Set("index", strconv.Itoa(fileIndex))
	return fmt.Sprintf("%s/stream?%s&play", c.base(), q.Encode())
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

// episodeKey identifies a compiled episode matcher in the cache.
type episodeKey struct{ season, episode int }

// episodeReCache memoises the compiled matchers. EpisodeFileIndex runs one matcher
// across every file of a season pack, so without this a 30-file pack recompiled the
// same regex 30 times per candidate release.
var episodeReCache sync.Map // episodeKey -> *regexp.Regexp

// episodeMatcher returns the compiled SxxEyy / NxNN matcher for one episode.
//
// This used to be a list of fmt.Sprintf'd substrings tested with strings.Contains, and
// that was a correctness bug, not just a sloppiness: the shorthand pattern "s1e5" is a
// prefix of "S1E50".."S1E59", and "s1e1" is a prefix of "S1E10".."S1E19". Because the
// same function is used BOTH to filter candidate releases AND to pick the file inside a
// season pack, a request for episode 5 could select episode 50's file and report the
// match as confident — silently playing the wrong episode.
//
// The fix is a real boundary on both ends:
//
//   - leading (?:^|[^0-9a-z]) — a separator must precede the token, so the height pair
//     "1920x1080" cannot satisfy a request for season 20 episode 108 ("20x108" is a
//     substring of it).
//   - trailing (?:$|[^0-9]) — a DIGIT may not follow, which is what kills the S1E5 /
//     S1E50 collision. Letters are deliberately still allowed so real-world suffixes
//     keep matching: "S01E05v2" (a re-encode) and "S01E05E06" (a double episode that
//     genuinely contains episode 5).
//
// 0* on each number absorbs zero-padding, so one pattern covers s1e5, s01e05 and
// s001e005 without enumerating them.
func episodeMatcher(season, episode int) *regexp.Regexp {
	key := episodeKey{season, episode}
	if re, ok := episodeReCache.Load(key); ok {
		return re.(*regexp.Regexp)
	}
	// The separator class covers s01e05, s01.e05, s01 e05, s01_e05, s01-e05, s01xe05
	// and the "Show S01/E05.mkv" directory layout.
	pat := fmt.Sprintf(`(?:^|[^0-9a-z])(?:s0*%[1]d[ ._/x-]?e0*%[2]d|0*%[1]dx0*%[2]d)(?:$|[^0-9])`,
		season, episode)
	re := regexp.MustCompile(pat)
	episodeReCache.Store(key, re)
	return re
}

// MatchesEpisode reports whether a name (release title or file path) contains a
// token clearly identifying the given season+episode. This is the reliable
// discriminator between Western episodic TV (SxxEyy) and anime "- NN" numbering,
// so a same-named anime never gets grabbed for a TV episode request.
func MatchesEpisode(name string, season, episode int) bool {
	if season < 0 || episode < 0 {
		return false
	}
	return episodeMatcher(season, episode).MatchString(strings.ToLower(name))
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

// baseName returns the last path component of a torrent file path. Torrent paths are
// "/"-separated, but Windows-authored torrents occasionally carry a backslash, so both
// separators are cut.
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// EpisodeFileIndex finds the file matching a specific S##E## episode and reports
// whether the match is trustworthy. matched=false means the torrent is a
// multi-file set with NO file for the requested episode — i.e. a mislabeled or
// wrong-show pack — and the caller should reject it rather than guess.
//
// The match is tried against the BASE FILENAME first, and this ordering is the whole
// point. Matching the full path let a directory component decide the answer for every
// file underneath it: in "Show.S01E01-E10.COMPLETE/Show.S01E07.mkv" the folder name
// contains "S01E01", so a request for episode 1 matched EVERY file in the pack, took
// the largest one, and returned it as a confident match. The viewer got a random
// episode with no indication anything had gone wrong.
//
// The full path is still consulted, because the token genuinely lives in the directory
// for layouts like "Show S01/E05.mkv" — but only when the basename pass found nothing
// AND the path pass identifies EXACTLY ONE file. More than one file matching only on
// its path means the token came from a shared ancestor directory, which is precisely
// the poisoning case above and carries no information about which file to play.
func EpisodeFileIndex(files []FileInfo, season, episode int) (index int, matched bool) {
	if id, ok := largestMatching(files, season, episode, baseName); ok {
		return id, true // confident SxxEyy filename match
	}
	if id, ok := uniqueMatching(files, season, episode); ok {
		return id, true // the token is in the directory, and it points at one file only
	}
	// No episode token. Only trust a fallback for an unambiguous single-video
	// torrent (a lone episode file). Multiple files with no match = not confident.
	if vids := videoFiles(files); len(vids) == 1 {
		return vids[0].ID, true
	}
	return BestFileIndex(files), false
}

// largestMatching returns the biggest file whose name (as produced by nameOf) carries
// the episode token. Biggest wins so a "Show.S01E01.sample.mkv" never beats the episode
// it was cut from.
//
// The found flag is separate from the ID on purpose: file IDs start at 0 in some
// torrents, and the previous code used `bestID != 0` as its "did we match" test — so a
// correct match on file 0 was reported as no match at all, and the caller fell through
// to the largest-file guess.
func largestMatching(files []FileInfo, season, episode int, nameOf func(string) string) (int, bool) {
	var bestID int
	var bestSize int64
	found := false
	for _, f := range files {
		if !MatchesEpisode(nameOf(f.Path), season, episode) {
			continue
		}
		if !found || f.Length > bestSize {
			bestID, bestSize, found = f.ID, f.Length, true
		}
	}
	return bestID, found
}

// uniqueMatching returns the single file matching on its full path, or found=false if
// zero or more than one file matches. See EpisodeFileIndex for why "more than one" is
// treated as no answer rather than as a tie to be broken by size.
func uniqueMatching(files []FileInfo, season, episode int) (int, bool) {
	id, n := 0, 0
	for _, f := range files {
		if MatchesEpisode(f.Path, season, episode) {
			id, n = f.ID, n+1
		}
	}
	return id, n == 1
}

func (c *Client) post(path string, payload any, out any) error {
	if c.base() == "" {
		return ErrNotConfigured
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// c.base() takes the read lock. Reading c.baseURL directly here raced with
	// Configure() writing it from the Settings handler (reproduced under -race);
	// every other accessor already went through base().
	resp, err := c.httpClient.Post(c.base()+path, "application/json", bytes.NewReader(body))
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

// Ping reports whether TorrServer is reachable. Used by the pre-flight dependency gate.
func (c *Client) Ping(ctx context.Context) error {
	base := c.base()
	if base == "" {
		return ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/echo", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("TorrServer is unreachable — check it is running and the URL in Settings \u2192 Connections")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("TorrServer returned HTTP %d", resp.StatusCode)
	}
	return nil
}
