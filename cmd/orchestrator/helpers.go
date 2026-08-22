package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"jellyfreedom/internal/api"
	"jellyfreedom/internal/config"
	"jellyfreedom/internal/indexer"
	"jellyfreedom/internal/jellyfin"
	"jellyfreedom/internal/store"
	"jellyfreedom/internal/torrserver"
)

// streamClient proxies media. It deliberately has NO timeout: one response can carry a
// two-hour film, and any Client-level timeout would cut playback mid-stream. Per-request
// cancellation still comes from the request context.
var streamClient = &http.Client{}

// viewerOf resolves the caller into (user, username, isAdmin).
//
// The username is "" for an ANONYMOUS caller, and the store treats "" as anonymous —
// the most restrictive branch. It used to mean "admin", which is why logging out
// revealed private items that logging in hid.
func viewerOf(r *http.Request) (*store.User, string, bool) {
	u := api.UserFromContext(r)
	if u == nil {
		return nil, "", false
	}
	return u, u.Username, u.IsAdmin
}

// decodeBody reads a bounded JSON request body. Only ONE decode site in the whole
// codebase was bounded before; every other one would read an unbounded body into memory.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	return api.DecodeJSON(w, r, dst)
}

// indexerMessage turns an indexer error into a SPECIFIC, actionable message with no
// transport detail (and therefore no URL and no API key) in it.
func indexerMessage(err error) string {
	switch {
	case errors.Is(err, indexer.ErrNotConfigured), errors.Is(err, indexer.ErrBadKey):
		return err.Error()
	default:
		return "the indexer search failed — try Settings → Connections → Test"
	}
}

// preflight checks the dependencies a request actually needs BEFORE queueing work.
//
// Without it, an unreachable Prowlarr was discovered 150 seconds into a search, after
// the user had already been shown a queued item that would eventually fail with a
// transport error. Returns "" when everything needed is available.
func preflight(ctx context.Context, idx *indexer.Client, ts *torrserver.Client) string {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := idx.Ping(ctx); err != nil {
		if errors.Is(err, indexer.ErrNotConfigured) || errors.Is(err, indexer.ErrBadKey) {
			return err.Error()
		}
		return "Prowlarr is unreachable — Settings → Connections → Test"
	}
	if err := ts.Ping(ctx); err != nil {
		if errors.Is(err, torrserver.ErrNotConfigured) {
			return err.Error()
		}
		return "TorrServer is unreachable — Settings → Connections → Test"
	}
	return ""
}

// notifyJellyfinScan triggers a library scan and SURFACES failures.
//
// This error was discarded at four call sites, which produced precisely the "strange
// availability" failure this project set out to avoid: the .strm was written, the item
// was marked ready, JellyFreedom said "available" — and Jellyfin had never been told to
// look, so the user saw nothing.
func notifyJellyfinScan(jf *jellyfin.Client) {
	if err := jf.TriggerLibraryScan(); err != nil {
		slog.Error("JELLYFIN SCAN NOT TRIGGERED — new items will not appear in Jellyfin "+
			"until it scans. Run the 'jellyfin-scan' task from the dashboard, or fix "+
			"Settings → Connections → Jellyfin.", "err", err)
	}
}

// ── Play token enforcement ────────────────────────────────────────────────────

var playTokensEnforced atomic.Bool

func playTokenEnforced() bool { return playTokensEnforced.Load() }

// migrateStrmTokens rewrites every library .strm with a capability-tokenised /play URL
// and only then switches enforcement on.
//
// Order matters: enforcing first would instantly break playback of every item already
// in the library, because their .strm files carry no token. If any file cannot be
// rewritten we stay permissive and say so loudly, rather than half-enforcing.
func migrateStrmTokens(db *store.Store, cfg *config.Config) {
	items, err := db.ListAllItems()
	if err != nil {
		slog.Error("play tokens: could not list library items; capability tokens stay DISABLED", "err", err)
		return
	}
	rewritten, failed := 0, 0
	for _, it := range items {
		if it.StrmPath == "" {
			continue
		}
		want := playURL(cfg.Server.PublicURL, it.MediaType, it.TMDBID, it.Season, it.Episode)
		cur, rerr := os.ReadFile(it.StrmPath)
		if rerr == nil && strings.TrimSpace(string(cur)) == want {
			continue // already tokenised and current
		}
		if rerr != nil && !os.IsNotExist(rerr) {
			slog.Error("play tokens: could not read a .strm", "path", it.StrmPath, "err", rerr)
			failed++
			continue
		}
		if mkerr := os.MkdirAll(filepath.Dir(it.StrmPath), 0o755); mkerr != nil {
			slog.Error("play tokens: could not create the .strm directory", "path", it.StrmPath, "err", mkerr)
			failed++
			continue
		}
		if werr := os.WriteFile(it.StrmPath, []byte(want), 0o644); werr != nil {
			slog.Error("play tokens: could not rewrite a .strm", "path", it.StrmPath, "err", werr)
			failed++
			continue
		}
		rewritten++
	}
	if failed > 0 {
		slog.Error("play tokens: some .strm files could not be rewritten; capability tokens stay DISABLED "+
			"so playback keeps working. Fix the errors above and restart to enable them.",
			"rewritten", rewritten, "failed", failed)
		return
	}
	playTokensEnforced.Store(true)
	if err := db.SetSetting(playTokenRequiredSetting, "true"); err != nil {
		// Enforcement is in-memory and already on; the setting is only a record.
		slog.Warn("play tokens: enabled, but the marker setting could not be persisted", "err", err)
	}
	sweepOrphanStrmTokens(db, cfg)
	if rewritten > 0 {
		slog.Info("play tokens: rewrote .strm files with capability URLs", "count", rewritten)
	}
	slog.Info("play capability tokens are ENFORCED on /play")
}

// ── Jellyfin webhook authentication ───────────────────────────────────────────

const webhookSecretSetting = "webhook.secret"

// WebhookHeader is the header Jellyfin's webhook plugin is configured to send.
const WebhookHeader = "X-JellyFreedom-Token"

// ensureWebhookSecret generates the shared secret on first run and returns it.
func ensureWebhookSecret(db *store.Store) (string, error) {
	v, err := db.GetSetting(webhookSecretSetting)
	if err != nil {
		return "", err
	}
	if v != "" {
		return v, nil
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	v = hex.EncodeToString(b)
	if err := db.SetSetting(webhookSecretSetting, v); err != nil {
		return "", err
	}
	return v, nil
}

// webhookAuthorised validates the shared secret on a Jellyfin webhook call.
//
// The endpoint is state-changing (a PlaybackStop drops that torrent) and had NO
// authentication, so anyone who guessed an ItemId could kill a stream in progress. It
// cannot use a session — Jellyfin's plugin has no way to log in — so a shared secret in
// a custom header, which the plugin does support, is the mechanism.
func webhookAuthorised(db *store.Store, r *http.Request) bool {
	want, err := db.GetSetting(webhookSecretSetting)
	if err != nil {
		slog.Error("webhook: could not read the shared secret", "err", err)
		return false
	}
	if want == "" {
		// Fail CLOSED: no secret configured means the endpoint is closed, not open.
		return false
	}
	got := r.Header.Get(WebhookHeader)
	if got == "" {
		// Some deployments can only add a query parameter to the webhook URL.
		got = r.URL.Query().Get("token")
	}
	return got != "" && hmac.Equal([]byte(got), []byte(want))
}

// ── VPN config sanitisation (privilege contract, layer 1) ─────────────────────

// dangerousWGDirectives are the config keys wg-quick would execute as root shell
// commands, plus Table (which can silently suppress route installation and defeat a
// routing-only kill switch), SaveConfig, and DNS (needs resolvconf; handled per-netns).
var dangerousWGDirectives = []string{
	"postup", "postdown", "preup", "predown", "table", "saveconfig", "dns",
}

// vpnSanitizeConf strips every dangerous directive from an uploaded WireGuard config
// and reports which ones were removed, so the UI can tell the user what changed.
//
// The config directory is owned by the SERVICE user, so the service user controls the
// bytes that root's wg-quick parses. This is layer 1; the root-owned helper re-strips
// the same directives into a root-owned copy under /run at vpn-up (layer 2). Both are
// required: layer 2 makes the escalation impossible rather than merely unlikely, and
// survives any future bug in this validator.
func vpnSanitizeConf(conf string) (clean string, stripped []string) {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(conf, "\n") {
		t := strings.ToLower(strings.TrimSpace(line))
		drop := ""
		for _, d := range dangerousWGDirectives {
			// Match "Key =" / "Key=" only, so a value containing the word is unaffected.
			rest := strings.TrimPrefix(t, d)
			if len(rest) < len(t) && strings.HasPrefix(strings.TrimSpace(rest), "=") {
				drop = d
				break
			}
		}
		if drop != "" {
			if !seen[drop] {
				seen[drop] = true
				stripped = append(stripped, drop)
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"), stripped
}

// vpnState reports whether a VPN config exists and whether the tunnel is up.
func vpnState(dir string) (configured, active bool) {
	if b, err := os.ReadFile(filepath.Join(dir, "wg0-vpntorrent.conf")); err == nil && len(b) > 0 {
		configured = true
	}
	return configured, api.VPNConnected()
}

// applyTorrCacheSettings pushes an already-snapshotted cache profile to TorrServer.
func applyTorrCacheSettings(ts *torrserver.Client, cc config.TorrCacheConfig) error {
	if cc.Mode == "" {
		return nil
	}
	if cc.Mode == "disk" && cc.Path == "" {
		return fmt.Errorf("%w: disk cache mode requires torrserver.cache.path", errCacheProfileConfig)
	}
	return ts.ApplyCacheSettings(torrserver.CacheSettings{
		Mode:               cc.Mode,
		SizeMB:             cc.SizeMB,
		Path:               cc.Path,
		DisconnectTimeoutS: cc.DisconnectTimeoutS,
		ConnectionsLimit:   cc.ConnectionsLimit,
		RetrackersMode:     cc.RetrackersMode,
		UploadRateLimitKB:  cc.UploadRateLimitKB,
	})
}

// sweepOrphanStrmTokens re-signs .strm files that no library row points at.
//
// migrateStrmTokens walks database rows, so a .strm whose row has since been removed is
// invisible to it. Those files kept working before capability tokens existed, because
// /play resolves identity from the URL and needs no row — but once tokens are enforced an
// unsigned URL is a 403, and the item silently stops playing with no explanation.
//
// Identity is recoverable from the URL itself, so re-sign anything we can and report the
// rest rather than leaving a user to discover it during playback.
func sweepOrphanStrmTokens(db *store.Store, cfg *config.Config) {
	var rewritten, orphaned int
	seen := map[string]bool{}
	for _, lib := range cfg.Libraries {
		if lib.Path == "" {
			continue
		}
		_ = filepath.Walk(lib.Path, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() || !strings.EqualFold(filepath.Ext(path), ".strm") {
				return nil
			}
			if seen[path] {
				return nil
			}
			seen[path] = true
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			cur := strings.TrimSpace(string(raw))
			if cur == "" || strings.Contains(cur, "?t=") {
				return nil // already carries a capability token
			}
			mediaType, tmdbID, season, episode, ok := parsePlayURL(cur)
			if !ok {
				// Legacy /proxy/stream?link=<hash> form predates resolve-at-play. The hash is
				// the only identity it carries, so recover the rest from the library if the
				// item is still known.
				if h := parseLegacyStreamHash(cur); h != "" {
					if it, gerr := db.GetByHash(h); gerr == nil && it != nil {
						mediaType, tmdbID, season, episode, ok = it.MediaType, it.TMDBID, it.Season, it.Episode, true
					}
				}
			}
			if !ok || tmdbID == 0 {
				orphaned++
				slog.Warn("play tokens: a .strm could not be re-signed and will not play; "+
					"re-request the item to regenerate it, or delete the file",
					"path", path, "contents", cur)
				return nil
			}
			want := playURL(cfg.Server.PublicURL, mediaType, tmdbID, season, episode)
			if werr := os.WriteFile(path, []byte(want), 0o644); werr != nil {
				slog.Error("play tokens: could not re-sign an orphaned .strm", "path", path, "err", werr)
				orphaned++
				return nil
			}
			rewritten++
			return nil
		})
	}
	if rewritten > 0 {
		slog.Info("play tokens: re-signed .strm files with no library row", "count", rewritten)
	}
	if orphaned > 0 {
		slog.Warn("play tokens: some .strm files could not be re-signed", "count", orphaned)
	}
}

// parsePlayURL extracts the identity from a /play/... URL this server wrote.
func parsePlayURL(raw string) (mediaType string, tmdbID, season, episode int, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, 0, 0, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	// play/movie/{tmdb}  |  play/tv/{tmdb}/{season}/{episode}
	if len(parts) < 3 || parts[0] != "play" {
		return "", 0, 0, 0, false
	}
	switch parts[1] {
	case "movie":
		if id, cerr := strconv.Atoi(parts[2]); cerr == nil {
			return "movie", id, 0, 0, true
		}
	case "tv":
		if len(parts) < 5 {
			return "", 0, 0, 0, false
		}
		id, e1 := strconv.Atoi(parts[2])
		sn, e2 := strconv.Atoi(parts[3])
		ep, e3 := strconv.Atoi(parts[4])
		if e1 == nil && e2 == nil && e3 == nil {
			return "tv", id, sn, ep, true
		}
	}
	return "", 0, 0, 0, false
}

// parseLegacyStreamHash pulls the info hash out of a pre-resolve-at-play .strm.
func parseLegacyStreamHash(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !strings.Contains(u.Path, "/proxy/stream") {
		return ""
	}
	return u.Query().Get("link")
}
