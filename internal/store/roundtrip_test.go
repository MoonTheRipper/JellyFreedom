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

// ── The provider dimension ────────────────────────────────────────────────────

// TestUpsertDerivesProviderFromTMDBID: the same write-path invariant Enqueue has. Every
// Upsert call site in cmd/orchestrator builds an Item{TMDBID: n} literal and knows
// nothing about providers; it must still produce a canonical identity.
func TestUpsertDerivesProviderFromTMDBID(t *testing.T) {
	s := newTestStore(t)
	it := &Item{TMDBID: 1622, MediaType: "tv", Title: "Supernatural S14E01", Year: "2018",
		StrmPath: "/tv/s14e01.strm", Status: "ready", Season: 14, Episode: 1, Updated: time.Now()}
	if err := s.Upsert(it); err != nil {
		t.Fatal(err)
	}
	if it.Provider != ProviderTMDB || it.ProviderID != "1622" {
		t.Fatalf("caller's struct left at (%q,%q)", it.Provider, it.ProviderID)
	}
	got, err := s.GetByStrmPath("/tv/s14e01.strm")
	if err != nil || got == nil {
		t.Fatalf("GetByStrmPath: %v %v", got, err)
	}
	if got.Provider != ProviderTMDB || got.ProviderID != "1622" || got.TMDBID != 1622 {
		t.Fatalf("stored identity = (%q,%q,%d)", got.Provider, got.ProviderID, got.TMDBID)
	}
}

// TestItemIdentityIsProviderScoped: two catalogues' identically-numbered shows are two
// library entries, and neither lookup may reach the other.
func TestItemIdentityIsProviderScoped(t *testing.T) {
	s := newTestStore(t)
	if err := s.Upsert(&Item{
		TMDBID: 1622, MediaType: "tv", Title: "Supernatural S14E01", Year: "2018",
		StrmPath: "/tmdb/s14e01.strm", Status: "ready", Season: 14, Episode: 1, Updated: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	other := &Item{MediaType: "tv", Title: "Something Else S14E01", Year: "2019",
		StrmPath: "/anidb/s14e01.strm", Status: "ready", Season: 14, Episode: 1, Updated: time.Now()}
	other.SetProviderIdentity("anidb", "1622")
	if err := s.Upsert(other); err != nil {
		t.Fatal(err)
	}

	// The TMDB path — the one a live /play/tv/1622/14/1 URL takes — finds its own row.
	viaTMDB, err := s.GetByIdentity(1622, "tv", 14, 1)
	if err != nil || viaTMDB == nil {
		t.Fatalf("GetByIdentity: %v %v", viaTMDB, err)
	}
	if viaTMDB.StrmPath != "/tmdb/s14e01.strm" {
		t.Fatalf("the TMDB identity resolved to %q", viaTMDB.StrmPath)
	}
	viaProvider, err := s.GetByProviderIdentity(Identity{
		Provider: "anidb", ProviderID: "1622", MediaType: "tv", Season: 14, Episode: 1})
	if err != nil || viaProvider == nil {
		t.Fatalf("GetByProviderIdentity: %v %v", viaProvider, err)
	}
	if viaProvider.StrmPath != "/anidb/s14e01.strm" {
		t.Fatalf("the anidb identity resolved to %q", viaProvider.StrmPath)
	}

	// GetEpisode, which series/episode removal goes through, is scoped the same way.
	ep, err := s.GetEpisode(1622, 14, 1)
	if err != nil || ep == nil || ep.StrmPath != "/tmdb/s14e01.strm" {
		t.Fatalf("GetEpisode crossed providers: %+v (%v)", ep, err)
	}
	// And so is the whole-title fetch that "remove this show" enumerates.
	tmdbRows, err := s.ItemsByTMDB(1622, "tv")
	if err != nil {
		t.Fatal(err)
	}
	if len(tmdbRows) != 1 || tmdbRows[0].StrmPath != "/tmdb/s14e01.strm" {
		t.Fatalf("ItemsByTMDB(1622) returned %d rows including another provider's", len(tmdbRows))
	}
	anidbRows, err := s.ItemsByTitle("anidb", "1622", "tv")
	if err != nil {
		t.Fatal(err)
	}
	if len(anidbRows) != 1 || anidbRows[0].StrmPath != "/anidb/s14e01.strm" {
		t.Fatalf("ItemsByTitle(anidb,1622) returned %d rows", len(anidbRows))
	}
}

// TestItemUUIDProviderIDRoundTrips: the UUID must come back byte for byte, and the row
// must not be reachable through TMDB's zero identity.
func TestItemUUIDProviderIDRoundTrips(t *testing.T) {
	s := newTestStore(t)
	const uuid = "cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c"
	it := &Item{MediaType: "movie", Title: "UUID Movie", Year: "2024",
		StrmPath: "/uuid/movie.strm", Status: "ready", Updated: time.Now()}
	it.SetProviderIdentity("anidb", uuid)
	if err := s.Upsert(it); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetByProviderIdentity(Identity{Provider: "anidb", ProviderID: uuid, MediaType: "movie"})
	if err != nil || got == nil {
		t.Fatalf("GetByProviderIdentity: %v %v", got, err)
	}
	if got.ProviderID != uuid {
		t.Fatalf("provider_id came back %q, want %q — it was coerced", got.ProviderID, uuid)
	}
	if got.TMDBID != 0 {
		t.Fatalf("a non-TMDB item picked up tmdb_id %d", got.TMDBID)
	}
	// GetByIdentity(0, …) is what a TMDB lookup for "id 0" would be. It must NOT find
	// this row: that is the collapse an INTEGER provider column would have produced.
	miss, err := s.GetByIdentity(0, "movie", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if miss != nil {
		t.Fatalf("a TMDB lookup for id 0 reached a UUID-identified row: %+v", miss)
	}
}

// TestSetTMDBIdentityKeepsTheThreeFieldsTogether covers the helper directly: there is no
// sequence of calls on it that leaves an item with half an identity.
func TestSetTMDBIdentityKeepsTheThreeFieldsTogether(t *testing.T) {
	var it Item
	it.SetTMDBIdentity(1622)
	if it.TMDBID != 1622 || it.Provider != ProviderTMDB || it.ProviderID != "1622" {
		t.Fatalf("SetTMDBIdentity left (%d,%q,%q)", it.TMDBID, it.Provider, it.ProviderID)
	}
	if got := it.Identity(); got != (Identity{Provider: "tmdb", ProviderID: "1622"}) {
		t.Fatalf("Identity() = %+v", got)
	}
	// Switching to another provider must drop the TMDB id rather than leave the row
	// claiming two identities at once.
	it.SetProviderIdentity("anidb", "abc")
	if it.TMDBID != 0 || it.Provider != "anidb" || it.ProviderID != "abc" {
		t.Fatalf("SetProviderIdentity left (%d,%q,%q)", it.TMDBID, it.Provider, it.ProviderID)
	}

	var q QueueItem
	q.SetTMDBIdentity(27205)
	if q.TMDBID != 27205 || q.Provider != ProviderTMDB || q.ProviderID != "27205" {
		t.Fatalf("QueueItem.SetTMDBIdentity left (%d,%q,%q)", q.TMDBID, q.Provider, q.ProviderID)
	}
	var sub Subscription
	sub.SetTMDBIdentity(1622)
	if sub.TMDBID != 1622 || sub.Provider != ProviderTMDB || sub.ProviderID != "1622" {
		t.Fatalf("Subscription.SetTMDBIdentity left (%d,%q,%q)", sub.TMDBID, sub.Provider, sub.ProviderID)
	}
}

// TestGetStatusByTMDBIDsStaysWithinTMDB: the search-badge endpoint speaks TMDB integers
// and returns a map keyed on them, so it must not be answered by another provider's row
// whose tmdb_id column is 0.
func TestGetStatusByTMDBIDsStaysWithinTMDB(t *testing.T) {
	s := newTestStore(t)
	it := &Item{MediaType: "movie", Title: "UUID Movie", Year: "2024",
		StrmPath: "/uuid/movie.strm", Status: "ready", Updated: time.Now()}
	it.SetProviderIdentity("anidb", "cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c")
	if err := s.Upsert(it); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetStatusByTMDBIDs([]int{0}, vw("alice", true))
	if err != nil {
		t.Fatal(err)
	}
	if _, found := got[0]; found {
		t.Fatal("a non-TMDB row answered a TMDB status lookup for id 0")
	}
}

// TestItemJSONCarriesBothIdentities: tmdb_id must stay exactly where the web UI reads it
// (214 call sites), with provider/provider_id added BESIDE it, not in place of it.
func TestItemJSONCarriesBothIdentities(t *testing.T) {
	var it Item
	it.SetTMDBIdentity(1622)
	b, err := jsonMarshal(it)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"tmdb_id":1622`, `"provider":"tmdb"`, `"provider_id":"1622"`} {
		if !containsStr(string(b), want) {
			t.Errorf("Item JSON is missing %s:\n%s", want, b)
		}
	}
	var q QueueItem
	q.SetTMDBIdentity(1622)
	b, err = jsonMarshal(q)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"tmdb_id":1622`, `"provider":"tmdb"`, `"provider_id":"1622"`} {
		if !containsStr(string(b), want) {
			t.Errorf("QueueItem JSON is missing %s:\n%s", want, b)
		}
	}
}
