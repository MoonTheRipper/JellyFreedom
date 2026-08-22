package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
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
