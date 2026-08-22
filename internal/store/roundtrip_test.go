package store

import (
	"testing"
	"time"
)

// TestUpsertPreserveSemantics pins down the COALESCE(NULLIF(...)) behaviour in Upsert.
//
// Those clauses mean "an empty incoming value must NOT overwrite a stored one" for
// requested_by, poster_url, magnet and release_title — the health check re-upserts items
// with only the fields it knows, and without this an item would lose its owner and its
// magnet on every pass. Everything else is last-write-wins.
func TestUpsertPreserveSemantics(t *testing.T) {
	s := newTestStore(t)

	const path = "/lib/Movie (2020)/Movie (2020).strm"
	original := &Item{
		TMDBID: 100, MediaType: "movie", Title: "Movie", Year: "2020",
		InfoHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FileIndex: 3,
		StrmPath: path, LibraryName: "Movies", Status: "ready", Seeders: 50,
		Updated: time.Now(), RequestedBy: "alice", IsPrivate: true,
		PosterURL: "http://img/p.jpg", Magnet: "magnet:?xt=urn:btih:AAA",
		ReleaseTitle: "Movie.2020.1080p", Season: 0, Episode: 0,
	}
	if err := s.Upsert(original); err != nil {
		t.Fatal(err)
	}

	// A partial re-upsert, as the health check does: blank owner/poster/magnet/release.
	if err := s.Upsert(&Item{
		TMDBID: 100, MediaType: "movie", Title: "Movie", Year: "2020",
		InfoHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", FileIndex: 5,
		StrmPath: path, LibraryName: "Movies", Status: "ready", Seeders: 90,
		Updated: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetByStrmPath(path)
	if err != nil || got == nil {
		t.Fatalf("GetByStrmPath: %v %v", got, err)
	}

	// PRESERVED — a blank incoming value must not clear a stored one.
	if got.RequestedBy != "alice" {
		t.Errorf("requested_by = %q, want preserved %q", got.RequestedBy, "alice")
	}
	if got.PosterURL != "http://img/p.jpg" {
		t.Errorf("poster_url = %q, want preserved", got.PosterURL)
	}
	if got.Magnet != "magnet:?xt=urn:btih:AAA" {
		t.Errorf("magnet = %q, want preserved", got.Magnet)
	}
	if got.ReleaseTitle != "Movie.2020.1080p" {
		t.Errorf("release_title = %q, want preserved", got.ReleaseTitle)
	}

	// OVERWRITTEN — these are last-write-wins.
	if got.InfoHash != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("info_hash = %q, want overwritten", got.InfoHash)
	}
	if got.FileIndex != 5 {
		t.Errorf("file_index = %d, want 5", got.FileIndex)
	}
	if got.Seeders != 90 {
		t.Errorf("seeders = %d, want 90", got.Seeders)
	}
	// is_private is NOT preserve-semantics — it is overwritten, which is how the
	// dashboard toggle can turn privacy back off.
	if got.IsPrivate {
		t.Errorf("is_private = true, want overwritten to false")
	}
}

// TestMarkStaleAndRevive covers the ready -> stale -> ready lifecycle and the
// nil-vs-set semantics of stale_since.
func TestMarkStaleAndRevive(t *testing.T) {
	s := newTestStore(t)
	const path = "/lib/x.strm"

	if err := s.Upsert(&Item{
		TMDBID: 1, MediaType: "movie", Title: "X", Year: "2020", StrmPath: path,
		Status: "ready", Updated: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetByStrmPath(path)
	if got.StaleSince != nil {
		t.Fatalf("a ready item must have stale_since nil, got %v", got.StaleSince)
	}

	if err := s.MarkStale(path); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetByStrmPath(path)
	if got.Status != "stale" {
		t.Fatalf("status = %q, want stale", got.Status)
	}
	if got.StaleSince == nil {
		t.Fatal("stale_since must be set on the transition to stale")
	}
	firstStale := *got.StaleSince

	// A SECOND MarkStale must NOT move the timestamp — stale_since records when it
	// FIRST went stale, which is what "expired N days ago" is computed from.
	time.Sleep(10 * time.Millisecond)
	if err := s.MarkStale(path); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetByStrmPath(path)
	if !got.StaleSince.Equal(firstStale) {
		t.Errorf("stale_since moved on a repeat MarkStale: %v -> %v", firstStale, *got.StaleSince)
	}

	// Revival clears it, exactly as the health check does.
	got.Status = "ready"
	got.StaleSince = nil
	got.Updated = time.Now()
	if err := s.Upsert(got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetByStrmPath(path)
	if got.Status != "ready" {
		t.Fatalf("status = %q, want ready", got.Status)
	}
	if got.StaleSince != nil {
		t.Fatalf("stale_since = %v, want nil after revival", *got.StaleSince)
	}
}

func TestGetByIdentityAndEpisode(t *testing.T) {
	s := newTestStore(t)
	if err := s.Upsert(&Item{
		TMDBID: 55, MediaType: "tv", Title: "Show S01E05", Year: "2021",
		StrmPath: "/tv/s01e05.strm", Status: "ready", Season: 1, Episode: 5, Updated: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetByIdentity(55, "tv", 1, 5)
	if err != nil || got == nil {
		t.Fatalf("GetByIdentity: %v %v", got, err)
	}
	// A miss must be (nil, nil), not an error — every caller branches on that.
	miss, err := s.GetByIdentity(55, "tv", 1, 6)
	if err != nil {
		t.Fatalf("a miss must not be an error: %v", err)
	}
	if miss != nil {
		t.Fatalf("expected nil for a missing identity, got %+v", miss)
	}
	ep, err := s.GetEpisode(55, 1, 5)
	if err != nil || ep == nil {
		t.Fatalf("GetEpisode: %v %v", ep, err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	v, err := s.GetSetting("missing")
	if err != nil {
		t.Fatalf("a missing setting must not be an error: %v", err)
	}
	if v != "" {
		t.Fatalf("missing setting = %q, want empty", v)
	}
	if err := s.SetSetting("k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("k", "v2"); err != nil {
		t.Fatalf("SetSetting must upsert, not conflict: %v", err)
	}
	if v, _ := s.GetSetting("k"); v != "v2" {
		t.Fatalf("setting = %q, want v2", v)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateUser(&User{Username: "alice", PasswordHash: "h", AuthSource: "local"}); err != nil {
		t.Fatal(err)
	}
	u, _ := s.GetUserByUsername("alice")

	if err := s.CreateSession("tok1", u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("tok2", u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetSessionUser("tok1"); !ok {
		t.Fatal("tok1 should be valid")
	}

	// A password change must invalidate every OTHER session but keep the caller's.
	if err := s.DeleteSessionsForUser(u.ID, "tok1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetSessionUser("tok1"); !ok {
		t.Error("the kept token must survive")
	}
	if _, ok := s.GetSessionUser("tok2"); ok {
		t.Error("other sessions must be invalidated on a password change")
	}

	// An expired session must not authenticate.
	if err := s.CreateSession("old", u.ID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetSessionUser("old"); ok {
		t.Error("an expired session must not be valid")
	}
	if err := s.PurgeSessions(); err != nil {
		t.Fatal(err)
	}
}

// TestUserPasswordHashNeverSerialised guards the json:"-" tag on User.PasswordHash.
func TestUserPasswordHashNeverSerialised(t *testing.T) {
	u := User{ID: 1, Username: "alice", PasswordHash: "$2a$10$SECRETHASHVALUE", IsAdmin: true}
	b, err := jsonMarshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if containsStr(string(b), "SECRETHASH") || containsStr(string(b), "password") {
		t.Fatalf("User marshalled its password hash: %s", b)
	}
	if !containsStr(string(b), `"username":"alice"`) {
		t.Fatalf("User lost its public fields: %s", b)
	}
}
