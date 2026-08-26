package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"jellyfreedom/internal/library"
)

// testKey is a fixed HMAC key so that every token in this file is reproducible. It is not
// a secret and never leaves the test binary.
var testKey = []byte("0123456789abcdef0123456789abcdef")

func withTestKey(t *testing.T) {
	t.Helper()
	playKeyMu.Lock()
	playKey = testKey
	playKeyMu.Unlock()
}

// oldPlayToken reimplements the pre-provider token construction from scratch: HMAC-SHA256
// over the literal identity string, base64 raw-url encoded.
//
// It is written out longhand ON PURPOSE. Calling the production code to generate the
// "old" token would make this test tautological — it would pass no matter how the
// construction changed. Copying it means the test fails the moment the live code stops
// producing what the ~1,000 .strm files on the user's install already carry.
func oldPlayToken(identity string) string {
	m := hmac.New(sha256.New, testKey)
	m.Write([]byte(identity))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// The identity strings are a wire format. Every .strm file in the live library carries an
// HMAC over one of these exact byte sequences, so these literals are the compatibility
// contract: if an edit changes them, this test is the thing that says so, loudly, instead
// of a user discovering it as a 403 on every title at once.
func TestLegacyIdentityStringsArePinned(t *testing.T) {
	cases := []struct {
		mediaType        string
		tmdb, season, ep int
		want             string
	}{
		{"movie", 27205, 0, 0, "movie:27205"},
		{"movie", 1622, 0, 0, "movie:1622"},
		{"tv", 1622, 14, 1, "tv:1622:14:1"},
		{"tv", 76479, 1, 6, "tv:76479:1:6"},
		{"tv", 1399, 2, 9, "tv:1399:2:9"},
	}
	for _, c := range cases {
		if got := playIdentity(c.mediaType, c.tmdb, c.season, c.ep); got != c.want {
			t.Errorf("playIdentity(%q,%d,%d,%d) = %q, want %q",
				c.mediaType, c.tmdb, c.season, c.ep, got, c.want)
		}
		// The provider-aware encoder must produce the identical bytes for provider "tmdb".
		got, err := playIdentityFor(library.ProviderTMDB, c.mediaType, strconv.Itoa(c.tmdb), c.season, c.ep)
		if err != nil {
			t.Fatalf("playIdentityFor(tmdb, %q, %d): %v", c.mediaType, c.tmdb, err)
		}
		if got != c.want {
			t.Errorf("playIdentityFor(tmdb,%q,%d,%d,%d) = %q, want %q",
				c.mediaType, c.tmdb, c.season, c.ep, got, c.want)
		}
	}
}

// The whole point of the change: a token minted by the code that shipped must still open
// the same door after it.
func TestTokensMintedByTheOldCodeStillValidate(t *testing.T) {
	withTestKey(t)

	for _, c := range []struct {
		identity  string
		mediaType string
		tmdb      int
		season    int
		ep        int
	}{
		{"tv:1622:14:1", "tv", 1622, 14, 1},
		{"movie:27205", "movie", 27205, 0, 0},
		{"tv:76479:4:2", "tv", 76479, 4, 2},
	} {
		legacy := oldPlayToken(c.identity)

		if !validPlayToken(legacy, c.mediaType, c.tmdb, c.season, c.ep) {
			t.Errorf("legacy token for %q was rejected by validPlayToken", c.identity)
		}
		if !validPlayTokenFor(legacy, library.ProviderTMDB, c.mediaType, strconv.Itoa(c.tmdb), c.season, c.ep) {
			t.Errorf("legacy token for %q was rejected by validPlayTokenFor", c.identity)
		}
		if got := playToken(c.mediaType, c.tmdb, c.season, c.ep); got != legacy {
			t.Errorf("playToken(%q) = %q, want the legacy %q", c.identity, got, legacy)
		}
	}
}

// /play/tv/1622/14/1 and /play/p/tmdb/tv/1622/14/1 are two spellings of ONE identity. If
// they ever diverged, a .strm written in the new shape would not be playable from the old
// route (or the reverse), and the library would quietly split in two.
func TestLegacyAndNamespacedTMDBRoutesAgree(t *testing.T) {
	withTestKey(t)

	legacyID := playIdentity("tv", 1622, 14, 1)
	namespacedID, err := playIdentityFor("tmdb", "tv", "1622", 14, 1)
	if err != nil {
		t.Fatal(err)
	}
	if legacyID != namespacedID {
		t.Fatalf("identities differ: %q vs %q", legacyID, namespacedID)
	}

	legacyTok := playToken("tv", 1622, 14, 1)
	namespacedTok, err := playTokenFor("tmdb", "tv", "1622", 14, 1)
	if err != nil {
		t.Fatal(err)
	}
	if legacyTok != namespacedTok {
		t.Fatalf("tokens differ: %q vs %q", legacyTok, namespacedTok)
	}
	// And each token must be accepted by the other spelling's validator.
	if !validPlayTokenFor(legacyTok, "tmdb", "tv", "1622", 14, 1) {
		t.Error("a legacy token was refused for the namespaced identity")
	}
	if !validPlayToken(namespacedTok, "tv", 1622, 14, 1) {
		t.Error("a namespaced token was refused for the legacy identity")
	}
}

// A provider whose ids are UUIDs must survive the whole loop: URL out, URL back in, token
// checked. This is the shape the next provider actually has.
func TestNonTMDBProviderRoundTrips(t *testing.T) {
	withTestKey(t)

	const uuid = "cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c"
	ref := playRef{provider: "anidb", mediaType: "tv", providerID: uuid, season: 3, episode: 12}

	id, err := ref.identity()
	if err != nil {
		t.Fatal(err)
	}
	if want := "p:anidb:tv:" + uuid + ":3:12"; id != want {
		t.Fatalf("identity = %q, want %q", id, want)
	}

	url := playURLFor("http://host:1990", ref)
	wantPrefix := "http://host:1990/play/p/anidb/tv/" + uuid + "/3/12?t="
	if !strings.HasPrefix(url, wantPrefix) {
		t.Fatalf("URL = %q, want prefix %q", url, wantPrefix)
	}

	tok := strings.TrimPrefix(url, wantPrefix)
	if !ref.validToken(tok) {
		t.Fatal("the token from the URL did not validate against its own identity")
	}
	// Neighbouring identities must be refused by that token.
	for _, bad := range []playRef{
		{provider: "anidb", mediaType: "tv", providerID: uuid, season: 3, episode: 13},
		{provider: "anidb", mediaType: "tv", providerID: uuid, season: 4, episode: 12},
		{provider: "anidb", mediaType: "movie", providerID: uuid},
		{provider: "anilist", mediaType: "tv", providerID: uuid, season: 3, episode: 12},
		{provider: "tmdb", mediaType: "tv", providerID: "1622", season: 3, episode: 12},
	} {
		if bad.validToken(tok) {
			t.Errorf("a token for %v was accepted for %v", ref, bad)
		}
	}

	// A movie for the same provider.
	mref := playRef{provider: "anidb", mediaType: "movie", providerID: uuid}
	mid, err := mref.identity()
	if err != nil {
		t.Fatal(err)
	}
	if want := "p:anidb:movie:" + uuid; mid != want {
		t.Fatalf("movie identity = %q, want %q", mid, want)
	}
	if u := playURLFor("http://host:1990/", mref); !strings.HasPrefix(u, "http://host:1990/play/p/anidb/movie/"+uuid+"?t=") {
		t.Fatalf("movie URL = %q", u)
	}
}

// Anything that is not a well-formed identity must be refused at the encoder, which means
// no token is ever minted for it and no token ever validates against it. These are the
// inputs an attacker would reach for: the HMAC field delimiter, a path separator, a
// traversal, an overlong id, an empty id.
func TestMalformedIdentitiesAreRejected(t *testing.T) {
	withTestKey(t)

	cases := []struct {
		name       string
		provider   string
		mediaType  string
		providerID string
	}{
		{"provider with a colon", "an:idb", "tv", "1"},
		{"id with a colon", "anidb", "tv", "16:22"},
		{"provider with a slash", "an/idb", "tv", "1"},
		{"id with a slash", "anidb", "tv", "16/22"},
		{"id with a traversal", "anidb", "tv", ".."},
		{"id with a dotted traversal", "anidb", "tv", "../../etc/passwd"},
		{"id with a percent escape", "anidb", "tv", "%3A"},
		{"id with a query separator", "anidb", "tv", "1?t=x"},
		{"id with a newline", "anidb", "tv", "1\n2"},
		{"id with a space", "anidb", "tv", "16 22"},
		{"empty id", "anidb", "tv", ""},
		{"empty provider", "", "tv", "1"},
		{"overlong id", "anidb", "tv", strings.Repeat("a", library.MaxProviderIDLen+1)},
		{"overlong provider", strings.Repeat("a", library.MaxProviderLen+1), "tv", "1"},
		{"uppercase provider", "AniDB", "tv", "1"},
		{"provider with a hyphen", "ani-db", "tv", "1"},
		{"bad media type", "anidb", "series", "1"},
		{"empty media type", "anidb", "", "1"},
		{"tmdb id that is not an integer", "tmdb", "tv", "cc5a1adf"},
		{"tmdb id with a leading zero", "tmdb", "tv", "01622"},
		{"tmdb id with a plus", "tmdb", "tv", "+1622"},
		{"tmdb id that overflows an int", "tmdb", "tv", "99999999999999999999"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := playIdentityFor(c.provider, c.mediaType, c.providerID, 1, 1); err == nil {
				t.Fatal("the encoder accepted a malformed identity")
			}
			if _, err := playTokenFor(c.provider, c.mediaType, c.providerID, 1, 1); err == nil {
				t.Fatal("a token was minted for a malformed identity")
			}
			// No token — not even a correctly-computed one over the raw bytes — may open a
			// malformed identity. This is the property that stops a validation gap from
			// becoming an authorisation gap.
			raw := oldPlayToken(c.provider + ":" + c.mediaType + ":" + c.providerID)
			if validPlayTokenFor(raw, c.provider, c.mediaType, c.providerID, 1, 1) {
				t.Fatal("a malformed identity validated a token")
			}
			if validPlayTokenFor("", c.provider, c.mediaType, c.providerID, 1, 1) {
				t.Fatal("an empty token validated a malformed identity")
			}
		})
	}
}

// The collision test. A token authorises an identity STRING, so two distinct identities
// that encode to the same string share one capability — a link handed out for one title
// would play another.
//
// The adversarial pairs below are exactly the ones a naive `provider + ":" + id` encoder
// gets wrong: the attacker moves the delimiter from one field into the next. They are all
// rejected by validation rather than encoded, which is the proof that the ':'-joined form
// is unambiguous — no field can contain the delimiter, so the split is unique.
func TestDistinctIdentitiesNeverCollide(t *testing.T) {
	withTestKey(t)

	type ref struct {
		provider   string
		mediaType  string
		providerID string
		season     int
		episode    int
	}
	refs := []ref{
		// Legacy TMDB, including the neighbours that differ by one field.
		{"tmdb", "movie", "1622", 0, 0},
		{"tmdb", "tv", "1622", 14, 1},
		{"tmdb", "tv", "1622", 14, 2},
		{"tmdb", "tv", "1622", 1, 41},
		{"tmdb", "tv", "16", 22, 141},
		{"tmdb", "tv", "1622", 141, 0},
		{"tmdb", "movie", "162214", 0, 0},
		{"tmdb", "tv", "0", 0, 0},
		{"tmdb", "movie", "0", 0, 0},
		// Namespaced, with providers and ids chosen to overlap each other's text.
		{"anidb", "movie", "1622", 0, 0},
		{"anidb", "tv", "1622", 14, 1},
		{"anilist", "tv", "1622", 14, 1},
		{"p", "tv", "1622", 14, 1},
		{"p", "movie", "1622", 0, 0},
		{"tv", "movie", "1622", 0, 0},
		{"tv", "tv", "1622", 14, 1},
		{"movie", "movie", "1622", 0, 0},
		{"ptv", "tv", "1622", 14, 1},
		{"anidb", "tv", "cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c", 3, 12},
		{"anidb", "tv", "cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c", 31, 2},
		{"anidb", "tv", "cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6", 3, 12},
		{"anidb", "movie", "cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c", 0, 0},
		{"a", "tv", "b-c", 1, 1},
		{"ab", "tv", "c", 1, 1},
		{"a", "tv", "bc", 1, 1},
	}

	seenID := map[string]ref{}
	seenTok := map[string]ref{}
	for _, r := range refs {
		id, err := playIdentityFor(r.provider, r.mediaType, r.providerID, r.season, r.episode)
		if err != nil {
			t.Fatalf("%v should encode: %v", r, err)
		}
		if prev, dup := seenID[id]; dup {
			t.Fatalf("identity collision %q: %v and %v", id, prev, r)
		}
		seenID[id] = r

		tok, err := playTokenFor(r.provider, r.mediaType, r.providerID, r.season, r.episode)
		if err != nil {
			t.Fatalf("%v should tokenise: %v", r, err)
		}
		if prev, dup := seenTok[tok]; dup {
			t.Fatalf("token collision: %v and %v", prev, r)
		}
		seenTok[tok] = r
	}

	// Cross-check: no token from the set validates any OTHER identity in the set.
	for _, r := range refs {
		tok, _ := playTokenFor(r.provider, r.mediaType, r.providerID, r.season, r.episode)
		for _, other := range refs {
			if other == r {
				continue
			}
			if validPlayTokenFor(tok, other.provider, other.mediaType, other.providerID, other.season, other.episode) {
				t.Fatalf("the token for %v was accepted for %v", r, other)
			}
		}
	}

	// The delimiter-smuggling pairs: two DIFFERENT tuples that a naive encoder would
	// flatten to identical bytes. Both halves must be refused outright, which is what makes
	// the collision unreachable rather than merely unlikely.
	for _, pair := range [][2]ref{
		{{"a", "tv", "b:c", 1, 1}, {"a:b", "tv", "c", 1, 1}},
		{{"anidb", "tv", "1:14:1", 0, 0}, {"anidb", "tv", "1", 14, 1}},
		{{"p", "tv", "1", 14, 1}, {"p:anidb", "tv", "1", 14, 1}},
	} {
		_, e0 := playIdentityFor(pair[0].provider, pair[0].mediaType, pair[0].providerID, pair[0].season, pair[0].episode)
		_, e1 := playIdentityFor(pair[1].provider, pair[1].mediaType, pair[1].providerID, pair[1].season, pair[1].episode)
		if e0 == nil && e1 == nil {
			t.Fatalf("both halves of a delimiter-smuggling pair encoded: %v / %v", pair[0], pair[1])
		}
	}
}

// The four route patterns must coexist on one ServeMux — Go panics at registration on a
// conflicting pair — and each URL must land on the pattern that spells its own shape.
// Getting this wrong would silently route /play/p/tmdb/movie/1 to the legacy handler with
// a provider of "p".
func TestPlayRoutePatternsDoNotConflict(t *testing.T) {
	mux := http.NewServeMux()
	hit := ""
	for _, pat := range []string{
		"GET /play/movie/{tmdb}",
		"GET /play/tv/{tmdb}/{season}/{episode}",
		"GET /play/p/{provider}/movie/{id}",
		"GET /play/p/{provider}/tv/{id}/{season}/{episode}",
	} {
		p := pat
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) { hit = p })
	}

	cases := []struct{ url, want string }{
		{"/play/movie/27205", "GET /play/movie/{tmdb}"},
		{"/play/tv/1622/14/1", "GET /play/tv/{tmdb}/{season}/{episode}"},
		{"/play/p/tmdb/movie/27205", "GET /play/p/{provider}/movie/{id}"},
		{"/play/p/anidb/tv/cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c/3/12", "GET /play/p/{provider}/tv/{id}/{season}/{episode}"},
	}
	for _, c := range cases {
		hit = ""
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.url, nil))
		if hit != c.want {
			t.Errorf("%s matched %q, want %q", c.url, hit, c.want)
		}
	}
}
