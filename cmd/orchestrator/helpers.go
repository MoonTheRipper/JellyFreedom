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
	"regexp"
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
		// From the row's own identity, not its TMDB spelling — see itemRef.
		want := playURLFor(cfg.Server.PublicURL, itemRef(it))
		if want == "" {
			// playURLFor already logged which identity it refused. Writing the empty
			// string here would replace a broken pointer with an unreadable one.
			failed++
			continue
		}
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

// ── Machine-caller shared secrets ─────────────────────────────────────────────
//
// Two endpoints in this program are called by a PROGRAM rather than by a browser: the
// Jellyfin webhook, and the external-provider ingest API. Neither can hold a session —
// Jellyfin's webhook plugin has no way to log in, and a daemon should not be storing a
// human's password — so both are authenticated by a secret generated on first run,
// stored in the settings table, shown to an admin once through the dashboard, and
// presented in a request header.
//
// The generation and the comparison live in ONE place each, below, so that a future
// third machine caller cannot arrive with its own subtly weaker copy: a comparison that
// forgot to be constant-time, or one that treated a missing secret as "no auth
// required". Both of those have shipped in real systems and both are one careless
// copy-paste away.

const webhookSecretSetting = "webhook.secret"

// WebhookHeader is the header Jellyfin's webhook plugin is configured to send.
const WebhookHeader = "X-JellyFreedom-Token"

// ensureSharedSecret generates a 24-byte random secret for key on first run, persists
// it, and returns whatever is stored.
//
// 24 bytes from crypto/rand is 192 bits of entropy, rendered as 48 hex characters. That
// is far past brute force over a network, which matters because these endpoints have no
// rate limit and no lockout: the secret's own size is the entire defence against
// guessing, so it is sized to make guessing not worth modelling.
func ensureSharedSecret(db *store.Store, key string) (string, error) {
	v, err := db.GetSetting(key)
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
	if err := db.SetSetting(key, v); err != nil {
		return "", err
	}
	return v, nil
}

// sharedSecretMatch compares a presented secret against the stored one, in constant
// time, and FAILS CLOSED on every abnormal case.
//
// The three refusals are all deliberate and all mean "no":
//
//   - The setting could not be read. A database error is not permission to proceed.
//   - No secret is stored at all. An endpoint with no secret configured is CLOSED, not
//     open — the opposite reading turns a failed first-run initialisation into an
//     unauthenticated, state-changing, file-writing endpoint.
//   - The caller presented nothing. Checked explicitly so that an empty presented value
//     can never compare equal to an empty stored one.
//
// hmac.Equal is subtle.ConstantTimeCompare, so a wrong secret takes the same time to
// reject regardless of how many leading bytes were right. A byte-by-byte == would leak
// the secret one character at a time to a caller who can measure the difference, and
// these endpoints are reachable from anywhere the orchestrator's port is.
func sharedSecretMatch(db *store.Store, key, got string) bool {
	want, err := db.GetSetting(key)
	if err != nil {
		slog.Error("shared secret: could not read the stored value", "setting", key, "err", err)
		return false
	}
	if want == "" || got == "" {
		return false
	}
	return hmac.Equal([]byte(got), []byte(want))
}

// ensureWebhookSecret generates the shared secret on first run and returns it.
func ensureWebhookSecret(db *store.Store) (string, error) {
	return ensureSharedSecret(db, webhookSecretSetting)
}

// webhookAuthorised validates the shared secret on a Jellyfin webhook call.
//
// The endpoint is state-changing (a PlaybackStop drops that torrent) and had NO
// authentication, so anyone who guessed an ItemId could kill a stream in progress. It
// cannot use a session — Jellyfin's plugin has no way to log in — so a shared secret in
// a custom header, which the plugin does support, is the mechanism.
func webhookAuthorised(db *store.Store, r *http.Request) bool {
	got := r.Header.Get(WebhookHeader)
	if got == "" {
		// Some deployments can only add a query parameter to the webhook URL.
		got = r.URL.Query().Get("token")
	}
	return sharedSecretMatch(db, webhookSecretSetting, got)
}

// ── External-provider ingest authentication ───────────────────────────────────

const ingestSecretSetting = "ingest.secret"

// IngestHeader is the header a provider daemon presents its secret in.
const IngestHeader = "X-JellyFreedom-Ingest"

// ensureIngestSecret generates the ingest secret on first run and returns it.
func ensureIngestSecret(db *store.Store) (string, error) {
	return ensureSharedSecret(db, ingestSecretSetting)
}

// ingestAuthorised validates the shared secret on an ingest API call.
//
// Header ONLY, with no query-parameter fallback — which is the one place this
// deliberately differs from webhookAuthorised. The webhook has that fallback because
// some Jellyfin deployments genuinely cannot set a custom header; a daemon written
// against this API always can. A secret in a query string is a secret in every access
// log, every proxy log and every Referer, and there is no reason to accept one here.
func ingestAuthorised(db *store.Store, r *http.Request) bool {
	return sharedSecretMatch(db, ingestSecretSetting, r.Header.Get(IngestHeader))
}

// ── External-provider ingest input validation ─────────────────────────────────
//
// Everything below treats the request body as HOSTILE. The ingest caller is trusted to
// the extent that it holds the secret, and no further: it names a file that gets
// written, a library it gets written into, a magnet that gets handed to TorrServer, and
// a poster URL that gets rendered in a browser. Each of those is a different sink with a
// different dangerous character set, so each field is validated against the sink it
// reaches rather than against one generic notion of "clean".

// Field bounds. Every one of these is a length, and every one of them exists because
// the unbounded version is a real failure and not a theoretical one:
//
//   - A title becomes a directory name AND a filename; unbounded, it is ENAMETOOLONG at
//     best (internal/library caps it too — this is the earlier, louder refusal).
//   - A poster URL is echoed into the web UI's <img src>; unbounded, it is a row that
//     bloats every library listing for everyone.
//   - A magnet is stored and later placed in an outbound request to TorrServer.
//   - Season, episode and file index are rendered into a path and an upstream query.
const (
	maxIngestBody       = 16 << 10 // 16 KiB — the largest legitimate body is well under 2 KiB
	maxIngestTitleLen   = 200      // runes, not bytes: a CJK title is short in runes and long in bytes
	maxIngestPosterLen  = 512
	maxIngestMagnetLen  = 4096
	maxIngestReleaseLen = 300
	maxIngestSeasonOrEp = 9999
	maxIngestFileIndex  = 9999
)

// yearRe accepts a four-digit year or nothing at all. Tight on purpose: the year is
// concatenated into the .strm directory name, and "any short string" would put arbitrary
// text there for no benefit — nothing in the system reads a year that is not a year.
var yearRe = regexp.MustCompile(`^[0-9]{4}$`)

// hasControlChars reports whether s contains any C0/C1 control character.
//
// Applied to every free-text field that is stored and later logged. internal/library
// strips controls out of the FILENAME, but the untouched string still reaches slog and
// the JSON API, and a '\n' in a title is a forged extra line in the server log — the
// oldest way there is to make an audit trail say something it did not.
func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return true
		}
	}
	return false
}

// validPosterURL reports whether u is a poster URL safe to store and hand to a browser.
//
// The scheme allowlist is the load-bearing part. This string ends up as the src of an
// <img> in the media UI, and "javascript:" and "data:text/html" in that position are
// script execution in the session of whoever is looking at the library — from an input
// supplied by a daemon that is only supposed to be naming pictures. An allowlist of
// http and https is the only version of this check that cannot be walked around with a
// scheme nobody thought of.
func validPosterURL(u string) bool {
	if len(u) > maxIngestPosterLen || hasControlChars(u) {
		return false
	}
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return false
	}
	return p.Scheme == "http" || p.Scheme == "https"
}

// magnetInfoHash extracts the v1 info hash from a magnet link, or reports failure.
//
// It parses the magnet as a URL rather than pattern-matching the string, so a caller
// cannot hide a second xt inside something that merely LOOKS like a query. url.Parse
// also rejects raw control characters, which matters because this string is later
// interpolated into an outbound request to TorrServer.
//
// Only the canonical 40-hex btih form is accepted. Base32 magnets exist, but every
// other part of this system — the items table, /proxy/stream, torrserver.ValidInfoHash —
// speaks 40-hex, and accepting a spelling we would then have to convert means two
// spellings of one identity and a real chance of caching a release under a hash that
// does not match the one playback looks up.
func magnetInfoHash(magnet string) (string, bool) {
	u, err := url.Parse(magnet)
	if err != nil || !strings.EqualFold(u.Scheme, "magnet") {
		return "", false
	}
	for _, xt := range u.Query()["xt"] {
		const prefix = "urn:btih:"
		if len(xt) <= len(prefix) || !strings.EqualFold(xt[:len(prefix)], prefix) {
			continue
		}
		h := xt[len(prefix):]
		if torrserver.ValidInfoHash(h) {
			return strings.ToLower(h), true
		}
	}
	return "", false
}

// errIngestSource is the single refusal for every way the playable source can be wrong.
// One message for all of them so the endpoint cannot be used as an oracle that
// distinguishes "your magnet parsed but the hash was bad" from "your hash disagreed
// with your magnet" — and, more prosaically, so the message never contains the input.
var errIngestSource = errors.New("a magnet with a 40-hex btih info hash, or an info_hash, is required")

// ingestSource validates the caller's playable source and returns the canonical pair.
//
// Either field alone is enough, and both together must AGREE. That last rule is not
// pedantry: the info hash is what /play looks the torrent up by and what the orphan
// cleaner counts references with, while the magnet is what actually gets added to
// TorrServer. If the two named different torrents, JellyFreedom would add one and then
// stream, drop and account for the other — permanently, and silently.
//
// When only a hash is given, the magnet is synthesised from it. A bare
// magnet:?xt=urn:btih:<hash> is a working magnet on its own (DHT and the configured
// retrackers supply the peers), so this is a real magnet and not a placeholder.
func ingestSource(infoHash, magnet string) (hash, canonicalMagnet string, err error) {
	infoHash = strings.TrimSpace(infoHash)
	magnet = strings.TrimSpace(magnet)
	if len(magnet) > maxIngestMagnetLen {
		return "", "", errIngestSource
	}
	if infoHash != "" && !torrserver.ValidInfoHash(infoHash) {
		return "", "", errIngestSource
	}
	infoHash = strings.ToLower(infoHash)
	if magnet == "" {
		if infoHash == "" {
			return "", "", errIngestSource
		}
		return infoHash, "magnet:?xt=urn:btih:" + infoHash, nil
	}
	fromMagnet, ok := magnetInfoHash(magnet)
	if !ok {
		return "", "", errIngestSource
	}
	if infoHash != "" && infoHash != fromMagnet {
		return "", "", errIngestSource
	}
	return fromMagnet, magnet, nil
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
