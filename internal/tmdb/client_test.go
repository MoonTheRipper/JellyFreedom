package tmdb

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestClient points a Client at a local test server. This is what making baseURL a
// struct field (instead of a package constant) unblocked — TMDB was previously untestable.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("testkey")
	c.SetBaseURL(srv.URL)
	return c
}

func TestConfigured(t *testing.T) {
	c := New("")
	if c.Configured() {
		t.Error("a client with no key must not report configured")
	}
	c.Configure("abc123")
	if !c.Configured() {
		t.Error("a client with a key must report configured")
	}
}

func TestBaseURLDefaultsWhenUnset(t *testing.T) {
	c := New("k")
	if c.base() != DefaultBaseURL {
		t.Errorf("base() = %q, want %q", c.base(), DefaultBaseURL)
	}
	c.SetBaseURL("http://localhost:1234/")
	if got := c.base(); got != "http://localhost:1234" {
		t.Errorf("base() = %q, want the trailing slash trimmed", got)
	}
	c.SetBaseURL("")
	if c.base() != DefaultBaseURL {
		t.Errorf("an empty base URL must fall back to the default, got %q", c.base())
	}
}

func TestSearchParsesResults(t *testing.T) {
	var gotPath, gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("query")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"id": 27205, "media_type": "movie", "title": "Inception", "release_date": "2010-07-16",
					"poster_path": "/p.jpg", "overview": "A thief.", "vote_average": 8.4},
				{"id": 1399, "media_type": "tv", "name": "Game of Thrones", "first_air_date": "2011-04-17"},
				{"id": 99, "media_type": "person", "name": "Someone"},
			},
		})
	})

	results, err := c.Search("inception")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPath != "/search/multi" {
		t.Errorf("path = %q, want /search/multi", gotPath)
	}
	if gotQuery != "inception" {
		t.Errorf("query = %q", gotQuery)
	}
	// A person result is not playable media and must be dropped.
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (the person entry should be filtered out): %+v", len(results), results)
	}
	if results[0].Title != "Inception" || results[0].Year != "2010" || results[0].MediaType != "movie" {
		t.Errorf("movie result wrong: %+v", results[0])
	}
	if results[1].Title != "Game of Thrones" || results[1].Year != "2011" {
		t.Errorf("tv result wrong: %+v", results[1])
	}
}

func TestSearchReportsUpstreamFailure(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	if _, err := c.Search("x"); err == nil {
		t.Fatal("a 401 from TMDB must be an error")
	}
}

func TestDetailsMovieAndTV(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/movie/27205":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 27205, "title": "Inception", "release_date": "2010-07-16", "poster_path": "/p.jpg",
			})
		case "/tv/1399":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1399, "name": "Game of Thrones", "first_air_date": "2011-04-17", "status": "Ended",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	m, err := c.Details(27205, "movie")
	if err != nil {
		t.Fatalf("Details(movie): %v", err)
	}
	if m.Title != "Inception" || m.Year != "2010" {
		t.Errorf("movie details wrong: %+v", m)
	}

	tv, err := c.Details(1399, "tv")
	if err != nil {
		t.Fatalf("Details(tv): %v", err)
	}
	if tv.Title != "Game of Thrones" || tv.Year != "2011" {
		t.Errorf("tv details wrong: %+v", tv)
	}
	// "Ended" must not be reported as airing — this drives subscription retirement.
	if tv.IsAiring() {
		t.Error("a show with status=Ended must not report IsAiring")
	}
}

func TestIsAiring(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
	}{
		{"Returning Series", true},
		{"In Production", true},
		{"Ended", false},
		{"Canceled", false},
		{"", false},
	} {
		d := &Details{Status: tc.status}
		if got := d.IsAiring(); got != tc.want {
			t.Errorf("IsAiring(status=%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestTVEpisodes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"episodes": []map[string]any{
				{"episode_number": 1, "name": "Winter Is Coming", "air_date": "2011-04-17"},
				{"episode_number": 2, "name": "The Kingsroad", "air_date": "2011-04-24"},
			},
		})
	})
	eps, err := c.TVEpisodes(1399, 1)
	if err != nil {
		t.Fatalf("TVEpisodes: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("got %d episodes, want 2", len(eps))
	}
	if eps[0].Number != 1 || eps[0].AirDate != "2011-04-17" {
		t.Errorf("episode 1 wrong: %+v", eps[0])
	}
}

func TestYearHelper(t *testing.T) {
	cases := map[string]string{
		"2010-07-16": "2010",
		"2011":       "2011",
		"":           "",
		// Anything shorter than four characters yields "" rather than a partial year.
		"bad": "",
	}
	for in, want := range cases {
		if got := year(in); got != want {
			t.Errorf("year(%q) = %q, want %q", in, got, want)
		}
	}
}
