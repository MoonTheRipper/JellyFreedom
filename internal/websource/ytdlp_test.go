package websource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeYTDLP writes an executable that prints the given stdout and exits with the given
// code, and records its own argv. Testing against a fake rather than the real thing is
// what makes the format-selection and error-mapping logic testable at all: the real
// yt-dlp needs a network, a site that is up, and a video that still exists.
func fakeYTDLP(t *testing.T, stdout, stderr string, exitCode int) (bin, argvFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake extractor is a shell script")
	}
	dir := t.TempDir()
	bin = filepath.Join(dir, "yt-dlp")
	argvFile = filepath.Join(dir, "argv")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argvFile + "\n" +
		"cat <<'YTEOF'\n" + stdout + "\nYTEOF\n" +
		"cat >&2 <<'YTERR'\n" + stderr + "\nYTERR\n" +
		"exit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake yt-dlp: %v", err)
	}
	return bin, argvFile
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	return string(rune('0' + i))
}

func newClient(bin string) Client {
	return Client{Binary: bin, ProxyURL: "socks5h://10.42.0.2:1080", Timeout: 10 * time.Second}
}

// The official yt-dlp binary is a self-extracting PyInstaller bundle that unpacks ~76MB
// into TMPDIR on EVERY run. With an empty environment and no TMPDIR it falls back to
// /tmp, which on a stock Ubuntu is a RAM-backed tmpfs — so this is the difference
// between spending disk and spending memory per extraction, and between a clear failure
// and an unreadable "decompression resulted in return code -1" when that tmpfs fills up.
func TestExtractorGetsADiskBackedTempDir(t *testing.T) {
	c := Client{Binary: "yt-dlp", ProxyURL: "socks5h://x:1", TempDir: "/var/lib/jellyfreedom/tmp"}
	env := c.env()
	for _, want := range []string{"TMPDIR=/var/lib/jellyfreedom/tmp", "TMP=/var/lib/jellyfreedom/tmp", "TEMP=/var/lib/jellyfreedom/tmp"} {
		if !slices.Contains(env, want) {
			t.Errorf("env is missing %q: %v", want, env)
		}
	}
	// And the environment is built from nothing, so a variable added to the service unit
	// later cannot redirect the extractor away from the tunnel.
	for _, v := range env {
		name, _, _ := strings.Cut(v, "=")
		switch name {
		case "PATH", "HOME", "TMPDIR", "TMP", "TEMP":
		default:
			t.Errorf("unexpected variable %q in the extractor environment", name)
		}
	}
	if slices.ContainsFunc(Client{Binary: "yt-dlp"}.env(), func(v string) bool {
		return strings.HasPrefix(v, "TMPDIR=")
	}) {
		t.Error("an unset TempDir should leave TMPDIR alone rather than inventing one")
	}
}

// The shape here is copied from a real archive.org extraction: a float duration, null
// codecs, and a null age_limit. Every one of those decoded into a plain int or string
// would fail the whole extraction on a video that is perfectly fine.
const realWorldJSON = `{
  "id": "BigBuckBunny_124",
  "title": "Big Buck Bunny",
  "uploader": "jake@archive.org",
  "duration": 596.46,
  "age_limit": null,
  "is_live": null,
  "extractor_key": "ArchiveOrg",
  "webpage_url": "https://archive.org/details/BigBuckBunny_124",
  "thumbnail": "https://archive.org/thumb.jpg",
  "formats": [
    {"format_id":"hls","url":"https://cdn/master.m3u8","ext":"mp4","protocol":"m3u8_native","height":1080},
    {"format_id":"audio","url":"https://cdn/a.m4a","ext":"m4a","protocol":"https","vcodec":"none","acodec":"mp4a","height":0},
    {"format_id":"2","url":"https://cdn/bunny.mp4","ext":"mp4","protocol":"https","vcodec":null,"acodec":null,"height":720,"filesize":332243668,
     "http_headers":{"User-Agent":"Mozilla/5.0","Referer":"https://archive.org/"},"cookies":"sess=abc"}
  ]
}`

func TestInspectParsesRealWorldShape(t *testing.T) {
	bin, argvFile := fakeYTDLP(t, realWorldJSON, "", 0)
	info, err := newClient(bin).Inspect(context.Background(), "https://archive.org/details/BigBuckBunny_124")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.Title != "Big Buck Bunny" || info.Uploader != "jake@archive.org" {
		t.Errorf("metadata = %q by %q", info.Title, info.Uploader)
	}
	if info.Duration != 596 {
		t.Errorf("duration = %d, want 596 (a float in the JSON)", info.Duration)
	}
	// The progressive 720p mp4, not the 1080p HLS manifest and not the audio-only track.
	if info.Stream.URL != "https://cdn/bunny.mp4" {
		t.Errorf("chose %q, want the progressive mp4", info.Stream.URL)
	}
	if info.Stream.SizeBytes != 332243668 || info.Stream.Height != 720 {
		t.Errorf("size/height = %d/%d", info.Stream.SizeBytes, info.Stream.Height)
	}
	// Headers and cookies are what make the CDN fetch succeed; dropping them is a 403.
	if info.Stream.Headers["Referer"] != "https://archive.org/" {
		t.Errorf("headers not carried through: %v", info.Stream.Headers)
	}
	if info.Stream.Cookie != "sess=abc" {
		t.Errorf("cookie not carried through: %q", info.Stream.Cookie)
	}

	// The argv is a security surface of its own.
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	for _, want := range []string{"--ignore-config", "--no-playlist", "--proxy", "socks5h://10.42.0.2:1080", "--"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("argv is missing %q:\n%s", want, argv)
		}
	}
}

func TestManifestOnlyVideoIsItsOwnError(t *testing.T) {
	const hlsOnly = `{"id":"x","title":"Adaptive only","formats":[
	  {"format_id":"hls","url":"https://cdn/master.m3u8","ext":"mp4","protocol":"m3u8_native","height":1080},
	  {"format_id":"dash","url":"https://cdn/manifest.mpd","ext":"mp4","protocol":"http_dash_segments","height":2160}]}`
	bin, _ := fakeYTDLP(t, hlsOnly, "", 0)
	_, err := newClient(bin).Inspect(context.Background(), "https://example.com/v/1")
	if !errors.Is(err, ErrNoProgressiveFormat) {
		t.Fatalf("err = %v, want ErrNoProgressiveFormat", err)
	}
}

// A format with no formats array but a top-level url is a real extractor shape.
func TestSingleTopLevelURLIsAFormat(t *testing.T) {
	const single = `{"id":"x","title":"Direct","url":"https://cdn/v.mp4","ext":"mp4","protocol":"https",
	  "http_headers":{"Referer":"https://example.com/"}}`
	bin, _ := fakeYTDLP(t, single, "", 0)
	info, err := newClient(bin).Inspect(context.Background(), "https://example.com/v/1")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.Stream.URL != "https://cdn/v.mp4" || info.Stream.Headers["Referer"] == "" {
		t.Fatalf("stream = %+v", info.Stream)
	}
}

func TestLiveStreamsAreRefused(t *testing.T) {
	const live = `{"id":"x","title":"Live now","is_live":true,"formats":[
	  {"format_id":"1","url":"https://cdn/live.mp4","protocol":"https"}]}`
	bin, _ := fakeYTDLP(t, live, "", 0)
	_, err := newClient(bin).Inspect(context.Background(), "https://example.com/live")
	if !errors.Is(err, ErrUnsupportedURL) {
		t.Fatalf("err = %v, want ErrUnsupportedURL — a live stream has no length to seek", err)
	}
}

func TestExtractionFailurePassesThroughOneUsefulLine(t *testing.T) {
	bin, _ := fakeYTDLP(t, "",
		"ERROR: [generic] None: Unable to download webpage: HTTP Error 404: Not Found; please report this issue on https://github.com/yt-dlp/yt-dlp/issues", 1)
	_, err := newClient(bin).Inspect(context.Background(), "https://example.com/gone")
	if !errors.Is(err, ErrExtractionFailed) {
		t.Fatalf("err = %v, want ErrExtractionFailed", err)
	}
	if strings.Contains(err.Error(), "please report this issue") {
		t.Errorf("the GitHub banner reached the user-facing message: %v", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("the useful part was dropped: %v", err)
	}
}

// Refusing to run without a proxy is the single most important behaviour in this
// package: going direct would put the user's home address on a request to the site.
func TestClientWithoutProxyRefusesToRun(t *testing.T) {
	bin, _ := fakeYTDLP(t, realWorldJSON, "", 0)
	c := Client{Binary: bin, Timeout: time.Second}
	if err := c.Available(); !errors.Is(err, ErrNoProxy) {
		t.Fatalf("Available() = %v, want ErrNoProxy", err)
	}
	if _, err := c.Inspect(context.Background(), "https://example.com/v/1"); !errors.Is(err, ErrNoProxy) {
		t.Fatalf("Inspect ran anyway: %v", err)
	}
}

func TestMissingBinaryIsItsOwnError(t *testing.T) {
	c := Client{Binary: "/nonexistent/yt-dlp", ProxyURL: "socks5h://10.42.0.2:1080"}
	if err := c.Available(); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Available() = %v, want ErrNotInstalled", err)
	}
}

func TestValidatePageURL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"https://example.com/watch?v=1", true},
		{"http://example.com/v/1", true},
		{"https://example.com/path with space", true}, // parses; the site can 404 it
		{"", false},
		{"ytsearch:cats", false}, // yt-dlp would run a SEARCH
		{"/etc/passwd", false},   // yt-dlp would read a local file
		{"file:///etc/passwd", false},
		{"ftp://example.com/v.mp4", false},
		{"https://user:pw@example.com/v", false}, // a credential in a stored, logged value
		{"https://127.0.0.1/v", false},
		{"https://10.42.0.1:1990/", false},
		{"https://[::1]/v", false},
		{"https://169.254.169.254/latest/meta-data/", false},
		{"https://example.com/\nHost: evil", false}, // control characters
	} {
		_, err := ValidatePageURL(tc.in)
		if (err == nil) != tc.want {
			t.Errorf("ValidatePageURL(%q): err = %v, want ok = %v", tc.in, err, tc.want)
		}
	}
}

func TestValidatePageURLRejectsOverlongInput(t *testing.T) {
	if _, err := ValidatePageURL("https://example.com/" + strings.Repeat("a", maxURLLen)); err == nil {
		t.Fatal("an overlong URL was accepted")
	}
}

func TestPickFormatPrefersBothTracksOverResolution(t *testing.T) {
	s := func(v string) *string { return &v }
	got, ok := pickFormat([]rawFormat{
		{FormatID: "video-only-4k", URL: "https://a", Protocol: "https", Height: 2160, VCodec: s("h264"), ACodec: s("none")},
		{FormatID: "both-480", URL: "https://b", Protocol: "https", Height: 480, VCodec: s("h264"), ACodec: s("aac")},
	})
	if !ok || got.FormatID != "both-480" {
		t.Fatalf("picked %q — a silent 4K stream is worse than a 480p one with sound", got.FormatID)
	}
}
