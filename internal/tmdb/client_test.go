package tmdb

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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

// ── Genre vocabulary ──────────────────────────────────────────────────────────

func TestGenresParsesAndCaches(t *testing.T) {
	var hits int32
	var gotPaths []string
	var mu sync.Mutex
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		mu.Lock()
		gotPaths = append(gotPaths, r.URL.Path)
		mu.Unlock()
		name := "Action"
		if r.URL.Path == "/genre/tv/list" {
			name = "Animation"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"genres": []map[string]any{
				{"id": 28, "name": name},
				{"id": 0, "name": "no id"}, // must be dropped
				{"id": 12, "name": ""},     // must be dropped
			},
		})
	})

	got, err := c.Genres("movie")
	if err != nil {
		t.Fatalf("Genres(movie): %v", err)
	}
	if len(got) != 1 || got[0].ID != 28 || got[0].Name != "Action" {
		t.Fatalf("movie genres = %+v, want just {28 Action}", got)
	}

	// The whole point of the cache: a second call must be served in-process. If this
	// ever regresses, every browse page load pays a TMDB round trip again.
	if _, err := c.Genres("movie"); err != nil {
		t.Fatalf("second Genres(movie): %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("TMDB was hit %d times for two identical calls, want 1", n)
	}

	// The cache is per media type — a cached movie list must not answer a TV request.
	tv, err := c.Genres("tv")
	if err != nil {
		t.Fatalf("Genres(tv): %v", err)
	}
	if len(tv) != 1 || tv[0].Name != "Animation" {
		t.Errorf("tv genres = %+v, want the TV list, not the cached movie one", tv)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("TMDB hit %d times, want 2 (one per media type)", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotPaths) != 2 || gotPaths[0] != "/genre/movie/list" || gotPaths[1] != "/genre/tv/list" {
		t.Errorf("paths = %v, want [/genre/movie/list /genre/tv/list]", gotPaths)
	}
}

func TestGenresReturnedSliceIsACopy(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"genres": []map[string]any{{"id": 28, "name": "Action"}},
		})
	})
	first, err := c.Genres("movie")
	if err != nil {
		t.Fatalf("Genres: %v", err)
	}
	first[0].Name = "MUTATED"
	second, err := c.Genres("movie")
	if err != nil {
		t.Fatalf("Genres: %v", err)
	}
	if second[0].Name != "Action" {
		t.Errorf("a caller mutating its slice corrupted the shared cache: %+v", second)
	}
}

func TestGenresDoesNotCacheFailures(t *testing.T) {
	var hits int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"genres": []map[string]any{{"id": 28, "name": "Action"}},
		})
	})
	if _, err := c.Genres("movie"); err == nil {
		t.Fatal("a 500 from TMDB must be an error")
	}
	// A blip must not poison the cache for a day.
	got, err := c.Genres("movie")
	if err != nil {
		t.Fatalf("retry after a failure: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("retry returned %+v", got)
	}
}

func TestGenresRejectsUnknownMediaTypeWithoutCallingTMDB(t *testing.T) {
	var hits int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	})
	if _, err := c.Genres("person"); err == nil {
		t.Fatal("an unknown media type must be rejected")
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Error("an unknown media type must not reach TMDB at all")
	}
}

func TestGenresEmptyListIsAnErrorNotACachedEmptyVocabulary(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"genres": []map[string]any{}})
	})
	if _, err := c.Genres("movie"); err == nil {
		t.Fatal("an empty genre list is a TMDB fault and must be an error")
	}
}

// ── Studio lookup ─────────────────────────────────────────────────────────────

func TestSearchCompanies(t *testing.T) {
	var gotPath, gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query().Get("query")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"id": 420, "name": "Marvel Studios", "logo_path": "/m.png"},
				{"id": 4, "name": "Paramount"},
				{"id": 0, "name": "bogus"}, // must be dropped
			},
		})
	})
	got, err := c.SearchCompanies("  marvel  ")
	if err != nil {
		t.Fatalf("SearchCompanies: %v", err)
	}
	if gotPath != "/search/company" {
		t.Errorf("path = %q, want /search/company", gotPath)
	}
	if gotQuery != "marvel" {
		t.Errorf("query = %q, want the trimmed term", gotQuery)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v, want 2 studios", got)
	}
	if got[0].ID != 420 || got[0].LogoURL != "https://image.tmdb.org/t/p/w92/m.png" {
		t.Errorf("studio 0 wrong: %+v", got[0])
	}
	if got[1].LogoURL != "" {
		t.Errorf("a company with no logo_path must have no logo_url: %+v", got[1])
	}
}

func TestSearchCompaniesRejectsEmptyQuery(t *testing.T) {
	var hits int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	})
	if _, err := c.SearchCompanies("   "); err == nil {
		t.Fatal("a blank company query must be rejected")
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Error("a blank query must not reach TMDB")
	}
}

// ── Browse filter allowlist ───────────────────────────────────────────────────

// TestDiscoverParamsIsAnAllowlist is the security test for the unauthenticated browse
// route: anything the caller sends that is not an understood filter must be DROPPED, not
// forwarded. api_key is the one that matters most — forwarding it would let an anonymous
// caller run TMDB queries against an account of their choosing through this instance.
func TestDiscoverParamsIsAnAllowlist(t *testing.T) {
	q := url.Values{
		"genres":               {"28"},
		"api_key":              {"attacker-key"},
		"language":             {"ru-RU"},
		"region":               {"RU"},
		"with_watch_providers": {"8"},
		"certification":        {"NC-17"},
		"sort_by":              {"revenue.asc"}, // the TMDB spelling, not our `sort`
		"include_adult":        {"true"},
	}
	params, err := DiscoverParams("movie", q)
	if err != nil {
		t.Fatalf("DiscoverParams: %v", err)
	}
	for _, forbidden := range []string{"api_key", "language", "region", "with_watch_providers", "certification"} {
		if v, ok := params[forbidden]; ok {
			t.Errorf("caller-supplied %q was forwarded to TMDB as %q", forbidden, v)
		}
	}
	// include_adult is ours, not the caller's: a raw `sort_by`/`include_adult` in the
	// query must not be able to overwrite what we set.
	if params["include_adult"] != "false" {
		t.Errorf("include_adult = %q, want the value we control", params["include_adult"])
	}
	if params["sort_by"] != "popularity.desc" {
		t.Errorf("sort_by = %q, want our default — a raw sort_by must not be honoured", params["sort_by"])
	}
	for k, v := range params {
		for _, c := range v {
			if c == '&' || c == '?' || c == '=' {
				t.Errorf("param %q value %q carries a query separator", k, v)
			}
		}
	}
}

// TestDiscoverParamsYearAsymmetry pins the trap: the year filter has a DIFFERENT
// parameter name per media type, and TMDB silently ignores the wrong one instead of
// erroring, so a regression here looks like "the year filter does nothing".
func TestDiscoverParamsYearAsymmetry(t *testing.T) {
	for _, tc := range []struct {
		mediaType string
		year      string
		wantKey   string
		wantGone  string
		wantErr   bool
	}{
		{mediaType: "movie", year: "1999", wantKey: "primary_release_year", wantGone: "first_air_date_year"},
		{mediaType: "tv", year: "1999", wantKey: "first_air_date_year", wantGone: "primary_release_year"},
		{mediaType: "movie", year: "1887", wantErr: true},  // before the first film
		{mediaType: "movie", year: "2101", wantErr: true},  // absurdly far ahead
		{mediaType: "tv", year: "nineteen", wantErr: true}, // not a number at all
		{mediaType: "tv", year: "19 99", wantErr: true},
		{mediaType: "movie", year: "-1999", wantErr: true},
	} {
		params, err := DiscoverParams(tc.mediaType, url.Values{"year": {tc.year}})
		if tc.wantErr {
			if err == nil {
				t.Errorf("year=%q type=%s: want an error, got %+v", tc.year, tc.mediaType, params)
			}
			continue
		}
		if err != nil {
			t.Errorf("year=%q type=%s: %v", tc.year, tc.mediaType, err)
			continue
		}
		if params[tc.wantKey] != tc.year {
			t.Errorf("type=%s: %s = %q, want %q", tc.mediaType, tc.wantKey, params[tc.wantKey], tc.year)
		}
		if _, ok := params[tc.wantGone]; ok {
			t.Errorf("type=%s: the other media type's year parameter (%s) was also sent", tc.mediaType, tc.wantGone)
		}
	}
}

func TestDiscoverParamsSortAllowlist(t *testing.T) {
	for _, tc := range []struct {
		mediaType string
		sort      string
		want      string
		wantErr   bool
	}{
		{mediaType: "movie", sort: "popularity.desc", want: "popularity.desc"},
		{mediaType: "movie", sort: "vote_average.desc", want: "vote_average.desc"},
		{mediaType: "movie", sort: "revenue.desc", want: "revenue.desc"},
		{mediaType: "movie", sort: "primary_release_date.desc", want: "primary_release_date.desc"},
		// TV has no primary_release_date; TMDB calls it first_air_date and 422s on the
		// movie spelling.
		{mediaType: "tv", sort: "primary_release_date.desc", want: "first_air_date.desc"},
		// TV has no revenue at all, so there is nothing honest to translate it into.
		{mediaType: "tv", sort: "revenue.desc", wantErr: true},
		// Off the allowlist, however plausible it looks.
		{mediaType: "movie", sort: "popularity.asc", wantErr: true},
		{mediaType: "movie", sort: "release_date.desc", wantErr: true},
		{mediaType: "movie", sort: "popularity.desc&api_key=x", wantErr: true},
		{mediaType: "movie", sort: "POPULARITY.DESC", wantErr: true},
	} {
		params, err := DiscoverParams(tc.mediaType, url.Values{"sort": {tc.sort}})
		if tc.wantErr {
			if err == nil {
				t.Errorf("sort=%q type=%s: want an error, got sort_by=%q", tc.sort, tc.mediaType, params["sort_by"])
			}
			continue
		}
		if err != nil {
			t.Errorf("sort=%q type=%s: %v", tc.sort, tc.mediaType, err)
			continue
		}
		if params["sort_by"] != tc.want {
			t.Errorf("sort=%q type=%s: sort_by = %q, want %q", tc.sort, tc.mediaType, params["sort_by"], tc.want)
		}
	}
}

func TestDiscoverParamsBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		key     string
		in      string
		wantKey string
		want    string
		wantErr bool
	}{
		{name: "page ok", key: "page", in: "3", wantKey: "page", want: "3"},
		{name: "page one", key: "page", in: "1", wantKey: "page", want: "1"},
		{name: "page max", key: "page", in: "500", wantKey: "page", want: "500"},
		{name: "page past TMDB's ceiling", key: "page", in: "501", wantErr: true},
		{name: "page zero", key: "page", in: "0", wantErr: true},
		{name: "page negative", key: "page", in: "-1", wantErr: true},
		{name: "page not a number", key: "page", in: "2; DROP", wantErr: true},
		{name: "page huge", key: "page", in: "99999999999999999999", wantErr: true},
		{name: "min_votes ok", key: "min_votes", in: "500", wantKey: "vote_count.gte", want: "500"},
		{name: "min_votes zero is allowed", key: "min_votes", in: "0", wantKey: "vote_count.gte", want: "0"},
		{name: "min_votes too big", key: "min_votes", in: "100001", wantErr: true},
		{name: "min_votes negative", key: "min_votes", in: "-5", wantErr: true},
		{name: "min_votes float", key: "min_votes", in: "1.5", wantErr: true},
	} {
		params, err := DiscoverParams("movie", url.Values{tc.key: {tc.in}})
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: %s=%q must be rejected, got %+v", tc.name, tc.key, tc.in, params)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if params[tc.wantKey] != tc.want {
			t.Errorf("%s: %s = %q, want %q", tc.name, tc.wantKey, params[tc.wantKey], tc.want)
		}
	}
}

// TestDiscoverParamsGenreJoin pins TMDB's separator semantics, which are the easiest
// thing in this file to get backwards: "," is AND, "|" is OR.
func TestDiscoverParamsGenreJoin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		q       url.Values
		want    string
		wantErr bool
	}{
		{name: "default is OR", q: url.Values{"genres": {"28,12"}}, want: "28|12"},
		{name: "match=any is OR", q: url.Values{"genres": {"28,12"}, "match": {"any"}}, want: "28|12"},
		{name: "match=all is AND", q: url.Values{"genres": {"28,12"}, "match": {"all"}}, want: "28,12"},
		{name: "whitespace tolerated", q: url.Values{"genres": {" 28 , 12 "}}, want: "28|12"},
		{name: "single id", q: url.Values{"genres": {"27"}}, want: "27"},
		{name: "unknown match mode", q: url.Values{"genres": {"28"}, "match": {"either"}}, wantErr: true},
		{name: "non-numeric id", q: url.Values{"genres": {"28,drama"}}, wantErr: true},
		{name: "injected separator", q: url.Values{"genres": {"28&api_key=x"}}, wantErr: true},
		{name: "caller-supplied pipe", q: url.Values{"genres": {"28|12"}}, wantErr: true},
		{name: "empty element", q: url.Values{"genres": {"28,"}}, wantErr: true},
		{name: "too many ids", q: url.Values{"genres": {strings.Repeat("28,", 20) + "28"}}, wantErr: true},
	} {
		params, err := DiscoverParams("movie", tc.q)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: want an error, got with_genres=%q", tc.name, params["with_genres"])
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if params["with_genres"] != tc.want {
			t.Errorf("%s: with_genres = %q, want %q", tc.name, params["with_genres"], tc.want)
		}
	}
}

func TestDiscoverParamsStudiosAndNetworks(t *testing.T) {
	params, err := DiscoverParams("movie", url.Values{"studios": {"128064,9993"}})
	if err != nil {
		t.Fatalf("studios: %v", err)
	}
	// Two studios means "either studio". AND would read as a broken filter.
	if params["with_companies"] != "128064|9993" {
		t.Errorf("with_companies = %q, want the OR join", params["with_companies"])
	}
	// The homepage carousels were built against the older `companies=` spelling.
	legacy, err := DiscoverParams("movie", url.Values{"companies": {"420"}})
	if err != nil {
		t.Fatalf("companies: %v", err)
	}
	if legacy["with_companies"] != "420" {
		t.Errorf("legacy companies= not honoured: %+v", legacy)
	}
	tv, err := DiscoverParams("tv", url.Values{"networks": {"213"}})
	if err != nil {
		t.Fatalf("networks: %v", err)
	}
	if tv["with_networks"] != "213" {
		t.Errorf("with_networks = %q", tv["with_networks"])
	}
	// with_networks is TV-only; TMDB would ignore it on /discover/movie and hand back an
	// unfiltered list, so it is refused rather than silently dropped.
	if _, err := DiscoverParams("movie", url.Values{"networks": {"213"}}); err == nil {
		t.Error("networks on type=movie must be an error")
	}
	if _, err := DiscoverParams("person", url.Values{}); err == nil {
		t.Error("an unknown media type must be an error")
	}
}

// TestDiscoverParamsRatingSortGetsAVoteFloor covers why min_votes exists: sorting by
// rating with no floor returns obscure titles with a single 10/10 vote.
func TestDiscoverParamsRatingSortGetsAVoteFloor(t *testing.T) {
	params, err := DiscoverParams("movie", url.Values{"sort": {"vote_average.desc"}})
	if err != nil {
		t.Fatalf("DiscoverParams: %v", err)
	}
	if params["vote_count.gte"] == "" || params["vote_count.gte"] == "0" {
		t.Errorf("vote_average.desc without min_votes must get a default floor, got %q", params["vote_count.gte"])
	}
	// An explicit value wins, including a deliberate 0.
	params, err = DiscoverParams("movie", url.Values{"sort": {"vote_average.desc"}, "min_votes": {"0"}})
	if err != nil {
		t.Fatalf("DiscoverParams: %v", err)
	}
	if params["vote_count.gte"] != "0" {
		t.Errorf("an explicit min_votes=0 must win, got %q", params["vote_count.gte"])
	}
	// The floor is only for rating sorts — it must not quietly narrow a popularity list.
	params, err = DiscoverParams("movie", url.Values{"sort": {"popularity.desc"}})
	if err != nil {
		t.Fatalf("DiscoverParams: %v", err)
	}
	if _, ok := params["vote_count.gte"]; ok {
		t.Errorf("popularity.desc must not get a vote floor: %+v", params)
	}
}

// TestDiscoverEscapesParamValues covers the old string-concatenation bug: values were
// appended raw, so a "&" in one turned into an extra outbound parameter, and the "|" an
// OR-ed genre list needs never survived the trip.
func TestDiscoverEscapesParamValues(t *testing.T) {
	var got url.Values
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{}})
	})
	if _, err := c.Discover("movie", map[string]string{
		"with_genres": "28|12",
		"evil":        "1&api_key=attacker",
	}); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.Get("with_genres") != "28|12" {
		t.Errorf("with_genres arrived as %q, want the literal OR list", got.Get("with_genres"))
	}
	if got.Get("api_key") != "testkey" {
		t.Errorf("api_key = %q — a value containing & injected a parameter", got.Get("api_key"))
	}
	if got.Get("evil") != "1&api_key=attacker" {
		t.Errorf("evil = %q, want it escaped into a single value", got.Get("evil"))
	}
}
