package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	url2 "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"jellyfreedom/internal/api"
	"jellyfreedom/internal/config"
	"jellyfreedom/internal/indexer"
	"jellyfreedom/internal/jellyfin"
	"jellyfreedom/internal/library"
	"jellyfreedom/internal/picker"
	"jellyfreedom/internal/redact"
	"jellyfreedom/internal/store"
	"jellyfreedom/internal/tmdb"
	"jellyfreedom/internal/torrserver"
	"jellyfreedom/internal/update"
)

// url2 aliases net/url; the local identifier "url" is used as a variable in several
// handlers in this file.

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	dbPath := flag.String("db", "jellyfreedom.db", "path to the SQLite database")
	assetsDir := flag.String("assets", "", "serve web assets from this directory instead of the embedded copy (development)")
	showVersion := flag.Bool("version", false, "print the build version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	slog.Info("jellyfreedom starting", "version", version, "go", runtime.Version(),
		"os", runtime.GOOS, "arch", runtime.GOARCH)

	// One cancellable root context for EVERY background worker, cancelled on SIGINT/
	// SIGTERM. Previously each of them got context.Background(), so nothing was ever
	// cancelled — including the queue worker, whose restart-recovery branch is keyed on
	// ctx.Err() and could therefore never fire.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	// public_url is written verbatim into every .strm; if it only resolves on this box,
	// Jellyfin will list items as ready and refuse to play them from any other device.
	// config.Load hard-fails on the installer placeholder; this covers the localhost case,
	// which is legitimate for a single-box setup and so is only a warning.
	if msg := cfg.WarnIfPublicURLNotReachableFromLAN(); msg != "" {
		slog.Warn(msg)
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		slog.Error("failed to open store", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	api.SetStore(db)
	api.SetSecureCookies(cfg.Server.SecureCookies)

	// The dashboard can restart the orchestrator itself. It needs no sudo rule: the
	// trigger cancels this very context, and main then exits non-zero so systemd's
	// Restart=on-failure starts us again. See selfrestart.go.
	wireSelfRestart(stop)

	// The play capability key must exist before any .strm URL is minted or verified.
	if err := loadPlayKey(db); err != nil {
		slog.Error("failed to initialise play capability key", "err", err)
		os.Exit(1)
	}

	tmdbClient := tmdb.New("")
	indexerClient := indexer.New("", "")
	tsClient := torrserver.New("")
	jfClient := jellyfin.New("", "")

	// Connection config lives in the DB (admin-editable in Settings) so the app
	// ships with no baked-in keys. On first run, seed the DB from config.yaml
	// (if present) so existing deployments keep working, then point each client
	// at the effective values. Updated live by the Settings → Connections UI.
	if err := applyConnections(db, cfg, tmdbClient, indexerClient, jfClient, tsClient); err != nil {
		slog.Error("failed to load connection settings from the database", "err", err)
		os.Exit(1)
	}

	// Load any DB-persisted quality/cache overrides over the config.yaml seed.
	if err := loadQualityOverrides(db, cfg); err != nil {
		slog.Error("failed to load quality settings", "err", err)
		os.Exit(1)
	}
	if err := loadCacheOverrides(db, cfg); err != nil {
		slog.Error("failed to load cache settings", "err", err)
		os.Exit(1)
	}

	// Shared secret for the Jellyfin webhook (generated on first run).
	if _, err := ensureWebhookSecret(db); err != nil {
		slog.Error("failed to initialise the Jellyfin webhook secret", "err", err)
		os.Exit(1)
	}

	// Apply the cache profile to TorrServer so the same binary adapts to the host.
	// Retries in the background: at boot we can win the race against TorrServer.
	applyTorrCacheAtStartup(ctx, tsClient, cfg)

	api.SetJellyfinClient(jfClient)

	assets := webFS(*assetsDir, *assetsDir != "")
	// The setup/login pages are Go templates served from the same asset tree as the
	// rest of the UI, so they share its stylesheets instead of an inline CSS slab.
	api.SetAuthTemplateFS(assets, "auth/index.html")

	worker := &queueWorker{
		db: db, tmdb: tmdbClient, indexer: indexerClient, ts: tsClient, jf: jfClient, cfg: cfg,
		resolves: newResolveGroup(),
	}
	go worker.run(ctx)

	// Rewrite existing .strm files with capability-tokenised /play URLs. Until this
	// succeeds we do NOT enforce the token, because every pre-existing .strm lacks one
	// and enforcing first would break playback of the whole library.
	migrateStrmTokens(db, cfg)

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
			rels, err := indexerClient.SearchContext(ctx, "1080p", []int{indexer.CatMovies})
			if err != nil {
				return err
			}
			slog.Info("indexer warmup complete", "results", len(rels))
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
			return db.PurgeSessions()
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

	// Start all scheduled tasks under the cancellable root context.
	registry.Start(ctx)

	// Component probes behind GET /api/health/summary (public) and /readyz (public).
	api.SetHealthProbes([]api.HealthProbe{
		{Component: api.CompTMDB, Check: func(context.Context) bool { return tmdbClient.Configured() }},
		{Component: api.CompProwlarr, Check: func(c context.Context) bool { return indexerClient.Ping(c) == nil }},
		{Component: api.CompJellyfin, Check: func(c context.Context) bool { return jfClient.Ping(c) == nil }},
		{Component: api.CompTorrServer, Check: func(c context.Context) bool { return tsClient.Ping(c) == nil }},
		{Component: api.CompVPN, Check: func(context.Context) bool { return api.VPNConnected() }},
	})

	mux := http.NewServeMux()

	// ------------------------------------------------------------------ //
	// Public routes (no auth)
	// ------------------------------------------------------------------ //

	// /healthz is a real LIVENESS check: the process is up AND its own database
	// answers. It used to be a hardcoded string, so it stayed "ok" with a dead DB.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(); err != nil {
			slog.Error("healthz: database unreachable", "err", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "error", "version": version, "detail": "database unreachable",
			})
			return
		}
		jsonOK(w, map[string]any{"status": "ok", "version": version})
	})

	// /readyz reports per-dependency reachability — "can this install actually serve a
	// request end to end", as opposed to /healthz's "is the process alive".
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		rctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		components := map[string]bool{
			api.CompTMDB:       tmdbClient.Configured(),
			api.CompProwlarr:   indexerClient.Ping(rctx) == nil,
			api.CompJellyfin:   jfClient.Ping(rctx) == nil,
			api.CompTorrServer: tsClient.Ping(rctx) == nil,
			api.CompVPN:        api.VPNConnected(),
		}
		ready := db.Ping() == nil
		for _, ok := range components {
			if !ok {
				ready = false
			}
		}
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		jsonOK(w, map[string]any{"ready": ready, "version": version, "components": components})
	})

	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]string{"version": version})
	})

	// Public, minimal health for the header dot. Deliberately carries no hostnames,
	// keys, IPs or unit names — see internal/api/health.go. (API contract §4.)
	mux.HandleFunc("GET /api/health/summary", api.HealthSummaryHandler)

	// Lightweight "is anyone watching?" signal for the VPN watchdog / port-forward keeper so
	// they can DEFER non-urgent TorrServer restarts while a stream is live.
	//
	// FAILS CLOSED. This used to report {"active":false} on ANY error, so an unreachable
	// Jellyfin told the watchdog nobody was watching and it would restart TorrServer in
	// the middle of a stream. When the answer is unknown, "someone is watching" is the
	// only safe answer.
	mux.HandleFunc("GET /api/playback/active", func(w http.ResponseWriter, r *http.Request) {
		n, err := jfClient.ActivePlaybackCount()
		if err != nil {
			slog.Warn("playback/active: assuming active because Jellyfin could not be queried", "err", err)
			jsonOK(w, map[string]any{"active": true, "known": false})
			return
		}
		jsonOK(w, map[string]any{"active": n > 0, "known": true})
	})

	// Media UI — served from the embedded assets (or --assets in development).
	mux.Handle("/", http.FileServerFS(subFS(assets, "public")))

	// TMDB search (used by search UI, public)
	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			jsonErr(w, "q is required", http.StatusBadRequest)
			return
		}
		results, err := tmdbClient.Search(q)
		if err != nil {
			httpFail(w, r, http.StatusBadGateway,
				"TMDB search failed — check Settings → Connections → TMDB", err)
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
			httpFail(w, r, http.StatusBadGateway, "could not load seasons from TMDB", err)
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
			httpFail(w, r, http.StatusBadGateway, "could not load episodes from TMDB", err)
			return
		}
		jsonOK(w, eps)
	})

	// /api/status and /api/leak are ADMIN ONLY (API contract §3).
	//
	// /api/status returned the WireGuard peer public key, the VPN endpoint IP and every
	// service's bind address; /api/leak returned the host's real public IPv4 and the VPN
	// exit IP — both to any unauthenticated visitor on the LAN. The search UI's health
	// dot now uses /api/health/summary, which needs none of that.
	mux.Handle("GET /api/status", api.RequireAdmin(http.HandlerFunc(api.StatusHandler)))
	mux.Handle("GET /api/leak", api.RequireAdmin(http.HandlerFunc(api.LeakCheckHandler)))

	// Auth endpoints for the media-side inline login
	mux.HandleFunc("GET /api/me", api.MeHandler)
	mux.HandleFunc("POST /api/auth/login", api.APILoginHandler)
	mux.HandleFunc("POST /api/auth/logout", api.APILogoutHandler)

	// Changing YOUR OWN password requires only a session. It was registered on the
	// protected (RequireAdmin) mux, so every non-admin got a 403 trying to change their
	// own password — while the handler itself was already written for the calling user.
	mux.Handle("POST /api/auth/change-password", api.RequireAuth(http.HandlerFunc(api.ChangePasswordHandler)))

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

	// /api/library — privacy-aware list of ready items (My Library strip on search page).
	//
	// An anonymous caller gets ONLY public items, and gets them with magnet, strm_path
	// and requested_by stripped (API contract §2). Before this, "" was treated as
	// admin-equivalent, so logging OUT showed private items that logging IN as a
	// non-admin correctly hid.
	mux.Handle("GET /api/library", api.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viewer, username, isAdmin := viewerOf(r)
		items, err := db.ListVisible(username, isAdmin)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
			return
		}
		out := make([]store.Item, 0, len(items))
		for _, it := range items {
			if viewer == nil {
				out = append(out, it.Redacted())
			} else {
				out = append(out, *it)
			}
		}
		jsonOK(w, out)
	})))

	// /api/library/status — batch status check by TMDB IDs (used by search card badges).
	// Now privacy-filtered: it previously exposed a private item's existence, title and
	// library to anyone who guessed its TMDB id.
	mux.Handle("GET /api/library/status", api.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query().Get("ids")
		if raw == "" {
			jsonOK(w, map[string]any{})
			return
		}
		_, username, isAdmin := viewerOf(r)
		var ids []int
		for _, s := range strings.Split(raw, ",") {
			if id, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
				ids = append(ids, id)
			}
			if len(ids) >= 200 {
				break // bound the IN(...) list
			}
		}
		statuses, err := db.GetStatusByTMDBIDs(ids, username, isAdmin)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
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
	})))

	// ── Queue API ───────────────────────────────────────────────────────────────
	mux.Handle("GET /api/queue", api.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viewer, username, isAdmin := viewerOf(r)
		items, err := db.ListQueue(username, isAdmin)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the queue", err)
			return
		}
		out := make([]store.QueueItem, 0, len(items))
		for _, it := range items {
			if viewer == nil {
				out = append(out, it.Redacted())
			} else {
				out = append(out, *it)
			}
		}
		jsonOK(w, out)
	})))

	// Cancel/delete report what ACTUALLY happened. They used to discard the store error
	// and return {"status":"cancelled"} unconditionally, so a row that was not yours, no
	// longer pending, or never written still reported success to the UI.
	mux.Handle("POST /api/queue/{id}/cancel", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonErr(w, "bad id", http.StatusBadRequest)
			return
		}
		user := api.UserFromContext(r)
		n, err := db.CancelQueueItem(id, user.Username, user.IsAdmin)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not cancel that request", err)
			return
		}
		if n == 0 {
			jsonErr(w, "nothing to cancel — it is not yours, or it is no longer pending", http.StatusConflict)
			return
		}
		jsonOK(w, map[string]string{"status": "cancelled"})
	})))

	mux.Handle("DELETE /api/queue/{id}", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonErr(w, "bad id", http.StatusBadRequest)
			return
		}
		user := api.UserFromContext(r)
		n, err := db.DeleteQueueItem(id, user.Username, user.IsAdmin)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not delete that request", err)
			return
		}
		if n == 0 {
			jsonErr(w, "nothing to delete — it is not yours, or it is still in flight", http.StatusConflict)
			return
		}
		jsonOK(w, map[string]string{"status": "deleted"})
	})))

	// Per-queue-item failure diagnosis (API contract §7): why did this fail, and which
	// rule rejected each release the indexer returned.
	mux.Handle("GET /api/queue/{id}/diagnosis", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonErr(w, "bad id", http.StatusBadRequest)
			return
		}
		item, err := db.GetQueueItem(id)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read that request", err)
			return
		}
		user := api.UserFromContext(r)
		if item == nil || (!user.IsAdmin && item.RequestedBy != user.Username) {
			jsonErr(w, "not found", http.StatusNotFound)
			return
		}
		if item.Diagnosis == "" {
			jsonOK(w, map[string]any{"reason": "unavailable", "error_msg": item.ErrorMsg})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(item.Diagnosis))
	})))

	mux.HandleFunc("GET /api/queue/count", func(w http.ResponseWriter, r *http.Request) {
		n, err := db.QueuePendingCount()
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the queue", err)
			return
		}
		jsonOK(w, map[string]int{"count": n})
	})

	// ── Subscriptions API ───────────────────────────────────────────────────────
	mux.Handle("GET /api/subscriptions", api.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viewer, username, isAdmin := viewerOf(r)
		subs, err := db.ListSubscriptions(username, isAdmin)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read subscriptions", err)
			return
		}
		out := make([]store.Subscription, 0, len(subs))
		for _, sub := range subs {
			if viewer == nil {
				out = append(out, sub.Redacted())
			} else {
				out = append(out, *sub)
			}
		}
		jsonOK(w, out)
	})))

	mux.Handle("POST /api/subscriptions", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TMDBID    int    `json:"tmdb_id"`
			Season    int    `json:"season"`
			Title     string `json:"title"`
			PosterURL string `json:"poster_url"`
			Library   string `json:"library"`
		}
		if err := decodeBody(w, r, &req); err != nil || req.TMDBID == 0 || req.Season == 0 {
			jsonErr(w, "tmdb_id and season required", http.StatusBadRequest)
			return
		}
		user := api.UserFromContext(r)
		if err := db.UpsertSubscription(&store.Subscription{
			TMDBID: req.TMDBID, Season: req.Season, Title: req.Title,
			PosterURL: req.PosterURL, LibraryName: req.Library, RequestedBy: user.Username,
		}); err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not save the subscription", err)
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
		n, err := db.DeleteSubscription(id, user.Username, user.IsAdmin)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not unsubscribe", err)
			return
		}
		if n == 0 {
			jsonErr(w, "no such subscription", http.StatusNotFound)
			return
		}
		jsonOK(w, map[string]string{"status": "unsubscribed"})
	})))

	// ── Browse endpoints (homepage carousels) ───────────────────────────────────
	mux.HandleFunc("GET /api/browse/trending", func(w http.ResponseWriter, r *http.Request) {
		results, err := tmdbClient.Trending()
		if err != nil {
			httpFail(w, r, http.StatusBadGateway, "TMDB is unavailable right now", err)
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
			httpFail(w, r, http.StatusBadGateway, "TMDB is unavailable right now", err)
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
		if err := decodeBody(w, r, &req); err != nil || req.TMDBID == 0 {
			jsonErr(w, "tmdb_id and type required", http.StatusBadRequest)
			return
		}
		// Pre-flight: fail in a second with a specific, actionable message rather than
		// enqueuing work that will discover the same problem 150 seconds into a search.
		if msg := preflight(r.Context(), indexerClient, tsClient); msg != "" {
			jsonErr(w, msg, http.StatusServiceUnavailable)
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
		// flight, must NOT spawn a duplicate queue row. Stale items fall through to a fresh
		// enqueue (a re-request is how the user revives an expired one).
		//
		// The in-flight check runs for EVERY request, magnet or not. It used to be skipped
		// whenever a magnet was supplied, on the reasoning that an explicit magnet means the
		// user is deliberately re-picking — but "re-pick" is an instruction about WHICH release
		// a request should use, not a licence to create a second request. With the check
		// bypassed there was nothing at all between a repeating client and the INSERT: no
		// unique constraint, no rate limit. One client that re-fired on each response put
		// 19,430 identical rows in the queue in ten minutes, and the worker then spent a day
		// re-resolving the same magnet every five seconds. A re-pick now repoints the row the
		// user is already watching (RequeueWithMagnet) instead of enqueuing beside it.
		existing, err := db.GetByIdentity(req.TMDBID, req.MediaType, req.Season, req.Episode)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
			return
		}
		// A ready item short-circuits only for a bare re-request. With a magnet the user is
		// deliberately swapping the release, so fall through and re-resolve.
		if req.Magnet == "" && existing != nil && existing.Status == "ready" {
			if err := db.ClearTerminalQueue(req.TMDBID, req.MediaType, req.Season, req.Episode); err != nil {
				// Cosmetic only: a leftover terminal row shows a stale badge next to
				// an item that is genuinely ready. Not worth failing the request.
				slog.Warn("could not clear terminal queue rows", "tmdb", req.TMDBID, "err", err)
			}
			jsonOK(w, map[string]any{"status": "ready", "already": true, "title": title, "year": req.Year})
			return
		}
		active, err := db.ActiveQueueItem(req.TMDBID, req.MediaType, req.Season, req.Episode, requestedBy)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the queue", err)
			return
		}
		if active != nil {
			status, stage := active.Status, active.Stage
			// Same identity, different release: repoint the existing row rather than
			// inserting a duplicate. Same release: nothing to do, just report the row back.
			if req.Magnet != "" && req.Magnet != active.MagnetOverride {
				if err := db.RequeueWithMagnet(active.ID, req.Magnet); err != nil {
					httpFail(w, r, http.StatusInternalServerError, "could not update that request", err)
					return
				}
				status, stage = "pending", store.StageQueued
				slog.Info("re-pointed queue item at a new release", "id", active.ID, "title", title)
			}
			jsonOK(w, map[string]any{"queue_id": active.ID, "status": status, "stage": stage, "already": true, "title": title, "year": req.Year})
			return
		}
		// Supersede any finished (failed/cancelled/done) rows for this identity so the new
		// request replaces them rather than piling up next to the library entry.
		if err := db.ClearTerminalQueue(req.TMDBID, req.MediaType, req.Season, req.Episode); err != nil {
			slog.Warn("could not clear terminal queue rows", "tmdb", req.TMDBID, "err", err)
		}
		qItem := &store.QueueItem{
			TMDBID: req.TMDBID, MediaType: req.MediaType,
			Title: title, Year: req.Year, PosterURL: req.PosterURL,
			Season: req.Season, Episode: req.Episode,
			LibraryName: req.Library, RequestedBy: requestedBy,
			MagnetOverride: req.Magnet,
		}
		id, err := db.Enqueue(qItem)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not queue that request", err)
			return
		}
		slog.Info("enqueued request", "id", id, "title", title, "type", req.MediaType)
		jsonOK(w, map[string]any{
			"queue_id": id,
			"status":   "pending",
			"stage":    store.StageQueued,
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
		if err := decodeBody(w, r, &req); err != nil || req.TMDBID == 0 || req.Season == 0 {
			jsonErr(w, "tmdb_id and season required", http.StatusBadRequest)
			return
		}
		if msg := preflight(r.Context(), indexerClient, tsClient); msg != "" {
			jsonErr(w, msg, http.StatusServiceUnavailable)
			return
		}
		episodes, err := tmdbClient.TVEpisodes(req.TMDBID, req.Season)
		if err != nil {
			httpFail(w, r, http.StatusBadGateway, "could not load the episode list from TMDB", err)
			return
		}
		user := api.UserFromContext(r)
		requestedBy := ""
		if user != nil {
			requestedBy = user.Username
		}
		var queued []int64
		skipped, failed := 0, 0
		for _, ep := range episodes {
			// Skip episodes already ready in the library or already in flight in the
			// queue — makes "Request All" / "Request Remaining" idempotent.
			active, err := db.EpisodeActive(req.TMDBID, req.Season, ep.Number)
			if err != nil {
				slog.Error("season request: episode-active check failed; skipping to avoid a duplicate",
					"tmdb", req.TMDBID, "season", req.Season, "episode", ep.Number, "err", err)
			}
			if active {
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
			if err := db.ClearTerminalQueue(req.TMDBID, "tv", req.Season, ep.Number); err != nil {
				slog.Warn("could not clear terminal queue rows", "tmdb", req.TMDBID, "err", err)
			}
			qItem := &store.QueueItem{
				TMDBID: req.TMDBID, MediaType: "tv",
				Title: title, Year: req.Year, PosterURL: req.PosterURL,
				Season: req.Season, Episode: ep.Number,
				LibraryName: req.Library, RequestedBy: requestedBy,
			}
			id, err := db.Enqueue(qItem)
			if err != nil {
				slog.Error("season request: enqueue failed",
					"tmdb", req.TMDBID, "season", req.Season, "episode", ep.Number, "err", err)
				failed++
				continue
			}
			queued = append(queued, id)
		}
		// If the show is still airing, auto-subscribe so new episodes are grabbed
		// as they release (idempotent per tmdb_id+season).
		subscribed := false
		if details, err := tmdbClient.Details(req.TMDBID, "tv"); err == nil && details.IsAiring() {
			title := req.Title
			if title == "" {
				title = details.Title
			}
			if err := db.UpsertSubscription(&store.Subscription{
				TMDBID: req.TMDBID, Season: req.Season, Title: title,
				PosterURL: req.PosterURL, LibraryName: req.Library, RequestedBy: requestedBy,
			}); err != nil {
				// The episodes are queued either way; only the auto-follow failed.
				slog.Error("auto-subscribe failed", "tmdb", req.TMDBID, "season", req.Season, "err", err)
			} else {
				subscribed = true
				slog.Info("auto-subscribed to airing season", "tmdb", req.TMDBID, "season", req.Season)
			}
		}

		jsonOK(w, map[string]any{
			"enqueued":   len(queued),
			"skipped":    skipped,
			"failed":     failed,
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
		// hash is 40-hex-validated by every caller; StreamURL additionally escapes the
		// query values rather than interpolating them raw.
		if !torrserver.ValidInfoHash(hash) {
			http.Error(w, "bad torrent reference", http.StatusBadRequest)
			return
		}
		target := tsClient.StreamURL(hash, index)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
		if err != nil {
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		if rng := r.Header.Get("Range"); rng != "" {
			req.Header.Set("Range", rng)
		}
		// A dedicated client with NO timeout: a film streams for hours on one response,
		// and http.DefaultClient's transport state is shared process-wide.
		resp, err := streamClient.Do(req)
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
		if _, err := io.Copy(w, resp.Body); err != nil {
			// Almost always the player seeking or closing the connection; log at debug
			// level so a normal seek does not look like an error.
			slog.Debug("stream copy ended", "hash", hash, "err", err)
		}
	}

	// Legacy hash-pinned stream URL (older .strm files written before Resolve-at-Play).
	// New .strm files use /play/... (identity-keyed, self-healing) instead.
	// This endpoint MUST stay unauthenticated — Jellyfin fetches .strm URLs with no
	// session — but it used to accept ANY info hash and add that torrent to TorrServer.
	// An unauthenticated stranger on the LAN could therefore make the box download
	// arbitrary content over the owner's VPN.
	//
	// Two constraints close that: the hash must be well-formed 40-hex, and it must
	// already exist in this library. An unknown hash is now a 404, never an add.
	mux.HandleFunc("GET /proxy/stream", func(w http.ResponseWriter, r *http.Request) {
		link := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("link")))
		idx := r.URL.Query().Get("index")
		if link == "" || idx == "" {
			http.Error(w, "missing link or index", http.StatusBadRequest)
			return
		}
		if !torrserver.ValidInfoHash(link) {
			http.Error(w, "bad torrent reference", http.StatusBadRequest)
			return
		}
		index, err := strconv.Atoi(idx)
		if err != nil || index < 0 {
			http.Error(w, "bad index", http.StatusBadRequest)
			return
		}
		it, err := db.GetByHash(link)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
			return
		}
		if it == nil {
			slog.Warn("proxy/stream: refused an info hash that is not in this library",
				"hash", link, "remote", r.RemoteAddr)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// The queue drops torrents after validating, so re-add on demand (with the
		// stored full magnet → trackers) if it isn't loaded. No-op once it's live.
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		tsClient.EnsureLoaded(ctx, link, it.Magnet, it.Title, 8)
		streamProxy(w, r, link, index)
	})

	// ------------------------------------------------------------------ //
	// Resolve-at-Play — the .strm points here with a STABLE identity URL. We pick the best
	// CURRENTLY-live release at play time, so the library self-heals against seeder decay.
	// (DECISIONS.md D9, ARCHITECTURE.md §10.)
	// ------------------------------------------------------------------ //
	// resolveDeadline bounds the SLOW path only. The fast path (a live cached release)
	// must stay well inside it; the slow path used to be able to run ~5 minutes — a 150s
	// indexer search plus four candidates at up to 35s each — with the client blocked and
	// no way to interrupt it.
	const resolveDeadline = 90 * time.Second

	playHandler := func(w http.ResponseWriter, r *http.Request, mediaType string, tmdbID, season, episode int) {
		// Log every playback attempt and its outcome.
		//
		// This used to log ONLY on rejection or error, so a user watching `journalctl -u
		// jellyfreedom` while pressing play in Jellyfin saw an empty screen whether playback
		// worked or not — reported as "nothing comes up and the logs are also empty", which
		// made the problem impossible to diagnose from the outside. A successful play is the
		// single most useful line this service can emit.
		playStart := time.Now()
		slog.Info("play: request",
			"type", mediaType, "tmdb", tmdbID, "s", season, "e", episode,
			"remote", r.RemoteAddr, "range", r.Header.Get("Range"), "ua", r.UserAgent())
		defer func() {
			slog.Info("play: finished",
				"type", mediaType, "tmdb", tmdbID, "s", season, "e", episode,
				"took", time.Since(playStart).Round(time.Millisecond).String())
		}()

		// Capability check. /play cannot require a session (Jellyfin fetches .strm URLs
		// anonymously), so possession of the HMAC tag in the URL — which only a .strm this
		// server wrote can contain — is the credential. Enforcement is switched on only
		// once the startup migration has retokenised every existing .strm.
		if playTokenEnforced() && !validPlayToken(r.URL.Query().Get("t"), mediaType, tmdbID, season, episode) {
			slog.Warn("play: rejected a request with a missing/invalid capability token",
				"type", mediaType, "tmdb", tmdbID, "s", season, "e", episode, "remote", r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// This item is now playing — stop any keep-warm loop for it; real playback takes over.
		cancelWarm(tmdbID, season, episode)

		item, err := db.GetByIdentity(tmdbID, mediaType, season, episode)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
			return
		}

		fastCtx, cancelFast := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancelFast()

		// Fast path: a cached release that's still alive (resolves a file list quickly). Re-derive
		// the file index from the LIVE file list rather than trusting item.FileIndex — a stale or
		// buggy cached index (e.g. a legacy index=0) would otherwise stream the wrong/empty file.
		tryCached := func(ctx context.Context, it *store.Item) bool {
			if it == nil || !torrserver.ValidInfoHash(it.InfoHash) {
				return false
			}
			if !tsClient.EnsureLoaded(ctx, it.InfoHash, it.Magnet, it.Title, 12) {
				return false
			}
			idx, length, ok := resolveFileIndex(tsClient, it.InfoHash, mediaType, season, episode)
			if !ok || !tsClient.WaitConnectable(ctx, it.InfoHash, 12) {
				return false
			}
			if idx != it.FileIndex {
				it.FileIndex = idx
				if err := db.Upsert(it); err != nil {
					// Not fatal to playback — it just means the next play re-derives the
					// index again — but it IS the bug that made every play slow, so it
					// must be visible rather than silently dropped.
					slog.Error("play: could not persist the corrected file index",
						"tmdb", tmdbID, "s", season, "e", episode, "err", err)
				}
			}
			maybePreWarm(r, worker, mediaType, tmdbID, season, episode, length)
			streamProxy(w, r, it.InfoHash, idx)
			return true
		}

		if tryCached(fastCtx, item) {
			return
		}
		if r.Context().Err() != nil {
			return // client went away
		}
		slog.Info("play: cached release unusable/ghost, re-resolving",
			"tmdb", tmdbID, "s", season, "e", episode)

		// Single-flight the expensive resolve per identity, so a refresh-happy client (or
		// Jellyfin probing the file while the player also requests it) does not multiply
		// a 90-second search by the number of concurrent requests.
		key := playIdentity(mediaType, tmdbID, season, episode)
		slowCtx, cancelSlow := context.WithTimeout(r.Context(), resolveDeadline)
		defer cancelSlow()
		release, ok := worker.resolves.lock(slowCtx, key)
		if !ok {
			http.Error(w, "timed out waiting to resolve this title", http.StatusGatewayTimeout)
			return
		}
		defer release()

		// Re-check the fast path: while we queued, the winner may have cached a live release.
		if fresh, ferr := db.GetByIdentity(tmdbID, mediaType, season, episode); ferr == nil && fresh != nil &&
			item != nil && fresh.InfoHash != item.InfoHash {
			recheckCtx, cancelRecheck := context.WithTimeout(r.Context(), 25*time.Second)
			done := tryCached(recheckCtx, fresh)
			cancelRecheck()
			if done {
				return
			}
		}

		// Slow path: resolve the best live release now, cache it, then stream.
		libName := ""
		if item != nil {
			libName = item.LibraryName
		}
		res, err := worker.resolvePlayable(slowCtx, mediaType, libName, tmdbID, season, episode, "", nil)
		if err != nil {
			if slowCtx.Err() != nil && r.Context().Err() == nil {
				slog.Warn("play: resolve hit the deadline", "tmdb", tmdbID, "s", season, "e", episode)
				http.Error(w, "could not find a playable release within the time limit — try again shortly",
					http.StatusGatewayTimeout)
				return
			}
			slog.Warn("play: resolve failed", "tmdb", tmdbID, "s", season, "e", episode, "err", err)
			// A caller-visible reason, with no transport detail or upstream URL in it.
			http.Error(w, "no playable release available right now", http.StatusBadGateway)
			return
		}
		worker.cacheResolved(res, item, mediaType, tmdbID, season, episode)
		maybePreWarm(r, worker, mediaType, tmdbID, season, episode, res.lengthBytes)
		streamProxy(w, r, res.hash, res.fileIndex)
	}
	mux.HandleFunc("GET /play/movie/{tmdb}", func(w http.ResponseWriter, r *http.Request) {
		tmdbID, err := strconv.Atoi(r.PathValue("tmdb"))
		if err != nil || tmdbID <= 0 {
			http.Error(w, "bad tmdb id", http.StatusBadRequest)
			return
		}
		playHandler(w, r, "movie", tmdbID, 0, 0)
	})
	mux.HandleFunc("GET /play/tv/{tmdb}/{season}/{episode}", func(w http.ResponseWriter, r *http.Request) {
		tmdbID, e1 := strconv.Atoi(r.PathValue("tmdb"))
		season, e2 := strconv.Atoi(r.PathValue("season"))
		episode, e3 := strconv.Atoi(r.PathValue("episode"))
		if e1 != nil || e2 != nil || e3 != nil || tmdbID <= 0 || season < 0 || episode < 0 {
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
			httpFail(w, r, http.StatusBadGateway, "TMDB is unavailable right now", err)
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
			httpFail(w, r, http.StatusBadGateway, "metadata lookup failed", err)
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
		// err.Error() here embedded the whole upstream URL — including the Prowlarr API
		// key while it travelled as a query parameter — and was returned verbatim to an
		// UNAUTHENTICATED caller. Verified: the key appeared in the response body.
		releases, err := indexerClient.SearchContext(r.Context(), query, cats)
		if err != nil {
			httpFail(w, r, http.StatusBadGateway, indexerMessage(err), err)
			return
		}
		scored := picker.Score(releases, livePicker(cfg), details.Title, details.Year)
		jsonOK(w, scored)
	})

	// Remove — fully deletes an item from the library (user-initiated, not expiry).
	// Accessible to any logged-in user so the media UI can remove items.
	// mayRemove enforces ownership on every library deletion.
	//
	// These endpoints were RequireAuth with NO ownership or admin check at all, so any
	// authenticated user could delete any other user's movie, season or series — verified
	// with a non-admin account deleting someone else's season and getting a 200.
	mayRemove := func(user *store.User, it *store.Item) bool {
		return user != nil && (user.IsAdmin || it.RequestedBy == user.Username)
	}

	// removeItems deletes each item the caller is allowed to delete (row + .strm), then
	// drops any torrent no longer referenced by a remaining library row — safe for season
	// packs that share a hash. Returns how many were removed and how many were skipped
	// for lack of permission, so a mixed batch reports honestly instead of failing whole.
	removeItems := func(user *store.User, items []*store.Item) (removed int, skipped int, err error) {
		var done []*store.Item
		for _, it := range items {
			if !mayRemove(user, it) {
				skipped++
				continue
			}
			if rerr := library.RemoveStrm(it.StrmPath); rerr != nil {
				slog.Error("could not remove .strm file", "path", it.StrmPath, "err", rerr)
			}
			if derr := db.DeleteItem(it.StrmPath); derr != nil {
				// Leaving the row while the file is gone would show a permanently broken
				// library entry, so report rather than swallow.
				return len(done), skipped, fmt.Errorf("delete library row: %w", derr)
			}
			done = append(done, it)
		}
		seen := map[string]bool{}
		for _, it := range done {
			if it.InfoHash == "" || seen[it.InfoHash] {
				continue
			}
			seen[it.InfoHash] = true
			n, cerr := db.CountByHash(it.InfoHash)
			if cerr != nil {
				// Unknown reference count: keep the torrent. Dropping one another
				// episode still needs is worse than leaving one cached.
				slog.Error("could not count references for hash; keeping the torrent",
					"hash", it.InfoHash, "err", cerr)
				continue
			}
			if n == 0 {
				if derr := tsClient.Drop(it.InfoHash); derr != nil {
					slog.Warn("could not drop torrent from TorrServer", "hash", it.InfoHash, "err", derr)
				}
			}
		}
		if len(done) > 0 {
			notifyJellyfinScan(jfClient)
		}
		return len(done), skipped, nil
	}

	mux.Handle("POST /api/library/{hash}/drop", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash := strings.ToLower(strings.TrimSpace(r.PathValue("hash")))
		if !torrserver.ValidInfoHash(hash) {
			jsonErr(w, "bad hash", http.StatusBadRequest)
			return
		}
		item, err := db.GetByHash(hash)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
			return
		}
		if item == nil {
			jsonErr(w, "not found", http.StatusNotFound)
			return
		}
		user := api.UserFromContext(r)
		if !mayRemove(user, item) {
			jsonErr(w, "that item belongs to someone else", http.StatusForbidden)
			return
		}
		n, _, err := removeItems(user, []*store.Item{item})
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not remove that item", err)
			return
		}
		jsonOK(w, map[string]any{"status": "removed", "hash": hash, "count": n})
	})))

	// Remove an entire series.
	mux.Handle("POST /api/library/series/{tmdbid}/drop", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("tmdbid"))
		if err != nil {
			jsonErr(w, "bad tmdb id", http.StatusBadRequest)
			return
		}
		items, err := db.ItemsByTMDB(id, "tv")
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
			return
		}
		n, skipped, err := removeItems(api.UserFromContext(r), items)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not remove that series", err)
			return
		}
		jsonOK(w, map[string]any{"status": "removed", "count": n, "skipped": skipped})
	})))

	// Remove all episodes of one season.
	mux.Handle("POST /api/library/season/drop", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TMDBID int `json:"tmdb_id"`
			Season int `json:"season"`
		}
		if err := decodeBody(w, r, &req); err != nil || req.TMDBID == 0 {
			jsonErr(w, "tmdb_id and season required", http.StatusBadRequest)
			return
		}
		all, err := db.ItemsByTMDB(req.TMDBID, "tv")
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
			return
		}
		var inSeason []*store.Item
		for _, it := range all {
			if it.Season == req.Season {
				inSeason = append(inSeason, it)
			}
		}
		n, skipped, err := removeItems(api.UserFromContext(r), inSeason)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not remove that season", err)
			return
		}
		jsonOK(w, map[string]any{"status": "removed", "count": n, "skipped": skipped})
	})))

	// Remove a single episode (by tmdb_id+season+episode — never by shared hash).
	mux.Handle("POST /api/library/episode/drop", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TMDBID  int `json:"tmdb_id"`
			Season  int `json:"season"`
			Episode int `json:"episode"`
		}
		if err := decodeBody(w, r, &req); err != nil || req.TMDBID == 0 {
			jsonErr(w, "tmdb_id, season, episode required", http.StatusBadRequest)
			return
		}
		item, err := db.GetEpisode(req.TMDBID, req.Season, req.Episode)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
			return
		}
		if item == nil {
			jsonErr(w, "episode not in library", http.StatusNotFound)
			return
		}
		user := api.UserFromContext(r)
		if !mayRemove(user, item) {
			jsonErr(w, "that item belongs to someone else", http.StatusForbidden)
			return
		}
		if _, _, err := removeItems(user, []*store.Item{item}); err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not remove that episode", err)
			return
		}
		jsonOK(w, map[string]string{"status": "removed"})
	})))

	// ── Settings API (admin only) ─────────────────────────────────────────────
	// Public read of whether core services are configured (media UI shows a banner).
	// Extended per API contract §5. This is what makes a fresh install explain itself
	// instead of rendering a blank page: the UI can say exactly which piece is missing.
	//
	// indexer_count is the fix for the invisible "Prowlarr installed with zero indexers"
	// trap — a valid URL and key with no indexers behind it returns empty results forever
	// and looks identical to "nothing matched".
	//
	// jellyfin_url is the LAN URL only. The API key is NEVER included.
	mux.HandleFunc("GET /api/configured", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		tmdbOK := tmdbClient.Configured()
		prowlarrOK := indexerClient.Configured()
		jellyfinOK := jfClient.Configured()
		torrOK := tsClient.Configured()

		indexerCount := -1
		if prowlarrOK {
			if n, err := indexerClient.IndexerCount(ctx); err == nil {
				indexerCount = n
			} else {
				slog.Debug("configured: indexer count unavailable", "err", err)
			}
		}

		jellyfinURL, err := db.GetSetting("conn.jellyfin.url")
		if err != nil {
			slog.Error("configured: could not read the Jellyfin URL setting", "err", err)
		}

		vpnConfigured, vpnActive := vpnState(cfg.VPNConfigDir())

		setupComplete := tmdbOK && prowlarrOK && jellyfinOK && torrOK && indexerCount > 0

		jsonOK(w, map[string]any{
			"tmdb":           tmdbOK,
			"prowlarr":       prowlarrOK,
			"jellyfin":       jellyfinOK,
			"torrserver":     torrOK,
			"vpn_configured": vpnConfigured,
			"vpn_active":     vpnActive,
			"indexer_count":  indexerCount,
			"jellyfin_url":   jellyfinURL,
			"setup_complete": setupComplete,
		})
	})

	mux.Handle("GET /api/settings", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var readErr error
		get := func(k string) string {
			v, err := db.GetSetting(k)
			if err != nil && readErr == nil {
				readErr = fmt.Errorf("read %s: %w", k, err)
			}
			return v
		}
		keySet := func(k string) bool { return get(k) != "" }
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
		// The Jellyfin webhook secret is READ-ONLY here and admin-only (this whole
		// handler is behind RequireAdmin).
		//
		// It has to be readable somewhere: it is generated on first run, playback-stop
		// torrent dropping now requires it, and an operator has no other way to get at
		// it — sqlite3 is not installed on a stock box. Without this, an upgrade would
		// silently stop cleaning up torrents with no recoverable path.
		webhookSecret := get(webhookSecretSetting)
		if readErr != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the saved settings", readErr)
			return
		}
		jsonOK(w, map[string]any{
			"connections": map[string]any{
				"tmdb":       map[string]any{"key_set": keySet("conn.tmdb.key")},
				"prowlarr":   map[string]any{"url": get("conn.prowlarr.url"), "key_set": keySet("conn.prowlarr.key")},
				"jellyfin":   map[string]any{"url": get("conn.jellyfin.url"), "key_set": keySet("conn.jellyfin.key")},
				"torrserver": map[string]any{"url": get("conn.torrserver.url")},
			},
			// Everything the operator needs to configure the Jellyfin webhook plugin.
			"webhook": map[string]any{
				"secret": webhookSecret,
				"header": WebhookHeader,
				"url":    strings.TrimRight(cfg.Server.PublicURL, "/") + "/webhook/jellyfin",
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
		if err := decodeBody(w, r, &req); err != nil {
			jsonErr(w, "bad request", http.StatusBadRequest)
			return
		}
		// Every SetSetting error used to be discarded, so Settings reported "saved" for a
		// value that was never written — the setting silently reverted on the next read.
		var saveErr error
		setIf := func(key, val string, keepBlank bool) {
			if saveErr != nil || (val == "" && keepBlank) {
				return // blank means "leave existing key untouched"
			}
			if err := db.SetSetting(key, val); err != nil {
				saveErr = fmt.Errorf("save %s: %w", key, err)
			}
		}
		setIf("conn.tmdb.key", req.TMDBKey, true)
		setIf("conn.prowlarr.url", req.ProwlarrURL, false)
		setIf("conn.prowlarr.key", req.ProwlarrKey, true)
		setIf("conn.jellyfin.url", req.JellyfinURL, false)
		setIf("conn.jellyfin.key", req.JellyfinKey, true)
		setIf("conn.torrserver.url", req.TorrServerURL, false)
		if saveErr != nil {
			httpFail(w, r, http.StatusInternalServerError, "settings were NOT saved — see the server log", saveErr)
			return
		}
		// Reconfigure clients live from the now-updated DB values.
		if err := applyConnections(db, cfg, tmdbClient, indexerClient, jfClient, tsClient); err != nil {
			httpFail(w, r, http.StatusInternalServerError,
				"settings were saved but could not be applied — restart the service", err)
			return
		}
		jsonOK(w, map[string]string{"status": "saved"})
	})))

	// Test a connection with the supplied (unsaved) credentials.
	mux.Handle("POST /api/settings/connections/test", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Service string `json:"service"`
			URL     string `json:"url"`
			Key     string `json:"key"`
		}
		if err := decodeBody(w, r, &req); err != nil {
			jsonErr(w, "bad request", http.StatusBadRequest)
			return
		}
		// The service name indexes a settings key, so constrain it to the known set
		// rather than letting a request read an arbitrary setting.
		switch req.Service {
		case "tmdb", "prowlarr", "jellyfin", "torrserver":
		default:
			jsonErr(w, "unknown service", http.StatusBadRequest)
			return
		}
		// Blank field = test the currently-saved value (key is masked in the UI).
		if req.Key == "" {
			v, err := db.GetSetting("conn." + req.Service + ".key")
			if err != nil {
				httpFail(w, r, http.StatusInternalServerError, "could not read the saved key", err)
				return
			}
			req.Key = v
		}
		if req.URL == "" {
			v, err := db.GetSetting("conn." + req.Service + ".url")
			if err != nil {
				httpFail(w, r, http.StatusInternalServerError, "could not read the saved URL", err)
				return
			}
			req.URL = v
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
		if err := decodeBody(w, r, &req); err != nil {
			jsonErr(w, "bad request", http.StatusBadRequest)
			return
		}
		// Persist FIRST, then apply in memory. If the write fails the in-memory config is
		// left untouched, so what the process is running always matches what is on disk —
		// rather than reporting "saved", running with the new value, and reverting on the
		// next restart.
		writes := map[string]string{}
		if req.MinSeeders != nil {
			writes["quality.min_seeders"] = strconv.Itoa(*req.MinSeeders)
		}
		if req.MaxSizeGB != nil {
			writes["quality.max_size_gb"] = strconv.Itoa(*req.MaxSizeGB)
		}
		if req.RejectCAM != nil {
			writes["quality.reject_cam"] = strconv.FormatBool(*req.RejectCAM)
		}
		if req.VideoCodecs != nil {
			writes["quality.video_codecs"] = *req.VideoCodecs
		}
		if req.AudioCodecs != nil {
			writes["quality.audio_codecs"] = *req.AudioCodecs
		}
		if req.Containers != nil {
			writes["quality.containers"] = *req.Containers
		}
		for k, v := range writes {
			if err := db.SetSetting(k, v); err != nil {
				httpFail(w, r, http.StatusInternalServerError,
					"quality settings were NOT saved — see the server log", fmt.Errorf("save %s: %w", k, err))
				return
			}
		}

		settingsMu.Lock()
		if req.MinSeeders != nil {
			cfg.Picker.MinSeeders = *req.MinSeeders
		}
		if req.MaxSizeGB != nil {
			cfg.Picker.MaxSizeGB = *req.MaxSizeGB
		}
		if req.RejectCAM != nil {
			b := *req.RejectCAM
			cfg.Picker.RejectCAM = &b
		}
		if req.VideoCodecs != nil {
			cfg.Picker.PreferVideoCodecs = splitCSV(*req.VideoCodecs)
		}
		if req.AudioCodecs != nil {
			cfg.Picker.PreferAudioCodecs = splitCSV(*req.AudioCodecs)
		}
		if req.Containers != nil {
			cfg.Picker.PreferContainers = splitCSV(*req.Containers)
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
		if err := decodeBody(w, r, &req); err != nil {
			jsonErr(w, "bad request", http.StatusBadRequest)
			return
		}
		writes := map[string]string{}
		if req.Mode != nil {
			if *req.Mode != "ram" && *req.Mode != "disk" && *req.Mode != "" {
				jsonErr(w, "mode must be 'ram' or 'disk'", http.StatusBadRequest)
				return
			}
			writes["cache.mode"] = *req.Mode
		}
		if req.SizeMB != nil {
			writes["cache.size_mb"] = strconv.Itoa(*req.SizeMB)
		}
		if req.Path != nil {
			writes["cache.path"] = *req.Path
		}
		if req.DisconnectS != nil {
			writes["cache.disconnect_s"] = strconv.Itoa(*req.DisconnectS)
		}
		if req.Connections != nil {
			writes["cache.connections"] = strconv.Itoa(*req.Connections)
		}
		if req.Retrackers != nil {
			writes["cache.retrackers"] = strconv.Itoa(*req.Retrackers)
		}
		if req.UploadKB != nil {
			writes["cache.upload_kb"] = strconv.Itoa(*req.UploadKB)
		}
		for k, v := range writes {
			if err := db.SetSetting(k, v); err != nil {
				httpFail(w, r, http.StatusInternalServerError,
					"cache settings were NOT saved — see the server log", fmt.Errorf("save %s: %w", k, err))
				return
			}
		}

		// This block mutated cfg.TorrServer.Cache field-by-field with NO lock, while other
		// paths (the settings reader, applyTorrCache, and the background startup retry
		// goroutine) read the same struct under settingsMu.
		settingsMu.Lock()
		c := &cfg.TorrServer.Cache
		if req.Mode != nil {
			c.Mode = *req.Mode
		}
		if req.SizeMB != nil {
			c.SizeMB = *req.SizeMB
		}
		if req.Path != nil {
			c.Path = *req.Path
		}
		if req.DisconnectS != nil {
			c.DisconnectTimeoutS = *req.DisconnectS
		}
		if req.Connections != nil {
			c.ConnectionsLimit = *req.Connections
		}
		if req.Retrackers != nil {
			v := *req.Retrackers
			c.RetrackersMode = &v
		}
		if req.UploadKB != nil {
			c.UploadRateLimitKB = *req.UploadKB
		}
		// Snapshot under the same lock so the apply below reads a consistent profile.
		snapshot := *c
		settingsMu.Unlock()

		if err := applyTorrCacheSettings(tsClient, snapshot); err != nil {
			httpFail(w, r, http.StatusBadGateway,
				"saved, but TorrServer would not accept the new cache profile", err)
			return
		}
		jsonOK(w, map[string]string{"status": "saved"})
	})))

	// ------------------------------------------------------------------ //
	// VPN config management (admin) — upload/list/activate/delete/download WireGuard configs
	// from the browser. Configs live in the orchestrator-owned dir (cfg.VPNConfigDir); upload
	// never auto-activates. Activation materializes the chosen config as the live tunnel and
	// brings the tunnel down and back up through the root-owned helper (no unit restart, no
	// new sudoers rules — the helper's fixed verb set is the entire privileged surface).
	// ------------------------------------------------------------------ //
	vpnDir := cfg.VPNConfigDir()

	mux.Handle("GET /api/vpn/configs", api.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activeB, err := os.ReadFile(filepath.Join(vpnDir, ".active"))
		if err != nil && !os.IsNotExist(err) {
			slog.Warn("could not read the active-VPN marker", "dir", vpnDir, "err", err)
		}
		active := strings.TrimSpace(string(activeB))
		entries, err := os.ReadDir(vpnDir)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError,
				"could not read the VPN config directory — check it exists and is writable by the service user", err)
			return
		}
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
			content, err := os.ReadFile(filepath.Join(vpnDir, n))
			if err != nil {
				slog.Warn("could not read a VPN config for listing", "name", n, "err", err)
			}
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
		// Privilege contract, LAYER 1: strip every directive wg-quick would execute as a
		// root shell command, plus Table (which can suppress route installation and
		// silently defeat a routing-only kill switch). The helper re-strips these at
		// vpn-up (layer 2) so a bug here cannot become root code execution.
		clean, stripped := vpnSanitizeConf(req.Content)
		if err := os.WriteFile(filepath.Join(vpnDir, slug+".conf"), []byte(clean), 0600); err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not store that config", err)
			return
		}
		slog.Info("vpn config uploaded", "name", slug, "stripped", stripped)
		jsonOK(w, map[string]any{"name": slug, "stored": true, "stripped": stripped})
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
		livePath := filepath.Join(vpnDir, "wg0-vpntorrent.conf")

		// Keep the current live config so a failed activation can be rolled back. Without
		// this, the previous handler overwrote the working tunnel and THEN discarded both
		// restart errors — reporting "activated" while the tunnel was down and the kill
		// switch was the only thing preventing a leak.
		previous, hadPrevious := os.ReadFile(livePath)
		restore := func() {
			if hadPrevious != nil {
				return
			}
			if err := os.WriteFile(livePath, previous, 0600); err != nil {
				slog.Error("could not restore the previous VPN config after a failed activation", "err", err)
			}
		}

		clean, _ := vpnSanitizeConf(string(content))
		if err := os.WriteFile(livePath, []byte(clean), 0600); err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not write the live VPN config", err)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()

		// Bounce the TUNNEL, not the whole unit. vpn-up re-sanitises the config,
		// re-pins the endpoint, refreshes the firewall rule and resets the default
		// route, so a server change is picked up — and it is far less disruptive than
		// restarting vpntorrent-netns.service, which tears the namespace down under
		// TorrServer.
		if out, err := api.VPNDown(ctx); err != nil {
			// A tunnel that was already down is not a failure of activation.
			slog.Info("vpn-down before activation reported a problem (continuing)", "err", err, "output", out)
		}
		if out, err := api.VPNUp(ctx); err != nil {
			restore()
			if errors.Is(err, api.ErrNoVPNConfig) {
				httpFail(w, r, http.StatusBadRequest,
					"no VPN config is active — upload one and activate it first", err)
				return
			}
			httpFail(w, r, http.StatusInternalServerError,
				"could not bring the tunnel up with that config — the previous config was restored: "+
					strings.TrimSpace(out),
				fmt.Errorf("vpn-up: %w (%s)", err, out))
			return
		}

		// A restart that "succeeded" still proves nothing: report activated only once the
		// tunnel has actually completed a handshake.
		if !api.WaitVPNHandshake(ctx, 30*time.Second) {
			restore()
			httpFail(w, r, http.StatusBadGateway,
				"the tunnel did not complete a handshake with that config — the previous config was restored",
				fmt.Errorf("no wireguard handshake after activating %q", slug))
			return
		}

		if err := os.WriteFile(filepath.Join(vpnDir, ".active"), []byte(slug), 0600); err != nil {
			// The tunnel IS up on the new config; only the bookkeeping failed.
			slog.Error("vpn activated but the .active marker could not be written", "name", slug, "err", err)
		}
		// TorrServer holds sockets bound inside the namespace, so it needs a restart to
		// pick up the new exit. Do it AFTER the tunnel is verified up, so a bad config
		// never costs the user a TorrServer bounce.
		if out, err := api.RestartUnit(ctx, "torrserver-netns"); err != nil {
			slog.Error("vpn activated but TorrServer could not be restarted",
				"name", slug, "err", err, "output", out)
			jsonOK(w, map[string]any{
				"activated": slug,
				"warning":   "the tunnel is up, but TorrServer could not be restarted — restart it from Services",
			})
			return
		}
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
			httpFail(w, r, http.StatusInternalServerError, "could not delete that config", err)
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
	// This endpoint is state-changing and cannot carry a session (Jellyfin's webhook
	// plugin has no way to log in), so it is authenticated by a SHARED SECRET sent in a
	// custom header — which that plugin does support. Without it, anyone who guessed an
	// ItemId could drop that torrent mid-playback.
	//
	// The secret is generated on first run and shown in Settings; if none is configured
	// the endpoint refuses rather than silently accepting anonymous calls.
	mux.HandleFunc("POST /webhook/jellyfin", func(w http.ResponseWriter, r *http.Request) {
		if !webhookAuthorised(db, r) {
			slog.Warn("jellyfin webhook: rejected an unauthenticated call", "remote", r.RemoteAddr)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
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
		n, err := db.CountReadyByHashExcept(item.InfoHash, item.StrmPath)
		if err != nil {
			// Unknown reference count: KEEP the torrent. Dropping one that another
			// episode is still streaming would kill someone else's playback.
			slog.Error("webhook: could not count references for this hash; keeping the torrent",
				"hash", item.InfoHash, "err", err)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if n == 0 {
			if err := tsClient.Drop(item.InfoHash); err != nil {
				slog.Warn("webhook: could not drop torrent after playback stop", "title", item.Title, "err", err)
			} else {
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
	// In-dashboard self-update. The whole feature lives behind RequireAdmin (below):
	// /api/update/apply runs a root-owned helper.
	updater := update.New(version)

	protected := api.RequireAdmin(buildProtectedMux(db, assets, indexerClient, jfClient, livePicker(cfg), updater))

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

	srv := newServer(cfg.Server.Listen, mux)
	slog.Info("starting orchestrator", "listen", cfg.Server.Listen, "version", version)
	if err := runServer(ctx, srv, 20*time.Second); err != nil {
		slog.Error("server error", "err", err)
		// Return rather than os.Exit so the deferred db.Close() actually runs — it
		// never did before, because ListenAndServe only returned on a fatal error and
		// the process was then killed with the SQLite connection still open.
		stop()
		_ = db.Close()
		os.Exit(1)
	}
	slog.Info("stopped cleanly", "version", version)

	// A self-restart is a clean shutdown that must NOT look like a clean exit to
	// systemd. Everything above has already run — workers cancelled, HTTP drained —
	// so all that is left is to close the store and exit non-zero.
	if selfRestartRequested.Load() {
		stop()
		if err := db.Close(); err != nil {
			slog.Error("self-restart: closing the store", "err", err)
		}
		slog.Warn("self-restart: exiting non-zero so systemd starts this service again",
			"exit_code", selfRestartExitCode)
		// LOAD-BEARING, not an error path: Restart=on-failure only fires for a non-zero
		// exit. Exiting 0 here would leave JellyFreedom stopped. See selfrestart.go.
		os.Exit(selfRestartExitCode)
	}
}

func buildProtectedMux(db *store.Store, assets fs.FS, indexerClient *indexer.Client,
	jfClient *jellyfin.Client, pickerCfg picker.Config, updater *update.Service) http.Handler {

	mux := http.NewServeMux()

	// Dashboard UI (served from web/dashboard/)
	mux.Handle("GET /dashboard/", http.StripPrefix("/dashboard/", http.FileServerFS(subFS(assets, "dashboard"))))
	mux.Handle("GET /dashboard", http.RedirectHandler("/dashboard/", http.StatusFound))

	// API — tasks
	mux.HandleFunc("GET /api/tasks", api.TasksHandler)
	mux.HandleFunc("POST /api/tasks/{name}/run", api.TaskRunHandler)

	// API — self-update (check / apply / status). See internal/update.
	updater.Register(mux)

	// API — service management
	mux.HandleFunc("GET /api/logs", api.LogsHandler)
	mux.HandleFunc("POST /api/services/{name}/restart", api.ServiceRestartHandler)
	mux.HandleFunc("GET /api/vpn", api.VPNHandler)
	// POST /api/auth/change-password is registered on the OUTER mux under RequireAuth —
	// it changes the CALLING user's password, so gating it behind RequireAdmin (as this
	// mux does) gave every non-admin a 403 on their own account.

	// API — user management
	mux.HandleFunc("GET /api/users", api.UsersHandler)
	mux.HandleFunc("POST /api/users", api.CreateUserHandler)
	mux.HandleFunc("POST /api/users/import", api.ImportUserHandler)
	mux.HandleFunc("PATCH /api/users/{id}", api.UpdateUserHandler)
	mux.HandleFunc("DELETE /api/users/{id}", api.DeleteUserHandler)
	mux.HandleFunc("GET /api/jellyfin/users", api.JellyfinUsersHandler(jfClient))

	mux.HandleFunc("POST /api/library/{hash}/toggle-private", func(w http.ResponseWriter, r *http.Request) {
		hash := strings.ToLower(strings.TrimSpace(r.PathValue("hash")))
		if !torrserver.ValidInfoHash(hash) {
			jsonErr(w, "bad hash", http.StatusBadRequest)
			return
		}
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

	// NOTE: the duplicate "POST /api/library/{hash}/drop" that used to live here was
	// DEAD — the outer mux registers the same pattern and matches first, so this copy was
	// never reachable. It also had no ownership check. Removed; the outer, ownership-
	// checked handler is the only one.

	// Release inspector. This was registered as "GET /debug/releases" on a mux that is
	// only ever reached via the "/api/" and "/dashboard/" prefixes, so it answered 404
	// (not 403) and was verified unreachable. Re-registered under /api/ so it works, and
	// so the UI can use its data (API contract §7).
	mux.HandleFunc("GET /api/debug/releases", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			jsonErr(w, "q is required", http.StatusBadRequest)
			return
		}
		releases, err := indexerClient.SearchContext(r.Context(), q, []int{indexer.CatMovies, indexer.CatTV})
		if err != nil {
			httpFail(w, r, http.StatusBadGateway, indexerMessage(err), err)
			return
		}
		scored := picker.Score(releases, pickerCfg, "", "")
		best := picker.Best(releases, pickerCfg)
		jsonOK(w, map[string]any{"total": len(releases), "best": best, "scored": scored, "all": releases})
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

	// resolves de-duplicates concurrent resolves of the SAME identity, so a client
	// hammering refresh cannot multiply a 90-second search.
	resolves *resolveGroup
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

// progress records human-readable prose AND the machine-readable stage token the UI
// renders as a stepper (API contract §6).
func (w *queueWorker) progress(item *store.QueueItem, stage, msg string) {
	item.Progress = msg
	item.Stage = stage
	item.UpdatedAt = time.Now()
	if err := w.db.UpdateQueue(item); err != nil {
		// Not fatal to the job itself, but it freezes the UI's progress display, so it
		// must be visible rather than silently dropped.
		slog.Error("queue: could not persist progress", "id", item.ID, "stage", stage, "err", err)
	}
}

func (w *queueWorker) processNext(ctx context.Context) {
	item, err := w.db.NextPending()
	if err != nil {
		slog.Error("queue: could not claim the next item", "err", err)
		return
	}
	if item == nil {
		return
	}
	slog.Info("queue: processing", "id", item.ID, "title", item.Title, "type", item.MediaType)

	fail := func(msg string, diag string) {
		item.Status = "failed"
		item.Stage = store.StageFailed
		item.ErrorMsg = msg
		item.Diagnosis = diag
		if err := w.db.UpdateQueue(item); err != nil {
			// Without this the row stays 'processing' forever and the UI spins.
			slog.Error("queue: could not record the failure", "id", item.ID, "err", err)
		}
	}

	res, err := w.resolvePlayable(ctx, item.MediaType, item.LibraryName,
		item.TMDBID, item.Season, item.Episode, item.MagnetOverride,
		func(stage, s string) { w.progress(item, stage, s) })
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down — leave the row 'processing' for restart recovery
		}
		var nr *noReleaseError
		if errors.As(err, &nr) {
			fail(nr.Error(), nr.JSON())
			return
		}
		fail(redact.String(err.Error()), "")
		return
	}

	w.progress(item, store.StageWriting, "Writing to library…")
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
		if derr := w.ts.Drop(res.hash); derr != nil {
			slog.Warn("queue: could not drop torrent after a failed library write", "hash", res.hash, "err", derr)
		}
		fail("Library write failed: "+err.Error(), "")
		return
	}

	// Drop the previously-cached torrent if this resolve picked a different release.
	if existing, gerr := w.db.GetByStrmPath(strmPath); gerr != nil {
		slog.Error("queue: could not look up the existing library row", "path", strmPath, "err", gerr)
	} else if existing != nil && existing.InfoHash != res.hash && existing.InfoHash != "" {
		if derr := w.ts.Drop(existing.InfoHash); derr != nil {
			slog.Warn("queue: could not drop the superseded torrent", "hash", existing.InfoHash, "err", derr)
		}
	}

	// THE critical write. Discarding this error left the .strm on disk and the queue row
	// marked done with NO library row behind it — so /play found nothing cached and
	// re-ran the whole search → add → validate pipeline on EVERY playback. That is the
	// "it works, but it takes 60 seconds every single time" symptom.
	if err := w.db.Upsert(&store.Item{
		TMDBID: item.TMDBID, MediaType: item.MediaType, Title: res.displayTitle,
		Year: res.year, InfoHash: res.hash, FileIndex: res.fileIndex,
		StrmPath: strmPath, LibraryName: res.lib.Name,
		Status: "ready", Seeders: res.seeders, Updated: time.Now(),
		RequestedBy: item.RequestedBy, PosterURL: item.PosterURL,
		Magnet: res.magnet, ReleaseTitle: res.releaseTitle,
		Season: item.Season, Episode: item.Episode, StaleSince: nil,
	}); err != nil {
		// Roll the .strm back so we do not leave a pointer file with no row behind it.
		if rerr := library.RemoveStrm(strmPath); rerr != nil {
			slog.Error("queue: could not roll back the .strm after a failed library write",
				"path", strmPath, "err", rerr)
		}
		if derr := w.ts.Drop(res.hash); derr != nil {
			slog.Warn("queue: could not drop torrent after a failed library write", "hash", res.hash, "err", derr)
		}
		fail("Could not record the item in the library — see the server log", "")
		slog.Error("queue: library upsert FAILED", "id", item.ID, "path", strmPath, "err", err)
		return
	}
	// Drop-after-validate: the .strm is written and the chosen hash is cached, so drop now.
	// /play re-adds on demand (and re-resolves if this release later dies). Requesting a whole
	// season therefore leaves ZERO background load.
	if derr := w.ts.Drop(res.hash); derr != nil {
		slog.Warn("queue: could not drop torrent after validating", "hash", res.hash, "err", derr)
	}
	notifyJellyfinScan(w.jf)

	item.Status = "done"
	item.Stage = store.StageDone
	item.Progress = ""
	item.ErrorMsg = ""
	item.Diagnosis = ""
	item.InfoHash = res.hash
	item.StrmPath = strmPath
	if err := w.db.UpdateQueue(item); err != nil {
		slog.Error("queue: item completed but the row could not be marked done", "id", item.ID, "err", err)
	}
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
func (w *queueWorker) resolvePlayable(ctx context.Context, mediaType, libraryName string, tmdbID, season, episode int, magnetOverride string, progress func(stage, msg string)) (*resolveResult, error) {
	prog := func(stage, s string) {
		if progress != nil {
			progress(stage, s)
		}
	}

	prog(store.StageIndexing, "Looking up metadata…")
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
		prog(store.StageIndexing, "Searching releases…")
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
		releases, err := w.indexer.SearchContext(ctx, query, cats)
		if err != nil {
			return nil, fmt.Errorf("%s", indexerMessage(err))
		}
		if mediaType == "tv" && len(releases) == 0 {
			fallback := fmt.Sprintf("%s Season %d", details.Title, season)
			fb, ferr := w.indexer.SearchContext(ctx, fallback, cats)
			if ferr != nil {
				slog.Warn("resolve: season-pack fallback search failed", "query", fallback, "err", ferr)
			} else {
				releases = fb
			}
		}
		allReleases := releases
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
		prog(store.StagePicking, "Ranking releases…")
		for _, sr := range picker.Score(releases, pc, details.Title, details.Year) {
			if pc.RejectCAM && sr.IsCAM {
				continue
			}
			candidates = append(candidates, sr.Release)
		}
		if len(candidates) == 0 {
			// Carry the full rejection breakdown so GET /api/queue/{id}/diagnosis can
			// answer "why?" without the user reading logs (API contract §7).
			return nil, newNoReleaseError(allReleases, pc, details.Title, details.Year)
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
			prog(store.StageAdding, fmt.Sprintf("Trying release %d…", n+1))
		} else {
			prog(store.StageAdding, "Adding to TorrServer…")
		}
		hash, err := w.ts.Add(cand.Magnet, details.Title)
		if err != nil {
			// A failing add (e.g. TorrServer 500 = crashed BT client) won't differ between
			// candidates, so don't burn the rest — surface it.
			return nil, fmt.Errorf("TorrServer add failed: %w", err)
		}

		prog(store.StageVerifying, "Waiting for file list…")
		var fileIndex int
		var chosenFile *torrserver.FileInfo
		episodeMatched := true
		resolved := false
		for i := 0; i < 10; i++ {
			if ctx.Err() != nil {
				w.dropQuietly(hash)
				return nil, ctx.Err()
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
			// Context-aware: a cancelled request stops burning the wait immediately.
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
		if !resolved {
			w.dropQuietly(hash)
			lastErr = fmt.Errorf("no file list (slow/dead swarm)")
			continue
		}
		if mediaType == "tv" && !episodeMatched {
			w.dropQuietly(hash)
			lastErr = fmt.Errorf("S%02dE%02d not in this release (wrong show or mislabeled pack)", season, episode)
			continue
		}
		if chosenFile != nil {
			if err := validateVideoFile(chosenFile, mediaType); err != nil {
				w.dropQuietly(hash)
				lastErr = err
				continue
			}
		}
		// M18: only commit a release that can actually reach the swarm — skip ghosts whose
		// scrape count is high but have no reachable peers (resolve metadata, never stream).
		prog(store.StageVerifying, "Checking peers…")
		if !w.ts.WaitConnectable(ctx, hash, 15) {
			w.dropQuietly(hash)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
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
	if err := w.db.Upsert(existing); err != nil {
		// Playback still works from the in-memory result; the cost is that the NEXT
		// play re-resolves from scratch. That silent degradation is exactly what made
		// "/play takes 60s every time" invisible, so it must be logged.
		slog.Error("could not cache the resolved release", "tmdb", tmdbID, "s", season, "e", episode, "err", err)
	}
}

// dropQuietly drops a torrent, logging rather than ignoring a failure. A leaked
// torrent keeps consuming the bounded TorrServer cache.
func (w *queueWorker) dropQuietly(hash string) {
	if err := w.ts.Drop(hash); err != nil {
		slog.Warn("could not drop torrent", "hash", hash, "err", err)
	}
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

	next, err := w.db.GetByIdentity(tmdbID, "tv", ns, ne)
	if err != nil {
		slog.Error("prewarm: could not look up the next episode", "tmdb", tmdbID, "s", ns, "e", ne, "err", err)
		return
	}
	if next == nil {
		return // next episode isn't in the library — nothing to warm
	}
	var hash string
	var idx int
	if next.InfoHash != "" && w.ts.EnsureLoaded(ctx, next.InfoHash, next.Magnet, next.Title, 25) {
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

// playURL builds the stable, identity-keyed Resolve-at-Play URL written into .strm
// files, carrying the capability token that authorises this exact identity.
//
// The token is what lets /play stay unauthenticated (Jellyfin fetches .strm URLs with
// no session) without letting an anonymous stranger drive resolution and torrent adds
// for arbitrary titles over the owner's VPN.
func playURL(publicURL, mediaType string, tmdbID, season, episode int) string {
	base := strings.TrimRight(publicURL, "/")
	var path string
	if mediaType == "movie" {
		path = fmt.Sprintf("%s/play/movie/%d", base, tmdbID)
	} else {
		path = fmt.Sprintf("%s/play/tv/%d/%d/%d", base, tmdbID, season, episode)
	}
	if tok := playToken(mediaType, tmdbID, season, episode); tok != "" {
		path += "?t=" + tok
	}
	return path
}

// applyTorrCache pushes the configured cache profile to TorrServer. Returns nil
// (no-op) when no mode is configured. Logs but does not crash on failure.
func applyTorrCache(tsClient *torrserver.Client, cfg *config.Config) error {
	err := applyTorrCacheOnce(tsClient, cfg)
	if err != nil && !errors.Is(err, errCacheProfileConfig) {
		slog.Warn("failed to apply TorrServer cache profile", "err", err)
	}
	return err
}

// errCacheProfileConfig marks a cache-profile failure that retrying cannot fix
// (bad config), so the startup retry loop gives up instead of spinning forever.
var errCacheProfileConfig = errors.New("invalid cache profile config")

// applyTorrCacheAtStartup applies the cache profile at boot, retrying in the
// background while TorrServer is still coming up. systemd starts us alongside
// torrserver-netns.service, so the very first attempt often hits a TorrServer
// that hasn't bound its port yet. Without this retry that single failure left
// the profile unapplied for the whole process lifetime — TorrServer would keep
// running on its own persisted cache settings, which is exactly the unbounded
// -growth hazard the profile exists to prevent.
func applyTorrCacheAtStartup(ctx context.Context, tsClient *torrserver.Client, cfg *config.Config) {
	settingsMu.RLock()
	mode := cfg.TorrServer.Cache.Mode
	settingsMu.RUnlock()
	if mode == "" {
		return
	}
	err := applyTorrCacheOnce(tsClient, cfg)
	if err == nil || errors.Is(err, errCacheProfileConfig) {
		return
	}
	slog.Info("TorrServer not ready for cache profile yet; retrying in background", "err", err)

	go func() {
		const giveUpAfter = 30 * time.Minute
		deadline := time.Now().Add(giveUpAfter)
		delay := 5 * time.Second
		for {
			select {
			case <-ctx.Done():
				return // shutting down
			case <-time.After(delay):
			}
			err := applyTorrCacheOnce(tsClient, cfg)
			if err == nil || errors.Is(err, errCacheProfileConfig) {
				return
			}
			if time.Now().After(deadline) {
				slog.Warn("giving up on TorrServer cache profile; TorrServer unreachable — "+
					"run the 'apply-cache-profile' task once it is back up",
					"waited", giveUpAfter, "err", err)
				return
			}
			if delay < time.Minute {
				delay *= 2
			}
		}
	}()
}

// applyTorrCacheOnce is the single, non-warning attempt behind both the one-shot
// callers and the startup retry loop.
func applyTorrCacheOnce(tsClient *torrserver.Client, cfg *config.Config) error {
	// Snapshot under the read lock: the Settings handler mutates this struct live.
	settingsMu.RLock()
	cc := cfg.TorrServer.Cache
	settingsMu.RUnlock()
	if cc.Mode == "" {
		return nil // leave TorrServer's own settings untouched
	}
	if cc.Mode == "disk" && cc.Path == "" {
		slog.Error("torrserver cache mode=disk requires a path; skipping cache apply")
		return fmt.Errorf("%w: disk cache mode requires torrserver.cache.path", errCacheProfileConfig)
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
// resolvableSeeders is called with the item's REAL title and year, and requires a title
// match.
//
// It used to pass empty strings, which disables picker's title check entirely — so ANY
// result above the seeder floor, for any title, kept the item "ready". A movie whose
// release had died stayed permanently green as long as the indexer returned literally
// anything for the query. Requiring TitleMatch is what makes the health check mean
// something.
func resolvableSeeders(releases []indexer.Release, pc picker.Config, hash, title, year string) int {
	best := -1
	for _, sr := range picker.Score(releases, pc, title, year) {
		if pc.RejectCAM && sr.IsCAM {
			continue
		}
		if !sr.TitleMatch {
			continue // a different title's release does not keep this item alive
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
//
// Revival back to ready happens lazily here, and the actual torrent is re-added on demand by
// /play — so a stale item self-heals the moment its release is seeded again.
// healthCheckBudget bounds ONE run of the check.
//
// It runs one Prowlarr search per library item, serially, each with a 150s client
// timeout, every 30 minutes. A 200-item library could therefore keep the indexer
// saturated indefinitely and overlap its own next run. The budget stops the run early
// and leaves the remaining items for the next pass, which is fine: staleness is
// eventually-consistent by design.
const (
	healthCheckBudget    = 12 * time.Minute
	healthCheckMaxItems  = 60
	healthCheckItemPause = 250 * time.Millisecond
)

func taskLibraryHealthCheck(ctx context.Context, db *store.Store, idxClient *indexer.Client, pickerCfg picker.Config) error {
	// Fail fast rather than spending the whole budget timing out per item.
	if err := idxClient.Ping(ctx); err != nil {
		return fmt.Errorf("skipping health check: %w", err)
	}

	items, err := db.ListAllItems() // ready + stale (managed) — background-job view
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, healthCheckBudget)
	defer cancel()

	revived, staleMark, checked, failed := 0, 0, 0, 0
	for _, item := range items {
		if ctx.Err() != nil {
			slog.Info("library health check stopped at its budget; remaining items roll to the next run",
				"checked", checked, "total", len(items))
			break
		}
		if checked >= healthCheckMaxItems {
			slog.Info("library health check hit its per-run item cap; remaining items roll to the next run",
				"checked", checked, "total", len(items))
			break
		}
		checked++

		cats := []int{indexer.CatMovies}
		if item.MediaType == "tv" {
			cats = []int{indexer.CatTV}
		}
		releases, err := idxClient.SearchContext(ctx, item.Title, cats)
		if err != nil {
			// Transient indexer error — NEVER flip state on a failed lookup, or an
			// indexer outage would mark the entire library stale.
			failed++
			continue
		}
		seeders := resolvableSeeders(releases, pickerCfg, item.InfoHash, item.Title, item.Year)

		if seeders >= 0 {
			// Still resolvable. Promote out of stale if needed, and refresh seeders.
			wasStale := item.Status == "stale"
			if wasStale {
				item.Status = "ready"
				item.StaleSince = nil
			}
			item.Seeders = seeders
			item.Updated = time.Now()
			if err := db.Upsert(item); err != nil {
				slog.Error("health check: could not update an item", "path", item.StrmPath, "err", err)
				failed++
				continue
			}
			if wasStale {
				revived++
			}
		} else if item.Status != "stale" {
			// Nothing resolvable right now — mark stale (records stale_since on first transition).
			if err := db.MarkStale(item.StrmPath); err != nil {
				slog.Error("health check: could not mark an item stale", "path", item.StrmPath, "err", err)
				failed++
				continue
			}
			staleMark++
		}

		// Be a good citizen towards Prowlarr/FlareSolverr between items.
		select {
		case <-ctx.Done():
		case <-time.After(healthCheckItemPause):
		}
	}
	slog.Info("library health check complete",
		"checked", checked, "total", len(items), "revived", revived, "marked_stale", staleMark, "errors", failed)
	if failed > 0 && failed == checked {
		return fmt.Errorf("every one of %d checks failed — the indexer is likely down", failed)
	}
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
		item, err := db.GetByHash(hash)
		if err != nil {
			// Unknown state: KEEP the torrent. Dropping one that a library row still
			// references would break playback of a live item.
			slog.Error("orphan cleanup: could not look up a hash; keeping it", "hash", hash, "err", err)
			continue
		}
		if item == nil {
			slog.Info("dropping orphan torrent", "hash", hash)
			if derr := tsClient.Drop(hash); derr != nil {
				slog.Warn("orphan cleanup: drop failed", "hash", hash, "err", derr)
				continue
			}
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
			if serr := db.SetPosterURL(item.ID, details.PosterURL); serr != nil {
				slog.Error("poster backfill: could not save a poster URL", "id", item.ID, "err", serr)
			} else {
				filled++
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
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
			active, aerr := db.EpisodeActive(sub.TMDBID, sub.Season, ep.Number)
			if aerr != nil {
				slog.Error("subscription check: episode-active check failed; skipping to avoid a duplicate",
					"tmdb", sub.TMDBID, "season", sub.Season, "episode", ep.Number, "err", aerr)
			}
			if active {
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
			}); err != nil {
				slog.Error("subscription: could not enqueue a new episode",
					"show", title, "season", sub.Season, "episode", ep.Number, "err", err)
			} else {
				enqueued++
				slog.Info("subscription: enqueued new episode",
					"show", title, "season", sub.Season, "episode", ep.Number)
			}
		}
		// Retire the subscription once the show is no longer airing.
		stillAiring := details.IsAiring()
		if err := db.MarkSubscriptionChecked(sub.ID, stillAiring); err != nil {
			// Without this the subscription is re-checked from scratch every run and
			// an ended show is never retired.
			slog.Error("subscription: could not record the check", "id", sub.ID, "err", err)
			continue
		}
		if !stillAiring {
			retired++
		}
	}
	slog.Info("subscription check complete", "subscriptions", len(subs), "enqueued", enqueued, "retired", retired)
	return nil
}

// ── Settings persistence (DB-backed, admin-editable, live) ─────────────────────

var settingsMu sync.RWMutex

// getOrSeed reads a setting, seeding it from config.yaml on first run. Both the read
// and the seeding write are reported rather than discarded — a failed seed used to look
// identical to "nothing configured", which is how an install could come up silently
// unconfigured after a successful setup.
func getOrSeed(db *store.Store, key, seed string) (string, error) {
	v, err := db.GetSetting(key)
	if err != nil {
		return "", fmt.Errorf("read setting %s: %w", key, err)
	}
	if v == "" && seed != "" {
		if err := db.SetSetting(key, seed); err != nil {
			return "", fmt.Errorf("seed setting %s: %w", key, err)
		}
		return seed, nil
	}
	return v, nil
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
func applyConnections(db *store.Store, cfg *config.Config, tm *tmdb.Client, idx *indexer.Client, jf *jellyfin.Client, ts *torrserver.Client) error {
	var firstErr error
	get := func(key, seed string) string {
		v, err := getOrSeed(db, key, seed)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return v
	}
	tmdbKey := get("conn.tmdb.key", cfg.TMDB.APIKey)
	prowlarrURL := get("conn.prowlarr.url", cfg.Indexer.BaseURL)
	prowlarrKey := get("conn.prowlarr.key", cfg.Indexer.APIKey)
	jellyfinURL := get("conn.jellyfin.url", cfg.Jellyfin.BaseURL)
	jellyfinKey := get("conn.jellyfin.key", cfg.Jellyfin.APIKey)
	torrURL := get("conn.torrserver.url", cfg.TorrServer.BaseURL)
	if firstErr != nil {
		return firstErr
	}
	tm.Configure(tmdbKey)
	idx.Configure(prowlarrURL, prowlarrKey)
	jf.Configure(jellyfinURL, jellyfinKey)
	ts.Configure(torrURL)
	// Register every secret with the redaction backstop, so none can reach a log line
	// or an error string even if a future code path formats a URL that carries one.
	redact.Register(tmdbKey)
	redact.Register(prowlarrKey)
	redact.Register(jellyfinKey)
	return nil
}

func loadQualityOverrides(db *store.Store, cfg *config.Config) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	var firstErr error
	getSetting := func(key string) string {
		v, err := db.GetSetting(key)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("read %s: %w", key, err)
		}
		return v
	}
	if v := getSetting("quality.min_seeders"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Picker.MinSeeders = n
		}
	}
	if v := getSetting("quality.max_size_gb"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Picker.MaxSizeGB = n
		}
	}
	if v := getSetting("quality.reject_cam"); v != "" {
		b := v == "true"
		cfg.Picker.RejectCAM = &b
	}
	if v := getSetting("quality.video_codecs"); v != "" {
		cfg.Picker.PreferVideoCodecs = splitCSV(v)
	}
	if v := getSetting("quality.audio_codecs"); v != "" {
		cfg.Picker.PreferAudioCodecs = splitCSV(v)
	}
	if v := getSetting("quality.containers"); v != "" {
		cfg.Picker.PreferContainers = splitCSV(v)
	}
	return firstErr
}

func loadCacheOverrides(db *store.Store, cfg *config.Config) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	var firstErr error
	getSetting := func(key string) string {
		v, err := db.GetSetting(key)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("read %s: %w", key, err)
		}
		return v
	}
	c := &cfg.TorrServer.Cache
	if v := getSetting("cache.mode"); v != "" {
		c.Mode = v
	}
	if v := getSetting("cache.size_mb"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			c.SizeMB = n
		}
	}
	if v := getSetting("cache.path"); v != "" {
		c.Path = v
	}
	if v := getSetting("cache.disconnect_s"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			c.DisconnectTimeoutS = n
		}
	}
	if v := getSetting("cache.connections"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			c.ConnectionsLimit = n
		}
	}
	if v := getSetting("cache.retrackers"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			c.RetrackersMode = &n
		}
	}
	if v := getSetting("cache.upload_kb"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			c.UploadRateLimitKB = n
		}
	}
	return firstErr
}

// testConnection probes a service with the given (possibly unsaved) credentials.
// API keys are alphanumeric/hex so direct concatenation is safe.
func testConnection(ctx context.Context, service, url, key string) (bool, string) {
	client := &http.Client{Timeout: 12 * time.Second}
	do := func(req *http.Request) (bool, string) {
		resp, err := client.Do(req)
		if err != nil {
			// Redacted: *url.Error embeds the full request URL.
			return false, "unreachable: " + redact.Error(err)
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
		// TMDB v3 keys are query-parameter only; a v4 read token is a JWT and goes in
		// an Authorization header, which keeps it out of any URL that could be logged.
		if strings.HasPrefix(key, "eyJ") {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, tmdb.DefaultBaseURL+"/configuration", nil)
			if err != nil {
				return false, "could not build the request"
			}
			req.Header.Set("Authorization", "Bearer "+key)
			return do(req)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			tmdb.DefaultBaseURL+"/configuration?api_key="+url2.QueryEscape(key), nil)
		if err != nil {
			return false, "could not build the request"
		}
		return do(req)
	case "prowlarr":
		if url == "" {
			return false, "no URL set"
		}
		// Header, not query string: as a query parameter this key ended up inside
		// *url.Error and was returned to unauthenticated callers.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/v1/system/status", nil)
		if err != nil {
			return false, "could not build the request"
		}
		req.Header.Set("X-Api-Key", key)
		return do(req)
	case "jellyfin":
		if url == "" {
			return false, "no URL set"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/System/Info", nil)
		if err != nil {
			return false, "could not build the request"
		}
		req.Header.Set("X-Emby-Token", key)
		return do(req)
	case "torrserver":
		if url == "" {
			return false, "no URL set"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/echo", nil)
		if err != nil {
			return false, "could not build the request"
		}
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
