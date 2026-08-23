package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	root "jellyfreedom"
	"jellyfreedom/internal/redact"
)

// newServer builds an http.Server with the timeouts a public-facing service needs.
//
// The old code called http.ListenAndServe directly, i.e. every timeout was zero: a
// single client that opened a connection and never sent a request header held a
// goroutine and an fd forever, and Slowloris needed no effort at all.
//
// WriteTimeout stays 0 ON PURPOSE. /play and /proxy/stream serve a movie for hours over
// one response; any non-zero WriteTimeout would cut playback mid-film. ReadHeaderTimeout
// and IdleTimeout give the actual protection without touching the streaming path.
func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		WriteTimeout:      0,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          nil,
	}
}

// runServer serves until ctx is cancelled, then drains in-flight requests.
//
// Shutdown matters here beyond tidiness: `defer db.Close()` in main never ran, because
// nothing ever returned from ListenAndServe under normal operation, so the process was
// always killed with the SQLite connection open.
//
// Streams are long-lived by design, so a hard grace cap applies: after it, Close()
// severs whatever is left rather than blocking the exit on a two-hour film.
func runServer(ctx context.Context, srv *http.Server, grace time.Duration) error {
	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down", "grace", grace)
	shutCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		slog.Warn("graceful shutdown timed out; closing active connections", "err", err)
		if cerr := srv.Close(); cerr != nil {
			slog.Warn("close listener", "err", cerr)
		}
	}
	return <-errCh
}

// webFS resolves the web asset tree.
//
// The embedded copy is the default so the binary is genuinely self-contained. --assets
// overrides it for UI development; when the flag names a directory that does not exist
// we say so loudly and fall back rather than silently serving 404s for the whole UI.
func webFS(assetsDir string, explicit bool) fs.FS {
	if explicit {
		if st, err := os.Stat(assetsDir); err == nil && st.IsDir() {
			slog.Info("serving web assets from disk (embedded copy overridden)", "dir", assetsDir)
			return os.DirFS(assetsDir)
		} else {
			slog.Error("--assets does not name a readable directory; using the embedded assets instead",
				"dir", assetsDir, "err", err)
		}
	}
	return root.WebFS()
}

// subFS returns a sub-tree, or an empty FS (plus a loud log) if it is missing, so a
// restructured web/ can never take the whole process down at startup.
func subFS(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		slog.Error("web assets: missing subdirectory", "dir", dir, "err", err)
		return emptyFS{}
	}
	return sub
}

type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

// ── Error responses ───────────────────────────────────────────────────────────

// httpFail logs the full error server-side and returns a STABLE, non-revealing message
// to the caller.
//
// Handlers used to return raw err.Error() to unauthenticated callers. Because *url.Error
// embeds the entire request URL and the Prowlarr key travelled as a query parameter,
// that literally handed the API key to anyone who could trigger a failed search — which
// the audit reproduced. redact.Error is the second line of defence for any path that
// still formats an error containing a secret.
func httpFail(w http.ResponseWriter, r *http.Request, code int, publicMsg string, err error) {
	if err != nil {
		slog.Error("request failed",
			"method", r.Method, "path", r.URL.Path, "code", code, "err", redact.Error(err))
	}
	jsonErr(w, publicMsg, code)
}

// ── Keyed single-flight ───────────────────────────────────────────────────────

// resolveGroup serialises expensive work per key.
//
// /play resolution costs a 150s indexer search plus up to four candidate attempts. A
// user hitting refresh, or Jellyfin probing a file while the client also requests it,
// used to multiply that work by the number of concurrent requests against the SAME
// title. Holders of the same key now queue behind one attempt and re-check the fast
// path afterwards.
type resolveGroup struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func newResolveGroup() *resolveGroup { return &resolveGroup{m: map[string]*sync.Mutex{}} }

// resolveCooldown suppresses repeat SLOW resolves for an identity that just failed one.
//
// resolveGroup collapses CONCURRENT attempts, but not SEQUENTIAL ones, and the expensive
// caller here is sequential: Jellyfin's ffprobe re-requests a .strm as soon as the last
// attempt errors, with no backoff of its own. In this deployment that produced 7,813
// probes of a single episode in five minutes, each one a full 90-second resolve budget —
// an indexer search plus up to four TorrServer add/drop cycles — for a title that had no
// playable release either time. 7,876 of 7,878 /play requests in the journal came from
// libavformat rather than a player.
//
// A failure is therefore remembered briefly and served straight back. This is a cache of
// the NEGATIVE result only: a success clears the entry immediately, so the moment a
// release becomes resolvable the next play gets it. Keyed on the same play identity the
// single-flight uses, so the two agree on what "the same title" means.
type resolveCooldown struct {
	mu  sync.Mutex
	m   map[string]time.Time
	ttl time.Duration
}

func newResolveCooldown(ttl time.Duration) *resolveCooldown {
	return &resolveCooldown{m: map[string]time.Time{}, ttl: ttl}
}

// blocked reports whether this identity failed to resolve recently, and how long is left.
func (c *resolveCooldown) blocked(key string) (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.m[key]
	if !ok {
		return 0, false
	}
	left := time.Until(until)
	if left <= 0 {
		delete(c.m, key)
		return 0, false
	}
	return left, true
}

// fail starts (or extends) the cooldown for an identity.
func (c *resolveCooldown) fail(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = time.Now().Add(c.ttl)
	// Opportunistic sweep: this map is keyed by title and only ever holds identities that
	// failed within the TTL, so it stays small — but a long-lived process should not grow
	// it without bound just because nobody asked about an old key again.
	if len(c.m) > 512 {
		now := time.Now()
		for k, until := range c.m {
			if until.Before(now) {
				delete(c.m, k)
			}
		}
	}
}

// succeed clears any cooldown, so a title that becomes playable is instantly playable.
func (c *resolveCooldown) succeed(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

// lock acquires the per-key mutex, honouring ctx while waiting. The returned release
// func must be called if (and only if) ok is true.
func (g *resolveGroup) lock(ctx context.Context, key string) (release func(), ok bool) {
	g.mu.Lock()
	mu := g.m[key]
	if mu == nil {
		mu = &sync.Mutex{}
		g.m[key] = mu
	}
	g.mu.Unlock()

	done := make(chan struct{})
	go func() {
		mu.Lock()
		close(done)
	}()
	select {
	case <-done:
		return mu.Unlock, true
	case <-ctx.Done():
		// Release the lock as soon as the winner hands it over, so the map entry is
		// not left permanently held by an abandoned waiter.
		go func() { <-done; mu.Unlock() }()
		return nil, false
	}
}

// searchLimiter is a small fixed-window IP limiter for the anonymous release-search
// endpoint. That endpoint drives a live Prowlarr query with a 150-second budget and had
// no throttle of any kind, so an unauthenticated caller on the LAN or the tailnet could
// pin the indexers indefinitely at no cost to themselves. The login limiter next door is
// keyed on failed credentials and does not fit a read endpoint that legitimately succeeds
// every time, hence a separate one.
type searchLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	window time.Duration
	max    int
}

func newSearchLimiter(max int, window time.Duration) *searchLimiter {
	return &searchLimiter{hits: map[string][]time.Time{}, window: window, max: max}
}

// allow records a hit for ip and reports whether it is within budget.
func (l *searchLimiter) allow(ip string) bool {
	now := time.Now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[ip] = kept
		return false
	}
	l.hits[ip] = append(kept, now)

	// Drop idle clients so the map does not accumulate one entry per address seen.
	if len(l.hits) > 1024 {
		for k, v := range l.hits {
			if len(v) == 0 || v[len(v)-1].Before(cutoff) {
				delete(l.hits, k)
			}
		}
	}
	return true
}

// clientIPOf keys the search limiter by peer address. X-Forwarded-For is deliberately NOT
// honoured, matching internal/api's own limiter: this service is reached directly on the
// LAN, and trusting a client-supplied header would let a caller mint a fresh bucket per
// request simply by varying it.
func clientIPOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
