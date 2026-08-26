package store

import (
	"testing"
	"time"
)

// mustProviderItem inserts one non-TMDB row for the provider-scoped query tests.
func mustProviderItem(t *testing.T, s *Store, provider, id, mediaType, title string, season, episode int, strm string) {
	t.Helper()
	it := &Item{
		MediaType: mediaType, Title: title, StrmPath: strm,
		Status: "ready", Updated: time.Now(), Season: season, Episode: episode,
	}
	it.SetProviderIdentity(provider, id)
	if err := s.Upsert(it); err != nil {
		t.Fatalf("Upsert(%s/%s): %v", provider, id, err)
	}
}

// TestItemsByProviderIsScoped is the security-relevant half: the ingest API's listing is
// keyed on the provider in the URL, and a secret that authorises one namespace must not
// enumerate another's — nor the TMDB library, which is where every ordinary request lands.
func TestItemsByProviderIsScoped(t *testing.T) {
	s := newTestStore(t)

	mustProviderItem(t, s, "alpha", "a1", "movie", "Alpha One", 0, 0, "/lib/a1.strm")
	mustProviderItem(t, s, "alpha", "a2", "movie", "Alpha Two", 0, 0, "/lib/a2.strm")
	mustProviderItem(t, s, "beta", "b1", "movie", "Beta One", 0, 0, "/lib/b1.strm")

	tmdbItem := &Item{
		MediaType: "movie", Title: "TMDB Film", StrmPath: "/lib/t1.strm",
		Status: "ready", Updated: time.Now(),
	}
	tmdbItem.SetTMDBIdentity(550)
	if err := s.Upsert(tmdbItem); err != nil {
		t.Fatalf("Upsert(tmdb): %v", err)
	}

	alpha, err := s.ItemsByProvider("alpha")
	if err != nil {
		t.Fatalf("ItemsByProvider: %v", err)
	}
	if len(alpha) != 2 {
		t.Fatalf("alpha rows = %d, want 2", len(alpha))
	}
	for _, it := range alpha {
		if it.Provider != "alpha" {
			t.Fatalf("row from provider %q leaked into the alpha listing", it.Provider)
		}
	}

	if beta, _ := s.ItemsByProvider("beta"); len(beta) != 1 {
		t.Fatalf("beta rows = %d, want 1", len(beta))
	}
	if none, _ := s.ItemsByProvider("gamma"); len(none) != 0 {
		t.Fatalf("an unknown provider returned %d rows, want 0", len(none))
	}
	// The TMDB namespace is reachable by name but is NOT swept up by another provider's
	// listing — the two are separate key spaces, which is the invariant the whole
	// provider dimension rests on.
	if tm, _ := s.ItemsByProvider(ProviderTMDB); len(tm) != 1 {
		t.Fatalf("tmdb rows = %d, want 1", len(tm))
	}
}

// TestItemsByProviderIDSpansMediaTypes: the ingest delete does not make the caller
// restate whether the id is a movie or a series, so the query must not filter on it.
func TestItemsByProviderIDSpansMediaTypes(t *testing.T) {
	s := newTestStore(t)

	mustProviderItem(t, s, "alpha", "show1", "tv", "Show S1E1", 1, 1, "/lib/s1e1.strm")
	mustProviderItem(t, s, "alpha", "show1", "tv", "Show S1E2", 1, 2, "/lib/s1e2.strm")
	mustProviderItem(t, s, "alpha", "show1", "movie", "Show The Movie", 0, 0, "/lib/movie.strm")
	mustProviderItem(t, s, "alpha", "show2", "tv", "Other S1E1", 1, 1, "/lib/o1.strm")
	mustProviderItem(t, s, "beta", "show1", "tv", "Beta S1E1", 1, 1, "/lib/b1.strm")

	got, err := s.ItemsByProviderID("alpha", "show1")
	if err != nil {
		t.Fatalf("ItemsByProviderID: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3 (two episodes and a movie)", len(got))
	}
	// The same id under a DIFFERENT provider must not come back. Two catalogues numbering
	// their titles from 1 is the normal case, not an edge case.
	for _, it := range got {
		if it.Provider != "alpha" || it.ProviderID != "show1" {
			t.Fatalf("unexpected row %s/%s in the listing", it.Provider, it.ProviderID)
		}
	}
}

// TestProviderRowRoundTripsIdentity: a row written with a provider identity must be
// findable by the exact identity /play spells, including the (0,0) a movie route sends.
func TestProviderRowRoundTripsIdentity(t *testing.T) {
	s := newTestStore(t)
	mustProviderItem(t, s, "alpha", "uuid-1", "movie", "A Film", 0, 0, "/lib/f.strm")

	got, err := s.GetByProviderIdentity(Identity{
		Provider: "alpha", ProviderID: "uuid-1", MediaType: "movie",
	})
	if err != nil || got == nil {
		t.Fatalf("GetByProviderIdentity = %v, %v", got, err)
	}
	if got.TMDBID != 0 {
		t.Fatalf("tmdb_id = %d, want 0 — a provider row has exactly one identity", got.TMDBID)
	}

	// A provider row carrying a tmdb_id as well must be REFUSED, not silently stored:
	// it would be found by a TMDB lookup and a provider lookup as two different things.
	bad := &Item{
		MediaType: "movie", Title: "Bad", StrmPath: "/lib/bad.strm",
		Status: "ready", Updated: time.Now(),
		Provider: "alpha", ProviderID: "uuid-2", TMDBID: 99,
	}
	if err := s.Upsert(bad); err == nil {
		t.Fatal("Upsert accepted a row claiming both a provider identity and a tmdb_id")
	}
}
