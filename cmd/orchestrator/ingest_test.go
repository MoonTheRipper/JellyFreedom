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

	"jellyfreedom/internal/config"
	"jellyfreedom/internal/jellyfin"
	"jellyfreedom/internal/store"
)

// ── Harness ───────────────────────────────────────────────────────────────────

type ingestEnv struct {
	mux    *http.ServeMux
	db     *store.Store
	cfg    *config.Config
	secret string
	root   string // the parent of both library dirs — nothing may ever appear here
	movies string
	shows  string
}

func newIngestEnv(t *testing.T) *ingestEnv {
	t.Helper()
	root := t.TempDir()
	movies := filepath.Join(root, "movies")
	shows := filepath.Join(root, "shows")
	for _, d := range []string{movies, shows} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// The ingest handler mints a capability token for every .strm it writes, so the play
	// key has to exist or every write would fail with "could not build a play URL".
	if err := loadPlayKey(db); err != nil {
		t.Fatalf("loadPlayKey: %v", err)
	}
	secret, err := ensureIngestSecret(db)
	if err != nil {
		t.Fatalf("ensureIngestSecret: %v", err)
	}
	if len(secret) < 32 {
		t.Fatalf("generated secret is only %d chars — too short to resist guessing", len(secret))
	}

	cfg := &config.Config{
		Server: config.ServerConfig{PublicURL: "http://host:1990"},
		Libraries: []config.Library{
			{Name: "Movies", Type: "movie", Path: movies, Default: true},
			{Name: "Shows", Type: "tv", Path: shows, Default: true},
		},
	}
	mux := http.NewServeMux()
	// An unconfigured Jellyfin client: TriggerLibraryScan returns ErrNotConfigured
	// without dialling anything, so these tests make no network calls.
	registerProviderIngest(mux, db, cfg, jellyfin.New("", ""))

	return &ingestEnv{mux: mux, db: db, cfg: cfg, secret: secret, root: root, movies: movies, shows: shows}
}

// do issues a request carrying the correct secret, unless an option overrides it.
func (e *ingestEnv) do(t *testing.T, method, path string, body any, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	switch v := body.(type) {
	case nil:
		rdr = bytes.NewReader(nil)
	case string:
		rdr = bytes.NewReader([]byte(v))
	default:
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rdr)
	r.Header.Set(IngestHeader, e.secret)
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	return w
}

// strmFiles lists every .strm anywhere under the temp root, so a test can assert both
// where a file DID land and that nothing landed anywhere else.
func (e *ingestEnv) strmFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(e.root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && strings.HasSuffix(p, ".strm") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

const testHash = "0123456789abcdef0123456789abcdef01234567"

func movieBody(title string) map[string]any {
	return map[string]any{
		"type": "movie", "title": title, "year": "2019",
		"magnet": "magnet:?xt=urn:btih:" + testHash + "&dn=x",
	}
}

// ── Happy path ────────────────────────────────────────────────────────────────

// TestIngestMovieRoundTrip: register, find the row by the identity /play uses, read the
// .strm, list it, delete it. The identity assertion is the load-bearing one — a row
// stored under an identity playback does not spell is a row that never plays.
func TestIngestMovieRoundTrip(t *testing.T) {
	e := newIngestEnv(t)

	if w := e.do(t, "PUT", "/api/provider/anidb/items/uuid-1", movieBody("A Film")); w.Code != 200 {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body)
	}

	it, err := e.db.GetByProviderIdentity(store.Identity{
		Provider: "anidb", ProviderID: "uuid-1", MediaType: "movie",
	})
	if err != nil || it == nil {
		t.Fatalf("no row under the identity /play resolves: %v, %v", it, err)
	}
	if it.TMDBID != 0 {
		t.Fatalf("tmdb_id = %d, want 0", it.TMDBID)
	}
	if it.InfoHash != testHash {
		t.Fatalf("info_hash = %q", it.InfoHash)
	}
	if it.LibraryName != "Movies" || it.Status != "ready" {
		t.Fatalf("library=%q status=%q", it.LibraryName, it.Status)
	}

	want := filepath.Join(e.movies, "A Film (2019)", "A Film (2019).strm")
	if it.StrmPath != want {
		t.Fatalf("strm_path = %q, want %q", it.StrmPath, want)
	}
	b, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("reading the .strm: %v", err)
	}
	// The URL must be the provider-namespaced shape AND must carry a capability token,
	// or /play returns 403 the moment enforcement is on.
	got := string(b)
	if !strings.HasPrefix(got, "http://host:1990/play/p/anidb/movie/uuid-1?t=") {
		t.Fatalf(".strm contents = %q", got)
	}
	ref := playRef{provider: "anidb", mediaType: "movie", providerID: "uuid-1"}
	if !ref.validToken(strings.SplitN(got, "?t=", 2)[1]) {
		t.Fatal("the token in the .strm does not authorise the identity in its own path")
	}

	// GET lists it, and does NOT disclose the server path or the magnet.
	w := e.do(t, "GET", "/api/provider/anidb/items", nil)
	if w.Code != 200 {
		t.Fatalf("GET = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), e.movies) || strings.Contains(w.Body.String(), "magnet:") {
		t.Fatalf("the listing leaked a server path or a magnet: %s", w.Body)
	}
	var listed []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0]["id"] != "uuid-1" {
		t.Fatalf("listing = %v", listed)
	}

	// DELETE removes BOTH the row and the file.
	if w := e.do(t, "DELETE", "/api/provider/anidb/items/uuid-1", nil); w.Code != 200 {
		t.Fatalf("DELETE = %d: %s", w.Code, w.Body)
	}
	if gone, _ := e.db.GetByProviderIdentity(store.Identity{
		Provider: "anidb", ProviderID: "uuid-1", MediaType: "movie",
	}); gone != nil {
		t.Fatal("the library row survived the delete")
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatal("the .strm survived the delete")
	}
	// Idempotent: deleting again is a 200 with removed:0, not an error. A daemon
	// reconciling its own state re-issues deletes freely.
	if w := e.do(t, "DELETE", "/api/provider/anidb/items/uuid-1", nil); w.Code != 200 {
		t.Fatalf("second DELETE = %d: %s", w.Code, w.Body)
	}
}

// TestIngestTVRoundTrip covers the episode layout and the narrowed delete.
func TestIngestTVRoundTrip(t *testing.T) {
	e := newIngestEnv(t)
	upper := strings.ToUpper(testHash)
	ep := func(season, episode int) map[string]any {
		return map[string]any{
			"type": "tv", "title": "The Show", "year": "2020",
			"season": season, "episode": episode, "library": "Shows",
			"info_hash": upper,
		}
	}
	for _, se := range [][2]int{{1, 1}, {1, 2}} {
		if w := e.do(t, "PUT", "/api/provider/anidb/items/show-1", ep(se[0], se[1])); w.Code != 200 {
			t.Fatalf("PUT S%dE%d = %d: %s", se[0], se[1], w.Code, w.Body)
		}
	}
	want := filepath.Join(e.shows, "The Show (2020)", "Season 01", "The Show (2020) S01E02.strm")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected episode file missing: %v", err)
	}
	// An uppercase info_hash is canonicalised to lower case. /play and CountByHash both
	// match on the stored string, so two spellings would be two torrents.
	it, _ := e.db.GetByProviderIdentity(store.Identity{
		Provider: "anidb", ProviderID: "show-1", MediaType: "tv", Season: 1, Episode: 2,
	})
	if it == nil || it.InfoHash != testHash {
		t.Fatalf("info_hash not canonicalised: %+v", it)
	}

	// A narrowed delete takes exactly one episode.
	if w := e.do(t, "DELETE", "/api/provider/anidb/items/show-1?season=1&episode=2", nil); w.Code != 200 {
		t.Fatalf("narrowed DELETE = %d: %s", w.Code, w.Body)
	}
	if files := e.strmFiles(t); len(files) != 1 {
		t.Fatalf("after a narrowed delete there are %d .strm files, want 1: %v", len(files), files)
	}
	// One of season/episode alone is refused rather than silently defaulting the other
	// to 0 and deleting something the caller did not name.
	if w := e.do(t, "DELETE", "/api/provider/anidb/items/show-1?season=1", nil); w.Code != 400 {
		t.Fatalf("half-narrowed DELETE = %d, want 400", w.Code)
	}
	// The unnarrowed delete takes the rest of the title.
	if w := e.do(t, "DELETE", "/api/provider/anidb/items/show-1", nil); w.Code != 200 {
		t.Fatalf("whole-title DELETE = %d: %s", w.Code, w.Body)
	}
	if files := e.strmFiles(t); len(files) != 0 {
		t.Fatalf("files survived the whole-title delete: %v", files)
	}
}

// TestIngestMovieForcesZeroSeasonEpisode: a movie route has no season or episode to send,
// so it always looks up (0,0). A row stored under anything else would never be found.
func TestIngestMovieForcesZeroSeasonEpisode(t *testing.T) {
	e := newIngestEnv(t)
	body := movieBody("A Film")
	body["season"], body["episode"] = 5, 9
	if w := e.do(t, "PUT", "/api/provider/anidb/items/m1", body); w.Code != 200 {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body)
	}
	if it, _ := e.db.GetByProviderIdentity(store.Identity{
		Provider: "anidb", ProviderID: "m1", MediaType: "movie",
	}); it == nil {
		t.Fatal("a movie registered with a non-zero season is unreachable from /play")
	}
}

// TestIngestRenameLeavesNoOrphan: the items table conflicts on strm_path, so a
// re-registration under a new title would otherwise insert a SECOND row and strand the
// first file in the library forever.
func TestIngestRenameLeavesNoOrphan(t *testing.T) {
	e := newIngestEnv(t)
	if w := e.do(t, "PUT", "/api/provider/anidb/items/m1", movieBody("Old Title")); w.Code != 200 {
		t.Fatalf("first PUT = %d: %s", w.Code, w.Body)
	}
	if w := e.do(t, "PUT", "/api/provider/anidb/items/m1", movieBody("New Title")); w.Code != 200 {
		t.Fatalf("second PUT = %d: %s", w.Code, w.Body)
	}
	files := e.strmFiles(t)
	if len(files) != 1 || !strings.Contains(files[0], "New Title") {
		t.Fatalf("after a rename the library holds %v, want only the new title", files)
	}
	all, _ := e.db.ItemsByProviderID("anidb", "m1")
	if len(all) != 1 {
		t.Fatalf("rows = %d, want 1 — a rename must replace, not duplicate", len(all))
	}
}

// ── Authentication ────────────────────────────────────────────────────────────

// TestIngestRequiresSecret is the fail-closed test. Every shape of a missing or wrong
// secret must be a 401, and — the part that matters — must leave the library untouched.
func TestIngestRequiresSecret(t *testing.T) {
	e := newIngestEnv(t)
	cases := []struct {
		name string
		set  func(*http.Request)
	}{
		{"absent", func(r *http.Request) { r.Header.Del(IngestHeader) }},
		{"empty", func(r *http.Request) { r.Header.Set(IngestHeader, "") }},
		{"wrong", func(r *http.Request) { r.Header.Set(IngestHeader, "deadbeef") }},
		{"prefix", func(r *http.Request) { r.Header.Set(IngestHeader, e.secret[:len(e.secret)-1]) }},
		{"suffixed", func(r *http.Request) { r.Header.Set(IngestHeader, e.secret+"x") }},
		{"padded", func(r *http.Request) { r.Header.Set(IngestHeader, " "+e.secret) }},
	}
	for _, c := range cases {
		for _, req := range [][2]string{
			{"PUT", "/api/provider/anidb/items/m1"},
			{"DELETE", "/api/provider/anidb/items/m1"},
			{"GET", "/api/provider/anidb/items"},
		} {
			w := e.do(t, req[0], req[1], movieBody("A Film"), c.set)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s with a %s secret = %d, want 401", req[0], req[1], c.name, w.Code)
			}
			if strings.Contains(w.Body.String(), e.secret) {
				t.Fatal("the refusal echoed the secret back")
			}
		}
	}
	if files := e.strmFiles(t); len(files) != 0 {
		t.Fatalf("an unauthenticated call wrote %v", files)
	}
}

// TestIngestFailsClosedWithNoStoredSecret: an endpoint whose secret was never generated
// is CLOSED, not open. The opposite reading turns a failed first-run initialisation into
// an unauthenticated, file-writing endpoint.
func TestIngestFailsClosedWithNoStoredSecret(t *testing.T) {
	e := newIngestEnv(t)
	if err := e.db.SetSetting(ingestSecretSetting, ""); err != nil {
		t.Fatal(err)
	}
	for _, got := range []string{"", "anything", e.secret} {
		if sharedSecretMatch(e.db, ingestSecretSetting, got) {
			t.Fatalf("sharedSecretMatch accepted %q with no secret stored", got)
		}
	}
	w := e.do(t, "PUT", "/api/provider/anidb/items/m1", movieBody("A Film"),
		func(r *http.Request) { r.Header.Set(IngestHeader, "") })
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("= %d, want 401", w.Code)
	}
}

// ── Hostile input ─────────────────────────────────────────────────────────────

// TestIngestTitleTraversal: a title reaches the filesystem. A traversal attempt is
// ACCEPTED (it is only a name) but must be sanitised into one component inside the
// library — asserted by a canary and by a whole-tree walk, not by inspecting the name.
func TestIngestTitleTraversal(t *testing.T) {
	e := newIngestEnv(t)
	canary := filepath.Join(e.root, "canary.txt")
	if err := os.WriteFile(canary, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}

	titles := []string{
		"../canary",
		"../../../../etc/passwd",
		"..",
		"....//....//canary.txt",
		"/etc/shadow",
		`..\..\windows`,
		"~/.ssh/authorized_keys",
		"nul",
		// Long-but-legal: right at the handler's title cap, so it reaches
		// internal/library and exercises the truncation rather than the 400.
		strings.Repeat("A", maxIngestTitleLen),
	}
	for i, title := range titles {
		id := "m" + string(rune('a'+i))
		w := e.do(t, "PUT", "/api/provider/anidb/items/"+id, movieBody(title))
		if w.Code != 200 {
			t.Fatalf("PUT with title %q = %d: %s", title, w.Code, w.Body)
		}
	}
	files := e.strmFiles(t)
	if len(files) == 0 {
		t.Fatal("no files were written at all — the test is not exercising the write path")
	}
	for _, f := range files {
		if !strings.HasPrefix(f, e.movies+string(filepath.Separator)) {
			t.Fatalf("a .strm landed outside the movie library: %q", f)
		}
	}
	if b, err := os.ReadFile(canary); err != nil || string(b) != "untouched" {
		t.Fatalf("the canary outside the libraries was overwritten: %q, %v", b, err)
	}
}

// TestIngestRejectsBadIdentity: provider and id are validated with the SAME functions
// /play uses, so anything registerable is by construction routable and signable.
func TestIngestRejectsBadIdentity(t *testing.T) {
	e := newIngestEnv(t)
	// ".." and "." never reach the handler at all: net/http's ServeMux cleans the path
	// and answers with a redirect to the cleaned form, which matches no route. Asserted
	// as "not accepted" rather than "400" so the test documents the real behaviour
	// instead of pinning a status code that a router change could legitimately move.
	for _, p := range []string{"/api/provider/anidb/items/..", "/api/provider/anidb/items/."} {
		if w := e.do(t, "PUT", p, movieBody("A Film")); w.Code == 200 {
			t.Fatalf("PUT %s was accepted", p)
		}
	}
	bad := []string{
		"/api/provider/anidb/items/a:b",
		"/api/provider/anidb/items/a.b",
		"/api/provider/anidb/items/" + strings.Repeat("x", 65),
		"/api/provider/ANIDB/items/m1",
		"/api/provider/ani_db/items/m1",
		"/api/provider/ani.db/items/m1",
		"/api/provider/" + strings.Repeat("a", 17) + "/items/m1",
		// The TMDB namespace belongs to the built-in resolve pipeline, whose ~1,000 live
		// .strm tokens are HMACs over that identity space.
		"/api/provider/tmdb/items/550",
	}
	for _, p := range bad {
		if w := e.do(t, "PUT", p, movieBody("A Film")); w.Code != 400 {
			t.Fatalf("PUT %s = %d, want 400: %s", p, w.Code, w.Body)
		}
	}
	// A percent-encoded traversal in the id must not be decoded into one either: the
	// router decodes before PathValue, and ValidProviderID sees the decoded bytes.
	if w := e.do(t, "PUT", "/api/provider/anidb/items/%2e%2e%2f%2e%2e", movieBody("A Film")); w.Code == 200 {
		t.Fatalf("percent-encoded traversal in the id = %d, want a refusal", w.Code)
	}
	if files := e.strmFiles(t); len(files) != 0 {
		t.Fatalf("a refused identity still wrote %v", files)
	}
}

// TestIngestRejectsOversizedBody: the body is bounded well below the app-wide 1 MiB,
// because nothing legitimate here is even close to it.
func TestIngestRejectsOversizedBody(t *testing.T) {
	e := newIngestEnv(t)
	// An OTHERWISE VALID body, padded past the cap with a field the decoder ignores.
	// That is what makes this a test of the byte bound and nothing else: a body made
	// oversized by a giant title would be refused by the title bound too, so it would
	// still pass with MaxBytesReader deleted.
	body := movieBody("A Film")
	body["note"] = strings.Repeat("A", maxIngestBody)
	if w := e.do(t, "PUT", "/api/provider/anidb/items/m1", body); w.Code != 400 {
		t.Fatalf("oversized body = %d, want 400", w.Code)
	}
	// The same body under the cap is accepted, so the refusal above is about the SIZE
	// and not about the unknown field.
	small := movieBody("A Film")
	small["note"] = "short"
	if w := e.do(t, "PUT", "/api/provider/anidb/items/m2", small); w.Code != 200 {
		t.Fatalf("an under-cap body with an unknown field = %d, want 200: %s", w.Code, w.Body)
	}
	if err := e.db.DeleteItem(filepath.Join(e.movies, "A Film (2019)", "A Film (2019).strm")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(e.movies, "A Film (2019)")); err != nil {
		t.Fatal(err)
	}
	if w := e.do(t, "PUT", "/api/provider/anidb/items/m1", "{not json"); w.Code != 400 {
		t.Fatalf("malformed body = %d, want 400", w.Code)
	}
	if files := e.strmFiles(t); len(files) != 0 {
		t.Fatalf("a refused body still wrote %v", files)
	}
}

// TestIngestLibraryAuthorisation: an unconfigured name and a name of the wrong type give
// the IDENTICAL refusal, so the endpoint cannot be walked to enumerate library names.
func TestIngestLibraryAuthorisation(t *testing.T) {
	e := newIngestEnv(t)
	var messages []string
	// "Adults" does not exist; "Shows" exists but is a tv library, which is wrong for a
	// movie; "movies" is the right library with the wrong case. All three must look the same.
	for _, name := range []string{"Adults", "Shows", "movies"} {
		body := movieBody("A Film")
		body["library"] = name
		w := e.do(t, "PUT", "/api/provider/anidb/items/m1", body)
		if w.Code != 400 {
			t.Fatalf("library %q = %d, want 400: %s", name, w.Code, w.Body)
		}
		messages = append(messages, w.Body.String())
	}
	for _, m := range messages[1:] {
		if m != messages[0] {
			t.Fatalf("refusals differ, which enumerates library names:\n%s\n%s", messages[0], m)
		}
	}
	if !strings.Contains(messages[0], "unknown library") {
		t.Fatalf("refusal message = %s, want the shared 'unknown library' shape", messages[0])
	}
	if files := e.strmFiles(t); len(files) != 0 {
		t.Fatalf("a refused library still wrote %v", files)
	}
}

// TestIngestRejectsBadSource covers every way the playable source can be wrong. All of
// them share ONE message, so the endpoint is not an oracle about which check failed.
func TestIngestRejectsBadSource(t *testing.T) {
	e := newIngestEnv(t)
	cases := []map[string]any{
		{}, // neither a magnet nor a hash
		{"magnet": "not a magnet"},
		{"magnet": "http://example.com/x"},    // wrong scheme
		{"magnet": "magnet:?xt=urn:btih:xyz"}, // a btih that is not 40 hex
		{"magnet": "magnet:?dn=name"},         // no xt at all
		{"info_hash": "nothex"},
		{"info_hash": strings.Repeat("a", 39)},
		{"info_hash": strings.Repeat("a", 41)},
		// A magnet and a hash that name DIFFERENT torrents: JellyFreedom would add one
		// and then stream, drop and account for the other, permanently and silently.
		{"magnet": "magnet:?xt=urn:btih:" + testHash, "info_hash": strings.Repeat("b", 40)},
		// Over the magnet length cap (4560 bytes), but comfortably under the BODY cap —
		// encoding/json escapes each '&' to \u0026, so the repeat count is kept low
		// enough that this case exercises the magnet bound and not the body bound.
		{"magnet": "magnet:?xt=urn:btih:" + testHash + strings.Repeat("&tr=x", 900)},
	}
	var msgs []string
	for i, extra := range cases {
		body := map[string]any{"type": "movie", "title": "A Film", "year": "2019"}
		for k, v := range extra {
			body[k] = v
		}
		w := e.do(t, "PUT", "/api/provider/anidb/items/m1", body)
		if w.Code != 400 {
			t.Fatalf("case %d %v = %d, want 400: %s", i, extra, w.Code, w.Body)
		}
		msgs = append(msgs, w.Body.String())
	}
	for i, m := range msgs[1:] {
		if m != msgs[0] {
			t.Fatalf("source refusal for case %d differs: %q vs %q", i+1, msgs[0], m)
		}
	}
	if files := e.strmFiles(t); len(files) != 0 {
		t.Fatalf("a refused source still wrote %v", files)
	}
	// A bare hash IS accepted, and yields a working magnet: magnet:?xt=urn:btih:<hash>
	// finds peers through the DHT and the configured retrackers on its own.
	if w := e.do(t, "PUT", "/api/provider/anidb/items/m1",
		map[string]any{"type": "movie", "title": "A Film", "year": "2019", "info_hash": testHash}); w.Code != 200 {
		t.Fatalf("a bare info_hash = %d, want 200: %s", w.Code, w.Body)
	}
	it, _ := e.db.GetByProviderIdentity(store.Identity{Provider: "anidb", ProviderID: "m1", MediaType: "movie"})
	if it == nil || it.Magnet != "magnet:?xt=urn:btih:"+testHash {
		t.Fatalf("synthesised magnet = %+v", it)
	}
}

// TestIngestRejectsBadFields covers the remaining body validation, field by field. The
// poster URL cases are the sharpest: that string becomes an <img src> in the media UI,
// where a javascript: or data:text/html value is script execution in a viewer's session.
func TestIngestRejectsBadFields(t *testing.T) {
	e := newIngestEnv(t)
	overrides := []map[string]any{
		{"type": ""},
		{"type": "tvshow"},
		{"type": "MOVIE"},
		{"title": ""},
		{"title": "   "},
		{"title": strings.Repeat("A", maxIngestTitleLen+1)},
		{"title": "Evil\nINFO forged log line"},
		{"title": "Evil\x00"},
		{"year": "20x9"},
		{"year": "12345"},
		{"year": "'; DROP TABLE items;--"},
		{"poster_url": "javascript:alert(1)"},
		{"poster_url": "data:text/html;base64,PHNjcmlwdD4="},
		{"poster_url": "file:///etc/passwd"},
		{"poster_url": "/relative/path.jpg"},
		{"poster_url": "http://example.com/" + strings.Repeat("a", maxIngestPosterLen)},
		{"release_title": strings.Repeat("R", maxIngestReleaseLen+1)},
		{"file_index": -1},
		{"file_index": maxIngestFileIndex + 1},
	}
	for _, o := range overrides {
		body := movieBody("A Film")
		for k, v := range o {
			body[k] = v
		}
		if w := e.do(t, "PUT", "/api/provider/anidb/items/m1", body); w.Code != 400 {
			t.Fatalf("override %v = %d, want 400: %s", o, w.Code, w.Body)
		}
	}
	// TV season/episode bounds. Zero is legal (specials); negative is not routable,
	// because the /play route parses these back out of a path and rejects a negative.
	for _, o := range []map[string]any{
		{"season": -1, "episode": 1},
		{"season": 1, "episode": -1},
		{"season": maxIngestSeasonOrEp + 1, "episode": 1},
		{"season": 1, "episode": maxIngestSeasonOrEp + 1},
	} {
		body := map[string]any{"type": "tv", "title": "S", "year": "2020", "library": "Shows",
			"info_hash": testHash}
		for k, v := range o {
			body[k] = v
		}
		if w := e.do(t, "PUT", "/api/provider/anidb/items/s1", body); w.Code != 400 {
			t.Fatalf("tv override %v = %d, want 400: %s", o, w.Code, w.Body)
		}
	}
	if files := e.strmFiles(t); len(files) != 0 {
		t.Fatalf("a refused body still wrote %v", files)
	}
}

// TestIngestErrorsDoNotEchoInput: an error message that repeats the caller's string is a
// reflection primitive for whatever renders it, and one that repeats a server path tells
// an attacker the layout of the box.
func TestIngestErrorsDoNotEchoInput(t *testing.T) {
	e := newIngestEnv(t)
	const marker = "MARKER-9c1f"
	bodies := []map[string]any{
		{"type": marker, "title": "A Film"},
		{"type": "movie", "title": "A Film", "year": marker},
		{"type": "movie", "title": "A Film", "poster_url": "javascript:" + marker},
		{"type": "movie", "title": "A Film", "library": marker},
		{"type": "movie", "title": "A Film", "magnet": "magnet:?xt=urn:btih:" + marker},
	}
	for _, body := range bodies {
		w := e.do(t, "PUT", "/api/provider/anidb/items/m1", body)
		if w.Code != 400 {
			t.Fatalf("body %v = %d, want 400", body, w.Code)
		}
		if strings.Contains(w.Body.String(), marker) {
			t.Fatalf("the refusal echoed caller input back: %s", w.Body)
		}
		if strings.Contains(w.Body.String(), e.root) {
			t.Fatalf("the refusal leaked a server path: %s", w.Body)
		}
	}
	// And a bad path identity, likewise.
	if w := e.do(t, "PUT", "/api/provider/"+marker+"/items/m1", movieBody("A Film")); strings.Contains(w.Body.String(), marker) {
		t.Fatalf("the provider refusal echoed the provider name: %s", w.Body)
	}
}

// ── Unit-level checks on the validators ───────────────────────────────────────

func TestMagnetInfoHash(t *testing.T) {
	ok := []struct{ in, want string }{
		{"magnet:?xt=urn:btih:" + testHash, testHash},
		{"magnet:?xt=urn:btih:" + strings.ToUpper(testHash), testHash},
		{"magnet:?dn=Name&xt=urn:btih:" + testHash + "&tr=udp://x", testHash}, // xt not first
		{"MAGNET:?xt=URN:BTIH:" + testHash, testHash},                         // scheme/urn are case-insensitive
	}
	for _, c := range ok {
		got, valid := magnetInfoHash(c.in)
		if !valid || got != c.want {
			t.Errorf("magnetInfoHash(%q) = %q, %v; want %q, true", c.in, got, valid, c.want)
		}
	}
	bad := []string{
		"",
		"magnet:",
		"magnet:?xt=urn:btih:",
		"magnet:?xt=urn:btmh:1220ab", // a v2 multihash is not a v1 info hash
		"magnet:?xt=urn:btih:zzzz",
		"http://example.com/?xt=urn:btih:" + testHash,
		"magnet:?xt=urn:btih:" + testHash + "\n", // a control character must not survive
	}
	for _, in := range bad {
		if got, valid := magnetInfoHash(in); valid {
			t.Errorf("magnetInfoHash(%q) = %q, true; want false", in, got)
		}
	}
}

func TestHasControlChars(t *testing.T) {
	for _, s := range []string{"a\nb", "a\rb", "a\x00b", "a\x1bb", "\x7f", "ab"} {
		if !hasControlChars(s) {
			t.Errorf("hasControlChars(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"Amélie", "千と千尋", "Fast & Furious 6", "[REC]", "", "a b"} {
		if hasControlChars(s) {
			t.Errorf("hasControlChars(%q) = true, want false", s)
		}
	}
}

func TestValidPosterURL(t *testing.T) {
	for _, s := range []string{
		"http://image.tmdb.org/t/p/w500/x.jpg",
		"https://example.com/poster.png?v=2",
	} {
		if !validPosterURL(s) {
			t.Errorf("validPosterURL(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"javascript:alert(1)",
		"JavaScript:alert(1)",
		"data:image/png;base64,AAA",
		"file:///etc/passwd",
		"//example.com/x.jpg", // scheme-relative: no scheme to allow-list
		"http:///nohost.jpg",
		"relative.jpg",
		"http://example.com/\nX",
		"https://example.com/" + strings.Repeat("a", maxIngestPosterLen),
	} {
		if validPosterURL(s) {
			t.Errorf("validPosterURL(%q) = true, want false", s)
		}
	}
}
