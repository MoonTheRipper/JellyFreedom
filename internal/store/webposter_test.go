package store

import (
	"path/filepath"
	"testing"
	"time"
)

// Web-source posters used to be stored as the SOURCE SITE's CDN URL, and the library card
// renders poster_url directly — so every viewer's browser fetched that image from the tube
// site, from the home address, outside the VPN. Fixing the write path only helps new links;
// rows already in the library keep leaking until this migration runs.
func TestWebPostersAreRepointedAtTheRelay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "poster.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	web := &Item{
		MediaType: "movie", Title: "A Web Source", StrmPath: "/srv/x/a.strm",
		LibraryName: "Movies", Status: "ready", Updated: time.Now(),
		PosterURL: "https://cdn.example-tube.com/thumbs/abc.jpg?sig=deadbeef",
	}
	web.SetProviderIdentity("web", "abc123")
	if err := db.Upsert(web); err != nil {
		t.Fatalf("upsert web: %v", err)
	}
	tmdb := &Item{
		TMDBID: 550, MediaType: "movie", Title: "Fight Club", StrmPath: "/srv/x/b.strm",
		LibraryName: "Movies", Status: "ready", Updated: time.Now(),
		PosterURL: "https://image.tmdb.org/t/p/w500/poster.jpg",
	}
	tmdb.SetProviderIdentity("tmdb", "550")
	if err := db.Upsert(tmdb); err != nil {
		t.Fatalf("upsert tmdb: %v", err)
	}

	// Re-run the migration the way a restart would.
	if err := db.migrateWebPosterURLs(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	items, err := db.ListAllItems()
	if err != nil {
		t.Fatalf("ListAllItems: %v", err)
	}
	var gotWeb, gotTMDB string
	for _, it := range items {
		switch it.Provider {
		case "web":
			gotWeb = it.PosterURL
		case "tmdb":
			gotTMDB = it.PosterURL
		}
	}
	if want := "/api/websources/abc123/thumbnail"; gotWeb != want {
		t.Errorf("web poster = %q, want %q — the browser would still fetch it from the source", gotWeb, want)
	}
	// TMDB artwork is a different question and must not be touched by this.
	if gotTMDB != "https://image.tmdb.org/t/p/w500/poster.jpg" {
		t.Errorf("TMDB poster was rewritten: %q", gotTMDB)
	}

	// Idempotent: a second run must not stack another prefix on.
	if err := db.migrateWebPosterURLs(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	items, _ = db.ListAllItems()
	for _, it := range items {
		if it.Provider == "web" && it.PosterURL != "/api/websources/abc123/thumbnail" {
			t.Errorf("migration is not idempotent: %q", it.PosterURL)
		}
	}
	_ = db.Close()
}
