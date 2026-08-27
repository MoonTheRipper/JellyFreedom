// Package websource turns a video page URL into something playable.
//
// It is a thin, deliberately dumb wrapper around yt-dlp. yt-dlp does the part that
// cannot be done well any other way — knowing how a thousand different sites hide their
// media URLs, and keeping up as they change — and this package does the parts that are
// this project's problem: validating what is handed to it, choosing a format that can
// actually be streamed as one HTTP resource, and making sure every packet involved goes
// through the VPN.
//
// # THE RESOLVE-AT-PLAY CONTRACT
//
// Nothing here caches a media URL to disk, and the caller must not either. Tube sites
// sign their CDN links with a short expiry — minutes to hours — so a media URL frozen
// into a .strm file is a library entry that plays today and 403s on Friday. That is the
// same failure the torrent path already learned (a frozen info hash rots when its
// seeders leave), and it has the same fix: the .strm holds an identity, and the media
// URL is resolved fresh each time somebody presses play. Inspect is what "fresh" means.
//
// # EVERYTHING GOES THROUGH THE TUNNEL
//
// Extraction is not a lesser kind of traffic than playback. Fetching the page, running
// the site's JavaScript-shaped API calls and downloading the thumbnail all identify the
// requester to the site just as thoroughly as fetching the video does. So the proxy is
// not optional decoration: ProxyURL is passed to yt-dlp as --proxy with a socks5h scheme
// so even the DNS lookup happens inside the namespace, and a Client with no proxy
// refuses to run rather than quietly going direct. See ErrNoProxy.
package websource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// Errors callers branch on. Each corresponds to a different thing for a user to do, so
// they are distinct values rather than one wrapped string.
var (
	// ErrNotInstalled means yt-dlp is missing. The feature is unavailable; nothing the
	// user pastes will help.
	ErrNotInstalled = errors.New("yt-dlp is not installed")
	// ErrNoProxy means the client has no VPN proxy configured. Refusing is the whole
	// point: going direct would put the user's home address on a request to the site.
	ErrNoProxy = errors.New("web sources require the VPN proxy, which is not configured")
	// ErrUnsupportedURL means the string is not something this will hand to an extractor.
	ErrUnsupportedURL = errors.New("that is not an http(s) video page URL")
	// ErrNoProgressiveFormat means the site only offers the video as a manifest (HLS or
	// DASH), which cannot be proxied as a single byte range. Distinct because it is a
	// property of the site, not a mistake by the user or a transient failure.
	ErrNoProgressiveFormat = errors.New("that video is only offered as an adaptive stream, which cannot be proxied yet")
	// ErrExtractionFailed covers everything yt-dlp itself refused: a dead link, a private
	// video, a site whose extractor has broken.
	ErrExtractionFailed = errors.New("the video could not be extracted")
)

// maxOutputBytes caps what is read from yt-dlp. A full -J dump for a single video is
// tens of kilobytes; a few megabytes is already pathological, and the alternative to a
// cap is letting a hostile or broken extractor decide this process's memory use.
const maxOutputBytes = 8 << 20

// maxURLLen bounds the page URL. It goes into an argv, a log line and a database column.
const maxURLLen = 2048

// Client runs yt-dlp. The zero value is not usable; every field matters.
type Client struct {
	// Binary is the path to yt-dlp, or a bare name to be found on PATH.
	Binary string
	// ProxyURL is the socks5h:// URL of the in-namespace proxy. Required.
	ProxyURL string
	// Timeout bounds one extraction. Extraction is a handful of HTTP requests plus some
	// site-specific work, and it is running over a VPN, so this is generous rather than
	// tight — but it is never absent, because a hung extractor would otherwise hold a
	// playback request open forever.
	Timeout time.Duration
	// TempDir is where yt-dlp may write scratch files. Empty means the system default.
	//
	// This is not a detail. The official yt-dlp binary is a self-extracting PyInstaller
	// bundle: EVERY invocation unpacks ~76MB of interpreter and libraries into TMPDIR
	// before it does anything. On a box whose /tmp is a RAM-backed tmpfs — which is the
	// Ubuntu default on this project's own server — that is 76MB of RAM per extraction,
	// and when the tmpfs is full the failure is a PyInstaller decompression error that
	// says nothing about disk space. Pointing this at a disk-backed directory is what
	// keeps extraction working on a box under memory pressure.
	TempDir string
}

// Info is everything one extraction yielded: what the video is, and how to fetch it now.
type Info struct {
	// Identity and description, used when the item is added to the library.
	ID           string `json:"id"`
	Title        string `json:"title"`
	Uploader     string `json:"uploader"`
	Extractor    string `json:"extractor"`
	WebpageURL   string `json:"webpage_url"`
	ThumbnailURL string `json:"thumbnail_url"`
	Duration     int    `json:"duration_seconds"`
	AgeLimit     int    `json:"age_limit"`
	IsLive       bool   `json:"is_live"`

	// Stream is the format chosen for playback. Valid only for a few minutes — see the
	// resolve-at-play contract in the package comment.
	Stream Stream `json:"-"`
}

// Stream is one fetchable media URL and everything needed to fetch it.
type Stream struct {
	URL      string
	Ext      string
	Protocol string
	// Headers are the per-format headers yt-dlp says the site requires — typically a
	// Referer and a User-Agent. They are NOT cosmetic: many sites 403 a CDN request whose
	// Referer does not match, so a proxy that drops these plays nothing.
	Headers map[string]string
	// Cookie is the Cookie header value, when the extractor produced one.
	Cookie string
	// SizeBytes is the format's size when the site declares it, 0 when it does not.
	SizeBytes int64
	Height    int
}

// Available reports whether this client can run at all, without doing an extraction. It
// is what `doctor` and the dashboard use to say "web sources are not set up" instead of
// failing on the first paste.
func (c Client) Available() error {
	if c.ProxyURL == "" {
		return ErrNoProxy
	}
	bin := c.Binary
	if bin == "" {
		bin = "yt-dlp"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("%w: %v", ErrNotInstalled, err)
	}
	return nil
}

// Version returns yt-dlp's version string. Worth surfacing: an extractor failure on a
// site that used to work is nearly always a yt-dlp that needs updating, and the version
// is the first thing anyone will ask for.
func (c Client) Version(ctx context.Context) (string, error) {
	if err := c.Available(); err != nil {
		return "", err
	}
	bin := c.Binary
	if bin == "" {
		bin = "yt-dlp"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	// The same environment as an extraction: --version still unpacks the whole bundle,
	// so it fails in exactly the same way when TMPDIR has no room.
	cmd.Env = c.env()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("yt-dlp --version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Inspect extracts one video page. It is used both when an item is added — where the
// metadata is what matters, and a successful extraction is the proof that the link is
// worth adding at all — and at play time, where the freshly signed Stream URL is.
func (c Client) Inspect(ctx context.Context, pageURL string) (*Info, error) {
	if err := c.Available(); err != nil {
		return nil, err
	}
	clean, err := ValidatePageURL(pageURL)
	if err != nil {
		return nil, err
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin := c.Binary
	if bin == "" {
		bin = "yt-dlp"
	}
	// Every flag here is load-bearing:
	//
	//   --ignore-config   a yt-dlp config file anywhere on the box could otherwise change
	//                     what this does — including where it writes and what it executes.
	//                     This process's behaviour must come from this argv alone.
	//   --no-playlist     a URL that is also a playlist must yield one video, not a
	//                     hundred-megabyte dump of every video on a channel.
	//   -J                metadata as JSON on stdout; downloads nothing.
	//   --no-progress     nothing to render, and it keeps stderr to actual diagnostics.
	//   --proxy           the reason this package exists. socks5h so the DNS lookup goes
	//                     through the tunnel too.
	//   --socket-timeout  a stalled socket must fail, not hold the extraction open until
	//                     the context deadline.
	//   --                the page URL can never be read as a flag, whatever it contains.
	args := []string{
		"--ignore-config", "--no-playlist", "--no-progress", "--no-warnings",
		"-J", "--proxy", c.ProxyURL, "--socket-timeout", "20", "--retries", "2",
		"--", clean,
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// An empty environment, not the orchestrator's. yt-dlp reads HTTP_PROXY, ALL_PROXY,
	// XDG_CONFIG_HOME and more from the environment, and any of them could redirect this
	// away from the tunnel. PATH is kept only so the binary can locate its own helpers,
	// and TMPDIR because the self-extracting binary cannot start without somewhere to
	// unpack itself — see the TempDir field.
	cmd.Env = c.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, n: maxOutputBytes}
	cmd.Stderr = &limitedWriter{w: &stderr, n: 64 << 10}

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%w: it took longer than %s", ErrExtractionFailed, timeout)
		}
		// yt-dlp's own message is the useful one ("Video unavailable", "Private video",
		// "Unsupported URL"), so it is passed through — trimmed, one line, and with the
		// URL stripped out of it, since this string reaches a browser.
		return nil, fmt.Errorf("%w: %s", ErrExtractionFailed, firstUsefulLine(stderr.String()))
	}

	var raw rawInfo
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("%w: yt-dlp returned something that is not JSON", ErrExtractionFailed)
	}
	return raw.toInfo()
}

// env is the environment every yt-dlp invocation runs with. It is built from nothing
// rather than filtered from the parent, so a variable added to the service unit later
// cannot silently change what the extractor does.
func (c Client) env() []string {
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/nonexistent"}
	if c.TempDir != "" {
		// TMP and TEMP as well as TMPDIR: Python's tempfile consults all three, and
		// setting only one leaves the others to fall back to /tmp.
		env = append(env, "TMPDIR="+c.TempDir, "TMP="+c.TempDir, "TEMP="+c.TempDir)
	}
	return env
}

// ValidatePageURL checks a pasted string and returns the URL to actually use.
//
// It is strict because the value is handed to a program that is very good at fetching
// things. yt-dlp accepts far more than URLs — "ytsearch:cats" runs a search, a bare path
// reads a local file — so anything that is not unambiguously an absolute http(s) URL is
// refused here rather than discovered later.
func ValidatePageURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLLen {
		return "", fmt.Errorf("%w: empty or longer than %d characters", ErrUnsupportedURL, maxURLLen)
	}
	// Control characters would land in an argv, a log line and a JSON response.
	if strings.ContainsFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("%w: it contains control characters", ErrUnsupportedURL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnsupportedURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w: the scheme must be http or https", ErrUnsupportedURL)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: it has no host", ErrUnsupportedURL)
	}
	if u.User != nil {
		// user:password@host in a URL that gets logged and stored is a credential leak,
		// and no video page needs one.
		return "", fmt.Errorf("%w: it must not contain credentials", ErrUnsupportedURL)
	}
	// A literal private address is refused here as well as by the proxy. The proxy is the
	// real enforcement — it checks the RESOLVED address, which a name cannot dodge — but
	// catching the literal case here turns a confusing proxy-level failure into a clear
	// message, and it costs one parse.
	if host := u.Hostname(); host != "" {
		if ip, err := netip.ParseAddr(host); err == nil {
			if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
				ip.IsLinkLocalUnicast() || ip.IsMulticast() {
				return "", fmt.Errorf("%w: that address is not on the public internet", ErrUnsupportedURL)
			}
		}
	}
	return u.String(), nil
}

// rawInfo is the subset of yt-dlp's -J output this needs. Everything is typed to what
// yt-dlp ACTUALLY emits rather than what its documentation implies: duration is a float
// (596.46 seconds is a real value), and age_limit, vcodec and acodec are frequently null
// rather than absent or "none". Decoding those into plain ints and strings would fail
// the whole extraction on a perfectly good video.
type rawInfo struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Uploader    string            `json:"uploader"`
	Channel     string            `json:"channel"`
	Extractor   string            `json:"extractor_key"`
	WebpageURL  string            `json:"webpage_url"`
	Thumbnail   string            `json:"thumbnail"`
	Duration    *float64          `json:"duration"`
	AgeLimit    *int              `json:"age_limit"`
	IsLive      *bool             `json:"is_live"`
	LiveStatus  string            `json:"live_status"`
	URL         string            `json:"url"`
	Ext         string            `json:"ext"`
	Protocol    string            `json:"protocol"`
	Formats     []rawFormat       `json:"formats"`
	HTTPHeaders map[string]string `json:"http_headers"`
}

type rawFormat struct {
	FormatID    string            `json:"format_id"`
	URL         string            `json:"url"`
	Ext         string            `json:"ext"`
	Protocol    string            `json:"protocol"`
	VCodec      *string           `json:"vcodec"`
	ACodec      *string           `json:"acodec"`
	Height      int               `json:"height"`
	TBR         *float64          `json:"tbr"`
	Filesize    *int64            `json:"filesize"`
	FilesizeApx *int64            `json:"filesize_approx"`
	HTTPHeaders map[string]string `json:"http_headers"`
	Cookies     string            `json:"cookies"`
}

func (r rawInfo) toInfo() (*Info, error) {
	info := &Info{
		ID:           r.ID,
		Title:        strings.TrimSpace(r.Title),
		Uploader:     strings.TrimSpace(firstNonEmpty(r.Uploader, r.Channel)),
		Extractor:    r.Extractor,
		WebpageURL:   r.WebpageURL,
		ThumbnailURL: r.Thumbnail,
	}
	if r.Duration != nil && *r.Duration > 0 {
		info.Duration = int(*r.Duration)
	}
	if r.AgeLimit != nil {
		info.AgeLimit = *r.AgeLimit
	}
	info.IsLive = (r.IsLive != nil && *r.IsLive) || r.LiveStatus == "is_live"
	if info.IsLive {
		// A live stream has no end and no length. Jellyfin would show it as a file, and
		// seeking it means nothing. Refusing is honest; pretending is a broken entry.
		return nil, fmt.Errorf("%w: it is a live stream", ErrUnsupportedURL)
	}

	formats := r.Formats
	if len(formats) == 0 && r.URL != "" {
		// Some extractors return a single media URL at the top level with no formats
		// array at all. That is one format; treat it as one.
		formats = []rawFormat{{
			FormatID: "0", URL: r.URL, Ext: r.Ext, Protocol: r.Protocol,
			HTTPHeaders: r.HTTPHeaders,
		}}
	}
	best, ok := pickFormat(formats)
	if !ok {
		return nil, ErrNoProgressiveFormat
	}
	info.Stream = Stream{
		URL:       best.URL,
		Ext:       best.Ext,
		Protocol:  best.Protocol,
		Headers:   best.HTTPHeaders,
		Cookie:    best.Cookies,
		Height:    best.Height,
		SizeBytes: derefOr(best.Filesize, derefOr(best.FilesizeApx, 0)),
	}
	if info.Title == "" {
		info.Title = "Untitled"
	}
	return info, nil
}

// pickFormat chooses the format to stream.
//
// The hard requirement is a PROGRESSIVE format: one URL that answers Range requests with
// bytes of a playable file. Everything the proxy does downstream — forwarding a Range
// header, reporting a Content-Length, letting a player seek — assumes exactly that. An
// HLS or DASH manifest is a playlist of thousands of segment URLs, each separately
// signed; serving one to Jellyfin as if it were a video file produces a library entry
// that fails at play. So a manifest-only video is refused with its own error, and the
// user is told the site is the reason.
//
// Among progressive formats the preference is: has both tracks, then bigger picture,
// then higher bitrate, then mp4 as a tiebreak because it is what every Apple TV plays
// without transcoding.
//
// "Has both tracks" has to cope with vcodec/acodec being null. A null is UNKNOWN, not
// absent — archive.org returns null for a file that plainly has both — so only the
// explicit "none" counts against a format.
func pickFormat(formats []rawFormat) (rawFormat, bool) {
	var best rawFormat
	var bestScore int64 = -1
	for _, f := range formats {
		if f.URL == "" || !isProgressive(f.Protocol) {
			continue
		}
		var score int64
		if codecPresent(f.VCodec) && codecPresent(f.ACodec) {
			// Dominates everything else: a video-only format plays silently, which is
			// worse than a lower resolution in every case.
			score += 1 << 40
		}
		score += int64(f.Height) << 20
		if f.TBR != nil {
			score += int64(*f.TBR)
		}
		if f.Ext == "mp4" || f.Ext == "m4v" {
			score++
		}
		if score > bestScore {
			best, bestScore = f, score
		}
	}
	return best, bestScore >= 0
}

// isProgressive reports whether a yt-dlp protocol string names a plain HTTP resource.
// Everything else — m3u8, m3u8_native, dash, http_dash_segments, ism, rtmp — is a
// manifest or a non-HTTP transport.
func isProgressive(p string) bool {
	switch p {
	case "", "http", "https", "http_range":
		// Empty means the extractor did not say; those have always been plain HTTP in
		// practice, and the fetch will fail loudly if not.
		return true
	default:
		return false
	}
}

// codecPresent reports whether a codec field says a track EXISTS. A missing or null
// field means the extractor did not say, which is not the same as saying "none".
func codecPresent(c *string) bool { return c == nil || (*c != "none" && *c != "") }

func derefOr(p *int64, alt int64) int64 {
	if p != nil {
		return *p
	}
	return alt
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// firstUsefulLine reduces yt-dlp's stderr to the one line worth showing a user. Its
// errors are prefixed "ERROR: " and often followed by a stack trace or a "please report
// this" banner, none of which belongs in a dashboard toast.
func firstUsefulLine(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "ERROR: ")
		if i := strings.Index(line, ";"); i > 20 {
			line = line[:i] // drop "; please report this issue on ..."
		}
		if len(line) > 300 {
			line = line[:300]
		}
		return line
	}
	return "yt-dlp gave no reason"
}

// limitedWriter is io.Writer with a ceiling. Writes past the ceiling are discarded
// rather than erroring, because killing yt-dlp mid-write would lose the diagnostic in
// its stderr; the JSON decode fails afterwards with a clear message instead.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	if len(p) > l.n {
		l.w.Write(p[:l.n])
		l.n = 0
		return len(p), nil
	}
	l.n -= len(p)
	return l.w.Write(p)
}
