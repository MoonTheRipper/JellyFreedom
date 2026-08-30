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
