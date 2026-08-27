package store

import (
	"errors"
	"regexp"
	"testing"
	"time"
)

// The id is what makes a web source addressable. It has to satisfy the SAME charset the
// play routes and the capability-token encoder accept — internal/library's providerIDRe
// — so this pins the shape here rather than discovering a violation as a 400 at playback.
var providerIDShape = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func TestWebSourceIDIsRoutableAndStable(t *testing.T) {
	const page = "https://example.com/view_video.php?viewkey=abc123"
	id := WebSourceID(page)
	if !providerIDShape.MatchString(id) {
		t.Fatalf("id %q is not a legal provider id — it cannot appear in a /play URL", id)
	}
	if id != WebSourceID(page) {
		t.Fatal("the id is not deterministic — the same link would add a second entry every time")
	}
	if WebSourceID(page) == WebSourceID(page+"4") {
		t.Fatal("two different pages collide")
	}
}

// The id must be opaque: it appears in a .strm file, in Jellyfin's database and in every
// access log in between. If any part of the URL survived into it, all of those would
// carry the name of the site.
func TestWebSourceIDLeaksNothingFromTheURL(t *testing.T) {
	id := WebSourceID("https://very-distinctive-hostname.example/watch/12345")
	for _, frag := range []string{"very", "distinctive", "hostname", "example", "12345", "watch"} {
		if regexp.MustCompile(`(?i)` + frag).MatchString(id) {
			t.Fatalf("id %q contains %q from the URL", id, frag)
		}
	}
}

func TestWebSourceIDCanonicalisation(t *testing.T) {
	same := func(a, b string) {
		t.Helper()
		if WebSourceID(a) != WebSourceID(b) {
			t.Errorf("%q and %q should be the same page", a, b)
		}
	}
	differ := func(a, b string) {
		t.Helper()
		if WebSourceID(a) == WebSourceID(b) {
			t.Errorf("%q and %q should be different pages", a, b)
		}
	}
	same("https://Example.COM/watch/1", "https://example.com/watch/1")
	same("HTTPS://example.com/watch/1", "https://example.com/watch/1")
	same("https://example.com/watch/1/", "https://example.com/watch/1")
	same("  https://example.com/watch/1  ", "https://example.com/watch/1")

	// The path is case-SENSITIVE on most sites, and a video id is very often a query
	// parameter — collapsing either would merge unrelated videos into one entry.
	differ("https://example.com/watch/AbC", "https://example.com/watch/abc")
	differ("https://example.com/v?id=1", "https://example.com/v?id=2")
	differ("https://example.com/v?id=1", "https://example.com/v")
}

func TestWebSourceRoundTrip(t *testing.T) {
	s := newTestStore(t)
	const page = "https://example.com/watch/1"
	now := time.Now().Truncate(time.Second)

	ws := &WebSource{
		ID: WebSourceID(page), PageURL: page, Title: "A Video", Uploader: "someone",
		Extractor: "Generic", Duration: 596, Thumbnail: "https://example.com/t.jpg",
		AddedBy: "moon", AddedAt: now, LastOK: &now,
	}
	if err := s.UpsertWebSource(ws); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetWebSource(ws.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v (got %v)", err, got)
	}
	if got.Title != "A Video" || got.Duration != 596 || got.AddedBy != "moon" {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if got.LastOK == nil {
		t.Fatal("last_ok_at was not stored")
	}

	// Found by page URL as well as by id — this is what makes "you already added this"
	// answerable before an add rather than after it.
	byURL, err := s.WebSourceByPageURL("https://Example.com/watch/1/")
	if err != nil || byURL == nil || byURL.ID != ws.ID {
		t.Fatalf("lookup by page URL failed: %v %v", byURL, err)
	}
}

// Re-probing must refresh the description without rewriting who added the entry or when.
func TestUpsertPreservesProvenanceAndClearsTheLastError(t *testing.T) {
	s := newTestStore(t)
	const page = "https://example.com/watch/1"
	added := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	id := WebSourceID(page)

	if err := s.UpsertWebSource(&WebSource{ID: id, PageURL: page, Title: "Old title",
		AddedBy: "moon", AddedAt: added}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := s.MarkWebSourceFailed(id, "Video unavailable"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	// A refresh by somebody else, later.
	if err := s.UpsertWebSource(&WebSource{ID: id, PageURL: page, Title: "New title",
		AddedBy: "someone-else", AddedAt: time.Now()}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, _ := s.GetWebSource(id)
	if got.Title != "New title" {
		t.Errorf("metadata was not refreshed: %q", got.Title)
	}
	if got.AddedBy != "moon" {
		t.Errorf("added_by was overwritten: %q — provenance is not the refresher's to change", got.AddedBy)
	}
	if !got.AddedAt.Equal(added) {
		t.Errorf("added_at was overwritten: %v, want %v", got.AddedAt, added)
	}
	if got.LastError != "" {
		t.Errorf("last_error survived a successful re-probe: %q", got.LastError)
	}
}

// The failure record is the entire troubleshooting story for this feature: an uploader
// deleting a video, a site changing its player and a broken extractor all look identical
// from Jellyfin, and only this tells them apart.
func TestFailureRecordKeepsTheLastKnownGoodTime(t *testing.T) {
	s := newTestStore(t)
	const page = "https://example.com/watch/1"
	id := WebSourceID(page)
	ok := time.Now().Add(-time.Hour).Truncate(time.Second)

	if err := s.UpsertWebSource(&WebSource{ID: id, PageURL: page, AddedBy: "moon", LastOK: &ok}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.MarkWebSourceFailed(id, "Video unavailable"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	got, _ := s.GetWebSource(id)
	if got.LastError != "Video unavailable" {
		t.Errorf("last_error = %q", got.LastError)
	}
	if got.LastOK == nil || !got.LastOK.Equal(ok) {
		t.Errorf("last_ok_at was lost — 'it worked an hour ago' is the useful half of the message")
	}

	if err := s.MarkWebSourceOK(id); err != nil {
		t.Fatalf("mark ok: %v", err)
	}
	got, _ = s.GetWebSource(id)
	if got.LastError != "" {
		t.Errorf("last_error survived a success: %q", got.LastError)
	}
}

func TestMarkingAnUnknownWebSourceIsAnError(t *testing.T) {
	s := newTestStore(t)
	if err := s.MarkWebSourceOK("nosuchid"); !errors.Is(err, ErrWebSourceNotFound) {
		t.Errorf("MarkWebSourceOK = %v, want ErrWebSourceNotFound", err)
	}
	if err := s.MarkWebSourceFailed("nosuchid", "x"); !errors.Is(err, ErrWebSourceNotFound) {
		t.Errorf("MarkWebSourceFailed = %v, want ErrWebSourceNotFound", err)
	}
}

func TestGetUnknownWebSourceIsNotAnError(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetWebSource("nosuchid")
	if err != nil || got != nil {
		t.Fatalf("got (%v, %v), want (nil, nil) — matching every other getter here", got, err)
	}
}

func TestListAndDelete(t *testing.T) {
	s := newTestStore(t)
	for i, page := range []string{
		"https://example.com/a", "https://example.com/b", "https://example.com/c",
	} {
		err := s.UpsertWebSource(&WebSource{
			ID: WebSourceID(page), PageURL: page, Title: page,
			AddedAt: time.Now().Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatalf("upsert %s: %v", page, err)
		}
	}
	all, err := s.ListWebSources()
	if err != nil || len(all) != 3 {
		t.Fatalf("list: %d entries, %v", len(all), err)
	}
	if all[0].PageURL != "https://example.com/c" {
		t.Errorf("newest first is not holding: %q", all[0].PageURL)
	}
	if err := s.DeleteWebSource(all[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if all, _ = s.ListWebSources(); len(all) != 2 {
		t.Fatalf("after delete: %d entries", len(all))
	}
}

func TestUpsertRefusesAnIncompleteRow(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertWebSource(&WebSource{PageURL: "https://example.com/a"}); err == nil {
		t.Error("a row with no id was accepted — it would be unroutable")
	}
	if err := s.UpsertWebSource(&WebSource{ID: "abc"}); err == nil {
		t.Error("a row with no page URL was accepted — it could never be resolved")
	}
}
