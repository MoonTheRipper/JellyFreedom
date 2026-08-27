package websource_test

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"jellyfreedom/internal/netnsproxy"
	"jellyfreedom/internal/websource"
)

// TestLiveExtraction is the only test here that proves the whole chain: a real SOCKS
// proxy, the real yt-dlp binary, a real site, and this package's parsing of what came
// back. Everything else in this file's neighbour runs against a fake, which cannot
// notice that a site changed its page or that yt-dlp changed a field name.
//
// It is opt-in because it needs the network and it takes seconds, not milliseconds:
//
//	JF_WEBSOURCE_LIVE=/usr/local/bin/yt-dlp go test ./internal/websource/ -run Live -v
//
// Reach for it when a site that used to work stops working — it separates "yt-dlp is out
// of date" from "we broke the parsing", which is otherwise a slow thing to establish.
//
// The proxy here runs in the ORDINARY namespace, so this does not go through a VPN and
// is not a test of the tunnel. It is a test that the two halves speak to each other and
// that a real extractor accepts the arguments this package passes.
func TestLiveExtraction(t *testing.T) {
	bin := os.Getenv("JF_WEBSOURCE_LIVE")
	if bin == "" {
		t.Skip("set JF_WEBSOURCE_LIVE=/path/to/yt-dlp to run the live extraction test")
	}
	pageURL := os.Getenv("JF_WEBSOURCE_LIVE_URL")
	if pageURL == "" {
		// Public domain, hosted by a stable archive, and it has both a manifest and
		// progressive formats — so it exercises the selection rather than just the parse.
		pageURL = "https://archive.org/details/BigBuckBunny_124"
	}

	srv, err := netnsproxy.New([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, ln)

	c := websource.Client{
		Binary:   bin,
		ProxyURL: netnsproxy.Dialer{Addr: ln.Addr().String()}.ProxyURL(),
		Timeout:  120 * time.Second,
	}
	if v, err := c.Version(ctx); err != nil {
		t.Fatalf("yt-dlp --version: %v", err)
	} else {
		t.Logf("yt-dlp %s", v)
	}

	info, err := c.Inspect(ctx, pageURL)
	if err != nil {
		t.Fatalf("Inspect(%s): %v", pageURL, err)
	}
	t.Logf("title=%q uploader=%q duration=%ds height=%d ext=%s size=%d",
		info.Title, info.Uploader, info.Duration, info.Stream.Height, info.Stream.Ext, info.Stream.SizeBytes)

	if info.Title == "" {
		t.Error("no title")
	}
	if info.Duration <= 0 {
		t.Error("no duration — a video with no length cannot be seeked in Jellyfin")
	}
	if info.Stream.URL == "" {
		t.Fatal("no stream URL")
	}
	if len(info.Stream.Headers) == 0 {
		t.Error("no per-format headers — many CDNs 403 a request with no Referer")
	}
}
