package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"jellyfreedom/internal/api"
	"jellyfreedom/internal/config"
	"jellyfreedom/internal/indexer"
	"jellyfreedom/internal/jellyfin"
	"jellyfreedom/internal/library"
	"jellyfreedom/internal/picker"
	"jellyfreedom/internal/store"
	"jellyfreedom/internal/tmdb"
	"jellyfreedom/internal/torrserver"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	dbPath := flag.String("db", "jellyfreedom.db", "path to the SQLite database")
	assetsDir := flag.String("assets", "web", "path to the web assets directory (contains public/ and dashboard/)")
	flag.Parse()
	webAssets = *assetsDir // shared with buildProtectedMux for the dashboard file server

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		slog.Error("failed to open store", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	api.SetStore(db)

	tmdbClient := tmdb.New("")
	indexerClient := indexer.New("", "")
	tsClient := torrserver.New("")
	jfClient := jellyfin.New("", "")

	// Connection config lives in the DB (admin-editable in Settings) so the app
	// ships with no baked-in keys. On first run, seed the DB from config.yaml
	// (if present) so existing deployments keep working, then point each client
	// at the effective values. Updated live by the Settings → Connections UI.
	applyConnections(db, cfg, tmdbClient, indexerClient, jfClient, tsClient)

	// Load any DB-persisted quality/cache overrides over the config.yaml seed.
	loadQualityOverrides(db, cfg)
	loadCacheOverrides(db, cfg)

	// Apply the cache profile to TorrServer so the same binary adapts to the host.
	applyTorrCache(tsClient, cfg)

	api.SetJellyfinClient(jfClient)

	worker := &queueWorker{db: db, tmdb: tmdbClient, indexer: indexerClient, ts: tsClient, jf: jfClient, cfg: cfg}
	go worker.run(context.Background())

	// ── Task registry ──────────────────────────────────────────────────────
	registry := api.NewTaskRegistry()
	api.SetTaskRegistry(registry)

	registry.Register(
		"library-health-check",
		"Re-check resolvability of every library item via the indexer (no TorrServer load). Marks items stale when no release meets the minimum seeder threshold, and revives stale items the moment one is seeded again.",
		"library", "30 min", 30*time.Minute,
		func(ctx context.Context) error {
			return taskLibraryHealthCheck(ctx, db, indexerClient, livePicker(cfg))
		},
	)

	registry.Register(
		"orphan-cleanup",
		"Drop TorrServer torrents that have no matching library entry. Prevents unbounded TorrServer cache growth.",
		"library", "1 hour", time.Hour,
		func(ctx context.Context) error {
			return taskOrphanCleanup(ctx, db, tsClient)
		},
	)

	registry.Register(
		"subscription-check",
		"Check subscribed (airing) TV seasons for newly-released episodes and enqueue them automatically. Retires subscriptions when a show ends.",
		"library", "6 hours", 6*time.Hour,
		func(ctx context.Context) error {
			return taskSubscriptionCheck(ctx, db, tmdbClient)
		},
	)

	registry.Register(
		"indexer-warmup",
		"Run a lightweight Prowlarr search on a schedule to keep FlareSolverr/indexers warm, so real requests don't hit a 90s+ cold start after idle.",
		"system", "20 min", 20*time.Minute,
		func(ctx context.Context) error {
			n, err := indexerClient.SearchCount("1080p", []int{indexer.CatMovies})
			if err != nil {
				return err
			}
			slog.Info("indexer warmup complete", "results", n)
			return nil
		},
	)

	registry.Register(
		"poster-backfill",
		"Fetch missing poster images from TMDB for library items that were added before poster storage was introduced.",
		"metadata", "manual / startup", 0,
		func(ctx context.Context) error {
			return taskPosterBackfill(ctx, db, tmdbClient)
		},
	)

	registry.Register(
		"session-cleanup",
		"Purge expired login sessions from the database.",
		"system", "1 hour", time.Hour,
		func(ctx context.Context) error {
			db.PurgeSessions()
			return nil
		},
	)

	registry.Register(
		"jellyfin-scan",
		"Trigger a full Jellyfin library scan to pick up newly written .strm files.",
		"library", "manual", 0,
		func(ctx context.Context) error {
			return jfClient.TriggerLibraryScan()
		},
	)

	registry.Register(
		"torrserver-health",
		"Verify TorrServer is reachable and report how many torrents it currently holds.",
		"system", "manual", 0,
		func(ctx context.Context) error {
			_, err := tsClient.List()
			return err
		},
	)

	registry.Register(
		"apply-cache-profile",
		"Push the configured cache profile (RAM/disk, size, connections, disconnect timeout) to TorrServer. Re-run after a TorrServer reset.",
		"system", "manual", 0,
		func(ctx context.Context) error {
			return applyTorrCache(tsClient, cfg)
		},
	)

	// Run poster backfill once at startup (non-blocking)
	go registry.RunNow("poster-backfill")
	// Warm the indexers shortly after boot so the first real request is fast.
	go registry.RunNow("indexer-warmup")

	// Start all scheduled tasks
	registry.Start(context.Background())

	mux := http.NewServeMux()

	// ------------------------------------------------------------------ //
	// Public routes (no auth)
	// ------------------------------------------------------------------ //

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","version":"0.2.1"}`))
	})

	// Lightweight "is anyone watching?" signal for the VPN watchdog / port-forward keeper so
	// they can DEFER non-urgent TorrServer restarts while a stream is live. Public + cheap.
	mux.HandleFunc("GET /api/playback/active", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]any{"active": jfClient.ActivePlaybackCount() > 0})
	})

	// Search UI — served from web/search.html
	mux.Handle("/", http.FileServer(http.Dir(filepath.Join(webAssets, "public"))))

	// TMDB search (used by search UI, public)
	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			jsonErr(w, "q is required", http.StatusBadRequest)
			return
		}
		results, err := tmdbClient.Search(q)
		if err != nil {
			slog.Error("tmdb search", "err", err)
			jsonErr(w, "search failed", http.StatusInternalServerError)
			return
		}
		jsonOK(w, results)
	})

	// TV seasons/episodes (public — used by search UI)
	mux.HandleFunc("GET /api/tmdb/{id}/seasons", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonErr(w, "bad id", http.StatusBadRequest)
			return
		}
		seasons, err := tmdbClient.TVSeasons(id)
		if err != nil {
			jsonErr(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, seasons)
	})

	mux.HandleFunc("GET /api/tmdb/{id}/seasons/{n}/episodes", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		n, err2 := strconv.Atoi(r.PathValue("n"))
		if err != nil || err2 != nil {
			jsonErr(w, "bad id or season", http.StatusBadRequest)
			return
		}
		eps, err := tmdbClient.TVEpisodes(id, n)
		if err != nil {
			jsonErr(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, eps)
	})

	// /api/status is public (search UI shows a health dot)
	mux.HandleFunc("GET /api/status", api.StatusHandler)

	// /api/leak is public (dashboard leak panel calls it)
	mux.HandleFunc("GET /api/leak", api.LeakCheckHandler)

	// Auth endpoints for the media-side inline login
	mux.HandleFunc("GET /api/me", api.MeHandler)
	mux.HandleFunc("POST /api/auth/login", api.APILoginHandler)
	mux.HandleFunc("POST /api/auth/logout", api.APILogoutHandler)

	// /api/libraries — list of configured libraries for the request UI dropdown
	mux.HandleFunc("GET /api/libraries", func(w http.ResponseWriter, r *http.Request) {
		type libInfo struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Default bool   `json:"default"`
			Adult   bool   `json:"adult"`
		}
		out := make([]libInfo, len(cfg.Libraries))
		for i, l := range cfg.Libraries {
			out[i] = libInfo{Name: l.Name, Type: l.Type, Default: l.Default, Adult: l.Adult}
		}
		jsonOK(w, out)
	})

	// /api/library — privacy-aware list of ready items (My Library strip on search page)
	mux.Handle("GET /api/library", api.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viewer := api.UserFromContext(r)
		isAdmin := viewer != nil && viewer.IsAdmin
		username := ""
		if viewer != nil {
			username = viewer.Username
		}
		items, err := db.ListVisible(username, isAdmin)
		if err != nil {
			jsonErr(w, "db error", http.StatusInternalServerError)
			return
		}
		jsonOK(w, items)
	})))

	// /api/library/status — batch status check by TMDB IDs (used by search card badges)
	mux.HandleFunc("GET /api/library/status", func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query().Get("ids")
		if raw == "" {
			jsonOK(w, map[string]any{})
			return
		}
		var ids []int
		for _, s := range strings.Split(raw, ",") {
			if id, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
				ids = append(ids, id)
			}
		}
		statuses, err := db.GetStatusByTMDBIDs(ids)
		if err != nil {
			jsonErr(w, "db error", http.StatusInternalServerError)
			return
		}
		// Convert to string-keyed map for JSON
		out := make(map[string]any, len(statuses))
		for id, item := range statuses {
			out[strconv.Itoa(id)] = map[string]string{
				"status":  item.Status,
				"library": item.LibraryName,
				"title":   item.Title,
			}
		}
		jsonOK(w, out)
	})

	// ── Queue API ───────────────────────────────────────────────────────────────
	mux.Handle("GET /api/queue", api.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viewer := api.UserFromContext(r)
		isAdmin := viewer != nil && viewer.IsAdmin
		username := ""
		if viewer != nil {
			username = viewer.Username
		}
		items, err := db.ListQueue(username, isAdmin)
		if err != nil {
			jsonErr(w, "db error", http.StatusInternalServerError)
			return
		}
		if items == nil {
			items = []*store.QueueItem{}
		}
		jsonOK(w, items)
	})))

	mux.Handle("POST /api/queue/{id}/cancel", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonErr(w, "bad id", http.StatusBadRequest)
			return
		}
		user := api.UserFromContext(r)
		db.CancelQueueItem(id, user.Username, user.IsAdmin)
		jsonOK(w, map[string]string{"status": "cancelled"})
	})))

	mux.Handle("DELETE /api/queue/{id}", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonErr(w, "bad id", http.StatusBadRequest)
			return
		}
		user := api.UserFromContext(r)
		db.DeleteQueueItem(id, user.Username, user.IsAdmin)
		jsonOK(w, map[string]string{"status": "deleted"})
	})))

	mux.HandleFunc("GET /api/queue/count", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]int{"count": db.QueuePendingCount()})
	})

	// ── Subscriptions API ───────────────────────────────────────────────────────
	mux.Handle("GET /api/subscriptions", api.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viewer := api.UserFromContext(r)
		isAdmin := viewer != nil && viewer.IsAdmin
		username := ""
		if viewer != nil {
			username = viewer.Username
		}
		subs, err := db.ListSubscriptions(username, isAdmin)
		if err != nil {
			jsonErr(w, "db error", http.StatusInternalServerError)
			return
		}
		if subs == nil {
			subs = []*store.Subscription{}
		}
		jsonOK(w, subs)
	})))

	mux.Handle("POST /api/subscriptions", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TMDBID    int    `json:"tmdb_id"`
			Season    int    `json:"season"`
			Title     string `json:"title"`
			PosterURL string `json:"poster_url"`
			Library   string `json:"library"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TMDBID == 0 || req.Season == 0 {
			jsonErr(w, "tmdb_id and season required", http.StatusBadRequest)
			return
		}
		user := api.UserFromContext(r)
		if err := db.UpsertSubscription(&store.Subscription{
			TMDBID: req.TMDBID, Season: req.Season, Title: req.Title,
			PosterURL: req.PosterURL, LibraryName: req.Library, RequestedBy: user.Username,
		}); err != nil {
			jsonErr(w, "subscribe failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]any{"status": "subscribed", "tmdb_id": req.TMDBID, "season": req.Season})
	})))

	mux.Handle("DELETE /api/subscriptions/{id}", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonErr(w, "bad id", http.StatusBadRequest)
			return
		}
		user := api.UserFromContext(r)
		db.DeleteSubscription(id, user.Username, user.IsAdmin)
		jsonOK(w, map[string]string{"status": "unsubscribed"})
	})))

	// ── Browse endpoints (homepage carousels) ───────────────────────────────────
	mux.HandleFunc("GET /api/browse/trending", func(w http.ResponseWriter, r *http.Request) {
		results, err := tmdbClient.Trending()
		if err != nil {
			jsonErr(w, err.Error(), http.StatusBadGateway)
			return
		}
		jsonOK(w, results)
	})

	mux.HandleFunc("GET /api/browse/discover", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mediaType := q.Get("type")
		if mediaType != "movie" && mediaType != "tv" {
			jsonErr(w, "type=movie|tv required", http.StatusBadRequest)
			return
		}
		params := map[string]string{"sort_by": "popularity.desc"}
		if genres := q.Get("genres"); genres != "" {
			params["with_genres"] = genres
		}
		if companies := q.Get("companies"); companies != "" {
			params["with_companies"] = companies
		}
		if networks := q.Get("networks"); networks != "" {
			params["with_networks"] = networks
		}
		if sortBy := q.Get("sort"); sortBy != "" {
			params["sort_by"] = sortBy
		}
		results, err := tmdbClient.Discover(mediaType, params)
		if err != nil {
			jsonErr(w, err.Error(), http.StatusBadGateway)
			return
		}
		jsonOK(w, results)
	})

	// ── Release calendar ────────────────────────────────────────────────────────
	mux.Handle("GET /api/calendar", api.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viewer := api.UserFromContext(r)
		isAdmin := viewer != nil && viewer.IsAdmin
		username := ""
		if viewer != nil {
			username = viewer.Username
		}

		// Window: a few days back through ~90 days ahead.
		now := time.Now()
		lo := now.AddDate(0, 0, -3).Format("2006-01-02")
		hi := now.AddDate(0, 0, 90).Format("2006-01-02")

		var entries []tmdb.DatedRelease
		seen := map[string]bool{} // dedupe by tmdb_id+date+subtitle

		add := func(e tmdb.DatedRelease) {
			if e.Date < lo || e.Date > hi {
				return
			}
			key := fmt.Sprintf("%d|%s|%s", e.TMDBID, e.Date, e.Subtitle)
			if seen[key] {
				return
			}
			seen[key] = true
			entries = append(entries, e)
		}

		// 1) The viewer's subscribed shows — exact episode air dates.
		if subs, err := db.ListSubscriptions(username, isAdmin); err == nil {
			for _, sub := range subs {
				eps, err := tmdbClient.TVEpisodes(sub.TMDBID, sub.Season)
				if err != nil {
					continue
				}
				for _, ep := range eps {
					if ep.AirDate == "" {
						continue
					}
					add(tmdb.DatedRelease{
						TMDBID: sub.TMDBID, MediaType: "tv", Title: sub.Title,
						Date: ep.AirDate, Subtitle: fmt.Sprintf("S%02dE%02d", sub.Season, ep.Number),
						PosterURL: sub.PosterURL, Kind: "subscription",
					})
				}
			}
		}

		// 2) Upcoming movies.
		if mv, err := tmdbClient.UpcomingMovies(); err == nil {
			for _, e := range mv {
				add(e)
			}
		}

		// 3) TV premieres / next episodes from on-the-air shows (capped, best-effort).
		if tv, err := tmdbClient.OnTheAirPremieres(12); err == nil {
			for _, e := range tv {
				add(e)
			}
		}

		// Sort by date ascending, then title.
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].Date != entries[j].Date {
				return entries[i].Date < entries[j].Date
			}
			return entries[i].Title < entries[j].Title
		})
		if entries == nil {
			entries = []tmdb.DatedRelease{}
		}
		jsonOK(w, map[string]any{"entries": entries, "today": now.Format("2006-01-02")})
	})))

	// ------------------------------------------------------------------ //
	// POST /request — single episode or movie (enqueue)
	// ------------------------------------------------------------------ //
	mux.Handle("POST /request", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TMDBID    int    `json:"tmdb_id"`
			MediaType string `json:"type"`
			Season    int    `json:"season"`
			Episode   int    `json:"episode"`
			Library   string `json:"library"`
			Magnet    string `json:"magnet"`
			PosterURL string `json:"poster_url"`
			Title     string `json:"title"`
			Year      string `json:"year"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TMDBID == 0 {
			jsonErr(w, "tmdb_id and type required", http.StatusBadRequest)
			return
		}
		if req.MediaType != "movie" && req.MediaType != "tv" {
			jsonErr(w, "type must be 'movie' or 'tv'", http.StatusBadRequest)
			return
		}
		if req.MediaType == "tv" && (req.Season == 0 || req.Episode == 0) {
			jsonErr(w, "season and episode required for tv", http.StatusBadRequest)
			return
		}
		user := api.UserFromContext(r)
		requestedBy := ""
		if user != nil {
			requestedBy = user.Username
		}
		title := req.Title
		if title == "" {
			title = fmt.Sprintf("TMDB #%d", req.TMDBID)
		}
		// Idempotency — keep the queue consistent with the library. A bare re-request (no
		// explicit magnet override) for a title that's already available, or already in
		// flight, must NOT spawn a duplicate queue row. An explicit magnet means the user is
		// deliberately re-picking a release, so we always re-resolve in that case. Stale items
		// fall through to a fresh enqueue (a re-request is how the user revives an expired one).
		if req.Magnet == "" {
			if existing, _ := db.GetByIdentity(req.TMDBID, req.MediaType, req.Season, req.Episode); existing != nil && existing.Status == "ready" {
				db.ClearTerminalQueue(req.TMDBID, req.MediaType, req.Season, req.Episode)
				jsonOK(w, map[string]any{"status": "ready", "already": true, "title": title, "year": req.Year})
				return
			}
			if active, _ := db.ActiveQueueItem(req.TMDBID, req.MediaType, req.Season, req.Episode); active != nil {
				jsonOK(w, map[string]any{"queue_id": active.ID, "status": active.Status, "already": true, "title": title, "year": req.Year})
				return
			}
		}
		// Supersede any finished (failed/cancelled/done) rows for this identity so the new
		// request replaces them rather than piling up next to the library entry.
		db.ClearTerminalQueue(req.TMDBID, req.MediaType, req.Season, req.Episode)
		qItem := &store.QueueItem{
			TMDBID: req.TMDBID, MediaType: req.MediaType,
			Title: title, Year: req.Year, PosterURL: req.PosterURL,
			Season: req.Season, Episode: req.Episode,
			LibraryName: req.Library, RequestedBy: requestedBy,
			MagnetOverride: req.Magnet,
		}
		id, err := db.Enqueue(qItem)
		if err != nil {
			jsonErr(w, "enqueue failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		slog.Info("enqueued request", "id", id, "title", title, "type", req.MediaType)
		jsonOK(w, map[string]any{
			"queue_id": id,
			"status":   "pending",
			"title":    title,
			"year":     req.Year,
		})
	})))

	// ------------------------------------------------------------------ //
	// POST /request/season — enqueue all episodes of a season
	// ------------------------------------------------------------------ //
	mux.Handle("POST /request/season", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TMDBID    int    `json:"tmdb_id"`
			Season    int    `json:"season"`
			Library   string `json:"library"`
			PosterURL string `json:"poster_url"`
			Title     string `json:"title"`
			Year      string `json:"year"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TMDBID == 0 || req.Season == 0 {
			jsonErr(w, "tmdb_id and season required", http.StatusBadRequest)
			return
		}
		episodes, err := tmdbClient.TVEpisodes(req.TMDBID, req.Season)
		if err != nil {
			jsonErr(w, "failed to get episode list: "+err.Error(), http.StatusInternalServerError)
			return
		}
		user := api.UserFromContext(r)
		requestedBy := ""
		if user != nil {
			requestedBy = user.Username
		}
		var queued []int64
		skipped := 0
		for _, ep := range episodes {
			// Skip episodes already ready in the library or already in flight in the
			// queue — makes "Request All" / "Request Remaining" idempotent.
			if db.EpisodeActive(req.TMDBID, req.Season, ep.Number) {
				skipped++
				continue
			}
			title := req.Title
			if title == "" {
				title = fmt.Sprintf("TMDB #%d", req.TMDBID)
			}
			// Supersede a prior failed/cancelled attempt for this episode (EpisodeActive only
			// skips ready/in-flight, so a failed episode re-enqueues here) — drop the stale row
			// so it can't keep painting the season ring red after the retry succeeds.
			db.ClearTerminalQueue(req.TMDBID, "tv", req.Season, ep.Number)
			qItem := &store.QueueItem{
				TMDBID: req.TMDBID, MediaType: "tv",
				Title: title, Year: req.Year, PosterURL: req.PosterURL,
				Season: req.Season, Episode: ep.Number,
				LibraryName: req.Library, RequestedBy: requestedBy,
			}
			id, err := db.Enqueue(qItem)
			if err == nil {
				queued = append(queued, id)
			}
		}
		// If the show is still airing, auto-subscribe so new episodes are grabbed
		// as they release (idempotent per tmdb_id+season).
		subscribed := false
		if details, err := tmdbClient.Details(req.TMDBID, "tv"); err == nil && details.IsAiring() {
			title := req.Title
			if title == "" {
				title = details.Title
			}
			db.UpsertSubscription(&store.Subscription{
				TMDBID: req.TMDBID, Season: req.Season, Title: title,
				PosterURL: req.PosterURL, LibraryName: req.Library, RequestedBy: requestedBy,
			})
			subscribed = true
			slog.Info("auto-subscribed to airing season", "tmdb", req.TMDBID, "season", req.Season)
		}

		jsonOK(w, map[string]any{
			"enqueued":   len(queued),
			"skipped":    skipped,
			"total":      len(episodes),
			"season":     req.Season,
			"subscribed": subscribed,
		})
	})))

	// Stream proxy — lets LAN clients (Apple TV, browser) fetch torrent streams
	// directly without Jellyfin transcoding. The .strm file points here; we
	// forward bytes from TorrServer including Range headers for seeking.
	// streamProxy forwards a Range request to TorrServer's /stream for the given hash+index.
	// Shared by the legacy /proxy/stream path and Resolve-at-Play (/play).
	streamProxy := func(w http.ResponseWriter, r *http.Request, hash string, index int) {
		target := fmt.Sprintf("%s/stream?link=%s&index=%d&play", tsClient.BaseURL(), hash, index)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
		if err != nil {
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		if rng := r.Header.Get("Range"); rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}

	// Legacy hash-pinned stream URL (older .strm files written before Resolve-at-Play).
	// New .strm files use /play/... (identity-keyed, self-healing) instead.
	mux.HandleFunc("GET /proxy/stream", func(w http.ResponseWriter, r *http.Request) {
		link := r.URL.Query().Get("link")
		idx := r.URL.Query().Get("index")
		if link == "" || idx == "" {
			http.Error(w, "missing link or index", http.StatusBadRequest)
			return
		}
		index, _ := strconv.Atoi(idx)
		// The queue drops torrents after validating, so re-add on demand (with the
		// stored full magnet → trackers) if it isn't loaded. No-op once it's live.
		if it, _ := db.GetByHash(link); it != nil {
			tsClient.EnsureLoaded(link, it.Magnet, it.Title, 8)
		} else {
			tsClient.EnsureLoaded(link, "", "", 8)
		}
		streamProxy(w, r, link, index)
	})

	// ------------------------------------------------------------------ //
	// Resolve-at-Play — the .strm points here with a STABLE identity URL. We pick the best
	// CURRENTLY-live release at play time, so the library self-heals against seeder decay.
	// (DECISIONS.md D9, ARCHITECTURE.md §10.)
	// ------------------------------------------------------------------ //
	playHandler := func(w http.ResponseWriter, r *http.Request, mediaType string, tmdbID, season, episode int) {
		// This item is now playing — stop any keep-warm loop for it; real playback takes over.
		cancelWarm(tmdbID, season, episode)
		item, _ := db.GetByIdentity(tmdbID, mediaType, season, episode)
		// Fast path: a cached release that's still alive (resolves a file list quickly). Re-derive
		// the file index from the LIVE file list rather than trusting item.FileIndex — a stale or
		// buggy cached index (e.g. a legacy index=0) would otherwise stream the wrong/empty file.
		if item != nil && item.InfoHash != "" {
			if tsClient.EnsureLoaded(item.InfoHash, item.Magnet, item.Title, 12) {
				if idx, length, ok := resolveFileIndex(tsClient, item.InfoHash, mediaType, season, episode); ok && tsClient.WaitConnectable(item.InfoHash, 12) {
					if idx != item.FileIndex {
						item.FileIndex = idx
						db.Upsert(item)
					}
					maybePreWarm(r, worker, mediaType, tmdbID, season, episode, length)
					streamProxy(w, r, item.InfoHash, idx)
					return
				}
			}
			slog.Info("play: cached release unusable/ghost, re-resolving", "tmdb", tmdbID, "s", season, "e", episode, "hash", item.InfoHash)
		}
		// Slow path: resolve the best live release now, cache it, then stream.
		libName := ""
		if item != nil {
			libName = item.LibraryName
		}
		res, err := worker.resolvePlayable(r.Context(), mediaType, libName, tmdbID, season, episode, "", nil)
		if err != nil {
			slog.Warn("play: resolve failed", "tmdb", tmdbID, "s", season, "e", episode, "err", err)
			http.Error(w, "no playable release available right now: "+err.Error(), http.StatusBadGateway)
			return
		}
		worker.cacheResolved(res, item, mediaType, tmdbID, season, episode)
		maybePreWarm(r, worker, mediaType, tmdbID, season, episode, res.lengthBytes)
		streamProxy(w, r, res.hash, res.fileIndex)
	}
	mux.HandleFunc("GET /play/movie/{tmdb}", func(w http.ResponseWriter, r *http.Request) {
		tmdbID, err := strconv.Atoi(r.PathValue("tmdb"))
		if err != nil {
			http.Error(w, "bad tmdb id", http.StatusBadRequest)
			return
		}
		playHandler(w, r, "movie", tmdbID, 0, 0)
	})
	mux.HandleFunc("GET /play/tv/{tmdb}/{season}/{episode}", func(w http.ResponseWriter, r *http.Request) {
		tmdbID, e1 := strconv.Atoi(r.PathValue("tmdb"))
		season, e2 := strconv.Atoi(r.PathValue("season"))
		episode, e3 := strconv.Atoi(r.PathValue("episode"))
		if e1 != nil || e2 != nil || e3 != nil {
			http.Error(w, "bad tv path", http.StatusBadRequest)
			return
		}
		playHandler(w, r, "tv", tmdbID, season, episode)
	})

	// ------------------------------------------------------------------ //
	// Rich TMDB details + release list (public read, used by modal)
	// ------------------------------------------------------------------ //

	mux.HandleFunc("GET /api/tmdb/{id}/full", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonErr(w, "bad id", http.StatusBadRequest)
			return
		}
		mediaType := r.URL.Query().Get("type")
		if mediaType != "movie" && mediaType != "tv" {
			jsonErr(w, "type=movie|tv required", http.StatusBadRequest)
			return
		}
		details, err := tmdbClient.RichDetails(id, mediaType)
		if err != nil {
			jsonErr(w, "tmdb error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, details)
	})

	mux.HandleFunc("GET /api/releases", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		tmdbID, err := strconv.Atoi(q.Get("tmdb_id"))
		mediaType := q.Get("type")
		if err != nil || (mediaType != "movie" && mediaType != "tv") {
			jsonErr(w, "tmdb_id and type required", http.StatusBadRequest)
			return
		}
		details, err := tmdbClient.Details(tmdbID, mediaType)
		if err != nil {
			jsonErr(w, "metadata lookup failed", http.StatusInternalServerError)
			return
		}
		var query string
		cats := []int{indexer.CatMovies}
		if mediaType == "tv" {
			cats = []int{indexer.CatTV}
			season, _ := strconv.Atoi(q.Get("season"))
			episode, _ := strconv.Atoi(q.Get("episode"))
			if season > 0 && episode > 0 {
				query = fmt.Sprintf("%s S%02dE%02d", details.Title, season, episode)
			} else {
				query = details.Title
			}
		} else {
			query = details.Title
			if details.Year != "" {
				query += " " + details.Year
			}
		}
		releases, err := indexerClient.Search(query, cats)
		if err != nil {
			jsonErr(w, "search failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		scored := picker.Score(releases, livePicker(cfg), details.Title, details.Year)
		jsonOK(w, scored)
	})

	// Remove — fully deletes an item from the library (user-initiated, not expiry).
	// Accessible to any logged-in user so the media UI can remove items.
	mux.Handle("POST /api/library/{hash}/drop", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		item, err := db.GetByHash(hash)
		if err != nil || item == nil {
			jsonErr(w, "not found", http.StatusNotFound)
			return
		}
		library.RemoveStrm(item.StrmPath)
		db.DeleteItem(item.StrmPath)
		// Only drop the torrent if no other library rows still reference this hash
		// (season packs share one hash across episodes).
		if n, _ := db.CountByHash(hash); n == 0 {
			tsClient.Drop(hash)
		}
		jfClient.TriggerLibraryScan()
		jsonOK(w, map[string]string{"status": "removed", "hash": hash})
	})))

	// removeItems deletes each item (row + .strm) then drops any torrent no longer
	// referenced by a remaining library row — safe for season packs that share a hash.
	removeItems := func(items []*store.Item) int {
		for _, it := range items {
			library.RemoveStrm(it.StrmPath)
			db.DeleteItem(it.StrmPath)
		}
		seen := map[string]bool{}
		for _, it := range items {
			if it.InfoHash == "" || seen[it.InfoHash] {
				continue
			}
			seen[it.InfoHash] = true
			if n, _ := db.CountByHash(it.InfoHash); n == 0 {
				tsClient.Drop(it.InfoHash)
			}
		}
		if len(items) > 0 {
			jfClient.TriggerLibraryScan()
		}
		return len(items)
	}

	// Remove an entire series.
	mux.Handle("POST /api/library/series/{tmdbid}/drop", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("tmdbid"))
		if err != nil {
			jsonErr(w, "bad tmdb id", http.StatusBadRequest)
			return
		}
		items, err := db.ItemsByTMDB(id, "tv")
		if err != nil {
			jsonErr(w, "db error", http.StatusInternalServerError)
			return
		}
		n := removeItems(items)
		jsonOK(w, map[string]any{"status": "removed", "count": n})
	})))

	// Remove all episodes of one season.
	mux.Handle("POST /api/library/season/drop", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TMDBID int `json:"tmdb_id"`
			Season int `json:"season"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TMDBID == 0 {
			jsonErr(w, "tmdb_id and season required", http.StatusBadRequest)
			return
		}
		all, _ := db.ItemsByTMDB(req.TMDBID, "tv")
		var inSeason []*store.Item
		for _, it := range all {
			if it.Season == req.Season {
				inSeason = append(inSeason, it)
			}
		}
		n := removeItems(inSeason)
		jsonOK(w, map[string]any{"status": "removed", "count": n})
	})))

	// Remove a single episode (by tmdb_id+season+episode — never by shared hash).
	mux.Handle("POST /api/library/episode/drop", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TMDBID  int `json:"tmdb_id"`
			Season  int `json:"season"`
			Episode int `json:"episode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TMDBID == 0 {
			jsonErr(w, "tmdb_id, season, episode required", http.StatusBadRequest)
			return
		}
		item, err := db.GetEpisode(req.TMDBID, req.Season, req.Episode)
		if err != nil || item == nil {
			jsonErr(w, "episode not in library", http.StatusNotFound)
			return
		}
		removeItems([]*store.Item{item})
		jsonOK(w, map[string]string{"status": "removed"})
	})))

	// ── Settings API (admin only) ─────────────────────────────────────────────
	// Public read of whether core services are configured (media UI shows a banner).
	mux.HandleFunc("GET /api/configured", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]bool{
			"tmdb":       tmdbClient.Configured(),
			"prowlarr":   indexerClient.Configured(),
			"jellyfin":   jfClient.Configured(),
			"torrserver": tsClient.Configured(),
		})
	})

	mux.Handle("GET /api/settings", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keySet := func(k string) bool { v, _ := db.GetSetting(k); return v != "" }
		get := func(k string) string { v, _ := db.GetSetting(k); return v }
		settingsMu.RLock()
		quality := map[string]any{
			"min_seeders":  cfg.Picker.MinSeeders,
			"max_size_gb":  cfg.Picker.MaxSizeGB,
			"reject_cam":   cfg.Picker.RejectCAMValue(),
			"video_codecs": strings.Join(cfg.Picker.PreferVideoCodecs, ", "),
			"audio_codecs": strings.Join(cfg.Picker.PreferAudioCodecs, ", "),
			"containers":   strings.Join(cfg.Picker.PreferContainers, ", "),
		}
		c := cfg.TorrServer.Cache
		retr := 0
		if c.RetrackersMode != nil {
			retr = *c.RetrackersMode
		}
		cache := map[string]any{
			"mode": c.Mode, "size_mb": c.SizeMB, "path": c.Path,
			"disconnect_s": c.DisconnectTimeoutS, "connections": c.ConnectionsLimit,
			"retrackers": retr, "upload_kb": c.UploadRateLimitKB,
		}
		settingsMu.RUnlock()
		libs := make([]map[string]any, len(cfg.Libraries))
		for i, l := range cfg.Libraries {
			libs[i] = map[string]any{"name": l.Name, "type": l.Type, "path": l.Path, "default": l.Default}
		}
		jsonOK(w, map[string]any{
			"connections": map[string]any{
				"tmdb":       map[string]any{"key_set": keySet("conn.tmdb.key")},
				"prowlarr":   map[string]any{"url": get("conn.prowlarr.url"), "key_set": keySet("conn.prowlarr.key")},
				"jellyfin":   map[string]any{"url": get("conn.jellyfin.url"), "key_set": keySet("conn.jellyfin.key")},
				"torrserver": map[string]any{"url": get("conn.torrserver.url")},
			},
			"quality":   quality,
			"cache":     cache,
			"libraries": libs,
		})
	})))

	// Save connection settings + reconfigure clients live. Blank key = keep existing.
	mux.Handle("POST /api/settings/connections", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TMDBKey       string `json:"tmdb_key"`
			ProwlarrURL   string `json:"prowlarr_url"`
			ProwlarrKey   string `json:"prowlarr_key"`
			JellyfinURL   string `json:"jellyfin_url"`
			JellyfinKey   string `json:"jellyfin_key"`
			TorrServerURL string `json:"torrserver_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, "bad request", http.StatusBadRequest)
			return
		}
		setIf := func(key, val string, keepBlank bool) {
			if val == "" && keepBlank {
				return // blank means "leave existing key untouched"
			}
			db.SetSetting(key, val)
		}
		setIf("conn.tmdb.key", req.TMDBKey, true)
		setIf("conn.prowlarr.url", req.ProwlarrURL, false)
		setIf("conn.prowlarr.key", req.ProwlarrKey, true)
		setIf("conn.jellyfin.url", req.JellyfinURL, false)
		setIf("conn.jellyfin.key", req.JellyfinKey, true)
		setIf("conn.torrserver.url", req.TorrServerURL, false)
		// Reconfigure clients live from the now-updated DB values.
		applyConnections(db, cfg, tmdbClient, indexerClient, jfClient, tsClient)
		jsonOK(w, map[string]string{"status": "saved"})
	})))

	// Test a connection with the supplied (unsaved) credentials.
	mux.Handle("POST /api/settings/connections/test", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Service string `json:"service"`
			URL     string `json:"url"`
			Key     string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, "bad request", http.StatusBadRequest)
			return
		}
		// Blank field = test the currently-saved value (key is masked in the UI).
		if req.Key == "" {
			req.Key, _ = db.GetSetting("conn." + req.Service + ".key")
		}
		if req.URL == "" {
			req.URL, _ = db.GetSetting("conn." + req.Service + ".url")
		}
		ok, msg := testConnection(r.Context(), req.Service, strings.TrimRight(req.URL, "/"), req.Key)
		jsonOK(w, map[string]any{"ok": ok, "message": msg})
	})))

	// Save quality settings, applied live to the picker.
	mux.Handle("POST /api/settings/quality", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MinSeeders  *int    `json:"min_seeders"`
			MaxSizeGB   *int    `json:"max_size_gb"`
			RejectCAM   *bool   `json:"reject_cam"`
			VideoCodecs *string `json:"video_codecs"`
			AudioCodecs *string `json:"audio_codecs"`
			Containers  *string `json:"containers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, "bad request", http.StatusBadRequest)
			return
		}
		settingsMu.Lock()
		if req.MinSeeders != nil {
			cfg.Picker.MinSeeders = *req.MinSeeders
			db.SetSetting("quality.min_seeders", strconv.Itoa(*req.MinSeeders))
		}
		if req.MaxSizeGB != nil {
			cfg.Picker.MaxSizeGB = *req.MaxSizeGB
			db.SetSetting("quality.max_size_gb", strconv.Itoa(*req.MaxSizeGB))
		}
		if req.RejectCAM != nil {
			b := *req.RejectCAM
			cfg.Picker.RejectCAM = &b
			db.SetSetting("quality.reject_cam", strconv.FormatBool(b))
		}
		if req.VideoCodecs != nil {
			cfg.Picker.PreferVideoCodecs = splitCSV(*req.VideoCodecs)
			db.SetSetting("quality.video_codecs", *req.VideoCodecs)
		}
		if req.AudioCodecs != nil {
			cfg.Picker.PreferAudioCodecs = splitCSV(*req.AudioCodecs)
			db.SetSetting("quality.audio_codecs", *req.AudioCodecs)
		}
		if req.Containers != nil {
			cfg.Picker.PreferContainers = splitCSV(*req.Containers)
			db.SetSetting("quality.containers", *req.Containers)
		}
		settingsMu.Unlock()
		jsonOK(w, map[string]string{"status": "saved"})
	})))

	// Save cache settings, applied live to TorrServer.
	mux.Handle("POST /api/settings/cache", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Mode        *string `json:"mode"`
			SizeMB      *int    `json:"size_mb"`
			Path        *string `json:"path"`
			DisconnectS *int    `json:"disconnect_s"`
			Connections *int    `json:"connections"`
			Retrackers  *int    `json:"retrackers"`
			UploadKB    *int    `json:"upload_kb"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, "bad request", http.StatusBadRequest)
			return
		}
		c := &cfg.TorrServer.Cache
		if req.Mode != nil {
			c.Mode = *req.Mode
			db.SetSetting("cache.mode", *req.Mode)
		}
		if req.SizeMB != nil {
			c.SizeMB = *req.SizeMB
			db.SetSetting("cache.size_mb", strconv.Itoa(*req.SizeMB))
		}
		if req.Path != nil {
			c.Path = *req.Path
			db.SetSetting("cache.path", *req.Path)
		}
		if req.DisconnectS != nil {
			c.DisconnectTimeoutS = *req.DisconnectS
			db.SetSetting("cache.disconnect_s", strconv.Itoa(*req.DisconnectS))
		}
		if req.Connections != nil {
			c.ConnectionsLimit = *req.Connections
			db.SetSetting("cache.connections", strconv.Itoa(*req.Connections))
		}
		if req.Retrackers != nil {
			c.RetrackersMode = req.Retrackers
			db.SetSetting("cache.retrackers", strconv.Itoa(*req.Retrackers))
		}
		if req.UploadKB != nil {
			c.UploadRateLimitKB = *req.UploadKB
			db.SetSetting("cache.upload_kb", strconv.Itoa(*req.UploadKB))
		}
		if err := applyTorrCache(tsClient, cfg); err != nil {
			jsonErr(w, "saved, but applying to TorrServer failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		jsonOK(w, map[string]string{"status": "saved"})
	})))

	// ------------------------------------------------------------------ //
	// VPN config management (admin) — upload/list/activate/delete/download WireGuard configs
	// from the browser. Configs live in the orchestrator-owned dir (cfg.VPNConfigDir); upload
	// never auto-activates. Activation materializes the chosen config as the live tunnel and
	// restarts the netns stack (reuses existing restart perms — no new sudoers).
	// ------------------------------------------------------------------ //
	vpnDir := cfg.VPNConfigDir()

	mux.Handle("GET /api/vpn/configs", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activeB, _ := os.ReadFile(filepath.Join(vpnDir, ".active"))
		active := strings.TrimSpace(string(activeB))
		entries, _ := os.ReadDir(vpnDir)
		type cfgInfo struct {
			Name     string `json:"name"`
			Endpoint string `json:"endpoint"`
			Active   bool   `json:"active"`
			Uploaded string `json:"uploaded"`
		}
		out := []cfgInfo{}
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".conf") || n == "wg0-vpntorrent.conf" || strings.HasPrefix(n, ".") {
				continue
			}
			slug := strings.TrimSuffix(n, ".conf")
			content, _ := os.ReadFile(filepath.Join(vpnDir, n))
			up := ""
			if fi, err := e.Info(); err == nil {
				up = fi.ModTime().Format("2006-01-02 15:04")
			}
			out = append(out, cfgInfo{Name: slug, Endpoint: vpnParseEndpoint(string(content)), Active: slug == active, Uploaded: up})
		}
		jsonOK(w, out)
	})))

	mux.Handle("POST /api/vpn/configs", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
			jsonErr(w, "bad request", http.StatusBadRequest)
			return
		}
		slug := vpnSanitizeSlug(req.Name)
		if !vpnValidSlug(slug) {
			jsonErr(w, "invalid config name (use letters/digits/._- , not 'wg0-vpntorrent')", http.StatusBadRequest)
			return
		}
		if len(req.Content) == 0 || len(req.Content) > 16*1024 {
			jsonErr(w, "config empty or larger than 16 KB", http.StatusBadRequest)
			return
		}
		if !vpnIsWireGuardConf(req.Content) {
			jsonErr(w, "not a WireGuard config (needs [Interface], PrivateKey, [Peer], Endpoint)", http.StatusBadRequest)
			return
		}
		if err := os.WriteFile(filepath.Join(vpnDir, slug+".conf"), []byte(vpnStripDNS(req.Content)), 0600); err != nil {
			jsonErr(w, "store failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		slog.Info("vpn config uploaded", "name", slug)
		jsonOK(w, map[string]any{"name": slug, "stored": true})
	})))

	mux.Handle("POST /api/vpn/configs/{name}/activate", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug := vpnSanitizeSlug(r.PathValue("name"))
		if !vpnValidSlug(slug) {
			jsonErr(w, "invalid name", http.StatusBadRequest)
			return
		}
		content, err := os.ReadFile(filepath.Join(vpnDir, slug+".conf"))
		if err != nil {
			jsonErr(w, "config not found", http.StatusNotFound)
			return
		}
		// Materialize as the live config setup-netns.sh reads, then restart the stack so it
		// re-derives the endpoint route + DNS for the new server.
		if err := os.WriteFile(filepath.Join(vpnDir, "wg0-vpntorrent.conf"), []byte(vpnStripDNS(string(content))), 0600); err != nil {
			jsonErr(w, "activate write failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		os.WriteFile(filepath.Join(vpnDir, ".active"), []byte(slug), 0600)
		exec.Command("sudo", "systemctl", "restart", "vpntorrent-netns").Run()
		exec.Command("sudo", "systemctl", "restart", "torrserver-netns").Run()
		slog.Info("vpn config activated", "name", slug)
		jsonOK(w, map[string]any{"activated": slug})
	})))

	mux.Handle("DELETE /api/vpn/configs/{name}", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug := vpnSanitizeSlug(r.PathValue("name"))
		if !vpnValidSlug(slug) {
			jsonErr(w, "invalid name", http.StatusBadRequest)
			return
		}
		activeB, _ := os.ReadFile(filepath.Join(vpnDir, ".active"))
		if strings.TrimSpace(string(activeB)) == slug {
			jsonErr(w, "cannot delete the active config — activate another first", http.StatusConflict)
			return
		}
		if err := os.Remove(filepath.Join(vpnDir, slug+".conf")); err != nil {
			jsonErr(w, "delete failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]any{"deleted": slug})
	})))

	mux.Handle("GET /api/vpn/configs/{name}/download", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug := vpnSanitizeSlug(r.PathValue("name"))
		if !vpnValidSlug(slug) {
			jsonErr(w, "invalid name", http.StatusBadRequest)
			return
		}
		content, err := os.ReadFile(filepath.Join(vpnDir, slug+".conf"))
		if err != nil {
			jsonErr(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+slug+".conf\"")
		w.Write(content)
	})))

	// ------------------------------------------------------------------ //
	// Jellyfin webhook — public (called by Jellyfin server, no session)
	// ------------------------------------------------------------------ //
	mux.HandleFunc("POST /webhook/jellyfin", func(w http.ResponseWriter, r *http.Request) {
		payload, err := jellyfin.ParseWebhook(r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if payload.NotificationType != "PlaybackStop" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		strmPath, err := jfClient.GetItemPath(payload.ItemID)
		if err != nil || strmPath == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		item, err := db.GetByStrmPath(strmPath)
		if err != nil || item == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if active := jfClient.ActiveSessionsForItem(payload.ItemID); active > 0 {
			slog.Info("playback stopped but other sessions active, not dropping",
				"title", item.Title, "active_sessions", active)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Playback ended: free the TorrServer cache, but the item STAYS 'ready'. It's still
		// resolvable, and /play re-adds the torrent on demand next time. Staleness is decided
		// solely by resolvability (the health check) — never by playback ending. Drop only when
		// no OTHER live item (e.g. another episode from the same season pack) needs this hash.
		if n, _ := db.CountReadyByHashExcept(item.InfoHash, item.StrmPath); n == 0 {
			if err := tsClient.Drop(item.InfoHash); err == nil {
				slog.Info("torrent dropped after playback stop", "title", item.Title)
			}
		} else {
			slog.Info("torrent kept, other episodes still in library",
				"title", item.Title, "remaining", n)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// ------------------------------------------------------------------ //
	// Dashboard + protected API — admin only
	// ------------------------------------------------------------------ //
	protected := api.RequireAdmin(buildProtectedMux(db, tsClient, jfClient, indexerClient, toPicker(cfg.Picker)))

	mux.HandleFunc("/dashboard/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// Auth pages are always public
		if path == "/dashboard/setup" {
			api.SetupHandler(w, r)
			return
		}
		if path == "/dashboard/login" {
			api.LoginHandler(w, r)
			return
		}
		if path == "/dashboard/logout" {
			api.LogoutHandler(w, r)
			return
		}
		// Redirect to setup on first run
		if api.NeedsSetup() {
			http.Redirect(w, r, "/dashboard/setup", http.StatusFound)
			return
		}
		protected.ServeHTTP(w, r)
	})

	// Protected API routes (public ones already registered above)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		protected.ServeHTTP(w, r)
	})

	// All background work is now managed by the task registry above.

	slog.Info("starting orchestrator", "listen", cfg.Server.Listen)
	if err := http.ListenAndServe(cfg.Server.Listen, mux); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

func buildProtectedMux(db *store.Store, tsClient *torrserver.Client, jfClient *jellyfin.Client,
	indexerClient *indexer.Client, pickerCfg picker.Config) http.Handler {

	mux := http.NewServeMux()

	// Dashboard UI (served from web/dashboard/)
	mux.Handle("GET /dashboard/", http.StripPrefix("/dashboard/", http.FileServer(http.Dir(filepath.Join(webAssets, "dashboard")))))
	mux.Handle("GET /dashboard", http.RedirectHandler("/dashboard/", http.StatusFound))

	// API — tasks
	mux.HandleFunc("GET /api/tasks", api.TasksHandler)
	mux.HandleFunc("POST /api/tasks/{name}/run", api.TaskRunHandler)

	// API — service management
	mux.HandleFunc("GET /api/logs", api.LogsHandler)
	mux.HandleFunc("POST /api/services/{name}/restart", api.ServiceRestartHandler)
	mux.HandleFunc("GET /api/vpn", api.VPNHandler)
	mux.HandleFunc("POST /api/auth/change-password", api.ChangePasswordHandler)

	// API — user management
	mux.HandleFunc("GET /api/users", api.UsersHandler)
	mux.HandleFunc("POST /api/users", api.CreateUserHandler)
	mux.HandleFunc("POST /api/users/import", api.ImportUserHandler)
	mux.HandleFunc("PATCH /api/users/{id}", api.UpdateUserHandler)
	mux.HandleFunc("DELETE /api/users/{id}", api.DeleteUserHandler)
	mux.HandleFunc("GET /api/jellyfin/users", api.JellyfinUsersHandler(jfClient))

	mux.HandleFunc("POST /api/library/{hash}/toggle-private", func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		item, err := db.GetByHash(hash)
		if err != nil || item == nil {
			jsonErr(w, "not found", http.StatusNotFound)
			return
		}
		if err := db.SetPrivate(item.StrmPath, !item.IsPrivate); err != nil {
			jsonErr(w, "update failed", http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]any{"hash": hash, "is_private": !item.IsPrivate})
	})

	mux.HandleFunc("POST /api/library/{hash}/drop", func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		item, err := db.GetByHash(hash)
		if err != nil || item == nil {
			jsonErr(w, "not found", http.StatusNotFound)
			return
		}
		library.RemoveStrm(item.StrmPath)
		db.DeleteItem(item.StrmPath)
		if n, _ := db.CountByHash(hash); n == 0 {
			tsClient.Drop(hash)
		}
		jfClient.TriggerLibraryScan()
		jsonOK(w, map[string]string{"status": "removed", "hash": hash})
	})

	// Debug
	mux.HandleFunc("GET /debug/releases", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			jsonErr(w, "q is required", http.StatusBadRequest)
			return
		}
		releases, err := indexerClient.Search(q, []int{indexer.CatMovies, indexer.CatTV})
		if err != nil {
			jsonErr(w, err.Error(), http.StatusInternalServerError)
			return
		}
		best := picker.Best(releases, pickerCfg)
		jsonOK(w, map[string]any{"total": len(releases), "best": best, "all": releases})
	})

	return mux
}

// queueWorker processes items from the queue one at a time.
type queueWorker struct {
	db      *store.Store
	tmdb    *tmdb.Client
	indexer *indexer.Client
	ts      *torrserver.Client
	jf      *jellyfin.Client
	cfg     *config.Config
	mu      sync.Mutex // prevents concurrent processing
}

func (w *queueWorker) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
			w.mu.Lock()
			w.processNext(ctx)
			w.mu.Unlock()
		}
	}
}

func (w *queueWorker) progress(item *store.QueueItem, msg string) {
	item.Progress = msg
	item.UpdatedAt = time.Now()
	w.db.UpdateQueue(item)
}

func (w *queueWorker) processNext(ctx context.Context) {
	item, err := w.db.NextPending()
	if err != nil || item == nil {
		return
	}
	slog.Info("queue: processing", "id", item.ID, "title", item.Title, "type", item.MediaType)

	res, err := w.resolvePlayable(ctx, item.MediaType, item.LibraryName,
		item.TMDBID, item.Season, item.Episode, item.MagnetOverride,
		func(s string) { w.progress(item, s) })
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down — leave the row 'processing' for restart recovery
		}
		item.Status = "failed"
		item.ErrorMsg = err.Error()
		w.db.UpdateQueue(item)
		return
	}

	w.progress(item, "Writing to library…")
	// Resolve-at-Play: the .strm holds a STABLE identity URL, not the resolved hash. The
	// /play handler picks the best live release each time, so the pointer never rots.
	streamURL := playURL(w.cfg.Server.PublicURL, item.MediaType, item.TMDBID, item.Season, item.Episode)
	var strmPath string
	if item.MediaType == "movie" {
		strmPath, err = library.WriteMovieStrm(res.lib.Path, res.title, res.year, streamURL)
	} else {
		strmPath, err = library.WriteTVStrm(res.lib.Path, res.title, res.year, item.Season, item.Episode, streamURL)
	}
	if err != nil {
		w.ts.Drop(res.hash)
		item.Status = "failed"
		item.ErrorMsg = "Library write failed: " + err.Error()
		w.db.UpdateQueue(item)
		return
	}

	// Drop the previously-cached torrent if this resolve picked a different release.
	if existing, _ := w.db.GetByStrmPath(strmPath); existing != nil && existing.InfoHash != res.hash {
		w.ts.Drop(existing.InfoHash)
	}

	w.db.Upsert(&store.Item{
		TMDBID: item.TMDBID, MediaType: item.MediaType, Title: res.displayTitle,
		Year: res.year, InfoHash: res.hash, FileIndex: res.fileIndex,
		StrmPath: strmPath, LibraryName: res.lib.Name,
		Status: "ready", Seeders: res.seeders, Updated: time.Now(),
		RequestedBy: item.RequestedBy, PosterURL: item.PosterURL,
		Magnet: res.magnet, ReleaseTitle: res.releaseTitle,
		Season: item.Season, Episode: item.Episode, StaleSince: nil,
	})
	// Drop-after-validate: the .strm is written and the chosen hash is cached, so drop now.
	// /play re-adds on demand (and re-resolves if this release later dies). Requesting a whole
	// season therefore leaves ZERO background load.
	w.ts.Drop(res.hash)
	w.jf.TriggerLibraryScan()

	item.Status = "done"
	item.Progress = ""
	item.InfoHash = res.hash
	item.StrmPath = strmPath
	w.db.UpdateQueue(item)
	slog.Info("queue: done (torrent dropped; loads on play)", "id", item.ID, "title", res.displayTitle, "hash", res.hash)
}

// resolveResult is a playable release resolved at request- or play-time.
type resolveResult struct {
	hash         string
	fileIndex    int
	magnet       string
	releaseTitle string
	seeders      int
	title        string // clean show/movie title (for the .strm path)
	displayTitle string // "Show SxxEyy" for tv, else title (for the library row)
	year         string
	lengthBytes  int64 // size of the chosen video file (for near-end pre-warm detection)
	lib          *config.Library
}

// resolvePlayable runs the full search → pick → add → validate pipeline for one title and
// returns a playable (hash, fileIndex) WITHOUT writing a .strm or dropping the torrent on
// success (the caller decides). Shared by the queue worker (request-time) and the /play
// handler (play-time re-resolve when a cached release has decayed). magnetOverride forces a
// specific release; progress (optional) reports stage strings for the queue UI.
func (w *queueWorker) resolvePlayable(ctx context.Context, mediaType, libraryName string, tmdbID, season, episode int, magnetOverride string, progress func(string)) (*resolveResult, error) {
	prog := func(s string) {
		if progress != nil {
			progress(s)
		}
	}

	prog("Looking up metadata…")
	details, err := w.tmdb.Details(tmdbID, mediaType)
	if err != nil {
		return nil, fmt.Errorf("metadata lookup failed: %w", err)
	}

	lib := w.cfg.FindLibrary(libraryName)
	if lib == nil {
		lib = w.cfg.DefaultLibrary(mediaType)
	}
	if lib == nil {
		return nil, fmt.Errorf("no library configured for type %s", mediaType)
	}
	pc := livePickerFor(w.cfg, lib)

	// Build the ordered candidate list (best score first). We try each in turn and commit the
	// FIRST that resolves its file list, matches the episode, validates, AND can actually reach
	// the swarm — skipping "ghost" releases (high scrape seeder-count but zero connectable peers).
	var candidates []indexer.Release
	if magnetOverride != "" {
		candidates = []indexer.Release{{Magnet: magnetOverride, Title: details.Title}}
	} else {
		prog("Searching releases…")
		var query string
		cats := []int{indexer.CatMovies}
		if mediaType == "tv" {
			cats = []int{indexer.CatTV}
			query = fmt.Sprintf("%s S%02dE%02d", details.Title, season, episode)
		} else {
			query = details.Title
			if details.Year != "" {
				query += " " + details.Year
			}
		}
		releases, err := w.indexer.Search(query, cats)
		if err != nil {
			return nil, fmt.Errorf("search failed: %w", err)
		}
		if mediaType == "tv" && len(releases) == 0 {
			fallback := fmt.Sprintf("%s Season %d", details.Title, season)
			releases, _ = w.indexer.Search(fallback, cats)
		}
		// For an episode request, strongly prefer releases whose TITLE carries the exact
		// SxxEyy token — excludes same-named anime ("- 05" numbering) and stray mismatches.
		// Only narrow if at least one such release exists (season packs legitimately lack a
		// per-episode token; the file-level check below guards those).
		if mediaType == "tv" && episode > 0 {
			var labeled []indexer.Release
			for _, r := range releases {
				if torrserver.MatchesEpisode(r.Title, season, episode) {
					labeled = append(labeled, r)
				}
			}
			if len(labeled) > 0 {
				releases = labeled
			}
		}
		// Score sorts best-first; skip CAM when rejecting. Take them in rank order.
		for _, sr := range picker.Score(releases, pc, details.Title, details.Year) {
			if pc.RejectCAM && sr.IsCAM {
				continue
			}
			candidates = append(candidates, sr.Release)
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("no suitable release found")
		}
	}

	displayTitle := details.Title
	if mediaType == "tv" {
		displayTitle = fmt.Sprintf("%s S%02dE%02d", details.Title, season, episode)
	}

	const maxTry = 4
	lastErr := fmt.Errorf("no candidates")
	for n, cand := range candidates {
		if n >= maxTry {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if len(candidates) > 1 {
			prog(fmt.Sprintf("Trying release %d…", n+1))
		} else {
			prog("Adding to TorrServer…")
		}
		hash, err := w.ts.Add(cand.Magnet, details.Title)
		if err != nil {
			// A failing add (e.g. TorrServer 500 = crashed BT client) won't differ between
			// candidates, so don't burn the rest — surface it.
			return nil, fmt.Errorf("TorrServer add failed: %w", err)
		}

		prog("Waiting for file list…")
		var fileIndex int
		var chosenFile *torrserver.FileInfo
		episodeMatched := true
		resolved := false
		for i := 0; i < 10; i++ {
			select {
			case <-ctx.Done():
				w.ts.Drop(hash)
				return nil, ctx.Err()
			default:
			}
			info, statErr := w.ts.Stat(hash)
			if statErr == nil && len(info.Files) > 0 {
				resolved = true
				if mediaType == "tv" {
					fileIndex, episodeMatched = torrserver.EpisodeFileIndex(info.Files, season, episode)
				} else {
					fileIndex = torrserver.BestFileIndex(info.Files)
				}
				for j := range info.Files {
					if info.Files[j].ID == fileIndex {
						chosenFile = &info.Files[j]
						break
					}
				}
				break
			}
			time.Sleep(2 * time.Second)
		}
		if !resolved {
			w.ts.Drop(hash)
			lastErr = fmt.Errorf("no file list (slow/dead swarm)")
			continue
		}
		if mediaType == "tv" && !episodeMatched {
			w.ts.Drop(hash)
			lastErr = fmt.Errorf("S%02dE%02d not in this release (wrong show or mislabeled pack)", season, episode)
			continue
		}
		if chosenFile != nil {
			if err := validateVideoFile(chosenFile, mediaType); err != nil {
				w.ts.Drop(hash)
				lastErr = err
				continue
			}
		}
		// M18: only commit a release that can actually reach the swarm — skip ghosts whose
		// scrape count is high but have no reachable peers (resolve metadata, never stream).
		prog("Checking peers…")
		if !w.ts.WaitConnectable(hash, 15) {
			w.ts.Drop(hash)
			lastErr = fmt.Errorf("no connectable peers (ghost release)")
			continue
		}

		var length int64
		if chosenFile != nil {
			length = chosenFile.Length
		}
		return &resolveResult{
			hash: hash, fileIndex: fileIndex, magnet: cand.Magnet, releaseTitle: cand.Title,
			seeders: cand.Seeders, title: details.Title, displayTitle: displayTitle,
			year: details.Year, lengthBytes: length, lib: lib,
		}, nil
	}
	return nil, fmt.Errorf("no streamable release found (last: %v)", lastErr)
}

// cacheResolved persists a freshly-resolved release as the item's cached pointer so the next
// /play hits the fast path. Called by /play after a re-resolve. The .strm path is stable, so
// this only updates the existing row's hash/index/magnet/seeders.
func (w *queueWorker) cacheResolved(res *resolveResult, existing *store.Item, mediaType string, tmdbID, season, episode int) {
	if existing == nil {
		return // /play only fires from a .strm we wrote, so the row should already exist
	}
	existing.InfoHash = res.hash
	existing.FileIndex = res.fileIndex
	existing.Magnet = res.magnet
	existing.ReleaseTitle = res.releaseTitle
	existing.Seeders = res.seeders
	existing.Status = "ready"
	existing.StaleSince = nil
	existing.Updated = time.Now()
	w.db.Upsert(existing)
}

// resolveFileIndex re-derives the correct 1-based TorrServer file index (and the file's byte
// length) for a loaded torrent from its live file list (TV: the SxxEyy-matched file; movie:
// the largest video). Returns ok=false when there's no valid file — so a stale/buggy cached
// index (e.g. legacy index=0) can never stream the wrong or an empty file.
func resolveFileIndex(ts *torrserver.Client, hash, mediaType string, season, episode int) (idx int, length int64, ok bool) {
	info, err := ts.Stat(hash)
	if err != nil || len(info.Files) == 0 {
		return 0, 0, false
	}
	if mediaType == "tv" {
		var matched bool
		idx, matched = torrserver.EpisodeFileIndex(info.Files, season, episode)
		if !matched || idx <= 0 {
			return 0, 0, false
		}
	} else {
		idx = torrserver.BestFileIndex(info.Files)
		if idx <= 0 {
			return 0, 0, false
		}
	}
	for _, f := range info.Files {
		if f.ID == idx {
			length = f.Length
			break
		}
	}
	return idx, length, true
}

// ---- Pre-warming (M17): make binge auto-advance start instantly ----
// When the current episode nears its end, resolve the NEXT episode and keep its torrent hot
// (loaded + opening buffered) until it's played, defeating the 90s idle-drop. Only one
// keep-warm loop per episode runs at a time; it's cancelled the moment that episode plays.
var (
	preWarmMu      sync.Mutex
	preWarmCancels = map[string]context.CancelFunc{}
)

func warmKey(tmdbID, season, episode int) string {
	return fmt.Sprintf("%d/%d/%d", tmdbID, season, episode)
}

// cancelWarm stops a keep-warm loop (called when that episode actually starts playing, so
// real playback takes over the torrent).
func cancelWarm(tmdbID, season, episode int) {
	k := warmKey(tmdbID, season, episode)
	preWarmMu.Lock()
	if c, ok := preWarmCancels[k]; ok {
		c()
		delete(preWarmCancels, k)
	}
	preWarmMu.Unlock()
}

// parseRangeStart extracts the start byte of an HTTP "bytes=START-END" Range header.
func parseRangeStart(h string) int64 {
	if !strings.HasPrefix(h, "bytes=") {
		return 0
	}
	h = strings.TrimPrefix(h, "bytes=")
	if dash := strings.IndexByte(h, '-'); dash > 0 {
		n, _ := strconv.ParseInt(h[:dash], 10, 64)
		return n
	}
	return 0
}

// maybePreWarm triggers next-episode pre-warming once the player is reading the last ~20% of
// the current TV file (i.e. near the end of playback) — close enough that the warmed torrent
// survives to auto-advance.
func maybePreWarm(r *http.Request, w *queueWorker, mediaType string, tmdbID, season, episode int, length int64) {
	if mediaType != "tv" || length <= 0 {
		return
	}
	if parseRangeStart(r.Header.Get("Range")) < int64(0.80*float64(length)) {
		return
	}
	go w.preWarmNext(tmdbID, season, episode)
}

// preWarmNext resolves the next episode (re-resolving if the cached release died) and keeps
// it hot — periodic small reads reset TorrServer's idle-drop timer and pre-buffer the opening
// — until the episode plays (cancelWarm) or a ~12 min cap.
func (w *queueWorker) preWarmNext(tmdbID, season, episode int) {
	ns, ne := season, episode+1
	k := warmKey(tmdbID, ns, ne)
	preWarmMu.Lock()
	if _, busy := preWarmCancels[k]; busy {
		preWarmMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	preWarmCancels[k] = cancel
	preWarmMu.Unlock()
	defer cancelWarm(tmdbID, ns, ne)

	next, _ := w.db.GetByIdentity(tmdbID, "tv", ns, ne)
	if next == nil {
		return // next episode isn't in the library — nothing to warm
	}
	var hash string
	var idx int
	if next.InfoHash != "" && w.ts.EnsureLoaded(next.InfoHash, next.Magnet, next.Title, 25) {
		if i, _, ok := resolveFileIndex(w.ts, next.InfoHash, "tv", ns, ne); ok {
			hash, idx = next.InfoHash, i
		}
	}
	if hash == "" {
		res, err := w.resolvePlayable(ctx, "tv", next.LibraryName, tmdbID, ns, ne, "", nil)
		if err != nil {
			slog.Info("prewarm: next episode unavailable", "tmdb", tmdbID, "s", ns, "e", ne, "err", err)
			return
		}
		w.cacheResolved(res, next, "tv", tmdbID, ns, ne)
		hash, idx = res.hash, res.fileIndex
	}
	slog.Info("prewarm: next episode hot", "tmdb", tmdbID, "s", ns, "e", ne, "hash", hash)

	w.touchTorrent(ctx, hash, idx) // initial pre-buffer of the opening
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	deadline := time.After(12 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-tick.C:
			w.touchTorrent(ctx, hash, idx)
		}
	}
}

// touchTorrent reads a small opening range to reset TorrServer's idle-disconnect timer and
// keep the file's start buffered, so a pre-warmed torrent stays hot until played.
func (w *queueWorker) touchTorrent(ctx context.Context, hash string, index int) {
	url := fmt.Sprintf("%s/stream?link=%s&index=%d&play", w.ts.BaseURL(), hash, index)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Range", "bytes=0-262143")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// playURL builds the stable, identity-keyed Resolve-at-Play URL written into .strm files.
func playURL(publicURL, mediaType string, tmdbID, season, episode int) string {
	if mediaType == "movie" {
		return fmt.Sprintf("%s/play/movie/%d", publicURL, tmdbID)
	}
	return fmt.Sprintf("%s/play/tv/%d/%d/%d", publicURL, tmdbID, season, episode)
}

// applyTorrCache pushes the configured cache profile to TorrServer. Returns nil
// (no-op) when no mode is configured. Logs but does not crash on failure.
func applyTorrCache(tsClient *torrserver.Client, cfg *config.Config) error {
	cc := cfg.TorrServer.Cache
	if cc.Mode == "" {
		return nil // leave TorrServer's own settings untouched
	}
	if cc.Mode == "disk" && cc.Path == "" {
		slog.Error("torrserver cache mode=disk requires a path; skipping cache apply")
		return fmt.Errorf("disk cache mode requires torrserver.cache.path")
	}
	err := tsClient.ApplyCacheSettings(torrserver.CacheSettings{
		Mode:               cc.Mode,
		SizeMB:             cc.SizeMB,
		Path:               cc.Path,
		DisconnectTimeoutS: cc.DisconnectTimeoutS,
		ConnectionsLimit:   cc.ConnectionsLimit,
		RetrackersMode:     cc.RetrackersMode,
		UploadRateLimitKB:  cc.UploadRateLimitKB,
	})
	if err != nil {
		slog.Warn("failed to apply TorrServer cache profile", "err", err)
		return err
	}
	slog.Info("applied TorrServer cache profile",
		"mode", cc.Mode, "size_mb", cc.SizeMB, "disconnect_s", cc.DisconnectTimeoutS, "connections", cc.ConnectionsLimit)
	return nil
}

var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".m4v": true,
	".mov": true, ".ts": true, ".wmv": true, ".flv": true,
}

func validateVideoFile(f *torrserver.FileInfo, mediaType string) error {
	if f == nil || f.Path == "" {
		return nil
	}
	ext := strings.ToLower(filepath.Ext(f.Path))
	if !videoExts[ext] {
		return fmt.Errorf("torrent file is not a video (%s) — possible fake release", ext)
	}
	minSize := int64(200 * 1024 * 1024)
	if mediaType == "tv" {
		minSize = 50 * 1024 * 1024
	}
	if f.Length > 0 && f.Length < minSize {
		return fmt.Errorf("video file too small (%d MB) — possible fake release", f.Length/(1024*1024))
	}
	return nil
}

// ── Background task functions ─────────────────────────────────────────────────

// resolvableSeeders inspects indexer results for an item and returns the seeder count to
// record if the item is still resolvable, or -1 if nothing meets the picker's threshold.
// It prefers the seeder count of the EXACT cached release (matching hash) when that release
// is still listed; otherwise it falls back to the best-ranked eligible candidate. picker.Score
// already drops anything below MinSeeders and is sorted best-first, so the first eligible
// (non-CAM, when RejectCAM) entry is the strongest currently-available release.
func resolvableSeeders(releases []indexer.Release, pc picker.Config, hash string) int {
	best := -1
	for _, sr := range picker.Score(releases, pc, "", "") {
		if pc.RejectCAM && sr.IsCAM {
			continue
		}
		if hash != "" && sr.Release.InfoHash == hash {
			return sr.Release.Seeders // the user's exact chosen release is still alive
		}
		if best < 0 {
			best = sr.Release.Seeders // top-ranked fallback (Score is best-first)
		}
	}
	return best
}

// taskLibraryHealthCheck refreshes the ready/stale state of every managed item.
//
// Post-M16 (Resolve-at-Play), "ready" means RESOLVABLE — a release with enough seeders still
// exists — NOT "currently loaded in TorrServer". Between plays the torrent is deliberately
// dropped, so liveness is an INDEXER question, never a TorrServer one. This check therefore
// never touches TorrServer (re-adding torrents here would defeat the zero-background-load /
// bounded-cache design); it only searches the indexer and flips state accordingly:
//   - resolvable + was stale  → promote back to ready (clears stale_since)
//   - resolvable              → refresh the informational seeder count
//   - not resolvable + ready  → mark stale (records stale_since on first transition)
// Revival back to ready happens lazily here, and the actual torrent is re-added on demand by
// /play — so a stale item self-heals the moment its release is seeded again.
func taskLibraryHealthCheck(ctx context.Context, db *store.Store, idxClient *indexer.Client, pickerCfg picker.Config) error {
	items, err := db.ListReady() // ready + stale (managed)
	if err != nil {
		return err
	}
	revived, staleMark, checked := 0, 0, 0
	for _, item := range items {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		checked++

		cats := []int{indexer.CatMovies}
		if item.MediaType == "tv" {
			cats = []int{indexer.CatTV}
		}
		releases, err := idxClient.Search(item.Title, cats)
		if err != nil {
			continue // transient indexer error — never flip state on a failed lookup
		}
		seeders := resolvableSeeders(releases, pickerCfg, item.InfoHash)

		if seeders >= 0 {
			// Still resolvable. Promote out of stale if needed, and refresh seeders.
			if item.Status == "stale" {
				item.Status = "ready"
				item.StaleSince = nil
				revived++
			}
			item.Seeders = seeders
			item.Updated = time.Now()
			db.Upsert(item)
			continue
		}

		// Nothing resolvable right now — mark stale (records stale_since on first transition).
		if item.Status != "stale" {
			db.MarkStale(item.StrmPath)
			staleMark++
		}
	}
	slog.Info("library health check complete", "checked", checked, "revived", revived, "marked_stale", staleMark)
	return nil
}

func taskOrphanCleanup(ctx context.Context, db *store.Store, tsClient *torrserver.Client) error {
	list, err := tsClient.List()
	if err != nil {
		return err
	}
	dropped := 0
	for _, hash := range list {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		item, _ := db.GetByHash(hash)
		if item == nil {
			slog.Info("dropping orphan torrent", "hash", hash)
			tsClient.Drop(hash)
			dropped++
		}
	}
	slog.Info("orphan cleanup complete", "torrents_checked", len(list), "dropped", dropped)
	return nil
}

func taskPosterBackfill(ctx context.Context, db *store.Store, tmdbClient *tmdb.Client) error {
	items, err := db.ItemsNeedingPosters()
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	filled := 0
	for _, item := range items {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		details, err := tmdbClient.Details(item.TMDBID, item.MediaType)
		if err == nil && details.PosterURL != "" {
			db.SetPosterURL(item.ID, details.PosterURL)
			filled++
		}
		time.Sleep(200 * time.Millisecond)
	}
	slog.Info("poster backfill complete", "filled", filled, "total", len(items))
	return nil
}

// taskSubscriptionCheck walks active subscriptions, enqueues any newly-aired
// episodes not already in the library/queue, and retires subscriptions whose
// show has ended.
func taskSubscriptionCheck(ctx context.Context, db *store.Store, tmdbClient *tmdb.Client) error {
	subs, err := db.ListAiringSubscriptions()
	if err != nil {
		return err
	}
	today := time.Now().Format("2006-01-02")
	enqueued, retired := 0, 0
	for _, sub := range subs {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		details, err := tmdbClient.Details(sub.TMDBID, "tv")
		if err != nil {
			continue // transient; try again next run
		}
		episodes, err := tmdbClient.TVEpisodes(sub.TMDBID, sub.Season)
		if err != nil {
			continue
		}
		for _, ep := range episodes {
			// Only episodes that have actually aired (air_date <= today).
			if ep.AirDate == "" || ep.AirDate > today {
				continue
			}
			if db.EpisodeActive(sub.TMDBID, sub.Season, ep.Number) {
				continue
			}
			title := sub.Title
			if title == "" {
				title = details.Title
			}
			if _, err := db.Enqueue(&store.QueueItem{
				TMDBID: sub.TMDBID, MediaType: "tv",
				Title: title, Year: details.Year, PosterURL: sub.PosterURL,
				Season: sub.Season, Episode: ep.Number,
				LibraryName: sub.LibraryName, RequestedBy: sub.RequestedBy,
			}); err == nil {
				enqueued++
				slog.Info("subscription: enqueued new episode",
					"show", title, "season", sub.Season, "episode", ep.Number)
			}
		}
		// Retire the subscription once the show is no longer airing.
		stillAiring := details.IsAiring()
		db.MarkSubscriptionChecked(sub.ID, stillAiring)
		if !stillAiring {
			retired++
		}
	}
	slog.Info("subscription check complete", "subscriptions", len(subs), "enqueued", enqueued, "retired", retired)
	return nil
}

// ── Settings persistence (DB-backed, admin-editable, live) ─────────────────────

var settingsMu sync.RWMutex

func getOrSeed(db *store.Store, key, seed string) string {
	v, _ := db.GetSetting(key)
	if v == "" && seed != "" {
		db.SetSetting(key, seed)
		return seed
	}
	return v
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// applyConnections seeds connection settings from config.yaml on first run and
// points each client at the effective (DB) values.
func applyConnections(db *store.Store, cfg *config.Config, tm *tmdb.Client, idx *indexer.Client, jf *jellyfin.Client, ts *torrserver.Client) {
	tm.Configure(getOrSeed(db, "conn.tmdb.key", cfg.TMDB.APIKey))
	idx.Configure(getOrSeed(db, "conn.prowlarr.url", cfg.Indexer.BaseURL), getOrSeed(db, "conn.prowlarr.key", cfg.Indexer.APIKey))
	jf.Configure(getOrSeed(db, "conn.jellyfin.url", cfg.Jellyfin.BaseURL), getOrSeed(db, "conn.jellyfin.key", cfg.Jellyfin.APIKey))
	ts.Configure(getOrSeed(db, "conn.torrserver.url", cfg.TorrServer.BaseURL))
}

func loadQualityOverrides(db *store.Store, cfg *config.Config) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	if v, _ := db.GetSetting("quality.min_seeders"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Picker.MinSeeders = n
		}
	}
	if v, _ := db.GetSetting("quality.max_size_gb"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Picker.MaxSizeGB = n
		}
	}
	if v, _ := db.GetSetting("quality.reject_cam"); v != "" {
		b := v == "true"
		cfg.Picker.RejectCAM = &b
	}
	if v, _ := db.GetSetting("quality.video_codecs"); v != "" {
		cfg.Picker.PreferVideoCodecs = splitCSV(v)
	}
	if v, _ := db.GetSetting("quality.audio_codecs"); v != "" {
		cfg.Picker.PreferAudioCodecs = splitCSV(v)
	}
	if v, _ := db.GetSetting("quality.containers"); v != "" {
		cfg.Picker.PreferContainers = splitCSV(v)
	}
}

func loadCacheOverrides(db *store.Store, cfg *config.Config) {
	c := &cfg.TorrServer.Cache
	if v, _ := db.GetSetting("cache.mode"); v != "" {
		c.Mode = v
	}
	if v, _ := db.GetSetting("cache.size_mb"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			c.SizeMB = n
		}
	}
	if v, _ := db.GetSetting("cache.path"); v != "" {
		c.Path = v
	}
	if v, _ := db.GetSetting("cache.disconnect_s"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			c.DisconnectTimeoutS = n
		}
	}
	if v, _ := db.GetSetting("cache.connections"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			c.ConnectionsLimit = n
		}
	}
	if v, _ := db.GetSetting("cache.retrackers"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			c.RetrackersMode = &n
		}
	}
	if v, _ := db.GetSetting("cache.upload_kb"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			c.UploadRateLimitKB = n
		}
	}
}

// testConnection probes a service with the given (possibly unsaved) credentials.
// API keys are alphanumeric/hex so direct concatenation is safe.
func testConnection(ctx context.Context, service, url, key string) (bool, string) {
	client := &http.Client{Timeout: 12 * time.Second}
	do := func(req *http.Request) (bool, string) {
		resp, err := client.Do(req)
		if err != nil {
			return false, "unreachable: " + err.Error()
		}
		defer resp.Body.Close()
		switch {
		case resp.StatusCode == 200:
			return true, "Connected ✓"
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			return false, "reachable, but the API key was rejected"
		default:
			return false, fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
	}
	switch service {
	case "tmdb":
		if key == "" {
			return false, "no API key set"
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.themoviedb.org/3/configuration?api_key="+key, nil)
		return do(req)
	case "prowlarr":
		if url == "" {
			return false, "no URL set"
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/v1/system/status?apikey="+key, nil)
		return do(req)
	case "jellyfin":
		if url == "" {
			return false, "no URL set"
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/System/Info", nil)
		req.Header.Set("X-Emby-Token", key)
		return do(req)
	case "torrserver":
		if url == "" {
			return false, "no URL set"
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/echo", nil)
		return do(req)
	}
	return false, "unknown service"
}

// livePicker / livePickerFor read the (possibly live-updated) picker config under
// a read lock so dashboard quality edits take effect without a restart.
func livePicker(cfg *config.Config) picker.Config {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return toPicker(cfg.Picker)
}
func livePickerFor(cfg *config.Config, lib *config.Library) picker.Config {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return toPicker(cfg.PickerFor(lib))
}

// toPicker converts a config PickerConfig into the picker package's Config,
// carrying the reject_cam setting (default: reject camera copies).
func toPicker(cp config.PickerConfig) picker.Config {
	return picker.Config{
		MinSeeders:        cp.MinSeeders,
		PreferVideoCodecs: cp.PreferVideoCodecs,
		PreferAudioCodecs: cp.PreferAudioCodecs,
		PreferContainers:  cp.PreferContainers,
		MaxSizeGB:         cp.MaxSizeGB,
		RejectCAM:         cp.RejectCAMValue(),
	}
}

// --- VPN config helpers (used by the admin upload/activate endpoints) ---

// vpnSanitizeSlug turns a user-supplied name into a safe config slug (no path traversal).
func vpnSanitizeSlug(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, ".conf")
	name = strings.ReplaceAll(name, " ", "-")
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// vpnValidSlug guards every filesystem op: safe charset, length, no leading dot, and never the
// reserved live-config name.
func vpnValidSlug(s string) bool {
	if s == "" || s == "wg0-vpntorrent" || strings.HasPrefix(s, ".") || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// vpnIsWireGuardConf does a cheap sanity check that uploaded content is actually a WG config.
func vpnIsWireGuardConf(s string) bool {
	return strings.Contains(s, "[Interface]") && strings.Contains(s, "[Peer]") &&
		strings.Contains(s, "PrivateKey") && strings.Contains(s, "Endpoint")
}

// vpnStripDNS removes any `DNS = ...` lines — DNS is handled per-netns; a stray DNS line makes
// wg-quick require resolvconf and fail.
func vpnStripDNS(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		t := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(t, "dns") && strings.Contains(t, "=") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// vpnParseEndpoint extracts the peer Endpoint for display in the configs list.
func vpnParseEndpoint(s string) string {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "Endpoint") {
			if i := strings.Index(t, "="); i >= 0 {
				return strings.TrimSpace(t[i+1:])
			}
		}
	}
	return ""
}

// webAssets is the directory holding public/ and dashboard/ — set from the --assets flag in
// main() and read by buildProtectedMux. Default "web" (relative) preserves the dev layout.
var webAssets = "web"

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// suppress unused import warning while building incrementally
var _ = strings.Contains
