package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jellyfreedom/internal/api"
	"jellyfreedom/internal/config"
	"jellyfreedom/internal/jellyfin"
	"jellyfreedom/internal/netnsproxy"
	"jellyfreedom/internal/store"
	"jellyfreedom/internal/websource"
)

// ── Harness ───────────────────────────────────────────────────────────────────

type webEnv struct {
	mux     *http.ServeMux
	db      *store.Store
	cfg     *config.Config
	player  *webPlayer
	movies  string
	session string // an admin session cookie value
}

// fakeExtractor writes a stand-in yt-dlp that prints fixed JSON. Everything about the
// feature except the extraction itself is worth testing deterministically, and the real
// binary needs a network, a live site and a video that still exists.
func fakeExtractor(t *testing.T, mediaURL string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "yt-dlp")
	body := `{"id":"vid1","title":"A Pasted Video","uploader":"someone","extractor_key":"Generic",
	  "duration":123.4,"thumbnail":"https://example.com/thumb.jpg",
	  "formats":[{"format_id":"1","url":"` + mediaURL + `","ext":"mp4","protocol":"https","height":720,
	    "http_headers":{"Referer":"https://example.com/","User-Agent":"probe-ua"},"cookies":"sess=xyz"}]}`
	script := "#!/bin/sh\ncase \"$1\" in --version) echo 9999.01.01; exit 0;; esac\ncat <<'J'\n" + body + "\nJ\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func newWebEnv(t *testing.T, mediaURL string) *webEnv {
	t.Helper()
	root := t.TempDir()
	movies := filepath.Join(root, "movies")
	if err := os.MkdirAll(movies, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := loadPlayKey(db); err != nil {
		t.Fatalf("loadPlayKey: %v", err)
	}
	api.SetStore(db)

	if err := db.CreateUser(&store.User{Username: "admin", PasswordHash: "x", IsAdmin: true}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	// Read the row back for its id: CreateUser does not populate the struct, so a session
	// built from the value passed in would point at user 0 and never authenticate.
	admin, err := db.GetUserByUsername("admin")
	if err != nil || admin == nil {
		t.Fatalf("read back the admin: %v", err)
	}
	const token = "test-session-token"
	if err := db.CreateSession(token, admin.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}

	cfg := &config.Config{
		Server:    config.ServerConfig{PublicURL: "http://host:1990"},
		Libraries: []config.Library{{Name: "Movies", Type: "movie", Path: movies, Default: true}},
		WebSources: config.WebSourcesConfig{Enabled: true, YTDLPPath: fakeExtractor(t, mediaURL),
			TempDir: filepath.Join(root, "tmp")},
	}
	p := newWebPlayer(db, cfg)
	// The player's own transport dials through the namespace proxy, which does not exist
	// in a test. Everything else about it — headers, ranges, the retry — is what is under
	// test, so only the transport is replaced.
	p.http = &http.Client{}

	mux := http.NewServeMux()
	registerWebSourceAPI(mux, p, db, cfg, jellyfin.New("", ""))
	return &webEnv{mux: mux, db: db, cfg: cfg, player: p, movies: movies, session: token}
}

func (e *webEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body == nil {
		rdr = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "jf_session", Value: e.session})
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// ── The stream proxy ──────────────────────────────────────────────────────────

// The headers yt-dlp reports for a format are not decoration: many CDNs 403 a request
// whose Referer does not match, so a proxy that drops them plays nothing at all. The
// client's Range must reach upstream, and the site's Set-Cookie must NOT reach the LAN.
func TestStreamProxyCarriesTheRightHeadersInBothDirections(t *testing.T) {
	var gotUA, gotReferer, gotCookie, gotRange string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotReferer = r.Header.Get("User-Agent"), r.Header.Get("Referer")
		gotCookie, gotRange = r.Header.Get("Cookie"), r.Header.Get("Range")
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 10-19/1000")
		w.Header().Set("Set-Cookie", "cdn_session=leaked; Path=/")
		// Real headers seen from an archive.org CDN in testing. Every one of them names
		// the site or describes its security posture, and none is any use to a player.
		w.Header().Set("Server", "nginx/1.31.3")
		w.Header().Set("Strict-Transport-Security", "max-age=15724800")
		w.Header().Set("Content-Security-Policy-Report-Only", "report-uri https://archive.org/services/csp-report")
		w.Header().Set("Last-Modified", "Wed, 03 Dec 2014 14:25:31 GMT")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("0123456789"))
	}))
	defer upstream.Close()

	e := newWebEnv(t, upstream.URL+"/media.mp4")
	ws := &store.WebSource{ID: "abc", PageURL: "https://example.com/v/1"}
	stream := websource.Stream{
		URL:     upstream.URL + "/media.mp4",
		Headers: map[string]string{"User-Agent": "probe-ua", "Referer": "https://example.com/", "Range": "bytes=0-1"},
		Cookie:  "sess=xyz",
	}

	req := httptest.NewRequest(http.MethodGet, "/play/p/web/abc", nil)
	req.Header.Set("Range", "bytes=10-19")
	rec := httptest.NewRecorder()
	e.player.streamWebSource(rec, req, ws, stream)

	if gotUA != "probe-ua" || gotReferer != "https://example.com/" {
		t.Errorf("upstream saw UA=%q Referer=%q — a CDN would 403 this", gotUA, gotReferer)
	}
	if gotCookie != "sess=xyz" {
		t.Errorf("upstream saw Cookie=%q", gotCookie)
	}
	// The CLIENT's range, not the extractor's: a Range copied out of the format metadata
	// would pin every request to the same byte window.
	if gotRange != "bytes=10-19" {
		t.Errorf("upstream saw Range=%q, want the client's bytes=10-19", gotRange)
	}

	if rec.Code != http.StatusPartialContent {
		t.Errorf("status = %d, want 206 — Jellyfin needs the partial-content status to seek", rec.Code)
	}
	if rec.Header().Get("Content-Range") != "bytes 10-19/1000" {
		t.Errorf("Content-Range was not forwarded: %q", rec.Header().Get("Content-Range"))
	}
	if rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Errorf("Accept-Ranges = %q — without it Jellyfin will not offer seeking", rec.Header().Get("Accept-Ranges"))
	}
	// The client must learn nothing about where the video came from — that is the point
	// of proxying rather than redirecting, and a header is as much of a giveaway as an
	// IP connection would be.
	for _, leak := range []string{"Set-Cookie", "Server", "Strict-Transport-Security",
		"Content-Security-Policy-Report-Only"} {
		if got := rec.Header().Get(leak); got != "" {
			t.Errorf("%s reached the LAN client: %q", leak, got)
		}
	}
	// …while the validators a player uses to revalidate a cached range do come through.
	if rec.Header().Get("Last-Modified") == "" {
		t.Error("Last-Modified was dropped")
	}
	if rec.Body.String() != "0123456789" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// A signed CDN URL can expire inside the cache window. One re-resolve turns that from a
// failed playback into a hiccup — and it is what lets resolvedTTL be long enough to make
// seeking cheap.
func TestExpiredMediaURLIsReResolvedOnce(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if strings.Contains(r.URL.Path, "/stale") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte("fresh bytes"))
	}))
	defer upstream.Close()

	e := newWebEnv(t, upstream.URL+"/fresh.mp4") // what a re-extraction will return
	page := "https://example.com/v/1"
	ws := &store.WebSource{ID: store.WebSourceID(page), PageURL: page}
	if err := e.db.UpsertWebSource(ws); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/play/p/web/"+ws.ID, nil)
	rec := httptest.NewRecorder()
	e.player.streamWebSource(rec, req, ws, websource.Stream{URL: upstream.URL + "/stale.mp4"})

	if rec.Code != http.StatusOK || rec.Body.String() != "fresh bytes" {
		t.Fatalf("status=%d body=%q — the expired URL was not re-resolved", rec.Code, rec.Body.String())
	}
	if hits != 2 {
		t.Errorf("upstream hits = %d, want exactly 2 (the stale one, then the fresh one)", hits)
	}
}

// A .strm outliving its source is normal: the entry was deleted and Jellyfin has not
// rescanned. It must be a 404 so the player stops retrying, not a 500.
func TestPlayingADeletedSourceIs404(t *testing.T) {
	e := newWebEnv(t, "https://example.com/media.mp4")
	rec := httptest.NewRecorder()
	e.player.play(rec, httptest.NewRequest(http.MethodGet, "/play/p/web/gone", nil), "gone")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestPlayIsRefusedWhenTheFeatureIsOff(t *testing.T) {
	e := newWebEnv(t, "https://example.com/media.mp4")
	e.cfg.WebSources.Enabled = false
	rec := httptest.NewRecorder()
	e.player.play(rec, httptest.NewRequest(http.MethodGet, "/play/p/web/x", nil), "x")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// The single most important property of the whole feature: nothing is fetched outside
// the tunnel. With no proxy address the extractor must refuse rather than go direct.
func TestNoProxyMeansNoExtractionRatherThanADirectOne(t *testing.T) {
	cfg := &config.Config{
		WebSources: config.WebSourcesConfig{Enabled: true, YTDLPPath: fakeExtractor(t, "https://cdn/x.mp4"),
			ProxyAddr: "10.42.0.2:1080", TempDir: t.TempDir()},
	}
	p := newWebPlayer(nil, cfg)
	if err := p.enabled(); err != nil {
		t.Fatalf("a configured player should be enabled: %v", err)
	}
	if !strings.HasPrefix(p.client.ProxyURL, "socks5h://") {
		t.Errorf("proxy URL = %q — socks5h is what sends the DNS lookup through the tunnel too",
			p.client.ProxyURL)
	}

	cfg.WebSources.Enabled = false
	off := newWebPlayer(nil, cfg)
	if off.client.ProxyURL != "" || off.dialer.Addr != "" {
		t.Errorf("a disabled player still carries a proxy: %q", off.client.ProxyURL)
	}
	if _, err := off.dialer.DialContext(t.Context(), "tcp", "example.com:443"); err != netnsproxy.ErrNoProxy {
		t.Errorf("a disabled player dialled anyway: %v", err)
	}
}

// ── The dashboard API ─────────────────────────────────────────────────────────

func TestAddWritesAStrmWithAnIdentityAndNotTheMediaURL(t *testing.T) {
	e := newWebEnv(t, "https://cdn.example.com/signed/media.mp4?exp=1234&sig=abcd")
	rec := e.do(t, http.MethodPost, "/api/websources", map[string]string{"url": "https://example.com/watch/1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}

	var found string
	filepath.Walk(e.movies, func(path string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(path, ".strm") {
			found = path
		}
		return nil
	})
	if found == "" {
		t.Fatal("no .strm was written")
	}
	b, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(b)

	// The whole resolve-at-play contract, asserted on the bytes that actually reach disk.
	if strings.Contains(contents, "cdn.example.com") || strings.Contains(contents, "sig=") {
		t.Fatalf("the signed media URL was frozen into the .strm — it will 403 within hours:\n%s", contents)
	}
	id := store.WebSourceID("https://example.com/watch/1")
	if !strings.Contains(contents, "/play/p/web/movie/"+id) {
		t.Fatalf(".strm does not carry the web identity:\n%s", contents)
	}
	if !strings.Contains(contents, "?t=") {
		t.Fatalf(".strm carries no capability token — it would 403 at playback:\n%s", contents)
	}

	// And the library row plus the source row that back it.
	item, err := e.db.GetByProviderIdentity(store.Identity{Provider: "web", ProviderID: id, MediaType: "movie"})
	if err != nil || item == nil {
		t.Fatalf("no library row: %v", err)
	}
	if item.Title != "A Pasted Video" || item.Status != "ready" {
		t.Errorf("row = %+v", item)
	}
	ws, err := e.db.GetWebSource(id)
	if err != nil || ws == nil {
		t.Fatalf("no web source row: %v", err)
	}
	if ws.PageURL != "https://example.com/watch/1" || ws.AddedBy != "admin" || ws.Duration != 123 {
		t.Errorf("web source = %+v", ws)
	}
}

// Pasting the same link twice must update one entry, not produce a twin that Jellyfin
// shows as a duplicate.
func TestAddingTheSameLinkTwiceIsOneEntry(t *testing.T) {
	e := newWebEnv(t, "https://cdn/x.mp4")
	for _, u := range []string{"https://example.com/watch/1", "https://EXAMPLE.com/watch/1/"} {
		if rec := e.do(t, http.MethodPost, "/api/websources", map[string]string{"url": u}); rec.Code != http.StatusOK {
			t.Fatalf("add %s: %d %s", u, rec.Code, rec.Body.String())
		}
	}
	list, err := e.db.ListWebSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("%d entries, want 1 — the same page was added twice", len(list))
	}
	var strms int
	filepath.Walk(e.movies, func(path string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(path, ".strm") {
			strms++
		}
		return nil
	})
	if strms != 1 {
		t.Errorf("%d .strm files, want 1", strms)
	}
}

func TestPreviewDoesNotAddAnythingAndLeaksNoMediaURL(t *testing.T) {
	e := newWebEnv(t, "https://cdn.example.com/signed/media.mp4?sig=abcd")
	rec := e.do(t, http.MethodPost, "/api/websources/preview", map[string]string{"url": "https://example.com/watch/1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "cdn.example.com") || strings.Contains(rec.Body.String(), "sig=") {
		t.Fatalf("the preview handed the browser a direct media URL:\n%s", rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["title"] != "A Pasted Video" || out["already_added"] != false {
		t.Errorf("preview = %v", out)
	}
	if list, _ := e.db.ListWebSources(); len(list) != 0 {
		t.Errorf("preview added %d entries — it must only look", len(list))
	}
}

func TestDeleteRemovesTheEntryTheRowAndTheFile(t *testing.T) {
	e := newWebEnv(t, "https://cdn/x.mp4")
	if rec := e.do(t, http.MethodPost, "/api/websources", map[string]string{"url": "https://example.com/watch/1"}); rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	id := store.WebSourceID("https://example.com/watch/1")

	rec := e.do(t, http.MethodDelete, "/api/websources/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if ws, _ := e.db.GetWebSource(id); ws != nil {
		t.Error("the source row survived")
	}
	if items, _ := e.db.ItemsByProviderID("web", id); len(items) != 0 {
		t.Error("the library row survived — Jellyfin would show an entry that plays nothing")
	}
	var strms int
	filepath.Walk(e.movies, func(path string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(path, ".strm") {
			strms++
		}
		return nil
	})
	if strms != 0 {
		t.Errorf("%d .strm files survived the delete", strms)
	}

	// Idempotent: a second delete is a 200 with removed:0, not a 404.
	if rec := e.do(t, http.MethodDelete, "/api/websources/"+id, nil); rec.Code != http.StatusOK {
		t.Errorf("second delete: %d", rec.Code)
	}
}

func TestTheAPIRefusesRubbishURLs(t *testing.T) {
	e := newWebEnv(t, "https://cdn/x.mp4")
	for _, bad := range []string{"", "ytsearch:cats", "file:///etc/passwd", "/etc/passwd", "https://127.0.0.1/v"} {
		rec := e.do(t, http.MethodPost, "/api/websources", map[string]string{"url": bad})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("add %q: status %d, want 400", bad, rec.Code)
		}
	}
}

// Adding a link writes a file into a Jellyfin media root and runs an extractor against
// an arbitrary URL. That is an operator action.
func TestTheAPIIsAdminOnly(t *testing.T) {
	e := newWebEnv(t, "https://cdn/x.mp4")
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/websources"},
		{http.MethodGet, "/api/websources/status"},
		{http.MethodPost, "/api/websources"},
		{http.MethodPost, "/api/websources/preview"},
		{http.MethodDelete, "/api/websources/abc"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte(`{}`)))
		rec := httptest.NewRecorder()
		e.mux.ServeHTTP(rec, req) // no session cookie
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session: status %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestStatusExplainsItselfWhenUnavailable(t *testing.T) {
	e := newWebEnv(t, "https://cdn/x.mp4")
	rec := e.do(t, http.MethodGet, "/api/websources/status", nil)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["enabled"] != true || out["ytdlp_version"] != "9999.01.01" {
		t.Fatalf("status = %v", out)
	}

	e.cfg.WebSources.Enabled = false
	rec = e.do(t, http.MethodGet, "/api/websources/status", nil)
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["enabled"] != false || out["reason"] == "" {
		t.Fatalf("a disabled feature must say why: %v", out)
	}
}

func TestPlayFailureStatusSeparatesPermanentFromTransient(t *testing.T) {
	if got := playFailureStatus(websource.ErrNoProgressiveFormat); got != http.StatusNotImplemented {
		t.Errorf("adaptive-only = %d, want 501", got)
	}
	if got := playFailureStatus(websource.ErrExtractionFailed); got != http.StatusBadGateway {
		t.Errorf("extraction failure = %d, want 502 — the site may just be down", got)
	}
	if got := playFailureStatus(websource.ErrNotInstalled); got != http.StatusServiceUnavailable {
		t.Errorf("missing yt-dlp = %d, want 503", got)
	}
}
