package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"jellyfreedom/internal/api"
	"jellyfreedom/internal/config"
	"jellyfreedom/internal/jellyfin"
	"jellyfreedom/internal/library"
	"jellyfreedom/internal/netnsproxy"
	"jellyfreedom/internal/store"
	"jellyfreedom/internal/websource"
)

// ── Paste-a-link web sources ──────────────────────────────────────────────────
//
// A user pastes a video page URL into the dashboard. The orchestrator extracts what the
// page is, writes a .strm, and the video appears in Jellyfin like anything else.
//
// The design is Resolve-at-Play, unchanged from the torrent path and for the same
// reason. A torrent pointer rots because its seeders leave; a web pointer rots because
// the site signs its CDN links with a short expiry. Both fail the same way — the library
// says "ready" and playback 403s — and both have the same fix: the .strm holds an
// IDENTITY, /play/p/web/{id}, and the thing that actually delivers bytes is chosen fresh
// each time somebody presses play.
//
// The other half is that every packet involved goes through the tunnel. Fetching the
// page identifies the requester to the site exactly as thoroughly as fetching the video
// does, so extraction and playback both dial through the in-namespace SOCKS proxy (see
// internal/netnsproxy). There is no direct-connection fallback anywhere in this file. A
// misconfigured box does not play web sources; it does not play them the fast way.

// resolvedTTL is how long a resolved media URL is reused.
//
// The tension is real in both directions. Too long and a signed URL expires in the cache
// and every seek 403s. Too short and each seek in Jellyfin — which issues a fresh Range
// request, which is a fresh /play request — pays for another extraction, and extraction
// is seconds, over a VPN, per seek.
//
// Ten minutes sits well inside the shortest expiry seen in practice, and the cache is
// not the only protection: a 403 or 410 from the CDN invalidates the entry and re-
// resolves once, so an expiry shorter than this is a hiccup rather than a failure. See
// streamWebSource.
const resolvedTTL = 10 * time.Minute

// webPlayer owns everything the web-source feature needs at runtime.
type webPlayer struct {
	db     *store.Store
	cfg    *config.Config
	client websource.Client
	dialer netnsproxy.Dialer

	// http is the client that fetches the media itself. Its transport dials through the
	// namespace proxy and has Proxy explicitly nil — the default is
	// ProxyFromEnvironment, and an HTTP_PROXY in the unit's environment would silently
	// route the stream somewhere other than the tunnel.
	http *http.Client

	// resolves single-flights extraction per source id, so Jellyfin probing a file while
	// the player also opens it does not run two extractions of the same page.
	resolves *resolveGroup

	mu     sync.Mutex
	cached map[string]cachedStream
}

type cachedStream struct {
	stream websource.Stream
	at     time.Time
}

// newWebPlayer builds the feature from config. It always returns a usable value: when
// the feature is off or its dependencies are missing, `enabled` reports why, and every
// entry point answers with that reason instead of failing obscurely.
func newWebPlayer(db *store.Store, cfg *config.Config) *webPlayer {
	dialer := netnsproxy.Dialer{Addr: cfg.WebSourcesProxyAddr(), Timeout: 20 * time.Second}
	if !cfg.WebSources.Enabled {
		// An empty proxy address disables extraction outright (websource.Client refuses
		// to run without one), which is the correct behaviour for a disabled feature and
		// is enforced by the extractor rather than only by a branch here.
		dialer.Addr = ""
	}
	return &webPlayer{
		db:  db,
		cfg: cfg,
		client: websource.Client{
			Binary:   cfg.WebSources.YTDLPPath,
			ProxyURL: dialer.ProxyURL(),
			Timeout:  90 * time.Second,
		},
		dialer: dialer,
		http: &http.Client{
			// No Client-level timeout: a film streams for hours on one response. The
			// per-phase bounds below cover the ways a fetch can hang WITHOUT holding the
			// body open, which is the case a blanket timeout would be aimed at anyway.
			Transport: &http.Transport{
				Proxy:                 nil,
				DialContext:           dialer.DialContext,
				TLSHandshakeTimeout:   20 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				MaxIdleConnsPerHost:   4,
			},
		},
		resolves: newResolveGroup(),
		cached:   map[string]cachedStream{},
	}
}

// enabled reports nil when web sources can actually be used, and otherwise the reason
// they cannot — in words a dashboard can show verbatim.
func (p *webPlayer) enabled() error {
	if p == nil || !p.cfg.WebSources.Enabled {
		return errors.New("web sources are switched off in the configuration (web_sources.enabled)")
	}
	if err := p.client.Available(); err != nil {
		switch {
		case errors.Is(err, websource.ErrNotInstalled):
			return errors.New("yt-dlp is not installed — web sources cannot be extracted without it")
		case errors.Is(err, websource.ErrNoProxy):
			return errors.New("the VPN proxy address is not configured, and web sources are never fetched outside the tunnel")
		}
		return err
	}
	return nil
}

// resolve produces a currently-valid media URL for a source, using the cache and
// single-flighting concurrent callers.
func (p *webPlayer) resolve(ctx context.Context, ws *store.WebSource, force bool) (websource.Stream, error) {
	if !force {
		if s, ok := p.cachedStream(ws.ID); ok {
			return s, nil
		}
	}
	release, ok := p.resolves.lock(ctx, "web:"+ws.ID)
	if !ok {
		return websource.Stream{}, errors.New("timed out waiting to resolve this link")
	}
	defer release()

	// Re-check after queueing: the request we waited behind may have just resolved it.
	if !force {
		if s, ok := p.cachedStream(ws.ID); ok {
			return s, nil
		}
	}

	info, err := p.client.Inspect(ctx, ws.PageURL)
	if err != nil {
		// Record WHY on the row. An uploader deleting the video, a site changing its
		// player and a broken extractor are indistinguishable from Jellyfin, and this is
		// the only place the difference is ever written down.
		if merr := p.db.MarkWebSourceFailed(ws.ID, err.Error()); merr != nil {
			slog.Error("web: could not record the failure", "id", ws.ID, "err", merr)
		}
		return websource.Stream{}, err
	}
	if merr := p.db.MarkWebSourceOK(ws.ID); merr != nil {
		slog.Error("web: could not record the success", "id", ws.ID, "err", merr)
	}
	p.mu.Lock()
	p.cached[ws.ID] = cachedStream{stream: info.Stream, at: time.Now()}
	p.mu.Unlock()
	return info.Stream, nil
}

func (p *webPlayer) cachedStream(id string) (websource.Stream, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.cached[id]
	if !ok || time.Since(c.at) > resolvedTTL {
		return websource.Stream{}, false
	}
	return c.stream, true
}

func (p *webPlayer) invalidate(id string) {
	p.mu.Lock()
	delete(p.cached, id)
	p.mu.Unlock()
}

// play answers a /play/p/web/{id} request.
//
// It is called from playHandler AFTER the capability token has been checked, so by this
// point the caller has proved it holds a .strm this server wrote.
func (p *webPlayer) play(w http.ResponseWriter, r *http.Request, id string) {
	if err := p.enabled(); err != nil {
		slog.Warn("web: play refused", "id", id, "reason", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	ws, err := p.db.GetWebSource(id)
	if err != nil {
		httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
		return
	}
	if ws == nil {
		// The .strm exists but the source does not — the entry was deleted and Jellyfin
		// has not rescanned. A 404 is the honest answer and stops the player retrying.
		http.Error(w, "that link is no longer in the library", http.StatusNotFound)
		return
	}

	stream, err := p.resolve(r.Context(), ws, false)
	if err != nil {
		slog.Warn("web: could not resolve", "id", id, "err", err)
		http.Error(w, playFailureMessage(err), playFailureStatus(err))
		return
	}
	p.streamWebSource(w, r, ws, stream)
}

// playFailureStatus maps an extraction failure onto a status code a player can act on.
// The distinction that matters is permanent versus transient: a 404 stops Jellyfin
// retrying a video that no longer exists, while a 502 leaves the door open for a link
// that failed because the site was briefly down.
func playFailureStatus(err error) int {
	switch {
	case errors.Is(err, websource.ErrNoProgressiveFormat), errors.Is(err, websource.ErrUnsupportedURL):
		return http.StatusNotImplemented
	case errors.Is(err, websource.ErrNotInstalled), errors.Is(err, websource.ErrNoProxy):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

// playFailureMessage is what the player and the logs see. yt-dlp's own line is kept for
// the transient cases because it is genuinely the useful one ("Video unavailable",
// "This video is private"), and it contains no server path or secret.
func playFailureMessage(err error) string {
	if errors.Is(err, websource.ErrNoProgressiveFormat) {
		return "that video is only offered as an adaptive stream, which cannot be proxied yet"
	}
	return err.Error()
}

// hopByHop are the headers that describe THIS connection rather than the resource, and
// so must not be copied from the upstream response to the client.
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true,
}

// streamWebSource proxies the media through this process, over the tunnel.
//
// PROXYING RATHER THAN REDIRECTING was a deliberate choice, and it is the expensive one:
// every byte crosses this box twice. A 302 to the CDN would cost nothing — and would
// have the Apple TV connect to the site directly, putting the user's home address on the
// request and defeating the entire point of the namespace. It would also break outright
// on the many sites that check Referer, since a redirected player sends its own.
//
// The headers below are why it works at all. yt-dlp reports the exact User-Agent and
// Referer the site's CDN expects for that format, plus any cookie the extractor picked
// up; a proxy that dropped them would get a 403 from the CDN for a URL that is
// perfectly valid.
func (p *webPlayer) streamWebSource(w http.ResponseWriter, r *http.Request, ws *store.WebSource, stream websource.Stream) {
	resp, err := p.fetch(r, stream)
	if err != nil {
		slog.Warn("web: upstream fetch failed", "id", ws.ID, "err", err)
		http.Error(w, "could not reach the video (is the VPN up?)", http.StatusBadGateway)
		return
	}

	// A signed URL that expired inside the cache window: throw the entry away, extract
	// once more, and try again. This is the second half of the resolvedTTL bargain — it
	// is what lets the TTL be long enough to make seeking cheap without making a short
	// expiry fatal. Exactly one retry: if a freshly extracted URL is also refused, the
	// problem is not staleness.
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone {
		resp.Body.Close()
		slog.Info("web: the media URL was refused, re-resolving once", "id", ws.ID, "status", resp.StatusCode)
		p.invalidate(ws.ID)
		fresh, rerr := p.resolve(r.Context(), ws, true)
		if rerr != nil {
			http.Error(w, playFailureMessage(rerr), playFailureStatus(rerr))
			return
		}
		if resp, err = p.fetch(r, fresh); err != nil {
			http.Error(w, "could not reach the video (is the VPN up?)", http.StatusBadGateway)
			return
		}
	}
	defer resp.Body.Close()

	// Copy the response headers through, minus the hop-by-hop ones and minus anything
	// that would hand the site's state to the LAN client. Set-Cookie in particular: the
	// cookies belong to this proxy's conversation with the CDN, and forwarding them
	// would plant a tube site's cookie in the player.
	for k, vals := range resp.Header {
		lk := strings.ToLower(k)
		if hopByHop[lk] || lk == "set-cookie" {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	if w.Header().Get("Accept-Ranges") == "" {
		// Jellyfin will not offer seeking without it, and every progressive format this
		// accepts supports ranges by definition.
		w.Header().Set("Accept-Ranges", "bytes")
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// Overwhelmingly this is the player seeking or closing the connection, which is
		// normal traffic and not an error worth a warning.
		slog.Debug("web: stream copy ended", "id", ws.ID, "err", err)
	}
}

// fetch issues the upstream request for one stream, carrying the client's Range and the
// site-specific headers the format requires.
func (p *webPlayer) fetch(r *http.Request, stream websource.Stream) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, stream.URL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range stream.Headers {
		// Range is the client's to set, never the extractor's: a Range copied from the
		// format metadata would pin every request to the same byte window.
		if strings.EqualFold(k, "Range") {
			continue
		}
		req.Header.Set(k, v)
	}
	if stream.Cookie != "" {
		req.Header.Set("Cookie", stream.Cookie)
	}
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	return p.http.Do(req)
}

// ── The dashboard API ─────────────────────────────────────────────────────────

// maxWebSourceTitleLen bounds a title before it becomes a FILENAME. library.WriteMovieStrm
// sanitises what it is given, but a 40,000-character title extracted from a hostile page
// would still produce a path no filesystem accepts, and the failure would come from deep
// inside an os call rather than from here.
const maxWebSourceTitleLen = 200

// registerWebSourceAPI wires the dashboard endpoints.
//
// All four require an ADMIN session. Adding a web source writes a file into a Jellyfin
// media root and runs an extractor against an arbitrary URL — that is an operator
// action, not something to expose to every account that can browse the library.
func registerWebSourceAPI(mux *http.ServeMux, p *webPlayer, db *store.Store, cfg *config.Config, jf *jellyfin.Client) {
	// GET /api/websources/status — can this feature be used, and with what?
	//
	// The dashboard asks before rendering the form, so a box without yt-dlp shows one
	// clear sentence instead of a button that fails on click. The yt-dlp version is in
	// the answer because an extractor failure on a site that used to work is nearly
	// always an out-of-date yt-dlp, and this is the first thing anyone will ask for.
	mux.Handle("GET /api/websources/status", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{"enabled": false, "proxy": cfg.WebSourcesProxyAddr()}
		if err := p.enabled(); err != nil {
			out["reason"] = err.Error()
			jsonOK(w, out)
			return
		}
		out["enabled"] = true
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if v, err := p.client.Version(ctx); err == nil {
			out["ytdlp_version"] = v
		}
		jsonOK(w, out)
	})))

	// GET /api/websources — everything added so far, with its health.
	mux.Handle("GET /api/websources", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		list, err := db.ListWebSources()
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the web sources", err)
			return
		}
		if list == nil {
			list = []*store.WebSource{}
		}
		jsonOK(w, list)
	})))

	// POST /api/websources/preview — extract a URL and show what it is, WITHOUT adding it.
	//
	// A separate step from the add on purpose. Extraction is the only way to find out
	// whether a link is supported at all, what it is called, and whether it is a video
	// rather than a playlist or a live stream — and a user pasting a link deserves to see
	// that before a file appears in their library under a title they did not choose.
	mux.Handle("POST /api/websources/preview", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := p.enabled(); err != nil {
			jsonErr(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		var req struct {
			URL string `json:"url"`
		}
		if err := decodeSmallJSON(w, r, &req); err != nil {
			jsonErr(w, "malformed or oversized request body", http.StatusBadRequest)
			return
		}
		pageURL, err := websource.ValidatePageURL(req.URL)
		if err != nil {
			jsonErr(w, err.Error(), http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
		defer cancel()
		info, err := p.client.Inspect(ctx, pageURL)
		if err != nil {
			slog.Warn("web: preview failed", "err", err)
			jsonErr(w, err.Error(), http.StatusBadGateway)
			return
		}
		id := store.WebSourceID(pageURL)
		existing, _ := db.GetWebSource(id)
		// The response carries NO media URL. It is signed, short-lived and useless to the
		// browser, and an endpoint that hands one out is an endpoint that can be used to
		// launder a direct link to the site through this server.
		jsonOK(w, map[string]any{
			"id": id, "page_url": pageURL,
			"title": info.Title, "uploader": info.Uploader, "extractor": info.Extractor,
			"duration_seconds": info.Duration, "thumbnail_url": info.ThumbnailURL,
			"height": info.Stream.Height, "ext": info.Stream.Ext,
			"size_bytes":     info.Stream.SizeBytes,
			"already_added":  existing != nil,
			"age_restricted": info.AgeLimit >= 18,
		})
	})))

	// POST /api/websources — add the link to a library.
	mux.Handle("POST /api/websources", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := p.enabled(); err != nil {
			jsonErr(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		var req struct {
			URL     string `json:"url"`
			Title   string `json:"title"`
			Library string `json:"library"`
		}
		if err := decodeSmallJSON(w, r, &req); err != nil {
			jsonErr(w, "malformed or oversized request body", http.StatusBadRequest)
			return
		}
		pageURL, err := websource.ValidatePageURL(req.URL)
		if err != nil {
			jsonErr(w, err.Error(), http.StatusBadRequest)
			return
		}

		// A web source is a single video, so it goes into a MOVIE library. There is no
		// season or episode to give it, and /play/p/web/{id} — the shape the .strm will
		// carry — is the movie route.
		lib := cfg.FindLibrary(req.Library)
		if req.Library == "" {
			lib = cfg.DefaultLibrary("movie")
		}
		if lib == nil || lib.Type != "movie" || lib.Path == "" {
			// The same indistinguishable refusal the ingest API gives, for the same
			// reason: a caller that can tell "no such library" from "wrong type" can
			// enumerate the operator's library names one guess at a time.
			jsonErr(w, errUnknownLibrary.Error(), http.StatusBadRequest)
			return
		}

		// Extract before writing anything. A successful extraction is the only proof that
		// the link is worth an entry, and doing it first means a bad link produces an
		// error message rather than a library entry that never plays.
		ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
		defer cancel()
		info, err := p.client.Inspect(ctx, pageURL)
		if err != nil {
			slog.Warn("web: add failed during extraction", "err", err)
			jsonErr(w, err.Error(), http.StatusBadGateway)
			return
		}

		title := strings.TrimSpace(req.Title)
		if title == "" {
			title = info.Title
		}
		if title == "" {
			title = "Untitled"
		}
		if len([]rune(title)) > maxWebSourceTitleLen || hasControlChars(title) {
			jsonErr(w, fmt.Sprintf("the title must be at most %d printable characters", maxWebSourceTitleLen),
				http.StatusBadRequest)
			return
		}

		id := store.WebSourceID(pageURL)
		ref := playRef{provider: library.ProviderWeb, mediaType: "movie", providerID: id}
		streamURL := playURLFor(cfg.Server.PublicURL, ref)
		if streamURL == "" {
			// playURLFor returns "" for an identity it cannot encode or sign, and a .strm
			// with no capability token is a 403 at playback — a silent failure a user
			// discovers days later.
			slog.Error("web: could not build a play URL", "id", id)
			jsonErr(w, "could not build a play URL for that link", http.StatusInternalServerError)
			return
		}

		existingItem, err := db.GetByProviderIdentity(ref.storeIdentity())
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
			return
		}

		// The year is deliberately blank. A tube upload has an upload date, not a release
		// year, and putting one in the filename would have Jellyfin's metadata agents
		// match it against a film of that name and year.
		strmPath, err := library.WriteMovieStrm(lib.Path, title, "", streamURL)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not write the library file", err)
			return
		}

		_, username, _ := viewerOf(r)
		now := time.Now()
		ws := &store.WebSource{
			ID: id, PageURL: pageURL, Title: info.Title, Uploader: info.Uploader,
			Extractor: info.Extractor, Duration: info.Duration, Thumbnail: info.ThumbnailURL,
			AddedBy: username, AddedAt: now, LastOK: &now,
		}
		if err := db.UpsertWebSource(ws); err != nil {
			if existingItem == nil || existingItem.StrmPath != strmPath {
				if rerr := library.RemoveStrm(strmPath); rerr != nil {
					slog.Error("web: could not roll back the .strm", "err", rerr)
				}
			}
			httpFail(w, r, http.StatusInternalServerError, "could not record that link", err)
			return
		}

		item := &store.Item{
			MediaType: "movie", Title: title, StrmPath: strmPath,
			LibraryName: lib.Name, Status: "ready", Updated: now,
			PosterURL: info.ThumbnailURL, RequestedBy: username,
		}
		item.SetProviderIdentity(library.ProviderWeb, id)
		if err := db.Upsert(item); err != nil {
			if existingItem == nil || existingItem.StrmPath != strmPath {
				if rerr := library.RemoveStrm(strmPath); rerr != nil {
					slog.Error("web: could not roll back the .strm", "err", rerr)
				}
			}
			httpFail(w, r, http.StatusInternalServerError, "could not record that item", err)
			return
		}

		// Renamed: the identity now points at a different file, so the old one is
		// unreachable. Failures are logged rather than returned — the add SUCCEEDED, and
		// reporting failure would make the caller retry a write that already happened.
		if existingItem != nil && existingItem.StrmPath != "" && existingItem.StrmPath != strmPath {
			if rerr := library.RemoveStrm(existingItem.StrmPath); rerr != nil {
				slog.Error("web: could not remove the superseded .strm", "err", rerr)
			}
			if derr := db.DeleteItem(existingItem.StrmPath); derr != nil {
				slog.Error("web: could not remove the superseded library row", "err", derr)
			}
		}

		notifyJellyfinScan(jf)
		slog.Info("web: added a link", "id", id, "extractor", info.Extractor,
			"library", lib.Name, "by", username)
		jsonOK(w, map[string]any{
			"status": "ok", "id": id, "title": title, "library": lib.Name,
			"duration_seconds": info.Duration,
		})
	})))

	// DELETE /api/websources/{id} — remove the link, its library row and its .strm.
	//
	// Unlike the ingest API's delete, this DOES remove the library entry, because here
	// the two are one thing to the person who added them: they pasted a link and got a
	// video in Jellyfin, so deleting it must not leave a file behind that plays nothing.
	mux.Handle("DELETE /api/websources/{id}", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !library.ValidProviderID(id) {
			jsonErr(w, "bad id", http.StatusBadRequest)
			return
		}
		removed := 0
		items, err := db.ItemsByProviderID(library.ProviderWeb, id)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
			return
		}
		for _, it := range items {
			if it.StrmPath != "" {
				if rerr := library.RemoveStrm(it.StrmPath); rerr != nil {
					slog.Error("web: could not remove the .strm", "path", it.StrmPath, "err", rerr)
				}
				if derr := db.DeleteItem(it.StrmPath); derr != nil {
					httpFail(w, r, http.StatusInternalServerError, "could not remove that item", derr)
					return
				}
				removed++
			}
		}
		if err := db.DeleteWebSource(id); err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not remove that link", err)
			return
		}
		p.invalidate(id)
		if removed > 0 {
			notifyJellyfinScan(jf)
		}
		// Idempotent: deleting something already gone is a 200 with removed:0, not a 404.
		jsonOK(w, map[string]any{"status": "ok", "removed": removed})
	})))
}

// decodeSmallJSON reads a small JSON body. The bound is far below the app-wide 1 MiB
// because the largest legitimate body here is a URL and a title; MaxBytesReader makes
// the decode fail rather than buffering the excess.
func decodeSmallJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(v)
}
