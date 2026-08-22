package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ── Update check ──────────────────────────────────────────────────────────────
//
// The dashboard calls this on EVERY load, so the result is cached in memory. And it
// must never be able to break the dashboard: every failure path here returns a Result
// with Available=false and a readable Error, and the handler answers HTTP 200.

// DefaultReleaseAPI is the GitHub "latest published release" endpoint for this project.
// GitHub excludes drafts and pre-releases from it, so a pre-release tag is not offered.
const DefaultReleaseAPI = "https://api.github.com/repos/MoonTheRipper/JellyFreedom/releases/latest"

const (
	// CacheTTL is how long a SUCCESSFUL check is reused (contract: 6h).
	CacheTTL = 6 * time.Hour
	// failureTTL is a short negative cache. Caching a failure for the full 6h would
	// pin the dashboard to "offline" long after the network came back; caching it for
	// zero seconds would hammer GitHub on every dashboard load while offline.
	failureTTL = 5 * time.Minute
	// checkTimeout is the total outbound budget (contract: ~8s).
	checkTimeout = 8 * time.Second
	// maxReleaseBody bounds what we read from GitHub. A release body is a few KB; this
	// stops a hostile or broken upstream from feeding us unbounded JSON.
	maxReleaseBody = 1 << 20
)

// Result is the exact JSON body of GET /api/update/check. Field names and shape are
// frozen by coord/update-contract.md — the dashboard is coded against them.
type Result struct {
	Current     string   `json:"current"`
	Latest      string   `json:"latest"`
	Available   bool     `json:"available"`
	Notes       []string `json:"notes"`
	URL         string   `json:"url"`
	PublishedAt string   `json:"published_at"`
	CheckedAt   string   `json:"checked_at"`
	Error       string   `json:"error"`
}

// Checker fetches and caches the latest published release.
type Checker struct {
	current string
	apiURL  string
	client  *http.Client

	// mu guards every cache field below. An earlier version of this codebase shipped a
	// data race on exactly this shape of shared handler state, so nothing here is read
	// or written outside the lock, and the package is tested with -race.
	mu       sync.Mutex
	cached   Result
	cachedAt time.Time
	cachedOK bool

	// fetch serialises the outbound call itself, so N dashboards loading at once after
	// the TTL expires produce ONE request to GitHub, not N.
	fetch sync.Mutex

	now func() time.Time // injectable clock for tests
}

// NewChecker builds a Checker for the running build's version.
func NewChecker(current string) *Checker {
	return &Checker{
		current: strings.TrimSpace(current),
		apiURL:  DefaultReleaseAPI,
		client:  &http.Client{Timeout: checkTimeout},
		now:     time.Now,
	}
}

// SetAPIURL overrides the release endpoint (tests only).
func (c *Checker) SetAPIURL(u string) {
	c.mu.Lock()
	c.apiURL = u
	c.mu.Unlock()
}

// Check returns the cached result, refreshing it when the cache is cold, stale, or
// explicitly bypassed with refresh=true.
func (c *Checker) Check(ctx context.Context, refresh bool) Result {
	if !refresh {
		if r, ok := c.cachedResult(); ok {
			return r
		}
	}

	c.fetch.Lock()
	defer c.fetch.Unlock()
	// Another goroutine may have refreshed while we waited for the lock.
	if !refresh {
		if r, ok := c.cachedResult(); ok {
			return r
		}
	}

	res := c.live(ctx)

	c.mu.Lock()
	c.cached = res
	c.cachedAt = c.now()
	c.cachedOK = res.Error == ""
	c.mu.Unlock()
	return res
}

// cachedResult returns the cached result if it is still fresh.
func (c *Checker) cachedResult() (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cachedAt.IsZero() {
		return Result{}, false
	}
	ttl := CacheTTL
	if !c.cachedOK {
		ttl = failureTTL
	}
	if c.now().Sub(c.cachedAt) >= ttl {
		return Result{}, false
	}
	return c.cached, true
}

// live performs the actual check. It NEVER returns an error: a failure becomes a
// Result with Available=false and a human-readable Error, because a failed update
// check must not break the dashboard.
func (c *Checker) live(ctx context.Context) Result {
	res := Result{
		Current:   c.current,
		Notes:     []string{},
		CheckedAt: c.now().UTC().Format(time.RFC3339),
	}

	rel, err := c.fetchRelease(ctx)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	res.Latest = strings.TrimSpace(rel.TagName)
	res.URL = strings.TrimSpace(rel.HTMLURL)
	res.PublishedAt = strings.TrimSpace(rel.PublishedAt)
	res.Notes = Notes(rel.Body)

	if res.Latest == "" {
		res.Error = "the latest release has no version tag"
		return res
	}
	if IsDev(c.current) {
		// A source build is not a release. Report the latest for information, but never
		// offer to overwrite the developer's own binary with it.
		return res
	}
	if _, err := ParseVersion(c.current); err != nil {
		res.Error = fmt.Sprintf("this build reports an unrecognisable version (%q)", c.current)
		return res
	}
	if _, err := ParseVersion(res.Latest); err != nil {
		res.Error = fmt.Sprintf("the latest release has an unrecognisable tag (%q)", res.Latest)
		return res
	}
	res.Available = IsNewer(c.current, res.Latest)
	return res
}

// ghRelease is the subset of GitHub's release JSON we use.
type ghRelease struct {
	TagName     string `json:"tag_name"`
	HTMLURL     string `json:"html_url"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
}

// fetchRelease calls GitHub with a bounded context and turns every failure mode into a
// message a non-developer can act on.
func (c *Checker) fetchRelease(ctx context.Context) (ghRelease, error) {
	c.mu.Lock()
	apiURL := c.apiURL
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return ghRelease{}, errors.New("could not build the update request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "JellyFreedom/"+c.current)

	resp, err := c.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ghRelease{}, errors.New("the update check timed out")
		}
		return ghRelease{}, errors.New("could not reach GitHub to check for updates")
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ghRelease{}, errors.New("no published release was found for this project")
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		return ghRelease{}, errors.New("GitHub rate-limited the update check — try again later")
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return ghRelease{}, fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseBody))
	if err != nil {
		return ghRelease{}, errors.New("could not read GitHub's response")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return ghRelease{}, errors.New("GitHub returned an empty response")
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return ghRelease{}, errors.New("could not understand GitHub's response")
	}
	return rel, nil
}
