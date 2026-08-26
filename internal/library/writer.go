package library

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// WriteMovieStrm writes a .strm for a movie into dir and returns its path.
// Layout: <dir>/<Title> (<Year>)/<Title> (<Year>).strm
func WriteMovieStrm(dir, title, year, streamURL string) (string, error) {
	safe := safeName(fmt.Sprintf("%s (%s)", title, year))
	d := filepath.Join(dir, safe)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", fmt.Errorf("create movie dir: %w", err)
	}
	path := filepath.Join(d, safe+".strm")
	return path, os.WriteFile(path, []byte(streamURL), 0o644)
}

// WriteTVStrm writes a .strm for a TV episode into dir and returns its path.
// Layout: <dir>/<Show> (<Year>)/Season <NN>/<Show> (<Year>) S<NN>E<NN>.strm
func WriteTVStrm(dir, show, year string, season, episode int, streamURL string) (string, error) {
	path := TVStrmPath(dir, show, year, season, episode)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create tv dir: %w", err)
	}
	return path, os.WriteFile(path, []byte(streamURL), 0o644)
}

// TVStrmPath returns the path that WriteTVStrm would write, without creating anything.
func TVStrmPath(dir, show, year string, season, episode int) string {
	showSafe := safeName(fmt.Sprintf("%s (%s)", show, year))
	episodeName := fmt.Sprintf("%s S%02dE%02d", showSafe, season, episode)
	return filepath.Join(dir, showSafe, fmt.Sprintf("Season %02d", season), episodeName+".strm")
}

// RemoveStrm deletes a .strm file and its parent directory if empty.
func RemoveStrm(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	dir := filepath.Dir(path)
	if entries, _ := os.ReadDir(dir); len(entries) == 0 {
		os.Remove(dir)
	}
	return nil
}

var unsafeChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

func safeName(s string) string {
	return strings.TrimSpace(unsafeChars.ReplaceAllString(s, ""))
}

// ── Play URL shapes ───────────────────────────────────────────────────────────
//
// A .strm file holds exactly one line: the URL Jellyfin fetches when someone presses
// play. There are ~1,000 of those files in the live library already, every one of them
// carrying a capability token that is an HMAC over the identity encoded in its own path.
// That makes the bytes this package emits for a TMDB identity a compatibility contract
// rather than an implementation detail: change the path by one character and every
// existing token stops matching the identity the path now spells, and every one of those
// files 403s at once.
//
// So there are two shapes, and the first one is frozen:
//
//	TMDB (frozen):  <base>/play/movie/<id>            <base>/play/tv/<id>/<season>/<episode>
//	any provider:   <base>/play/p/<provider>/movie/<id>
//	                <base>/play/p/<provider>/tv/<id>/<season>/<episode>
//
// The `/p/` segment is what keeps the two apart. "p" cannot be confused with a media
// type — the only media types are "movie" and "tv" — so a router can tell which shape it
// is looking at from one path segment, and no legacy URL can ever be parsed as a
// provider-namespaced one or the reverse.

// ProviderTMDB names the provider every identity in the library had before a second
// metadata provider was possible. Its URLs and tokens are the ones that must not move.
const ProviderTMDB = "tmdb"

// Bounds on the two externally-supplied identity fields. Both land in a URL path AND in
// the HMAC input that authorises playback, so they are validated before either is built —
// never after, and never only in one of the two places.
const (
	// MaxProviderLen keeps a provider name to something a human would type. Providers are
	// registered in code, not supplied by users, so this is a backstop.
	MaxProviderLen = 16
	// MaxProviderIDLen comfortably fits a 36-character UUID with room for other providers'
	// id shapes, while refusing an id long enough to be an attempt at something else. An
	// unbounded id would go into a path segment, a log line and an HMAC input.
	MaxProviderIDLen = 64
)

// Season and episode are deliberately NOT range-checked.
//
// They are Go ints rendered with %d, which is at most 20 bytes and contains no ':' and no
// '/' for any possible value — so they are neither a delimiter risk nor a length risk, the
// two things this validation exists to prevent. A range check here would instead create a
// compatibility cliff: .strm files already on disk are re-signed by reading the season and
// episode back out of their own URL, and any file outside a new bound would be rewritten to
// something unusable rather than left alone. The HTTP routes reject negative seasons at the
// edge, where a 400 is the right answer; the encoder's job is only to be unambiguous.

// providerRe is a deliberately tiny allowlist: lowercase letters and digits only.
//
// It is an allowlist and not a denylist because the value is concatenated into a URL path
// and into an HMAC input, and the set of characters that are dangerous in one of those two
// places is not the set that is dangerous in the other. Enumerating what is safe is the
// only version of this that stays correct when a third use appears. In particular it
// cannot contain ':' (the HMAC field delimiter), '/' (a path separator), '.' (so ".." is
// unconstructible), or '%' (so nothing survives a percent-decoding round trip changed).
var providerRe = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

// providerIDRe allows exactly the characters needed by the id shapes that exist —
// TMDB's decimal integers, and UUIDs, which are hex with hyphens — plus '_' and an
// optional leading '-' for negative legacy integers, and nothing else.
//
// Same reasoning as providerRe, and one more: every character here is in RFC 3986's
// unreserved set, so the id needs no percent-encoding to sit in a path segment. That
// matters because the token signs the DECODED identity while the router sees the ENCODED
// path; if the two could differ, an attacker could pick an encoding that hashes to one
// identity and routes to another. They cannot differ if no character ever needs encoding.
var providerIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidProvider reports whether p is an acceptable provider name.
func ValidProvider(p string) bool { return providerRe.MatchString(p) }

// ValidProviderID reports whether id is an acceptable provider-scoped identifier.
func ValidProviderID(id string) bool { return providerIDRe.MatchString(id) }

// ValidMediaType reports whether mt is one of the two media types the system has. It is
// strict on purpose: the legacy identity encoder treats "anything that is not movie" as
// tv, which is fine for a closed call graph but is not something to carry into a value
// that comes off the wire.
func ValidMediaType(mt string) bool { return mt == "movie" || mt == "tv" }

// ValidPlayRef validates every field that goes into a play URL or a play token. Callers
// that build either one must run this first; both builders below run it themselves so
// that "forgot to validate" is not reachable.
func ValidPlayRef(provider, mediaType, providerID string, season, episode int) error {
	if !ValidProvider(provider) {
		return fmt.Errorf("invalid provider %q: want 1-%d chars of [a-z0-9]", provider, MaxProviderLen)
	}
	if !ValidProviderID(providerID) {
		return fmt.Errorf("invalid provider id (%d bytes): want 1-%d chars of [A-Za-z0-9_-]", len(providerID), MaxProviderIDLen)
	}
	if !ValidMediaType(mediaType) {
		return fmt.Errorf("invalid media type %q: want movie or tv", mediaType)
	}
	// season and episode need no check — see the comment on the constants above. The movie
	// shape ignores both, exactly as the legacy identity encoder does.
	return nil
}

// PlayPath returns the path part of a play URL — leading slash, no host, no query.
//
// For ProviderTMDB it returns the frozen legacy shape, byte for byte. For anything else
// it returns the /play/p/<provider>/... shape. No escaping happens here and none is
// needed: ValidPlayRef has already guaranteed every interpolated field consists solely of
// characters that are legal, unreserved and unescaped in a path segment.
func PlayPath(provider, mediaType, providerID string, season, episode int) (string, error) {
	if err := ValidPlayRef(provider, mediaType, providerID, season, episode); err != nil {
		return "", err
	}
	prefix := "/play"
	if provider != ProviderTMDB {
		prefix = "/play/p/" + provider
	}
	if mediaType == "movie" {
		return prefix + "/movie/" + providerID, nil
	}
	return fmt.Sprintf("%s/tv/%s/%d/%d", prefix, providerID, season, episode), nil
}

// PlayURL joins a public base URL, a play path and a capability token into the single
// line written into a .strm file.
//
// An empty token yields a URL with no "?t=" at all rather than an empty one. That is not
// cosmetic: the startup re-signing sweep skips any .strm already containing "?t=", so
// emitting "?t=" with nothing after it would permanently mark a broken file as done.
func PlayURL(publicURL, provider, mediaType, providerID string, season, episode int, token string) (string, error) {
	path, err := PlayPath(provider, mediaType, providerID, season, episode)
	if err != nil {
		return "", err
	}
	u := strings.TrimRight(publicURL, "/") + path
	if token != "" {
		u += "?t=" + token
	}
	return u, nil
}
