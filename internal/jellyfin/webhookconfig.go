package jellyfin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ── Configuring the Webhook plugin ────────────────────────────────────────────
//
// Playback-stop cleanup needs a Generic destination in Jellyfin's Webhook plugin, set up
// exactly right: the PlaybackStop event, movies and episodes, a payload template, and the
// shared secret in a header. A destination with no template sends a body the orchestrator
// cannot parse, and one with no header is rejected, so a near miss looks like it works and
// never frees a torrent. The plugin's configuration API takes all of it in one call.

// WebhookTemplate is the payload the orchestrator parses (see WebhookPayload). The plugin
// stores templates base64-encoded.
const WebhookTemplate = `{"NotificationType":"{{NotificationType}}","ItemId":"{{ItemId}}","ItemType":"{{ItemType}}"}`

// WebhookName names our destination in the plugin, so a re-run replaces it rather than
// adding a second one.
const WebhookName = "JellyFreedom"

// ErrNoWebhookPlugin means the Webhook plugin is missing or not active.
var ErrNoWebhookPlugin = errors.New("the Webhook plugin is not installed and active in Jellyfin — install it from Dashboard → Plugins → Catalog, restart Jellyfin, and run this again")

// ConfigureWebhook points the Webhook plugin at hookURL, sending the secret in header. It
// replaces any destination that is ours — by name, or by pointing at a /webhook/jellyfin
// URL — and leaves every other destination and setting alone. It reads the configuration
// back to confirm Jellyfin kept it, and returns the plugin's version.
func (c *Client) ConfigureWebhook(ctx context.Context, hookURL, header, secret string) (string, error) {
	if !c.Configured() {
		return "", ErrNotConfigured
	}
	if secret == "" {
		return "", errors.New("no webhook secret to configure")
	}

	var plugins []struct {
		ID      string `json:"Id"`
		Name    string `json:"Name"`
		Version string `json:"Version"`
		Status  string `json:"Status"`
	}
	if err := c.getJSON(ctx, "/Plugins", &plugins); err != nil {
		return "", err
	}
	var id, version string
	for _, p := range plugins {
		if p.Name == "Webhook" && p.Status == "Active" {
			id, version = p.ID, p.Version
			break
		}
	}
	if id == "" {
		return "", ErrNoWebhookPlugin
	}

	// Decoded generically, so settings this code does not know about survive the round trip.
	path := "/Plugins/" + id + "/Configuration"
	var conf map[string]any
	if err := c.getJSON(ctx, path, &conf); err != nil {
		return "", err
	}
	ours := map[string]any{
		"NotificationTypes":    []string{"PlaybackStop"},
		"WebhookName":          WebhookName,
		"WebhookUri":           hookURL,
		"EnableMovies":         true,
		"EnableEpisodes":       true,
		"EnableVideos":         true,
		"EnableSeries":         false,
		"EnableSeasons":        false,
		"EnableAlbums":         false,
		"EnableSongs":          false,
		"SendAllProperties":    false,
		"TrimWhitespace":       false,
		"SkipEmptyMessageBody": false,
		"EnableWebhook":        true,
		"Template":             base64.StdEncoding.EncodeToString([]byte(WebhookTemplate)),
		"UserFilter":           []string{},
		"Headers":              []map[string]string{{"Key": header, "Value": secret}},
		"Fields":               []any{},
	}
	kept := []any{}
	if existing, ok := conf["GenericOptions"].([]any); ok {
		for _, d := range existing {
			if !isOurDestination(d) {
				kept = append(kept, d)
			}
		}
	}
	conf["GenericOptions"] = append(kept, ours)
	if err := c.postJSON(ctx, path, conf); err != nil {
		return "", err
	}

	// Jellyfin answers 204 to a configuration it then fails to deserialise, so only
	// reading it back proves anything.
	var back struct {
		GenericOptions []struct {
			WebhookName       string
			WebhookURI        string `json:"WebhookUri"`
			NotificationTypes []string
			Template          string
			Headers           []struct{ Key, Value string }
		}
	}
	if err := c.getJSON(ctx, path, &back); err != nil {
		return "", err
	}
	for _, d := range back.GenericOptions {
		if d.WebhookName == WebhookName && d.WebhookURI == hookURL &&
			len(d.NotificationTypes) == 1 && d.NotificationTypes[0] == "PlaybackStop" &&
			d.Template == ours["Template"] &&
			len(d.Headers) == 1 && d.Headers[0].Key == header && d.Headers[0].Value == secret {
			return version, nil
		}
	}
	return "", errors.New("Jellyfin did not keep the webhook settings")
}

// isOurDestination reports whether a Generic destination was made for JellyFreedom.
func isOurDestination(d any) bool {
	m, ok := d.(map[string]any)
	if !ok {
		return false
	}
	name, _ := m["WebhookName"].(string)
	uri, _ := m["WebhookUri"].(string)
	return name == WebhookName || strings.Contains(uri, "/webhook/jellyfin")
}

func (c *Client) getJSON(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.auth())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("jellyfin %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jellyfin %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("jellyfin %s: %w", path, err)
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.auth())
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("jellyfin %s: %w", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("jellyfin %s: status %d", path, resp.StatusCode)
	}
	return nil
}
