package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"jellyfreedom/internal/redact"
)

type Client struct {
	mu         sync.RWMutex
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func New(baseURL, apiKey string) *Client {
	c := &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
	c.Configure(baseURL, apiKey)
	return c
}

// Configure updates the base URL and API key at runtime under a write lock.
func (c *Client) Configure(baseURL, apiKey string) {
	c.mu.Lock()
	c.baseURL = strings.TrimRight(baseURL, "/")
	c.apiKey = apiKey
	c.mu.Unlock()
	// Backstop: keep this key out of any error string or log line.
	redact.Register(apiKey)
}

// ErrNotConfigured is returned instead of attempting a request with no URL/key, so
// callers can render a specific setup message rather than a transport error.
var ErrNotConfigured = errors.New("Jellyfin is not configured — set its URL and API key in Settings → Connections")

// Ping reports whether Jellyfin is reachable and the API key is accepted.
func (c *Client) Ping(ctx context.Context) error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/System/Info", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Emby-Token", c.key())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Jellyfin is unreachable — check it is running and the URL in Settings → Connections")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("Jellyfin rejected the API key — re-enter it in Settings → Connections")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Jellyfin returned HTTP %d", resp.StatusCode)
	}
	return nil
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

// TriggerLibraryScan runs the "Scan Media Library" scheduled task, which picks
// up newly written .strm files. Falls back to /Library/Refresh if the task
// cannot be found.
func (c *Client) TriggerLibraryScan() error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	taskID, err := c.scanTaskID()
	if err == nil && taskID != "" {
		return c.apiPost("/ScheduledTasks/Running/"+taskID, nil)
	}
	return c.apiPost("/Library/Refresh", nil)
}

// scanTaskID returns the Jellyfin scheduled task ID for "Scan Media Library".
func (c *Client) scanTaskID() (string, error) {
	req, err := http.NewRequest(http.MethodGet, c.base()+"/ScheduledTasks", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Emby-Token", c.key())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var tasks []struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
		return "", err
	}
	for _, t := range tasks {
		if t.Name == "Scan Media Library" {
			return t.ID, nil
		}
	}
	return "", fmt.Errorf("task not found")
}

func (c *Client) apiPost(path string, body interface{}) error {
	req, err := http.NewRequest(http.MethodPost, c.base()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Emby-Token", c.key())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("jellyfin %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("jellyfin %s: status %d", path, resp.StatusCode)
	}
	return nil
}

// WebhookPayload is the body Jellyfin sends for playback events.
type WebhookPayload struct {
	NotificationType      string `json:"NotificationType"`
	ItemID                string `json:"ItemId"`
	ItemType              string `json:"ItemType"`
	PlaybackPositionTicks int64  `json:"PlaybackPositionTicks"`
}

// ParseWebhook decodes a Jellyfin webhook JSON body.
func ParseWebhook(r *http.Request) (*WebhookPayload, error) {
	var p WebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		return nil, fmt.Errorf("parse webhook: %w", err)
	}
	return &p, nil
}

type mediaItem struct {
	Path string `json:"Path"`
}

type itemResponse struct {
	MediaSources []mediaItem `json:"MediaSources"`
}

// ActiveSessionsForItem returns the number of Jellyfin sessions currently playing
// the given item ID. Used by the webhook handler to avoid dropping a torrent that
// another session is still streaming.
func (c *Client) ActiveSessionsForItem(itemID string) int {
	req, err := http.NewRequest(http.MethodGet, c.base()+"/Sessions", nil)
	if err != nil {
		return 0
	}
	req.Header.Set("X-Emby-Token", c.key())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var sessions []struct {
		NowPlayingItem *struct {
			ID string `json:"Id"`
		} `json:"NowPlayingItem"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return 0
	}
	n := 0
	for _, s := range sessions {
		if s.NowPlayingItem != nil && s.NowPlayingItem.ID == itemID {
			n++
		}
	}
	return n
}

// ActivePlaybackCount returns how many Jellyfin sessions are currently playing anything.
// Used by the VPN watchdog / port-forward keeper to DEFER non-urgent TorrServer restarts
// while someone is watching (so a port rotation or a transient blip never kills a stream).
//
// It FAILS CLOSED: any error returns an error, never 0. This used to return 0 on every
// failure path, which meant an unreachable or unconfigured Jellyfin told the watchdog
// "nobody is watching" and it would restart TorrServer in the middle of a stream. When
// the truth is unknown the safe answer is "assume someone is watching".
func (c *Client) ActivePlaybackCount() (int, error) {
	if !c.Configured() {
		return 0, ErrNotConfigured
	}
	req, err := http.NewRequest(http.MethodGet, c.base()+"/Sessions", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Emby-Token", c.key())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("jellyfin sessions: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("jellyfin sessions: status %d", resp.StatusCode)
	}
	var sessions []struct {
		NowPlayingItem *struct {
			ID string `json:"Id"`
		} `json:"NowPlayingItem"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return 0, fmt.Errorf("jellyfin sessions: %w", err)
	}
	n := 0
	for _, s := range sessions {
		if s.NowPlayingItem != nil {
			n++
		}
	}
	return n, nil
}

// JellyfinUser is a minimal user record returned by the Jellyfin /Users API.
type JellyfinUser struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

// ListUsers returns all Jellyfin users (requires admin API key).
func (c *Client) ListUsers() ([]JellyfinUser, error) {
	req, err := http.NewRequest(http.MethodGet, c.base()+"/Users", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Emby-Token", c.key())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jellyfin list users: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jellyfin list users: status %d", resp.StatusCode)
	}
	var users []JellyfinUser
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return nil, err
	}
	return users, nil
}

// AuthenticateUser validates a username+password pair against Jellyfin's auth endpoint.
// Returns nil on success, non-nil on failure or invalid credentials.
// AuthenticateUser verifies a password against Jellyfin and returns the ID of the account it
// actually authenticated.
//
// The ID matters, not just the yes/no. A JellyFreedom row imported from Jellyfin holds a
// jellyfin_user_id, but authentication is by NAME — so renaming that row silently repointed
// its credential at whoever now owns the new name in Jellyfin, inheriting the row's is_admin
// flag and library grants. The caller compares this against the stored ID.
func (c *Client) AuthenticateUser(username, password string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"Username": username,
		"Pw":       password,
	})
	req, err := http.NewRequest(http.MethodPost, c.base()+"/Users/AuthenticateByName",
		bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Authorization",
		`MediaBrowser Client="JellyFreedom", Device="Orchestrator", DeviceId="jellyfreedom-orchestrator", Version="1.0"`)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("jellyfin auth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("jellyfin auth: invalid credentials (status %d)", resp.StatusCode)
	}
	// Jellyfin answers with the account it authenticated. Decoding failure is not an auth
	// failure — an older server might not send it — so an empty id is returned and the
	// caller decides what to do with a row that has nothing to compare against.
	var out struct {
		User struct {
			ID string `json:"Id"`
		} `json:"User"`
	}
	if derr := json.NewDecoder(resp.Body).Decode(&out); derr != nil {
		return "", nil
	}
	return out.User.ID, nil
}

// GetItemPath resolves the file path for a Jellyfin item by its ID.
// Used to map a webhook ItemId back to a .strm path.
func (c *Client) GetItemPath(itemID string) (string, error) {
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/Items/%s", c.base(), itemID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Emby-Token", c.key())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("jellyfin get item: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("jellyfin get item: status %d", resp.StatusCode)
	}
	var item itemResponse
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return "", err
	}
	if len(item.MediaSources) > 0 {
		return item.MediaSources[0].Path, nil
	}
	return "", nil
}
