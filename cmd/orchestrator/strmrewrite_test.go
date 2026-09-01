package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jellyfreedom/internal/config"
	"jellyfreedom/internal/library"
	"jellyfreedom/internal/store"
)

// migrateStrmTokens rewrites every library .strm at startup. It used to build the URL from
// the row's TMDBID alone, which is 0 for anything that did not come from TMDB — so every
// web source in the library collapsed onto the single URL /play/movie/0, carrying one
// shared token, and /play answered "bad tmdb id".
//
// It landed on a restart, days after the entries were added and playing, which is the part
// worth guarding: nothing about adding a link exercises this path.
func TestMigrateStrmTokensKeepsProviderIdentity(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := loadPlayKey(db); err != nil {
		t.Fatalf("loadPlayKey: %v", err)
	}

	write := func(name string) string {
		dir := filepath.Join(root, "movies", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name+".strm")
		if err := os.WriteFile(p, []byte("http://old.example/stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	webStrm := write("A Web Source")
	web := &store.Item{
		MediaType: "movie", Title: "A Web Source", StrmPath: webStrm,
		LibraryName: "Movies", Status: "ready", Updated: time.Now(),
	}
	web.SetProviderIdentity(library.ProviderWeb, "abc123def456")
	if err := db.Upsert(web); err != nil {
		t.Fatalf("upsert web item: %v", err)
	}

	tmdbStrm := write("A TMDB Movie")
	tmdb := &store.Item{
		TMDBID: 550, MediaType: "movie", Title: "A TMDB Movie", StrmPath: tmdbStrm,
		LibraryName: "Movies", Status: "ready", Updated: time.Now(),
	}
	tmdb.SetProviderIdentity(library.ProviderTMDB, "550")
	if err := db.Upsert(tmdb); err != nil {
		t.Fatalf("upsert tmdb item: %v", err)
	}

	cfg := &config.Config{}
	cfg.Server.PublicURL = "http://192.168.0.2:1990"
	migrateStrmTokens(db, cfg)

	got := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	web1 := got(webStrm)
	if strings.Contains(web1, "/play/movie/0") {
		t.Errorf("the web source was rewritten to the TMDB route with id 0:\n  %s", web1)
	}
	if !strings.Contains(web1, "/play/p/web/movie/abc123def456?t=") {
		t.Errorf("web source .strm lost its provider identity:\n  %s", web1)
	}

	// The TMDB spelling is frozen — .strm files already in the wild carry it.
	tmdb1 := got(tmdbStrm)
	if !strings.Contains(tmdb1, "/play/movie/550?t=") {
		t.Errorf("TMDB .strm changed shape:\n  %s", tmdb1)
	}

	// Two rows of different identity must not share a token. They did: both hashed
	// "movie:0", so one stolen URL played everything.
	tokenOf := func(s string) string {
		if i := strings.Index(s, "?t="); i >= 0 {
			return s[i+3:]
		}
		return ""
	}
	if tokenOf(web1) == tokenOf(tmdb1) {
		t.Errorf("two different items share one capability token: %s", tokenOf(web1))
	}
}

// Enforcement is a RATCHET. It used to be re-derived on every boot from the outcome of the
// rewrite sweep, so an install that enforced correctly yesterday served /play
// unauthenticated today the moment one .strm could not be written — a library mount not up
// yet, a permission change. One log line, and the whole capability system off.
func TestEnforcementSurvivesAFailedRewrite(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "ratchet.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := loadPlayKey(db); err != nil {
		t.Fatalf("loadPlayKey: %v", err)
	}

	// This install has enforced before.
	if err := db.SetSetting(playTokenRequiredSetting, "true"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	playTokensEnforced.Store(false)

	// An item whose .strm cannot be written: the path is a directory.
	bad := filepath.Join(root, "movies", "Undeletable")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	it := &store.Item{
		TMDBID: 99, MediaType: "movie", Title: "Undeletable", StrmPath: bad,
		LibraryName: "Movies", Status: "ready", Updated: time.Now(),
	}
	it.SetProviderIdentity(library.ProviderTMDB, "99")
	if err := db.Upsert(it); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	cfg := &config.Config{}
	cfg.Server.PublicURL = "http://192.168.0.2:1990"
	migrateStrmTokens(db, cfg)

	if !playTokenEnforced() {
		t.Error("a failed rewrite turned enforcement OFF on an install that had already " +
			"enforced — /play would serve unauthenticated after an ordinary restart")
	}
}

// A token minted for the hash-pinned /proxy/stream route must not validate an
// identity-based /play request, or the reverse. The two key spaces are separated by field 0
// ("hash" versus a media type or the "p" namespace tag) and nothing else.
func TestStreamTokensAndPlayTokensDoNotCross(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "cross.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := loadPlayKey(db); err != nil {
		t.Fatalf("loadPlayKey: %v", err)
	}

	const hash = "0123456789abcdef0123456789abcdef01234567"
	st := streamToken(hash, 0)
	if st == "" {
		t.Fatal("streamToken returned empty")
	}
	if !validStreamToken(st, hash, 0) {
		t.Error("a freshly minted stream token did not validate")
	}
	if validStreamToken(st, hash, 1) {
		t.Error("a stream token validated for a different file index")
	}
	if validPlayToken(st, "movie", 0, 0, 0) {
		t.Error("a stream token validated an identity-based /play request")
	}
	if validStreamToken(playToken("movie", 550, 0, 0), hash, 0) {
		t.Error("a play token validated a hash-pinned /proxy/stream request")
	}
	if validStreamToken("", hash, 0) {
		t.Error("an empty token was accepted")
	}
}

// Rotating the play key is the ONLY revocation a capability URL has: a token never expires,
// is bound to no user, and survives deleting the item. So the rotation has to invalidate old
// URLs AND leave the library playable, or nobody will ever run it.
func TestRotatingThePlayKeyResignsTheLibrary(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "rotate.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := loadPlayKey(db); err != nil {
		t.Fatalf("loadPlayKey: %v", err)
	}

	dir := filepath.Join(root, "movies", "Rotate Me")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	strm := filepath.Join(dir, "Rotate Me.strm")
	if err := os.WriteFile(strm, []byte("http://old/stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	it := &store.Item{
		TMDBID: 77, MediaType: "movie", Title: "Rotate Me", StrmPath: strm,
		LibraryName: "Movies", Status: "ready", Updated: time.Now(),
	}
	it.SetProviderIdentity(library.ProviderTMDB, "77")
	if err := db.Upsert(it); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	cfg := &config.Config{}
	cfg.Server.PublicURL = "http://box:1990"
	migrateStrmTokens(db, cfg)
	before, err := os.ReadFile(strm)
	if err != nil {
		t.Fatal(err)
	}

	// Rotate exactly as the subcommand does, then reload as a restart would.
	if rc := runRotatePlayKey([]string{"--db", dbPath, "--yes"}); rc != 0 {
		t.Fatalf("rotate-play-key exited %d", rc)
	}
	if err := loadPlayKey(db); err != nil {
		t.Fatalf("reload key: %v", err)
	}
	migrateStrmTokens(db, cfg)
	after, err := os.ReadFile(strm)
	if err != nil {
		t.Fatal(err)
	}

	if string(before) == string(after) {
		t.Error("the token did not change, so nothing was revoked")
	}
	// Same identity, still playable — only the signature moved.
	if !strings.Contains(string(after), "/play/movie/77?t=") {
		t.Errorf("the .strm no longer points at the item after rotation: %s", after)
	}
	if !strings.Contains(string(before), "/play/movie/77?t=") {
		t.Errorf("baseline .strm was wrong: %s", before)
	}
}
