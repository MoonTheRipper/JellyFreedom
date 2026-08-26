package tmdb

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"jellyfreedom/internal/redact"
)

// DefaultBaseURL is the public TMDB API root. It is a per-Client field rather than a
// package constant so tests can point a Client at an httptest server.
const DefaultBaseURL = "https://api.themoviedb.org/3"

type Client struct {
	mu         sync.RWMutex
	apiKey     string
	baseURL    string
	httpClient *http.Client

	// genreMu guards genreCache only. It is deliberately NOT mu: mu is taken on every
	// single request to read the key and the base URL, and the genre cache is written
	// rarely and read from a different code path, so sharing one lock would put genre
	// refreshes in the way of unrelated traffic for no benefit.
	genreMu    sync.Mutex
	genreCache map[string]cachedGenres
}

func New(apiKey string) *Client {
	// The key travels as a TMDB query parameter (TMDB v3 has no header form for it), so
	// it WILL end up inside any *url.Error this client produces. Registering it with the
	// redaction backstop the moment the client learns it is what stops a wrapped error
	// from carrying it into a log line or a response body — the Prowlarr incident that
	// package documents started exactly this way.
	redact.Register(apiKey)
	return &Client{
		apiKey:     apiKey,
		baseURL:    DefaultBaseURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		genreCache: map[string]cachedGenres{},
	}
}

// SetBaseURL overrides the API root (tests only — production always uses DefaultBaseURL).
func (c *Client) SetBaseURL(u string) {
	c.mu.Lock()
	c.baseURL = strings.TrimRight(u, "/")
	c.mu.Unlock()
}

// base returns the current API root under a read lock.
func (c *Client) base() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.baseURL == "" {
		return DefaultBaseURL
	}
	return c.baseURL
}

// Configure updates the API key at runtime under a write lock.
//
// The new key is registered with redact for the same reason New does it: the Settings UI
// can swap the key while the process runs, and a key that was never registered is a key
// the backstop cannot scrub.
func (c *Client) Configure(apiKey string) {
	redact.Register(apiKey)
	c.mu.Lock()
	c.apiKey = apiKey
	c.mu.Unlock()
}

// Configured reports whether an API key is set.
func (c *Client) Configured() bool {
	return c.key() != ""
}

// key returns the current API key under a read lock.
func (c *Client) key() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.apiKey
}

type Result struct {
	TMDBID    int    `json:"tmdb_id"`
	MediaType string `json:"media_type"` // "movie" | "tv"
	Title     string `json:"title"`
	Year      string `json:"year"`
	Overview  string `json:"overview"`
	PosterURL string `json:"poster_url,omitempty"`
}

type searchResponse struct {
	Results []struct {
		ID           int    `json:"id"`
		MediaType    string `json:"media_type"`
		Title        string `json:"title"`          // movie
		Name         string `json:"name"`           // tv
		ReleaseDate  string `json:"release_date"`   // movie
		FirstAirDate string `json:"first_air_date"` // tv
		Overview     string `json:"overview"`
		PosterPath   string `json:"poster_path"`
	} `json:"results"`
}

func (c *Client) Search(query string) ([]Result, error) {
	u := fmt.Sprintf("%s/search/multi?api_key=%s&query=%s&include_adult=false",
		c.base(), c.key(), url.QueryEscape(query))

	resp, err := c.httpClient.Get(u)
	if err != nil {
		return nil, fmt.Errorf("tmdb search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb search: status %d", resp.StatusCode)
	}

	var sr searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, fmt.Errorf("tmdb search decode: %w", err)
	}

	var out []Result
	for _, r := range sr.Results {
		if r.MediaType != "movie" && r.MediaType != "tv" {
			continue
		}
		res := Result{
			TMDBID:    r.ID,
			MediaType: r.MediaType,
			Overview:  r.Overview,
		}
		if r.MediaType == "movie" {
			res.Title = r.Title
			res.Year = year(r.ReleaseDate)
		} else {
			res.Title = r.Name
			res.Year = year(r.FirstAirDate)
		}
		if r.PosterPath != "" {
			res.PosterURL = "https://image.tmdb.org/t/p/w342" + r.PosterPath
		}
		out = append(out, res)
	}
	return out, nil
}

type Details struct {
	TMDBID    int    `json:"tmdb_id"`
	MediaType string `json:"media_type"`
	Title     string `json:"title"`
	Year      string `json:"year"`
	Overview  string `json:"overview"`
	PosterURL string `json:"poster_url,omitempty"`
	Status    string `json:"status,omitempty"` // TV: "Returning Series" | "Ended" | "Canceled" | "In Production"
	// RuntimeMinutes is the episode/feature length, 0 when TMDB does not know it. The
	// picker divides size by this to rank a release on BITRATE rather than raw size,
	// which is the metric that actually decides whether a torrent streams: a 60GB remux
	// and a 4GB WEB-DL of the same film are 65Mbps and 4.5Mbps, and only one of them
	// feeds a bounded ring buffer over a VPN. A max-size cap cannot express that.
	// For TV this is episode_run_time[0] — TMDB reports it per show, not per episode.
	RuntimeMinutes int `json:"runtime_minutes,omitempty"`
}

// IsAiring reports whether a TV show is still producing new episodes.
func (d *Details) IsAiring() bool {
	switch d.Status {
	case "Returning Series", "In Production", "Planned", "Pilot":
		return true
	}
	return false
}

type movieDetails struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	ReleaseDate string `json:"release_date"`
	Overview    string `json:"overview"`
	PosterPath  string `json:"poster_path"`
	Runtime     int    `json:"runtime"`
}

type tvDetails struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	FirstAirDate string `json:"first_air_date"`
	Overview     string `json:"overview"`
	PosterPath   string `json:"poster_path"`
	Status       string `json:"status"`
	// TMDB returns a LIST here because some shows mix lengths (a 22-minute sitcom with
	// 44-minute specials). The first entry is the usual length, which is what a bitrate
	// estimate wants; an empty list simply leaves runtime unknown.
	EpisodeRunTime []int `json:"episode_run_time"`
}

func (c *Client) Details(tmdbID int, mediaType string) (*Details, error) {
	var endpoint string
	if mediaType == "movie" {
		endpoint = fmt.Sprintf("%s/movie/%d?api_key=%s", c.base(), tmdbID, c.key())
	} else {
		endpoint = fmt.Sprintf("%s/tv/%d?api_key=%s", c.base(), tmdbID, c.key())
	}

	resp, err := c.httpClient.Get(endpoint)
	if err != nil {
		return nil, fmt.Errorf("tmdb details: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb details: status %d", resp.StatusCode)
	}

	d := &Details{TMDBID: tmdbID, MediaType: mediaType}
	if mediaType == "movie" {
		var m movieDetails
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			return nil, fmt.Errorf("tmdb details decode: %w", err)
		}
		d.Title = m.Title
		d.Year = year(m.ReleaseDate)
		d.Overview = m.Overview
		d.RuntimeMinutes = m.Runtime
		if m.PosterPath != "" {
			d.PosterURL = "https://image.tmdb.org/t/p/w342" + m.PosterPath
		}
	} else {
		var t tvDetails
		if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
			return nil, fmt.Errorf("tmdb details decode: %w", err)
		}
		d.Title = t.Name
		d.Year = year(t.FirstAirDate)
		d.Overview = t.Overview
		d.Status = t.Status
		if len(t.EpisodeRunTime) > 0 {
			d.RuntimeMinutes = t.EpisodeRunTime[0]
		}
		if t.PosterPath != "" {
			d.PosterURL = "https://image.tmdb.org/t/p/w342" + t.PosterPath
		}
	}
	return d, nil
}

type Season struct {
	Number       int    `json:"season_number"`
	Name         string `json:"name"`
	EpisodeCount int    `json:"episode_count"`
	AirDate      string `json:"air_date"`
	PosterURL    string `json:"poster_url,omitempty"`
}

type Episode struct {
	Number   int    `json:"episode_number"`
	Name     string `json:"name"`
	AirDate  string `json:"air_date"`
	Overview string `json:"overview"`
}

type tvFull struct {
	Seasons []struct {
		Number       int    `json:"season_number"`
		Name         string `json:"name"`
		EpisodeCount int    `json:"episode_count"`
		AirDate      string `json:"air_date"`
		PosterPath   string `json:"poster_path"`
	} `json:"seasons"`
}

type seasonFull struct {
	Episodes []struct {
		Number   int    `json:"episode_number"`
		Name     string `json:"name"`
		AirDate  string `json:"air_date"`
		Overview string `json:"overview"`
	} `json:"episodes"`
}

func (c *Client) TVSeasons(tmdbID int) ([]Season, error) {
	url := fmt.Sprintf("%s/tv/%d?api_key=%s", c.base(), tmdbID, c.key())
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("tmdb tv seasons: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb tv seasons: status %d", resp.StatusCode)
	}
	var full tvFull
	if err := json.NewDecoder(resp.Body).Decode(&full); err != nil {
		return nil, err
	}
	var out []Season
	for _, s := range full.Seasons {
		if s.Number == 0 {
			continue // skip specials season
		}
		season := Season{
			Number:       s.Number,
			Name:         s.Name,
			EpisodeCount: s.EpisodeCount,
			AirDate:      s.AirDate,
		}
		if s.PosterPath != "" {
			season.PosterURL = "https://image.tmdb.org/t/p/w342" + s.PosterPath
		}
		out = append(out, season)
	}
	return out, nil
}

func (c *Client) TVEpisodes(tmdbID, seasonNum int) ([]Episode, error) {
	url := fmt.Sprintf("%s/tv/%d/season/%d?api_key=%s", c.base(), tmdbID, seasonNum, c.key())
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("tmdb tv episodes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb tv episodes: status %d", resp.StatusCode)
	}
	var full seasonFull
	if err := json.NewDecoder(resp.Body).Decode(&full); err != nil {
		return nil, err
	}
	var out []Episode
	for _, e := range full.Episodes {
		out = append(out, Episode{
			Number:   e.Number,
			Name:     e.Name,
			AirDate:  e.AirDate,
			Overview: e.Overview,
		})
	}
	return out, nil
}

// ── Rich details ─────────────────────────────────────────────────────────────

type RichDetails struct {
	TMDBID         int             `json:"tmdb_id"`
	MediaType      string          `json:"media_type"`
	Title          string          `json:"title"`
	Year           string          `json:"year"`
	Overview       string          `json:"overview"`
	Tagline        string          `json:"tagline,omitempty"`
	BackdropURL    string          `json:"backdrop_url,omitempty"`
	PosterURL      string          `json:"poster_url,omitempty"`
	VoteAverage    float64         `json:"vote_average"`
	RuntimeMinutes int             `json:"runtime_minutes"`
	Genres         []string        `json:"genres"`
	Cast           []CastMember    `json:"cast"`
	Collection     *CollectionInfo `json:"collection,omitempty"`
	Similar        []SimilarItem   `json:"similar"`
	DigitalRelease string          `json:"digital_release,omitempty"`
	Status         string          `json:"status,omitempty"` // TV airing status
	IsAiring       bool            `json:"is_airing"`        // TV: still producing episodes
}

type CastMember struct {
	Name       string `json:"name"`
	Character  string `json:"character"`
	ProfileURL string `json:"profile_url,omitempty"`
}

type CollectionInfo struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	PosterURL string `json:"poster_url,omitempty"`
}

type SimilarItem struct {
	TMDBID    int    `json:"tmdb_id"`
	MediaType string `json:"media_type"`
	Title     string `json:"title"`
	Year      string `json:"year"`
	PosterURL string `json:"poster_url,omitempty"`
	Group     string `json:"group,omitempty"` // section label: franchise > "More Like This" > cast
}

// movieList / tvList decode TMDB result arrays (similar, recommendations, collection parts).
type movieList struct {
	Results []struct {
		ID          int    `json:"id"`
		Title       string `json:"title"`
		PosterPath  string `json:"poster_path"`
		ReleaseDate string `json:"release_date"`
	} `json:"results"`
}
type tvList struct {
	Results []struct {
		ID           int    `json:"id"`
		Name         string `json:"name"`
		PosterPath   string `json:"poster_path"`
		FirstAirDate string `json:"first_air_date"`
	} `json:"results"`
}

func (l movieList) items(group string) []SimilarItem {
	var out []SimilarItem
	for _, s := range l.Results {
		it := SimilarItem{TMDBID: s.ID, MediaType: "movie", Title: s.Title, Year: year(s.ReleaseDate), Group: group}
		if s.PosterPath != "" {
			it.PosterURL = "https://image.tmdb.org/t/p/w185" + s.PosterPath
		}
		out = append(out, it)
	}
	return out
}
func (l tvList) items(group string) []SimilarItem {
	var out []SimilarItem
	for _, s := range l.Results {
		it := SimilarItem{TMDBID: s.ID, MediaType: "tv", Title: s.Name, Year: year(s.FirstAirDate), Group: group}
		if s.PosterPath != "" {
			it.PosterURL = "https://image.tmdb.org/t/p/w185" + s.PosterPath
		}
		out = append(out, it)
	}
	return out
}

// mergeRelated concatenates related-item tiers in priority order, dropping the current title
// and de-duplicating by (type,id) so a franchise entry never reappears under "More Like This".
// The first occurrence wins, so earlier (higher-priority) tiers keep their items and their label.
func mergeRelated(selfID int, capN int, tiers ...[]SimilarItem) []SimilarItem {
	seen := map[string]bool{}
	var out []SimilarItem
	for _, tier := range tiers {
		for _, it := range tier {
			if it.TMDBID == selfID || it.Title == "" {
				continue
			}
			key := it.MediaType + ":" + fmt.Sprint(it.TMDBID)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, it)
			if len(out) >= capN {
				return out
			}
		}
	}
	return out
}

// collectionParts fetches the movies in a TMDB collection (the franchise: sequels/prequels).
func (c *Client) collectionParts(collID int, label string) []SimilarItem {
	resp, err := c.httpClient.Get(fmt.Sprintf("%s/collection/%d?api_key=%s", c.base(), collID, c.key()))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var coll struct {
		Parts []struct {
			ID          int    `json:"id"`
			Title       string `json:"title"`
			PosterPath  string `json:"poster_path"`
			ReleaseDate string `json:"release_date"`
		} `json:"parts"`
	}
	if json.NewDecoder(resp.Body).Decode(&coll) != nil {
		return nil
	}
	// Chronological order so "Vol. I, Vol. II, …" reads naturally.
	sort.SliceStable(coll.Parts, func(i, j int) bool { return coll.Parts[i].ReleaseDate < coll.Parts[j].ReleaseDate })
	var out []SimilarItem
	for _, p := range coll.Parts {
		it := SimilarItem{TMDBID: p.ID, MediaType: "movie", Title: p.Title, Year: year(p.ReleaseDate), Group: label}
		if p.PosterPath != "" {
			it.PosterURL = "https://image.tmdb.org/t/p/w185" + p.PosterPath
		}
		out = append(out, it)
	}
	return out
}

// personTitles fetches a cast member's other notable titles (priority-3 "Starring …" tier),
// most popular first, so the related list still surfaces the same faces when there's no franchise.
func (c *Client) personTitles(personID int, label string) []SimilarItem {
	resp, err := c.httpClient.Get(fmt.Sprintf("%s/person/%d/combined_credits?api_key=%s", c.base(), personID, c.key()))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var cc struct {
		Cast []struct {
			ID           int     `json:"id"`
			MediaType    string  `json:"media_type"`
			Title        string  `json:"title"`
			Name         string  `json:"name"`
			PosterPath   string  `json:"poster_path"`
			ReleaseDate  string  `json:"release_date"`
			FirstAirDate string  `json:"first_air_date"`
			Popularity   float64 `json:"popularity"`
			VoteCount    int     `json:"vote_count"`
		} `json:"cast"`
	}
	if json.NewDecoder(resp.Body).Decode(&cc) != nil {
		return nil
	}
	sort.SliceStable(cc.Cast, func(i, j int) bool { return cc.Cast[i].Popularity > cc.Cast[j].Popularity })
	var out []SimilarItem
	for _, p := range cc.Cast {
		if p.MediaType != "movie" && p.MediaType != "tv" {
			continue
		}
		// Require a real audience footprint — filters out talk-show / award-ceremony
		// appearances (vote_count ~0) that otherwise pollute an actor's "other titles".
		if p.VoteCount < 50 {
			continue
		}
		title := p.Title
		date := p.ReleaseDate
		if p.MediaType == "tv" {
			title = p.Name
			date = p.FirstAirDate
		}
		if title == "" || p.PosterPath == "" { // require artwork so the strip looks clean
			continue
		}
		out = append(out, SimilarItem{TMDBID: p.ID, MediaType: p.MediaType, Title: title,
			Year: year(date), PosterURL: "https://image.tmdb.org/t/p/w185" + p.PosterPath, Group: label})
		if len(out) >= 12 {
			break
		}
	}
	return out
}

// RichDetails fetches full movie/TV details including cast, similar titles,
// collection membership, and digital release date — one TMDB API call.
func (c *Client) RichDetails(tmdbID int, mediaType string) (*RichDetails, error) {
	var endpoint string
	if mediaType == "movie" {
		endpoint = fmt.Sprintf(
			"%s/movie/%d?api_key=%s&append_to_response=credits,similar,recommendations,belongs_to_collection,release_dates",
			c.base(), tmdbID, c.key())
	} else {
		endpoint = fmt.Sprintf(
			"%s/tv/%d?api_key=%s&append_to_response=credits,similar,recommendations",
			c.base(), tmdbID, c.key())
	}
	resp, err := c.httpClient.Get(endpoint)
	if err != nil {
		return nil, fmt.Errorf("tmdb rich details: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb rich details: status %d", resp.StatusCode)
	}

	out := &RichDetails{TMDBID: tmdbID, MediaType: mediaType}

	// Related-items hierarchy, assembled after decoding: franchise (collection) → "More Like
	// This" (recommendations+similar) → "Starring <lead>" (top cast member's other titles).
	var related []SimilarItem // tier 2 (genre/recommendations)
	var collID int
	var collName string
	var castID int
	var castName string

	if mediaType == "movie" {
		var m struct {
			Title        string  `json:"title"`
			Overview     string  `json:"overview"`
			Tagline      string  `json:"tagline"`
			BackdropPath string  `json:"backdrop_path"`
			PosterPath   string  `json:"poster_path"`
			VoteAverage  float64 `json:"vote_average"`
			Runtime      int     `json:"runtime"`
			ReleaseDate  string  `json:"release_date"`
			Genres       []struct {
				Name string `json:"name"`
			} `json:"genres"`
			Credits struct {
				Cast []struct {
					ID          int    `json:"id"`
					Name        string `json:"name"`
					Character   string `json:"character"`
					ProfilePath string `json:"profile_path"`
				} `json:"cast"`
			} `json:"credits"`
			Similar             movieList `json:"similar"`
			Recommendations     movieList `json:"recommendations"`
			BelongsToCollection *struct {
				ID         int    `json:"id"`
				Name       string `json:"name"`
				PosterPath string `json:"poster_path"`
			} `json:"belongs_to_collection"`
			ReleaseDates struct {
				Results []struct {
					Country      string `json:"iso_3166_1"`
					ReleaseDates []struct {
						Type        int    `json:"type"`
						ReleaseDate string `json:"release_date"`
					} `json:"release_dates"`
				} `json:"results"`
			} `json:"release_dates"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			return nil, err
		}
		out.Title = m.Title
		out.Year = year(m.ReleaseDate)
		out.Overview = m.Overview
		out.Tagline = m.Tagline
		out.VoteAverage = m.VoteAverage
		out.RuntimeMinutes = m.Runtime
		if m.BackdropPath != "" {
			out.BackdropURL = "https://image.tmdb.org/t/p/w1280" + m.BackdropPath
		}
		if m.PosterPath != "" {
			out.PosterURL = "https://image.tmdb.org/t/p/w342" + m.PosterPath
		}
		for _, g := range m.Genres {
			out.Genres = append(out.Genres, g.Name)
		}
		for i, cm := range m.Credits.Cast {
			if i >= 6 {
				break
			}
			member := CastMember{Name: cm.Name, Character: cm.Character}
			if cm.ProfilePath != "" {
				member.ProfileURL = "https://image.tmdb.org/t/p/w185" + cm.ProfilePath
			}
			out.Cast = append(out.Cast, member)
		}
		if len(m.Credits.Cast) > 0 {
			castID, castName = m.Credits.Cast[0].ID, m.Credits.Cast[0].Name
		}
		if m.BelongsToCollection != nil {
			coll := &CollectionInfo{ID: m.BelongsToCollection.ID, Name: m.BelongsToCollection.Name}
			if m.BelongsToCollection.PosterPath != "" {
				coll.PosterURL = "https://image.tmdb.org/t/p/w342" + m.BelongsToCollection.PosterPath
			}
			out.Collection = coll
			collID, collName = m.BelongsToCollection.ID, m.BelongsToCollection.Name
		}
		// Tier 2: recommendations first (TMDB blends genre + cast + keywords), then plain similar.
		related = append(m.Recommendations.items("More Like This"), m.Similar.items("More Like This")...)
		// Find digital release (TMDB type 4) — US first, then any country
		for _, priority := range []string{"US", ""} {
			for _, country := range m.ReleaseDates.Results {
				if priority != "" && country.Country != priority {
					continue
				}
				for _, rd := range country.ReleaseDates {
					if rd.Type == 4 && rd.ReleaseDate != "" {
						if out.DigitalRelease == "" {
							if len(rd.ReleaseDate) >= 10 {
								out.DigitalRelease = rd.ReleaseDate[:10]
							}
						}
					}
				}
			}
			if out.DigitalRelease != "" {
				break
			}
		}
	} else {
		var t struct {
			Name         string  `json:"name"`
			Overview     string  `json:"overview"`
			Tagline      string  `json:"tagline"`
			BackdropPath string  `json:"backdrop_path"`
			PosterPath   string  `json:"poster_path"`
			VoteAverage  float64 `json:"vote_average"`
			FirstAirDate string  `json:"first_air_date"`
			Status       string  `json:"status"`
			Genres       []struct {
				Name string `json:"name"`
			} `json:"genres"`
			Credits struct {
				Cast []struct {
					ID          int    `json:"id"`
					Name        string `json:"name"`
					Character   string `json:"character"`
					ProfilePath string `json:"profile_path"`
				} `json:"cast"`
			} `json:"credits"`
			Similar         tvList `json:"similar"`
			Recommendations tvList `json:"recommendations"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
			return nil, err
		}
		out.Title = t.Name
		out.Year = year(t.FirstAirDate)
		out.Overview = t.Overview
		out.Tagline = t.Tagline
		out.VoteAverage = t.VoteAverage
		out.Status = t.Status
		out.IsAiring = (&Details{Status: t.Status}).IsAiring()
		if t.BackdropPath != "" {
			out.BackdropURL = "https://image.tmdb.org/t/p/w1280" + t.BackdropPath
		}
		if t.PosterPath != "" {
			out.PosterURL = "https://image.tmdb.org/t/p/w342" + t.PosterPath
		}
		for _, g := range t.Genres {
			out.Genres = append(out.Genres, g.Name)
		}
		for i, cm := range t.Credits.Cast {
			if i >= 6 {
				break
			}
			member := CastMember{Name: cm.Name, Character: cm.Character}
			if cm.ProfilePath != "" {
				member.ProfileURL = "https://image.tmdb.org/t/p/w185" + cm.ProfilePath
			}
			out.Cast = append(out.Cast, member)
		}
		if len(t.Credits.Cast) > 0 {
			castID, castName = t.Credits.Cast[0].ID, t.Credits.Cast[0].Name
		}
		related = append(t.Recommendations.items("More Like This"), t.Similar.items("More Like This")...)
	}

	// Fetch the franchise (collection) and lead-actor tiers concurrently, then merge by priority:
	// franchise first (so e.g. "Nymphomaniac Vol. II" always shows under Vol. I), then the
	// recommendation/similar blend, then other titles starring the lead — deduped, capped.
	var franchise, cast []SimilarItem
	var wg sync.WaitGroup
	if collID > 0 {
		label := collName
		if label == "" {
			label = "Franchise"
		}
		wg.Add(1)
		go func() { defer wg.Done(); franchise = c.collectionParts(collID, label) }()
	}
	if castID > 0 {
		wg.Add(1)
		go func() { defer wg.Done(); cast = c.personTitles(castID, "Starring "+castName) }()
	}
	wg.Wait()
	// Reserve room so the lowest-priority cast tier still surfaces: franchise is small, cap the
	// genre blend so a few "Starring <lead>" picks always make the final list (total capped too).
	if len(related) > 12 {
		related = related[:12]
	}
	out.Similar = mergeRelated(tmdbID, 18, franchise, related, cast)
	return out, nil
}

// ── Browse / Discover ─────────────────────────────────────────────────────────

type BrowseResult struct {
	TMDBID    int     `json:"tmdb_id"`
	MediaType string  `json:"media_type"`
	Title     string  `json:"title"`
	Year      string  `json:"year"`
	PosterURL string  `json:"poster_url,omitempty"`
	Rating    float64 `json:"vote_average"`
}

func (c *Client) Trending() ([]BrowseResult, error) {
	url := fmt.Sprintf("%s/trending/all/week?api_key=%s", c.base(), c.key())
	return c.browseGet(url, "")
}

// Discover fetches movies or TV shows filtered by genres, companies, networks and the
// rest of TMDB's /discover vocabulary. params holds TMDB parameter names verbatim, e.g.
// {"with_genres": "28|12", "sort_by": "popularity.desc"}.
//
// TWO THINGS THE CALLER OWNS, and neither is enforceable here:
//
//  1. The KEYS in params are written straight into the outbound query, so the caller must
//     build the map from an explicit allowlist. Handing this function a map assembled
//     from a request's query string would let an unauthenticated caller add `api_key`
//     (pointing TMDB at someone else's account) or any other TMDB parameter of their
//     choosing. cmd/orchestrator's browse handler is the allowlist; see discoverParams.
//  2. The VALUES are escaped here, but escaping is not validation. "with_genres=lolno"
//     is a perfectly well-formed query that TMDB answers with nonsense.
//
// Values are encoded with url.Values rather than concatenated. The old code appended
// "&"+k+"="+v raw, which meant a single "&" inside any value silently became a new
// outbound parameter — the same injection shape as (1), just reached through a value
// instead of a key. It matters concretely now: OR-ing genres needs a literal "|", which
// must reach TMDB percent-encoded.
func (c *Client) Discover(mediaType string, params map[string]string) ([]BrowseResult, error) {
	q := make(url.Values, len(params)+1)
	q.Set("api_key", c.key())
	for k, v := range params {
		q.Set(k, v)
	}
	return c.browseGet(fmt.Sprintf("%s/discover/%s?%s", c.base(), mediaType, q.Encode()), mediaType)
}

// ── Browse filter allowlist ───────────────────────────────────────────────────

// DiscoverSorts is the complete set of sort orders a caller may ask for, in the order a
// UI should offer them. It is an ALLOWLIST, not a suggestion: sort_by is forwarded to
// TMDB verbatim, so anything not on this list is refused rather than passed along.
var DiscoverSorts = []string{
	"popularity.desc",
	"vote_average.desc",
	"primary_release_date.desc",
	"revenue.desc",
}

const (
	// maxDiscoverPage is TMDB's own hard ceiling on /discover paging: page 501 is an
	// error upstream, so refusing it here turns a confusing 502 into an honest 400.
	maxDiscoverPage = 500
	// maxDiscoverVotes bounds vote_count.gte. The most-voted title on TMDB is in the
	// low tens of thousands, so anything past this is a caller probing, not filtering.
	maxDiscoverVotes = 100000
	// maxFilterIDs caps how many ids one filter may carry. A browse UI offers a handful
	// of chips; the cap exists so an unauthenticated caller cannot make us build a
	// multi-kilobyte outbound URL out of ten thousand comma-separated ids.
	maxFilterIDs = 20
	// minFilmYear is the year of Roundhay Garden Scene, the oldest surviving film, and
	// maxFilmYear leaves room for announced-but-unreleased titles. Fixed constants
	// rather than time.Now() so the accepted range does not drift under the tests.
	minFilmYear = 1888
	maxFilmYear = 2100
	// defaultRatingVotes is the vote floor applied when a caller sorts by rating and
	// says nothing about vote counts. Without it, vote_average.desc is worthless: the
	// top of that list is short films with a single 10/10 vote, not good movies. An
	// explicit min_votes (including min_votes=0) overrides it.
	defaultRatingVotes = 200
)

// DiscoverParams translates a browse request's query string into TMDB /discover
// parameters.
//
// THIS FUNCTION IS THE SECURITY BOUNDARY for /api/browse/discover, which is
// unauthenticated (SECURITY.md). It reads an explicit list of keys out of q and ignores
// everything else — it never ranges over q. That is the whole point: a passthrough would
// let anyone who can reach the port append `api_key=<their own>` and bill their traffic
// to this instance's TMDB account, or reach TMDB parameters (certification, region,
// with_watch_providers…) that the UI never intended to expose. Adding a filter means
// adding a case here; there is no way to "just pass this one through".
//
// Every error returned is built from string constants — never from caller input and never
// from an upstream error — so a handler may return the message verbatim to an anonymous
// caller with no risk of echoing back an API key or reflecting input.
//
// Two TMDB asymmetries are handled here, and both are the kind that look right in code
// review and return nothing at runtime:
//
//  1. THE YEAR PARAMETER IS NAMED DIFFERENTLY PER MEDIA TYPE. Movies filter on
//     primary_release_year, TV on first_air_date_year. Send a movie's parameter name to
//     /discover/tv and TMDB does not complain — it silently ignores the unknown
//     parameter and returns an unfiltered list, so the bug shows up as "the year filter
//     does nothing", never as an error.
//  2. SORT ORDERS ARE NOT SHARED. primary_release_date.desc is a movie concept; the TV
//     equivalent is first_air_date.desc, and it is translated below. revenue.desc has no
//     TV equivalent at all, so it is refused for TV rather than translated into
//     something that means something different.
func DiscoverParams(mediaType string, q url.Values) (map[string]string, error) {
	if mediaType != "movie" && mediaType != "tv" {
		return nil, errors.New("type must be movie or tv")
	}
	params := map[string]string{
		"sort_by": "popularity.desc",
		// Explicit rather than relying on the upstream default, which is a setting we do
		// not control.
		"include_adult": "false",
	}

	// ── Genres ────────────────────────────────────────────────────────────────
	//
	// TMDB's separators are the opposite way round from most people's intuition, and
	// getting them backwards yields a plausible-looking list instead of an error:
	//
	//	with_genres=28,12   → AND: a film that is BOTH Action and Adventure
	//	with_genres=28|12   → OR:  a film that is Action OR Adventure
	//
	// The caller always sends a plain comma-separated list; match= chooses the join.
	// The default is OR, because "Action & Adventure" as a browse heading means "show me
	// action and adventure films", not "show me films that are simultaneously both" —
	// which is a far smaller and much stranger set.
	sep := "|"
	switch q.Get("match") {
	case "", "any":
		sep = "|"
	case "all":
		sep = ","
	default:
		return nil, errors.New("match must be any or all")
	}
	if genres := q.Get("genres"); genres != "" {
		joined, err := joinIDs(genres, sep)
		if err != nil {
			return nil, errors.New("genres must be a comma-separated list of numeric TMDB genre ids")
		}
		params["with_genres"] = joined
	}

	// ── Studios and networks ──────────────────────────────────────────────────
	//
	// Both are always OR-joined. Asking for two studios means "either studio"; a title
	// co-produced by both is rare enough that AND would read as a broken filter. The
	// legacy `companies=` spelling is still accepted because the homepage carousels were
	// built against it.
	studios := q.Get("studios")
	if studios == "" {
		studios = q.Get("companies")
	}
	if studios != "" {
		joined, err := joinIDs(studios, "|")
		if err != nil {
			return nil, errors.New("studios must be a comma-separated list of numeric TMDB company ids")
		}
		params["with_companies"] = joined
	}
	if networks := q.Get("networks"); networks != "" {
		// with_networks is a TV-only concept. TMDB ignores it on /discover/movie, which
		// would quietly return an unfiltered movie list instead of saying no.
		if mediaType != "tv" {
			return nil, errors.New("networks is only available for type=tv")
		}
		joined, err := joinIDs(networks, "|")
		if err != nil {
			return nil, errors.New("networks must be a comma-separated list of numeric TMDB network ids")
		}
		params["with_networks"] = joined
	}

	// ── Year (see asymmetry 1 above) ──────────────────────────────────────────
	if y := q.Get("year"); y != "" {
		n, err := boundedInt(y, minFilmYear, maxFilmYear)
		if err != nil {
			return nil, fmt.Errorf("year must be a number between %d and %d", minFilmYear, maxFilmYear)
		}
		key := "primary_release_year"
		if mediaType == "tv" {
			key = "first_air_date_year"
		}
		params[key] = strconv.Itoa(n)
	}

	// ── Sort (see asymmetry 2 above) ──────────────────────────────────────────
	sortBy := q.Get("sort")
	if sortBy != "" {
		if !allowedSort(sortBy) {
			return nil, errors.New("sort must be one of: " + strings.Join(DiscoverSorts, ", "))
		}
		if mediaType == "tv" {
			switch sortBy {
			case "primary_release_date.desc":
				sortBy = "first_air_date.desc"
			case "revenue.desc":
				return nil, errors.New("sort=revenue.desc is only available for type=movie")
			}
		}
		params["sort_by"] = sortBy
	}

	// ── Vote floor ────────────────────────────────────────────────────────────
	if mv := q.Get("min_votes"); mv != "" {
		n, err := boundedInt(mv, 0, maxDiscoverVotes)
		if err != nil {
			return nil, fmt.Errorf("min_votes must be a number between 0 and %d", maxDiscoverVotes)
		}
		params["vote_count.gte"] = strconv.Itoa(n)
	} else if strings.HasPrefix(params["sort_by"], "vote_average.") {
		params["vote_count.gte"] = strconv.Itoa(defaultRatingVotes)
	}

	// ── Paging ────────────────────────────────────────────────────────────────
	if pg := q.Get("page"); pg != "" {
		n, err := boundedInt(pg, 1, maxDiscoverPage)
		if err != nil {
			return nil, fmt.Errorf("page must be a number between 1 and %d", maxDiscoverPage)
		}
		params["page"] = strconv.Itoa(n)
	}

	return params, nil
}

// allowedSort reports whether sortBy is on the DiscoverSorts allowlist.
func allowedSort(sortBy string) bool {
	for _, s := range DiscoverSorts {
		if s == sortBy {
			return true
		}
	}
	return false
}

// joinIDs parses a caller-supplied comma-separated id list and re-emits it joined by sep.
//
// It re-emits rather than passing the string through so that ONLY digits and the chosen
// separator can ever reach the outbound URL: whatever punctuation, whitespace or encoded
// separator the caller sent is discarded along the way, and the result is built from
// integers this function parsed itself.
func joinIDs(raw, sep string) (string, error) {
	parts := strings.Split(raw, ",")
	if len(parts) > maxFilterIDs {
		return "", fmt.Errorf("at most %d ids", maxFilterIDs)
	}
	ids := make([]string, 0, len(parts))
	for _, p := range parts {
		n, err := boundedInt(strings.TrimSpace(p), 1, 1<<31-1)
		if err != nil {
			return "", err
		}
		ids = append(ids, strconv.Itoa(n))
	}
	if len(ids) == 0 {
		return "", errors.New("no ids")
	}
	return strings.Join(ids, sep), nil
}

// boundedInt parses a decimal integer and requires it to fall inside [lo, hi].
//
// strconv.Atoi alone is not enough: it happily accepts "+7", "-1" and values that
// overflow into nonsense, and a caller who sends "1e9" deserves a 400 rather than a
// coerced 0. Anything that is not plain digits within range is an error, never a
// silently substituted default.
func boundedInt(s string, lo, hi int) (int, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("out of range [%d, %d]", lo, hi)
	}
	return n, nil
}

// ── Genre vocabulary ──────────────────────────────────────────────────────────

// Genre is one entry of TMDB's genre vocabulary: the id that /discover filters on, and
// the label a human reads.
type Genre struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// genreCacheTTL is how long a fetched genre list is served without re-asking TMDB.
//
// A whole day is deliberate. TMDB's genre vocabulary is a fixed list of roughly nineteen
// movie and sixteen TV genres; ids are permanent and the list has changed a handful of
// times in the lifetime of the v3 API. Meanwhile every browse page load needs it just to
// label its filter chips, so an uncached lookup puts a round trip to api.themoviedb.org
// in front of the first pixel of the filter bar — over a home connection that is the
// difference between an instant panel and a visible stall, and it is the entire reason
// this cache exists.
//
// The cost of being stale is bounded and boring: a genre added today shows up in the
// filter list within a day. The cost that would actually hurt — an id changing meaning —
// cannot happen. The cache is in-process, so restarting the orchestrator is always a way
// to force a refresh.
const genreCacheTTL = 24 * time.Hour

type cachedGenres struct {
	genres  []Genre
	fetched time.Time
}

// Genres returns TMDB's genre list for "movie" or "tv", cached for genreCacheTTL.
//
// Two properties worth stating, because both are easy to get wrong when adding a cache:
//
//   - The lock is NOT held across the HTTP call. Two concurrent first-callers may both
//     fetch, and the loser simply overwrites an identical list — which is free. Holding
//     the mutex over a request that can take the client's full 10-second timeout would
//     instead stall every browse request in the process behind one slow TMDB response,
//     including requests for the other media type.
//   - ONLY successes are cached. A failed fetch leaves the entry untouched, so a TMDB
//     blip cannot poison the list for a day; the next caller retries immediately.
//
// The returned slice is a copy: the cached one is shared by every caller and must not be
// handed out where a caller could sort or append to it.
func (c *Client) Genres(mediaType string) ([]Genre, error) {
	if mediaType != "movie" && mediaType != "tv" {
		return nil, fmt.Errorf("tmdb genres: unknown media type %q", mediaType)
	}

	c.genreMu.Lock()
	entry, ok := c.genreCache[mediaType]
	c.genreMu.Unlock()
	if ok && time.Since(entry.fetched) < genreCacheTTL {
		return append([]Genre(nil), entry.genres...), nil
	}

	u := fmt.Sprintf("%s/genre/%s/list?api_key=%s", c.base(), mediaType, url.QueryEscape(c.key()))
	resp, err := c.httpClient.Get(u)
	if err != nil {
		return nil, fmt.Errorf("tmdb genres: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb genres: status %d", resp.StatusCode)
	}
	var raw struct {
		Genres []Genre `json:"genres"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("tmdb genres: %w", err)
	}

	out := make([]Genre, 0, len(raw.Genres))
	for _, g := range raw.Genres {
		if g.ID > 0 && g.Name != "" {
			out = append(out, g)
		}
	}
	// An empty list is not a result worth remembering for a day — TMDB answering 200 with
	// nothing in it is a fault on their side, not a genuinely empty vocabulary.
	if len(out) == 0 {
		return nil, fmt.Errorf("tmdb genres: empty list for %s", mediaType)
	}

	c.genreMu.Lock()
	if c.genreCache == nil {
		c.genreCache = map[string]cachedGenres{}
	}
	c.genreCache[mediaType] = cachedGenres{genres: out, fetched: time.Now()}
	c.genreMu.Unlock()

	return append([]Genre(nil), out...), nil
}

// ── Studio / company lookup ───────────────────────────────────────────────────

// Studio is a production company as TMDB knows it. The id is what /discover accepts as
// with_companies; the name and logo are for an autocomplete row.
type Studio struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	LogoURL string `json:"logo_url,omitempty"`
}

// studioSearchLimit caps how many company matches are returned. TMDB pages at 20 and an
// autocomplete list longer than a screenful is noise, not choice.
const studioSearchLimit = 20

// SearchCompanies looks up production companies by name for the studio filter's
// autocomplete.
//
// Deliberately NOT cached: unlike the genre vocabulary this is an open-ended search space
// keyed on whatever a user typed, so a cache would be an unbounded map filled by
// unauthenticated callers — a memory-growth lever handed to anyone who can reach the
// port. The rate limit on the HTTP handler is the control that matters here instead.
func (c *Client) SearchCompanies(query string) ([]Studio, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("tmdb company search: empty query")
	}
	u := fmt.Sprintf("%s/search/company?api_key=%s&query=%s&page=1",
		c.base(), url.QueryEscape(c.key()), url.QueryEscape(query))
	resp, err := c.httpClient.Get(u)
	if err != nil {
		return nil, fmt.Errorf("tmdb company search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb company search: status %d", resp.StatusCode)
	}
	var raw struct {
		Results []struct {
			ID       int    `json:"id"`
			Name     string `json:"name"`
			LogoPath string `json:"logo_path"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("tmdb company search: %w", err)
	}
	out := make([]Studio, 0, len(raw.Results))
	for _, r := range raw.Results {
		if r.ID <= 0 || r.Name == "" {
			continue
		}
		st := Studio{ID: r.ID, Name: r.Name}
		if r.LogoPath != "" {
			st.LogoURL = "https://image.tmdb.org/t/p/w92" + r.LogoPath
		}
		out = append(out, st)
		if len(out) >= studioSearchLimit {
			break
		}
	}
	return out, nil
}

func (c *Client) browseGet(url, forceMediaType string) ([]BrowseResult, error) {
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("tmdb browse: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("tmdb browse: status %d", resp.StatusCode)
	}
	var raw struct {
		Results []struct {
			ID           int     `json:"id"`
			MediaType    string  `json:"media_type"`
			Title        string  `json:"title"`
			Name         string  `json:"name"`
			PosterPath   string  `json:"poster_path"`
			ReleaseDate  string  `json:"release_date"`
			FirstAirDate string  `json:"first_air_date"`
			VoteAverage  float64 `json:"vote_average"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	var out []BrowseResult
	for _, r := range raw.Results {
		mt := r.MediaType
		if mt == "" {
			mt = forceMediaType
		}
		if mt != "movie" && mt != "tv" {
			continue
		}
		res := BrowseResult{TMDBID: r.ID, MediaType: mt, Rating: r.VoteAverage}
		if mt == "movie" {
			res.Title = r.Title
			res.Year = year(r.ReleaseDate)
		} else {
			res.Title = r.Name
			res.Year = year(r.FirstAirDate)
		}
		if r.PosterPath != "" {
			res.PosterURL = "https://image.tmdb.org/t/p/w342" + r.PosterPath
		}
		if res.Title != "" {
			out = append(out, res)
		}
	}
	return out, nil
}

// ── Calendar ───────────────────────────────────────────────────────────────────

// DatedRelease is a calendar entry with a precise release date (YYYY-MM-DD).
type DatedRelease struct {
	TMDBID    int    `json:"tmdb_id"`
	MediaType string `json:"media_type"`
	Title     string `json:"title"`
	Date      string `json:"date"`     // YYYY-MM-DD
	Subtitle  string `json:"subtitle"` // e.g. "Theatrical", "S02E05"
	PosterURL string `json:"poster_url,omitempty"`
	Kind      string `json:"kind"` // "movie" | "tv_premiere" | "subscription"
}

// UpcomingMovies returns theatrically/digitally upcoming movies with their dates.
func (c *Client) UpcomingMovies() ([]DatedRelease, error) {
	u := fmt.Sprintf("%s/movie/upcoming?api_key=%s&region=US", c.base(), c.key())
	resp, err := c.httpClient.Get(u)
	if err != nil {
		return nil, fmt.Errorf("tmdb upcoming: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("tmdb upcoming: status %d", resp.StatusCode)
	}
	var raw struct {
		Results []struct {
			ID          int    `json:"id"`
			Title       string `json:"title"`
			PosterPath  string `json:"poster_path"`
			ReleaseDate string `json:"release_date"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	var out []DatedRelease
	for _, r := range raw.Results {
		if r.ReleaseDate == "" || r.Title == "" {
			continue
		}
		d := DatedRelease{TMDBID: r.ID, MediaType: "movie", Title: r.Title,
			Date: r.ReleaseDate, Subtitle: "Theatrical", Kind: "movie"}
		if r.PosterPath != "" {
			d.PosterURL = "https://image.tmdb.org/t/p/w342" + r.PosterPath
		}
		out = append(out, d)
	}
	return out, nil
}

// OnTheAirPremieres returns TV shows currently on the air, enriched (best-effort,
// capped) with their next episode air date so they can be placed on the calendar.
func (c *Client) OnTheAirPremieres(limit int) ([]DatedRelease, error) {
	u := fmt.Sprintf("%s/tv/on_the_air?api_key=%s", c.base(), c.key())
	resp, err := c.httpClient.Get(u)
	if err != nil {
		return nil, fmt.Errorf("tmdb on_the_air: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("tmdb on_the_air: status %d", resp.StatusCode)
	}
	var raw struct {
		Results []struct {
			ID         int    `json:"id"`
			Name       string `json:"name"`
			PosterPath string `json:"poster_path"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	var out []DatedRelease
	for i, r := range raw.Results {
		if i >= limit {
			break
		}
		// Enrich with next_episode_to_air via a per-show details call.
		nd, ne, np := c.nextEpisode(r.ID)
		if nd == "" {
			continue
		}
		d := DatedRelease{TMDBID: r.ID, MediaType: "tv", Title: r.Name,
			Date: nd, Subtitle: ne, Kind: "tv_premiere"}
		if np != "" {
			d.PosterURL = np
		} else if r.PosterPath != "" {
			d.PosterURL = "https://image.tmdb.org/t/p/w342" + r.PosterPath
		}
		out = append(out, d)
	}
	return out, nil
}

// nextEpisode returns (air_date, "S..E..", poster_url) for a TV show's next episode.
func (c *Client) nextEpisode(tmdbID int) (string, string, string) {
	u := fmt.Sprintf("%s/tv/%d?api_key=%s", c.base(), tmdbID, c.key())
	resp, err := c.httpClient.Get(u)
	if err != nil {
		return "", "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", ""
	}
	var t struct {
		PosterPath       string `json:"poster_path"`
		NextEpisodeToAir *struct {
			AirDate       string `json:"air_date"`
			SeasonNumber  int    `json:"season_number"`
			EpisodeNumber int    `json:"episode_number"`
		} `json:"next_episode_to_air"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", "", ""
	}
	if t.NextEpisodeToAir == nil || t.NextEpisodeToAir.AirDate == "" {
		return "", "", ""
	}
	sub := fmt.Sprintf("S%02dE%02d", t.NextEpisodeToAir.SeasonNumber, t.NextEpisodeToAir.EpisodeNumber)
	poster := ""
	if t.PosterPath != "" {
		poster = "https://image.tmdb.org/t/p/w342" + t.PosterPath
	}
	return t.NextEpisodeToAir.AirDate, sub, poster
}

// year extracts the 4-digit year from a TMDB date string (YYYY-MM-DD).
func year(date string) string {
	if len(date) >= 4 {
		return date[:4]
	}
	return ""
}
