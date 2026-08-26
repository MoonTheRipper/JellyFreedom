package library

import (
	"fmt"
	"strings"
	"testing"
)

// legacyPlayURL is the URL builder exactly as it stood before a second provider existed,
// copied here verbatim.
//
// It is the prior art this package's TMDB output is diffed against. ~1,000 .strm files on
// the live install contain a string this function produced, and each carries an HMAC over
// the identity its own path spells — so one changed character is a 403 on every title at
// once. Copying the old code rather than calling the new code is the whole point: a test
// that asks the new implementation whether it agrees with itself proves nothing.
func legacyPlayURL(publicURL, mediaType string, tmdbID, season, episode int, tok string) string {
	base := strings.TrimRight(publicURL, "/")
	var path string
	if mediaType == "movie" {
		path = fmt.Sprintf("%s/play/movie/%d", base, tmdbID)
	} else {
		path = fmt.Sprintf("%s/play/tv/%d/%d/%d", base, tmdbID, season, episode)
	}
	if tok != "" {
		path += "?t=" + tok
	}
	return path
}

func TestTMDBPlayURLsAreByteIdenticalToThePriorArt(t *testing.T) {
	const tok = "Zm9vYmFyYmF6"
	cases := []struct {
		mediaType        string
		tmdb, season, ep int
		token            string
	}{
		{"movie", 27205, 0, 0, tok},
		{"movie", 27205, 0, 0, ""}, // no key loaded yet — no "?t=" at all
		{"tv", 1622, 14, 1, tok},
		{"tv", 76479, 1, 6, tok},
		{"tv", 1399, 2, 9, ""},
		{"tv", 1, 0, 0, tok},
		{"movie", 0, 0, 0, tok},
		{"tv", 999999, 100, 900, tok},
	}
	for _, base := range []string{"http://192.168.178.2:1990", "http://host:1990/", "https://x/y/"} {
		for _, c := range cases {
			want := legacyPlayURL(base, c.mediaType, c.tmdb, c.season, c.ep, c.token)
			got, err := PlayURL(base, ProviderTMDB, c.mediaType, fmt.Sprint(c.tmdb), c.season, c.ep, c.token)
			if err != nil {
				t.Fatalf("PlayURL(%s, %v): %v", base, c, err)
			}
			if got != want {
				t.Errorf("PlayURL = %q, want the legacy %q", got, want)
			}
		}
	}
}

// The exact paths found in the live library, pinned as literals so that a future edit to
// the shape has to change a test that says why it must not.
func TestPlayPathsArePinned(t *testing.T) {
	cases := []struct {
		provider, mediaType, id string
		season, episode         int
		want                    string
	}{
		{"tmdb", "tv", "1622", 14, 1, "/play/tv/1622/14/1"},
		{"tmdb", "movie", "27205", 0, 0, "/play/movie/27205"},
		{"tmdb", "tv", "76479", 4, 2, "/play/tv/76479/4/2"},
		{"anidb", "tv", "cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c", 3, 12,
			"/play/p/anidb/tv/cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c/3/12"},
		{"anidb", "movie", "cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c", 0, 0,
			"/play/p/anidb/movie/cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c"},
	}
	for _, c := range cases {
		got, err := PlayPath(c.provider, c.mediaType, c.id, c.season, c.episode)
		if err != nil {
			t.Fatalf("PlayPath(%v): %v", c, err)
		}
		if got != c.want {
			t.Errorf("PlayPath(%v) = %q, want %q", c, got, c.want)
		}
	}
}

// Everything the validator must refuse. Each of these would otherwise reach a URL path and
// an HMAC input; the ':' and the path-separator cases are the ones that would be
// exploitable rather than merely wrong.
func TestPlayRefValidationRejectsHostileFields(t *testing.T) {
	long := strings.Repeat("a", MaxProviderIDLen+1)
	cases := []struct{ name, provider, mediaType, id string }{
		{"colon in provider", "an:idb", "tv", "1"},
		{"colon in id", "anidb", "tv", "1:1"},
		{"slash in provider", "an/idb", "tv", "1"},
		{"slash in id", "anidb", "tv", "a/b"},
		{"traversal", "anidb", "tv", ".."},
		{"traversal path", "anidb", "tv", "../../etc/passwd"},
		{"dot in id", "anidb", "tv", "1.2"},
		{"percent in id", "anidb", "tv", "%2e%2e"},
		{"question mark in id", "anidb", "tv", "1?t=x"},
		{"hash in id", "anidb", "tv", "1#x"},
		{"space in id", "anidb", "tv", "a b"},
		{"nul in id", "anidb", "tv", "a\x00b"},
		{"newline in id", "anidb", "tv", "a\nb"},
		{"empty provider", "", "tv", "1"},
		{"empty id", "anidb", "tv", ""},
		{"overlong id", "anidb", "tv", long},
		{"overlong provider", strings.Repeat("p", MaxProviderLen+1), "tv", "1"},
		{"uppercase provider", "AniDB", "tv", "1"},
		{"underscore in provider", "ani_db", "tv", "1"},
		{"bad media type", "anidb", "episode", "1"},
		{"empty media type", "anidb", "", "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidPlayRef(c.provider, c.mediaType, c.id, 1, 1); err == nil {
				t.Fatal("ValidPlayRef accepted it")
			}
			if _, err := PlayPath(c.provider, c.mediaType, c.id, 1, 1); err == nil {
				t.Fatal("PlayPath built a path for it")
			}
			if _, err := PlayURL("http://h", c.provider, c.mediaType, c.id, 1, 1, "tok"); err == nil {
				t.Fatal("PlayURL built a URL for it")
			}
		})
	}
}

// The shapes that must be accepted: TMDB integers (including the negative and zero values
// the legacy encoder could produce), and UUIDs.
func TestPlayRefValidationAcceptsRealIdentities(t *testing.T) {
	for _, c := range []struct{ provider, mediaType, id string }{
		{"tmdb", "tv", "1622"},
		{"tmdb", "movie", "0"},
		{"tmdb", "movie", "-5"},
		{"tmdb", "movie", "2147483647"},
		{"anidb", "tv", "cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c"},
		{"anilist2", "movie", "AB_cd-12"},
		{"a", "tv", "a"},
		{"anidb", "tv", strings.Repeat("a", MaxProviderIDLen)},
		{strings.Repeat("p", MaxProviderLen), "tv", "1"},
	} {
		if err := ValidPlayRef(c.provider, c.mediaType, c.id, 0, 0); err != nil {
			t.Errorf("ValidPlayRef(%v) = %v, want nil", c, err)
		}
	}
}

// No accepted field may need percent-encoding to sit in a path segment. If it did, the
// path the router decodes could differ from the string the token signed.
func TestAcceptedFieldsNeedNoEscaping(t *testing.T) {
	for _, s := range []string{
		"cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c", "1622", "-5", "AB_cd-12",
		strings.Repeat("a", MaxProviderIDLen),
	} {
		if !ValidProviderID(s) {
			t.Fatalf("%q should be a valid id", s)
		}
		for _, bad := range []string{"/", ":", "?", "#", "%", ".", " ", "\\"} {
			if strings.Contains(s, bad) {
				t.Errorf("accepted id %q contains %q", s, bad)
			}
		}
	}
}
