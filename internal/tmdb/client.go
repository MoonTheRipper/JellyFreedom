package tmdb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const baseURL = "https://api.themoviedb.org/3"

type Client struct {
	mu         sync.RWMutex
	apiKey     string
	httpClient *http.Client
}

func New(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Configure updates the API key at runtime under a write lock.
func (c *Client) Configure(apiKey string) {
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
		ID            int    `json:"id"`
		MediaType     string `json:"media_type"`
		Title         string `json:"title"`         // movie
		Name          string `json:"name"`          // tv
		ReleaseDate   string `json:"release_date"`  // movie
		FirstAirDate  string `json:"first_air_date"` // tv
		Overview      string `json:"overview"`
		PosterPath    string `json:"poster_path"`
	} `json:"results"`
}

func (c *Client) Search(query string) ([]Result, error) {
	u := fmt.Sprintf("%s/search/multi?api_key=%s&query=%s&include_adult=false",
		baseURL, c.key(), url.QueryEscape(query))

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
}

type tvDetails struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	FirstAirDate string `json:"first_air_date"`
	Overview     string `json:"overview"`
	PosterPath   string `json:"poster_path"`
	Status       string `json:"status"`
}

func (c *Client) Details(tmdbID int, mediaType string) (*Details, error) {
	var endpoint string
	if mediaType == "movie" {
		endpoint = fmt.Sprintf("%s/movie/%d?api_key=%s", baseURL, tmdbID, c.key())
	} else {
		endpoint = fmt.Sprintf("%s/tv/%d?api_key=%s", baseURL, tmdbID, c.key())
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
	url := fmt.Sprintf("%s/tv/%d?api_key=%s", baseURL, tmdbID, c.key())
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
	url := fmt.Sprintf("%s/tv/%d/season/%d?api_key=%s", baseURL, tmdbID, seasonNum, c.key())
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
}

// RichDetails fetches full movie/TV details including cast, similar titles,
// collection membership, and digital release date — one TMDB API call.
func (c *Client) RichDetails(tmdbID int, mediaType string) (*RichDetails, error) {
	var endpoint string
	if mediaType == "movie" {
		endpoint = fmt.Sprintf(
			"%s/movie/%d?api_key=%s&append_to_response=credits,similar,belongs_to_collection,release_dates",
			baseURL, tmdbID, c.key())
	} else {
		endpoint = fmt.Sprintf(
			"%s/tv/%d?api_key=%s&append_to_response=credits,similar",
			baseURL, tmdbID, c.key())
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
			Genres       []struct{ Name string `json:"name"` } `json:"genres"`
			Credits      struct {
				Cast []struct {
					Name        string `json:"name"`
					Character   string `json:"character"`
					ProfilePath string `json:"profile_path"`
				} `json:"cast"`
			} `json:"credits"`
			Similar struct {
				Results []struct {
					ID          int    `json:"id"`
					Title       string `json:"title"`
					PosterPath  string `json:"poster_path"`
					ReleaseDate string `json:"release_date"`
				} `json:"results"`
			} `json:"similar"`
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
		if m.BelongsToCollection != nil {
			coll := &CollectionInfo{ID: m.BelongsToCollection.ID, Name: m.BelongsToCollection.Name}
			if m.BelongsToCollection.PosterPath != "" {
				coll.PosterURL = "https://image.tmdb.org/t/p/w342" + m.BelongsToCollection.PosterPath
			}
			out.Collection = coll
		}
		for i, s := range m.Similar.Results {
			if i >= 8 {
				break
			}
			item := SimilarItem{TMDBID: s.ID, MediaType: "movie", Title: s.Title, Year: year(s.ReleaseDate)}
			if s.PosterPath != "" {
				item.PosterURL = "https://image.tmdb.org/t/p/w185" + s.PosterPath
			}
			out.Similar = append(out.Similar, item)
		}
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
			Genres       []struct{ Name string `json:"name"` } `json:"genres"`
			Credits      struct {
				Cast []struct {
					Name        string `json:"name"`
					Character   string `json:"character"`
					ProfilePath string `json:"profile_path"`
				} `json:"cast"`
			} `json:"credits"`
			Similar struct {
				Results []struct {
					ID           int    `json:"id"`
					Name         string `json:"name"`
					PosterPath   string `json:"poster_path"`
					FirstAirDate string `json:"first_air_date"`
				} `json:"results"`
			} `json:"similar"`
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
		for i, s := range t.Similar.Results {
			if i >= 8 {
				break
			}
			item := SimilarItem{TMDBID: s.ID, MediaType: "tv", Title: s.Name, Year: year(s.FirstAirDate)}
			if s.PosterPath != "" {
				item.PosterURL = "https://image.tmdb.org/t/p/w185" + s.PosterPath
			}
			out.Similar = append(out.Similar, item)
		}
	}
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
	url := fmt.Sprintf("%s/trending/all/week?api_key=%s", baseURL, c.key())
	return c.browseGet(url, "")
}

// Discover fetches movies or TV shows filtered by genres, companies, or networks.
// params is a map of Torznab query params, e.g. {"with_genres":"28","sort_by":"popularity.desc"}
func (c *Client) Discover(mediaType string, params map[string]string) ([]BrowseResult, error) {
	base := fmt.Sprintf("%s/discover/%s?api_key=%s", baseURL, mediaType, c.key())
	for k, v := range params {
		base += "&" + k + "=" + v
	}
	return c.browseGet(base, mediaType)
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
	u := fmt.Sprintf("%s/movie/upcoming?api_key=%s&region=US", baseURL, c.key())
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
	u := fmt.Sprintf("%s/tv/on_the_air?api_key=%s", baseURL, c.key())
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
	u := fmt.Sprintf("%s/tv/%d?api_key=%s", baseURL, tmdbID, c.key())
	resp, err := c.httpClient.Get(u)
	if err != nil {
		return "", "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", ""
	}
	var t struct {
		PosterPath      string `json:"poster_path"`
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
