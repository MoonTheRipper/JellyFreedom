package library

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ErrUnsafePath is returned when a computed .strm path would land outside the library
// directory it was supposed to go into.
//
// It should be unreachable: safeName strips every path separator and refuses a name
// made only of dots, so nothing it produces can traverse. That is exactly why the check
// exists. safeName is a string transformation, and a string transformation is one
// forgotten character class away from being wrong — while the containment check is a
// statement about the RESULT, which stays true no matter what safeName does. The
// library directory is a Jellyfin media root; a caller who could write one byte outside
// it could write into any of them.
var ErrUnsafePath = errors.New("library: refusing to write outside the library directory")

// MaxNameLen bounds ONE path component, in bytes, before the ".strm" suffix and the
// " S00E00" infix are appended.
//
// It is not cosmetic. Every filesystem this runs on caps a single name at 255 bytes
// (NAME_MAX on ext4/xfs/btrfs, and the same figure on APFS and on SMB shares), and a
// name over that limit fails the write with ENAMETOOLONG rather than being trimmed —
// so an unbounded title is a broken library entry, not merely an ugly one. 240 is
// chosen so that the LONGEST thing built from a safe name still fits: the TV episode
// file is "<name> S00E00.strm", which is name + 12 bytes, i.e. at most 252.
//
// Truncation cannot change the path of anything already in a live library: at 241 bytes
// or more, every name this package would have produced was already unwritable.
const MaxNameLen = 240

// unsafeChars is the set of bytes that must never reach a filename.
//
// Deliberately an ALLOW-nothing denylist of the characters that carry meaning to a
// filesystem or a shell-adjacent consumer, rather than an allowlist of printable ASCII:
// titles are real-world text in every script, and an allowlist would mangle most of the
// world's film names into underscores. The classes, and why each is here:
//
//   - '/' and '\\' are path separators. Removing them is what makes traversal
//     impossible in the first place — "../../etc" becomes "....etc", a single component.
//   - ':' separates a volume on macOS and an alternate data stream on SMB.
//   - '<' '>' '"' '|' '?' '*' are illegal on Windows/SMB and are wildcards to a shell.
//   - \x00-\x1f are C0 controls: NUL truncates a C string, and \n \r turn one filename
//     into two lines in every log this path is ever printed to.
//   - \x7f is DEL, which is invisible in a terminal and belongs to no title.
//   - U+200B-U+200F and U+202A-U+202E and U+2066-U+2069 are zero-width and
//     bidirectional-override characters. They are legal in a filename and invisible in
//     one, which is precisely the problem: they let a supplied title render in a file
//     manager as a completely different name from the bytes on disk.
//
// What is NOT stripped, on purpose: unicode characters that merely LOOK like a
// separator, e.g. U+2044 FRACTION SLASH or U+FF0F FULLWIDTH SOLIDUS. They are ordinary
// letters to every filesystem — they cannot traverse anything — and removing them would
// corrupt titles that legitimately contain them. Their risk is visual spoofing of a
// name a human reads, which is not a risk this function can meaningfully address and
// not one that reaches outside the library directory.
var unsafeChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f\x7f\x{200b}-\x{200f}\x{202a}-\x{202e}\x{2066}-\x{2069}]`)

// reservedNames are the DOS device names. Windows and SMB refuse them as a filename
// with or without an extension, so "NUL.strm" on a CIFS-mounted library is a write that
// fails for a reason nobody will guess. Free to handle, and unreachable from this
// package's own callers (which always append " (year)"), so no existing path moves.
var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// safeName turns arbitrary supplied text into ONE filesystem path component.
//
// The contract it owes its callers, and which its tests assert directly rather than
// through the callers that happen to satisfy it today:
//
//  1. The result contains no path separator, so it is exactly one component.
//  2. The result is never "." or ".." or any run of dots, so joining it onto a
//     directory can only ever go DOWNWARDS.
//  3. The result is never empty, so a caller cannot end up writing to the directory
//     itself or to a dotfile named ".strm".
//  4. The result is at most MaxNameLen bytes and is valid UTF-8.
//
// Point 2 deserves the emphasis. Before this, safeName was a character filter and
// nothing more: safeName("..") returned "..", and filepath.Join(dir, "..") is dir's
// PARENT. That was not exploitable through the callers in this repo, because both of
// them format the name as "%s (%s)" first and the parentheses survive filtering — so no
// input could ever produce a pure dot run. But the safety of the whole write path then
// rested on a format string in the caller rather than on the function whose name claims
// to provide it, and the first caller to pass a bare title through would have inherited
// a directory traversal with no warning. Point 2 moves the guarantee to where it is
// named. WriteMovieStrm and WriteTVStrm then check containment independently anyway.
func safeName(s string) string {
	s = unsafeChars.ReplaceAllString(s, "")
	// TrimSpace is unicode-aware, so a title padded with U+00A0 or an ideographic space
	// is trimmed too. Trailing dots go with it: they are stripped by Windows/SMB on
	// creation, which would silently make the written path differ from the recorded one.
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ". ")
	s = truncateRunes(s, MaxNameLen)
	// Truncation can expose a new trailing dot or space, so re-trim after it, not before.
	s = strings.TrimRight(strings.TrimSpace(s), ". ")
	if s == "" || reservedNames[strings.ToLower(s)] {
		// "" is the empty-after-sanitising case (a title of "///" or "..." or " ").
		// There is no meaningful name left to preserve, so use a fixed placeholder
		// rather than inventing one from the rejected input.
		if s == "" {
			return "untitled"
		}
		return "_" + s
	}
	return s
}

// truncateRunes cuts s to at most n bytes without splitting a UTF-8 sequence.
//
// A byte-wise cut would leave a partial rune, which is not valid UTF-8; SQLite stores
// the untruncated title so the two would disagree, and some filesystems reject the
// name outright. Cutting on a rune boundary keeps the result a well-formed string.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// containedPath joins elems onto dir and refuses the result if it escaped dir.
//
// filepath.Join cleans as it joins, so a ".." anywhere in elems is resolved BEFORE this
// comparison — which is what makes the comparison meaningful rather than a substring
// check on unnormalised text. dir itself is cleaned for the same reason.
//
// This is a lexical check, not a filesystem one: it does not resolve symlinks, and it
// is not trying to. The threat it answers is a supplied string steering the path, and a
// symlink inside the library directory was put there by the operator, not by a caller.
func containedPath(dir string, elems ...string) (string, error) {
	base := filepath.Clean(dir)
	p := filepath.Join(append([]string{base}, elems...)...)
	if p != base && !strings.HasPrefix(p, base+string(filepath.Separator)) {
		return "", ErrUnsafePath
	}
	if p == base {
		// Joining produced the directory itself, so there is no file to write.
		return "", ErrUnsafePath
	}
	return p, nil
}

// WriteMovieStrm writes a .strm for a movie into dir and returns its path.
// Layout: <dir>/<Title> (<Year>)/<Title> (<Year>).strm
func WriteMovieStrm(dir, title, year, streamURL string) (string, error) {
	safe := safeName(fmt.Sprintf("%s (%s)", title, year))
	path, err := containedPath(dir, safe, safe+".strm")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create movie dir: %w", err)
	}
	return path, os.WriteFile(path, []byte(streamURL), 0o644)
}

// WriteTVStrm writes a .strm for a TV episode into dir and returns its path.
// Layout: <dir>/<Show> (<Year>)/Season <NN>/<Show> (<Year>) S<NN>E<NN>.strm
func WriteTVStrm(dir, show, year string, season, episode int, streamURL string) (string, error) {
	path, err := TVStrmPath(dir, show, year, season, episode)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create tv dir: %w", err)
	}
	return path, os.WriteFile(path, []byte(streamURL), 0o644)
}

// TVStrmPath returns the path that WriteTVStrm would write, without creating anything.
//
// It returns an error rather than a bare string so that "where would this go" and
// "where did this go" cannot disagree: a caller that only wanted the path still learns
// that the name was unusable, instead of being handed a plausible-looking string.
func TVStrmPath(dir, show, year string, season, episode int) (string, error) {
	showSafe := safeName(fmt.Sprintf("%s (%s)", show, year))
	// %02d on a negative or very large season still contains no separator and no dot —
	// it is only ever '-' and digits — so the season directory needs no sanitising of
	// its own. The HTTP edge rejects negatives before it gets here.
	seasonDir := fmt.Sprintf("Season %02d", season)
	episodeName := fmt.Sprintf("%s S%02dE%02d.strm", showSafe, season, episode)
	return containedPath(dir, showSafe, seasonDir, episodeName)
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

// ProviderWeb names identities that came from a pasted video page URL rather than from a
// metadata database. Its ids are opaque digests of the page (store.WebSourceID), and its
// playback path resolves the media URL fresh at play time instead of searching indexers.
const ProviderWeb = "web"

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
