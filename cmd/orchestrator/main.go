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

// The two refusals the per-library visibility gate can produce on a write path.
//
// errUnknownLibrary is DELIBERATELY the answer to two different questions — "there is no
// such library" and "there is, but not for you". They have to be one message, because a
// caller who can tell them apart can enumerate every library on the box by guessing
// names and reading the error, which is exactly the knowledge a per-library restriction
// exists to withhold. The wording is the one that gives away least.
//
// errNoLibraryAvailable can only happen when the caller named NOTHING, so there is no
// name to protect and it can afford to be specific about what went wrong.
var (
	errUnknownLibrary     = errors.New("unknown library")
	errNoLibraryAvailable = errors.New("no library is available for you to request into — ask an administrator")
)

func main() {
	// Subcommands are dispatched before flag parsing, because the flag package would
	// treat a bare verb as a positional argument and silently ignore it — the server
	// would start instead of the subcommand, which is the worst possible way to get this
	// wrong for `netns-proxy`: it would put a second orchestrator on port 1990 rather
	// than a proxy inside the namespace.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch os.Args[1] {
		case "netns-proxy":
			os.Exit(runNetnsProxy(os.Args[2:]))
		default:
			fmt.Fprintf(os.Stderr, "unknown subcommand %q (the only one is: netns-proxy)\n", os.Args[1])
			os.Exit(2)
		}
	}

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

	// Shared secret for the external-provider ingest API (generated on first run).
	//
	// Fatal on failure, exactly like the webhook's. sharedSecretMatch fails closed on an
	// empty stored secret, so a silently-skipped initialisation would not open the
	// endpoint — it would make it permanently unusable instead, and an operator would
	// have no way to tell that from "my daemon's secret is wrong".
	if _, err := ensureIngestSecret(db); err != nil {
		slog.Error("failed to initialise the provider ingest secret", "err", err)
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
		// 60s is long enough to absorb a probe storm, short enough that a user who
		// fixes the cause (starts the VPN, adds an indexer) is not left waiting.
		cooldown: newResolveCooldown(60 * time.Second),
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

	// /api/libraries — the libraries THIS CALLER may see, for the request UI dropdown.
	//
	// This is the first place a hidden library would show itself, and the cheapest to
	// get wrong: the list is only a set of names and types, but a name is exactly what
	// a per-library restriction is meant to withhold. It is now OptionalAuth so the
	// handler knows who is asking, and the filtering decision is made by the store
	// (FilterLibraries) rather than re-derived here.
	//
	// An anonymous caller gets an empty list. That is not a degradation of the media UI:
	// the dropdown this feeds only appears for a signed-in user choosing where to put a
	// request, and POST /request has always been RequireAuth.
	mux.Handle("GET /api/libraries", api.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		type libInfo struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Default bool   `json:"default"`
			Adult   bool   `json:"adult"`
		}
		names := make([]string, len(cfg.Libraries))
		byName := make(map[string]config.Library, len(cfg.Libraries))
		for i, l := range cfg.Libraries {
			names[i] = l.Name
			byName[l.Name] = l
		}
		visible, err := db.FilterLibraries(store.ViewerOf(api.UserFromContext(r)), names)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read library access", err)
			return
		}
		out := make([]libInfo, 0, len(visible))
		for _, n := range visible {
			l := byName[n]
			out = append(out, libInfo{Name: l.Name, Type: l.Type, Default: l.Default, Adult: l.Adult})
		}
		jsonOK(w, out)
	})))

	// requestLibrary decides which library a request is written into, and refuses one the
	// caller is not allowed to use. Every enqueue path goes through it.
	//
	// It closes the write-side half of per-library visibility, which is not optional: a
	// gate that only hides libraries from READS still lets a restricted account push
	// content INTO one, and .strm files landing in the adults' Jellyfin library is a
	// worse outcome than being able to list it.
	//
	// TWO CASES, and the difference between them matters.
	//
	// A NAMED library is authorised, never redirected. Silently rewriting "Adults" to
	// "Kids" would report success for something that did not happen, and the caller
	// would learn just as much from the redirect as from an honest refusal. A name that
	// does not exist and a name the caller may not use return the IDENTICAL error, so
	// the endpoint cannot be walked to enumerate library names: both are simply
	// "unknown library".
	//
	// An EMPTY library names nothing, so there is nothing to refuse — the media UI omits
	// the picker entirely when there is only one library of a type. It resolves to the
	// configured default for the media type when the caller may use it, otherwise to the
	// first library of that type they can, and fails only if they can use none. For an
	// admin this always yields exactly cfg.DefaultLibrary(mediaType), which is what the
	// queue worker would have chosen from an empty name anyway — so the resolved name
	// changes nothing about how a single-admin install behaves, it merely records the
	// decision on the row instead of deferring it.
	requestLibrary := func(v store.Viewer, name, mediaType string) (string, error) {
		if name != "" {
			if cfg.FindLibrary(name) == nil {
				return "", errUnknownLibrary
			}
			ok, err := db.CanUseLibrary(v, name)
			if err != nil {
				return "", err
			}
			if !ok {
				return "", errUnknownLibrary
			}
			return name, nil
		}
		// Default first, then config order — FilterLibraries preserves the order, so
		// "the default unless you cannot see it" is expressed by the ordering.
		var candidates []string
		if def := cfg.DefaultLibrary(mediaType); def != nil {
			candidates = append(candidates, def.Name)
		}
		for i := range cfg.Libraries {
			l := &cfg.Libraries[i]
			if l.Type != mediaType || (len(candidates) > 0 && l.Name == candidates[0]) {
				continue
			}
			candidates = append(candidates, l.Name)
		}
		visible, err := db.FilterLibraries(v, candidates)
		if err != nil {
			return "", err
		}
		if len(visible) == 0 {
			return "", errNoLibraryAvailable
		}
		return visible[0], nil
	}

	// libraryRefusal turns a requestLibrary error into a response. A refusal is a 400
	// about the request body, not a 403 about the caller: a 403 would confirm that the
	// named library exists, which is the one fact the gate is there to withhold.
	libraryRefusal := func(w http.ResponseWriter, r *http.Request, err error) {
		if errors.Is(err, errUnknownLibrary) || errors.Is(err, errNoLibraryAvailable) {
			jsonErr(w, err.Error(), http.StatusBadRequest)
			return
		}
		httpFail(w, r, http.StatusInternalServerError, "could not check library access", err)
	}

	// /api/library — privacy-aware list of ready items (My Library strip on search page).
	//
	// An anonymous caller gets ONLY public items, and gets them with magnet, strm_path
	// and requested_by stripped (API contract §2). Before this, "" was treated as
	// admin-equivalent, so logging OUT showed private items that logging IN as a
	// non-admin correctly hid.
	mux.Handle("GET /api/library", api.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viewer, _, _ := viewerOf(r)
		items, err := db.ListVisible(store.ViewerOf(viewer))
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
		viewer, _, _ := viewerOf(r)
		var ids []int
		for _, s := range strings.Split(raw, ",") {
			if id, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
				ids = append(ids, id)
			}
			if len(ids) >= 200 {
				break // bound the IN(...) list
			}
		}
		statuses, err := db.GetStatusByTMDBIDs(ids, store.ViewerOf(viewer))
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
	// The flat feed, now optionally scoped. Unscoped it keeps its old shape and its
	// 100-row newest-first cap; scoped to a title (and optionally a season) it returns
	// that title's leaf rows, so the tree UI can fetch one expanded season instead of
	// hoping the episodes it wants happen to fall inside the newest 100 rows overall.
	//
	// That cap is why the queue page could not show the user's shows at all: with a
	// duplicate flood at the head of the list, 100 rows covered three titles out of
	// eighty. Scoping is the fix for the general case, not just the flood.
	mux.Handle("GET /api/queue", api.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viewer, _, _ := viewerOf(r)
		q := r.URL.Query()
		filter := store.QueueFilter{
			MediaType:  q.Get("media_type"),
			ActiveOnly: q.Get("active") == "1",
		}
		if v := q.Get("tmdb_id"); v != "" {
			id, err := strconv.Atoi(v)
			if err != nil || id < 0 {
				jsonErr(w, "tmdb_id must be a positive integer", http.StatusBadRequest)
				return
			}
			filter.TMDBID = id
		}
		// Season 0 is a real season (movies use it, and TV specials are season 0), so
		// presence of the parameter is what marks it set — not its value.
		if v := q.Get("season"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				jsonErr(w, "season must be a non-negative integer", http.StatusBadRequest)
				return
			}
			filter.Season, filter.SeasonSet = n, true
		}
		if filter.MediaType != "" && filter.MediaType != "movie" && filter.MediaType != "tv" {
			jsonErr(w, "media_type must be movie or tv", http.StatusBadRequest)
			return
		}
		items, err := db.ListQueueFiltered(store.ViewerOf(viewer), filter)
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
		n, err := db.CancelQueueItem(id, store.ViewerOf(user))
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
		n, err := db.DeleteQueueItem(id, store.ViewerOf(user))
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
	// The tree feed: one row per (title) and per (title, season) with per-status counts,
	// instead of every leaf row. Aggregating server-side is not an optimisation here, it
	// is what makes the view possible at all — the flat feed is capped at 100 rows, and
	// lifting the cap would have meant megabytes of JSON on a 4-second poll. This answers
	// in ~100 groups no matter how many rows sit behind them.
	//
	// Visibility is enforced inside the GROUP BY (see store.ListQueueGroups): a count
	// alone would leak that somebody else requested a title, so the predicate cannot live
	// in the handler. Nothing in the response carries requested_by, a magnet or a strm
	// path, so unlike the flat feed there is nothing here to redact.
	mux.Handle("GET /api/queue/groups", api.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viewer, _, _ := viewerOf(r)
		groups, err := db.ListQueueGroups(store.ViewerOf(viewer))
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the queue", err)
			return
		}
		jsonOK(w, groups)
	})))

	// Bulk clear, so "clear finished" is one call instead of a hundred. RequireAuth
	// rather than OptionalAuth: this deletes, and an anonymous caller owns no rows.
	mux.Handle("DELETE /api/queue/finished", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		viewer, username, isAdmin := viewerOf(r)
		n, err := db.DeleteFinishedQueue(store.ViewerOf(viewer))
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not clear the queue", err)
			return
		}
		slog.Info("cleared finished queue rows", "count", n, "by", username, "admin", isAdmin)
		jsonOK(w, map[string]any{"removed": n})
	})))

	mux.Handle("GET /api/queue/{id}/diagnosis", api.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonErr(w, "bad id", http.StatusBadRequest)
			return
		}
		// VisibleQueueItem, not GetQueueItem: the ownership AND library predicates are
		// in the query, so this handler cannot forget either one, and all three of
		// "never existed", "not yours" and "in a library you cannot see" arrive here as
		// the same nil and leave as the same 404. Distinguishing them would turn the id
		// space into an oracle over a library the caller was told nothing about.
		item, err := db.VisibleQueueItem(id, store.ViewerOf(api.UserFromContext(r)))
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read that request", err)
			return
		}
		if item == nil {
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
		viewer, _, _ := viewerOf(r)
		subs, err := db.ListSubscriptions(store.ViewerOf(viewer))
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
		// A subscription is a standing instruction to enqueue future episodes into a
		// library, so it is a write and takes the same authorisation as one. Without it
		// a restricted account could not request into a hidden library today but could
		// arrange for every future episode to land there.
		libName, err := requestLibrary(store.ViewerOf(user), req.Library, "tv")
		if err != nil {
			libraryRefusal(w, r, err)
			return
		}
		if err := db.UpsertSubscription(&store.Subscription{
			TMDBID: req.TMDBID, Season: req.Season, Title: req.Title,
			PosterURL: req.PosterURL, LibraryName: libName, RequestedBy: user.Username,
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
		n, err := db.DeleteSubscription(id, store.ViewerOf(user))
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

	// ── Browse endpoints (homepage carousels, browse filters) ───────────────────
	//
	// Every route in this block is UNAUTHENTICATED (SECURITY.md), and every one of them
	// turns a request from anyone who can reach port 1990 into an outbound call to TMDB
	// on this instance's API key. That is a small amplifier pointed at someone else's
	// rate limit and our own bandwidth, so it gets the same fixed-window per-address
	// throttle as the anonymous release search — the searchLimiter type from serve.go,
	// a second instance rather than a second implementation.
	//
	// The budget is far larger than the release limiter's twenty, and it has to be: the
	// homepage fires one discover request PER CAROUSEL — sixteen of them today — plus
	// trending, in a single page load, and the browse page then adds a genre list and a
	// page of results. A limit tuned for one search per user action would lock a real
	// visitor out on their second refresh. What this budget stops is a script in a loop,
	// which is exactly the case that matters, since a browse response is cheap for the
	// caller and expensive for us.
	browseLimiter := newSearchLimiter(180, time.Minute)
	browseAllowed := func(w http.ResponseWriter, r *http.Request) bool {
		if browseLimiter.allow(clientIPOf(r)) {
			return true
		}
		w.Header().Set("Retry-After", "60")
		jsonErr(w, "too many browse requests — slow down", http.StatusTooManyRequests)
		return false
	}

	mux.HandleFunc("GET /api/browse/trending", func(w http.ResponseWriter, r *http.Request) {
		if !browseAllowed(w, r) {
			return
		}
		results, err := tmdbClient.Trending()
		if err != nil {
			httpFail(w, r, http.StatusBadGateway, "TMDB is unavailable right now", err)
			return
		}
		jsonOK(w, results)
	})

	// GET /api/browse/discover?type=movie|tv&genres=&match=&studios=&networks=&year=
	//                          &sort=&min_votes=&page=
	//
	// The filter vocabulary and ALL of its validation live in tmdb.DiscoverParams, which
	// is an explicit allowlist — this handler deliberately does not touch r.URL.Query()
	// beyond handing it over and reading `type`. Read the comment on DiscoverParams
	// before adding a filter here; forwarding a caller's parameters as-is would let an
	// anonymous caller inject api_key and any other TMDB parameter they like.
	mux.HandleFunc("GET /api/browse/discover", func(w http.ResponseWriter, r *http.Request) {
		if !browseAllowed(w, r) {
			return
		}
		mediaType := r.URL.Query().Get("type")
		params, err := tmdb.DiscoverParams(mediaType, r.URL.Query())
		if err != nil {
			// Safe to return verbatim: DiscoverParams builds its messages from
			// constants, never from caller input or from an upstream error, so this
			// cannot reflect input back or leak a key. Contrast the /api/releases
			// incident below, where err.Error() came from *url.Error.
			jsonErr(w, err.Error(), http.StatusBadRequest)
			return
		}
		results, err := tmdbClient.Discover(mediaType, params)
		if err != nil {
			httpFail(w, r, http.StatusBadGateway, "TMDB is unavailable right now", err)
			return
		}
		jsonOK(w, results)
	})

	// GET /api/genres?type=movie|tv → [{id, name}], the vocabulary the filter chips are
	// built from. Served from an in-process cache with a long TTL (tmdb.genreCacheTTL),
	// so a page load does not wait on TMDB just to draw its filter bar.
	mux.HandleFunc("GET /api/genres", func(w http.ResponseWriter, r *http.Request) {
		if !browseAllowed(w, r) {
			return
		}
		mediaType := r.URL.Query().Get("type")
		if mediaType != "movie" && mediaType != "tv" {
			jsonErr(w, "type=movie|tv required", http.StatusBadRequest)
			return
		}
		genres, err := tmdbClient.Genres(mediaType)
		if err != nil {
			httpFail(w, r, http.StatusBadGateway, "TMDB is unavailable right now", err)
			return
		}
		jsonOK(w, genres)
	})

	// GET /api/studios?q=<name> → [{id, name, logo_url}] for the studio autocomplete.
	// The id is what /api/browse/discover accepts as studios=.
	mux.HandleFunc("GET /api/studios", func(w http.ResponseWriter, r *http.Request) {
		if !browseAllowed(w, r) {
			return
		}
		// Bounded before it reaches the network: an autocomplete sends a few characters,
		// and a caller who sends a megabyte is not typing. Trimmed here so that a query
		// of pure whitespace is a 400 rather than a pointless TMDB round trip.
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		if query == "" {
			jsonErr(w, "q is required", http.StatusBadRequest)
			return
		}
		if len(query) > 100 {
			jsonErr(w, "q is too long", http.StatusBadRequest)
			return
		}
		studios, err := tmdbClient.SearchCompanies(query)
		if err != nil {
			httpFail(w, r, http.StatusBadGateway, "TMDB is unavailable right now", err)
			return
		}
		jsonOK(w, studios)
	})

	// ── Release calendar ────────────────────────────────────────────────────────
	mux.Handle("GET /api/calendar", api.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The calendar reads the library only through ListSubscriptions, which carries
		// the library gate — so a show followed into a library the caller can no longer
		// see disappears from here too, without this handler having to know that. The
		// other two sections are TMDB's public upcoming/on-the-air feeds and describe
		// nothing about this box.
		v := store.ViewerOf(api.UserFromContext(r))

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
		if subs, err := db.ListSubscriptions(v); err == nil {
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
		// Authorise the destination BEFORE any of the idempotency lookups below. Those
		// lookups report whether a title is already ready or already in flight, which is
		// a fact about the library it lives in — answering them for a library the caller
		// may not use would leak through the refusal.
		libName, err := requestLibrary(store.ViewerOf(user), req.Library, req.MediaType)
		if err != nil {
			libraryRefusal(w, r, err)
			return
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
		// The identity lookup is library-BLIND — it has to be, because /play resolves the
		// same way from a .strm that carries no session — so the row it returns may live
		// in a library this caller cannot see. Answering {"status":"ready","already":true}
		// off such a row would report the existence and readiness of a title in a hidden
		// library, which is the same disclosure every read path above refuses to make.
		//
		// Treating it as absent is not merely the safe answer, it is the CORRECT one:
		// as far as this caller's libraries are concerned the title genuinely is not
		// there, so the request falls through and resolves a copy into a library they
		// can actually see. Distinct libraries mean distinct .strm paths, so the two
		// rows coexist rather than one overwriting the other.
		if existing != nil {
			visible, cerr := db.CanUseLibrary(store.ViewerOf(user), existing.LibraryName)
			if cerr != nil {
				httpFail(w, r, http.StatusInternalServerError, "could not check library access", cerr)
				return
			}
			if !visible {
				existing = nil
			}
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
			LibraryName: libName, RequestedBy: requestedBy,
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
		user := api.UserFromContext(r)
		// Before the TMDB round trip, not after: a refusal should cost the caller nothing
		// and tell them nothing about whether the season exists.
		libName, err := requestLibrary(store.ViewerOf(user), req.Library, "tv")
		if err != nil {
			libraryRefusal(w, r, err)
			return
		}
		episodes, err := tmdbClient.TVEpisodes(req.TMDBID, req.Season)
		if err != nil {
			httpFail(w, r, http.StatusBadGateway, "could not load the episode list from TMDB", err)
			return
		}
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
				LibraryName: libName, RequestedBy: requestedBy,
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
				PosterURL: req.PosterURL, LibraryName: libName, RequestedBy: requestedBy,
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

	// playHandler serves EVERY play URL shape. The frozen TMDB routes and the
	// provider-namespaced ones both funnel into this one function deliberately: a second
	// copy of resolve-at-play would drift from this one, and the half that drifted would
	// be the half nobody is watching in the logs.
	playHandler := func(w http.ResponseWriter, r *http.Request, ref playRef) {
		// Below this point the handler still speaks the TMDB-shaped four-tuple, because the
		// resolve pipeline behind it does. tmdbID is 0 for a non-TMDB identity, and every
		// use of it is gated on ref.isTMDB().
		mediaType, season, episode := ref.mediaType, ref.season, ref.episode
		tmdbID := ref.tmdbInt()

		// Encode the identity first. This validates every externally-supplied field
		// (provider charset, id charset and length, media type) in one place and yields the
		// single-flight/cooldown key used further down. An identity that cannot be encoded
		// is a bad request now, not a surprise five frames deeper.
		key, idErr := ref.identity()
		if idErr != nil {
			slog.Warn("play: rejected a malformed identity",
				"provider", ref.provider, "id", ref.providerID, "remote", r.RemoteAddr, "err", idErr)
			http.Error(w, "bad play identity", http.StatusBadRequest)
			return
		}

		// Log every playback attempt and its outcome.
		//
		// This used to log ONLY on rejection or error, so a user watching `journalctl -u
		// jellyfreedom` while pressing play in Jellyfin saw an empty screen whether playback
		// worked or not — reported as "nothing comes up and the logs are also empty", which
		// made the problem impossible to diagnose from the outside. A successful play is the
		// single most useful line this service can emit.
		playStart := time.Now()
		slog.Info("play: request",
			"type", mediaType, "provider", ref.provider, "id", ref.providerID, "s", season, "e", episode,
			"remote", r.RemoteAddr, "range", r.Header.Get("Range"), "ua", r.UserAgent())
		defer func() {
			slog.Info("play: finished",
				"type", mediaType, "provider", ref.provider, "id", ref.providerID, "s", season, "e", episode,
				"took", time.Since(playStart).Round(time.Millisecond).String())
		}()

		// Capability check. /play cannot require a session (Jellyfin fetches .strm URLs
		// anonymously), so possession of the HMAC tag in the URL — which only a .strm this
		// server wrote can contain — is the credential. Enforcement is switched on only
		// once the startup migration has retokenised every existing .strm.
		if playTokenEnforced() && !ref.validToken(r.URL.Query().Get("t")) {
			slog.Warn("play: rejected a request with a missing/invalid capability token",
				"type", mediaType, "provider", ref.provider, "id", ref.providerID, "s", season, "e", episode, "remote", r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// A HEAD is a metadata question, never a stream. Go's ServeMux serves HEAD from a
		// GET pattern, so without this a prober asking "does this exist and how big is it"
		// ran the identical handler — including the 90-second slow resolve and the
		// TorrServer add/drop cycle behind it. Answer from the library row if we have one
		// and decline otherwise; nothing that actually plays media uses HEAD.
		if r.Method == http.MethodHead {
			if it, herr := db.GetByProviderIdentity(ref.storeIdentity()); herr == nil && it != nil && it.Status == "ready" {
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Accept-Ranges", "bytes")
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Error(w, "no cached release for this title", http.StatusServiceUnavailable)
			return
		}

		// This item is now playing — stop any keep-warm loop for it; real playback takes over.
		// Keep-warm is keyed on a TMDB integer, so only a TMDB identity can have one; asking
		// for warmKey(0, s, e) on behalf of another provider would reach across providers.
		if ref.isTMDB() {
			cancelWarm(tmdbID, season, episode)
		}

		item, err := db.GetByProviderIdentity(ref.storeIdentity())
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
						"provider", ref.provider, "id", ref.providerID, "s", season, "e", episode, "err", err)
				}
			}
			if ref.isTMDB() {
				maybePreWarm(r, worker, mediaType, tmdbID, season, episode, length)
			}
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
			"provider", ref.provider, "id", ref.providerID, "s", season, "e", episode)

		// Everything past this point resolves a NEW release, which means searching indexers
		// for a title we can only name through a metadata provider. TMDB is the only
		// provider with that pipeline behind it today, so a non-TMDB identity gets an honest
		// 501 rather than being handed to the TMDB path as id 0 — which would search for
		// whatever TMDB calls title 0 and then cache the result against THIS identity.
		// A cached release still streams above; only re-resolution is unavailable.
		if !ref.isTMDB() {
			slog.Warn("play: no metadata provider is registered for this identity",
				"provider", ref.provider, "id", ref.providerID)
			http.Error(w, "no metadata provider is registered for this identity yet",
				http.StatusNotImplemented)
			return
		}

		// Did this identity just fail a slow resolve? Serve that answer back rather than
		// paying for it again. Without this, a client that retries on error (Jellyfin's
		// prober does, with no backoff) re-runs the full search-and-validate cycle for a
		// title we established seconds ago has nothing playable behind it.
		if left, blocked := worker.cooldown.blocked(key); blocked {
			slog.Info("play: identity is in resolve cooldown after a recent failure",
				"provider", ref.provider, "id", ref.providerID, "s", season, "e", episode,
				"retry_in", left.Round(time.Second).String(), "ua", r.UserAgent())
			w.Header().Set("Retry-After", strconv.Itoa(int(left.Seconds())+1))
			http.Error(w, "no playable release was found for this title a moment ago — try again shortly",
				http.StatusServiceUnavailable)
			return
		}

		// key — the encoded identity, computed at the top of the handler — single-flights the
		// expensive resolve, so a refresh-happy client (or Jellyfin probing the file while
		// the player also requests it) does not multiply a 90-second search by the number of
		// concurrent requests. It is the identity and not the URL, so the legacy and the
		// namespaced spelling of one TMDB title share a slot rather than racing each other.
		slowCtx, cancelSlow := context.WithTimeout(r.Context(), resolveDeadline)
		defer cancelSlow()
		release, ok := worker.resolves.lock(slowCtx, key)
		if !ok {
			http.Error(w, "timed out waiting to resolve this title", http.StatusGatewayTimeout)
			return
		}
		defer release()

		// Re-check the fast path: while we queued, the winner may have cached a live release.
		if fresh, ferr := db.GetByProviderIdentity(ref.storeIdentity()); ferr == nil && fresh != nil &&
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
				slog.Warn("play: resolve hit the deadline", "provider", ref.provider, "id", ref.providerID, "s", season, "e", episode)
				worker.cooldown.fail(key)
				http.Error(w, "could not find a playable release within the time limit — try again shortly",
					http.StatusGatewayTimeout)
				return
			}
			slog.Warn("play: resolve failed", "provider", ref.provider, "id", ref.providerID, "s", season, "e", episode, "err", err)
			worker.cooldown.fail(key)
			// A caller-visible reason, with no transport detail or upstream URL in it.
			http.Error(w, "no playable release available right now", http.StatusBadGateway)
			return
		}
		worker.cooldown.succeed(key)
		worker.cacheResolved(res, item, mediaType, tmdbID, season, episode)
		maybePreWarm(r, worker, mediaType, tmdbID, season, episode, res.lengthBytes)
		streamProxy(w, r, res.hash, res.fileIndex)
	}
	// The frozen TMDB routes. These are the exact paths inside every .strm file already on
	// disk, and each of those files carries an HMAC over the identity this path spells.
	// Neither the paths nor the identity they map to may change. See the play URL section
	// of internal/library/writer.go.
	mux.HandleFunc("GET /play/movie/{tmdb}", func(w http.ResponseWriter, r *http.Request) {
		tmdbID, err := strconv.Atoi(r.PathValue("tmdb"))
		if err != nil || tmdbID <= 0 {
			http.Error(w, "bad tmdb id", http.StatusBadRequest)
			return
		}
		playHandler(w, r, tmdbRef("movie", tmdbID, 0, 0))
	})
	mux.HandleFunc("GET /play/tv/{tmdb}/{season}/{episode}", func(w http.ResponseWriter, r *http.Request) {
		tmdbID, e1 := strconv.Atoi(r.PathValue("tmdb"))
		season, e2 := strconv.Atoi(r.PathValue("season"))
		episode, e3 := strconv.Atoi(r.PathValue("episode"))
		if e1 != nil || e2 != nil || e3 != nil || tmdbID <= 0 || season < 0 || episode < 0 {
			http.Error(w, "bad tv path", http.StatusBadRequest)
			return
		}
		playHandler(w, r, tmdbRef("tv", tmdbID, season, episode))
	})

	// The provider-namespaced routes, for identities whose stable id is not a TMDB integer
	// (the next provider's is a UUID). They exist BESIDE the routes above, never instead of
	// them, and they hand the same playRef to the same handler.
	//
	// /play/p/tmdb/... is legal and resolves to the identical identity and token as
	// /play/movie/... — the namespace is a URL shape, not a second key space. That is worth
	// keeping true: it means a future writer can emit one shape for everything without any
	// flag day, and it is asserted by a test.
	//
	// The provider and id are validated here, at the edge, as well as inside the identity
	// encoder. Two checks because they protect different things: this one turns a hostile
	// path into a 400 before it reaches a log line or a database query, and the encoder's
	// one guarantees no caller anywhere can mint a token over an unvalidated field.
	providerRef := func(w http.ResponseWriter, r *http.Request, mediaType string, season, episode int) (playRef, bool) {
		provider, id := r.PathValue("provider"), r.PathValue("id")
		if !library.ValidProvider(provider) {
			http.Error(w, "bad provider", http.StatusBadRequest)
			return playRef{}, false
		}
		if !library.ValidProviderID(id) {
			http.Error(w, "bad provider id", http.StatusBadRequest)
			return playRef{}, false
		}
		return playRef{
			provider:   provider,
			mediaType:  mediaType,
			providerID: id,
			season:     season,
			episode:    episode,
		}, true
	}
	mux.HandleFunc("GET /play/p/{provider}/movie/{id}", func(w http.ResponseWriter, r *http.Request) {
		ref, ok := providerRef(w, r, "movie", 0, 0)
		if !ok {
			return
		}
		playHandler(w, r, ref)
	})
	mux.HandleFunc("GET /play/p/{provider}/tv/{id}/{season}/{episode}", func(w http.ResponseWriter, r *http.Request) {
		season, e1 := strconv.Atoi(r.PathValue("season"))
		episode, e2 := strconv.Atoi(r.PathValue("episode"))
		if e1 != nil || e2 != nil || season < 0 || episode < 0 {
			http.Error(w, "bad tv path", http.StatusBadRequest)
			return
		}
		ref, ok := providerRef(w, r, "tv", season, episode)
		if !ok {
			return
		}
		playHandler(w, r, ref)
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

	// Anonymous callers may SEE the release list — the dashboard shows it before you sign
	// in — but they must not receive magnet links, and they must not be able to drive an
	// unbounded number of live indexer searches.
	//
	// Neither held before. The route was registered bare, and the response marshals
	// picker.ScoredRelease, which embeds indexer.Release and therefore its `magnet` field:
	// every magnet the search returned went to anyone who could reach port 1990, on a
	// service that listens on all interfaces. The web UI already had a branch for the
	// stripped case ("sign in to force this exact release") citing a contract clause that
	// was never actually implemented anywhere — so the UI expected this behaviour and only
	// the server was missing it.
	releaseSearchLimiter := newSearchLimiter(20, time.Minute)
	mux.Handle("GET /api/releases", api.OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signedIn := api.UserFromContext(r) != nil
		if !signedIn && !releaseSearchLimiter.allow(clientIPOf(r)) {
			w.Header().Set("Retry-After", "60")
			jsonErr(w, "too many release searches — slow down or sign in", http.StatusTooManyRequests)
			return
		}
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
		if !signedIn {
			// Field-wise rather than a filtered struct copy, so a new field added to
			// ScoredRelease later cannot silently become a second leak here.
			//
			// info_hash goes with the magnet. Stripping one and keeping the other would
			// be theatre: magnet:?xt=urn:btih:<hash> is a working magnet on its own for
			// anything the DHT can find peers for, which is exactly the well-seeded
			// releases this list ranks highest. The web UI reads info_hash only off
			// LIBRARY items (library.js, modal.js), never off a release row, so nothing
			// in the anonymous view needs it.
			for i := range scored {
				scored[i].Magnet = ""
				scored[i].InfoHash = ""
			}
		}
		jsonOK(w, scored)
	})))

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

	// visibleItem reports whether the caller may act on an item AT ALL, which is a
	// strictly earlier question than mayRemove's "is it theirs".
	//
	// The ORDER of the two checks is the security-relevant part. mayRemove answers 403
	// "that item belongs to someone else", which confirms the item exists; asked first,
	// it would confirm the existence of items in a library the caller was told nothing
	// about. So visibility is checked first and answers 404 — indistinguishable from an
	// item that is not there — and only an item the caller can actually see ever reaches
	// the ownership check.
	visibleItem := func(user *store.User, it *store.Item) (bool, error) {
		return db.CanUseLibrary(store.ViewerOf(user), it.LibraryName)
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
		if ok, err := visibleItem(user, item); err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not check library access", err)
			return
		} else if !ok {
			jsonErr(w, "not found", http.StatusNotFound)
			return
		}
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
		// VisibleItemsByTMDB, not ItemsByTMDB: a row in a hidden library must be ABSENT
		// from this list rather than filtered out of it afterwards, because the response
		// reports a "skipped" count and a count is a measurement.
		user := api.UserFromContext(r)
		items, err := db.VisibleItemsByTMDB(id, "tv", store.ViewerOf(user))
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
			return
		}
		n, skipped, err := removeItems(user, items)
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
		user := api.UserFromContext(r)
		all, err := db.VisibleItemsByTMDB(req.TMDBID, "tv", store.ViewerOf(user))
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
		n, skipped, err := removeItems(user, inSeason)
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
		if ok, err := visibleItem(user, item); err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not check library access", err)
			return
		} else if !ok {
			jsonErr(w, "episode not in library", http.StatusNotFound)
			return
		}
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
		// The provider-ingest secret, for the same reason and under the same admin-only
		// gate: it is generated on first run, a daemon cannot call the API without it,
		// and there is otherwise no way to read it off a box with no sqlite3 installed.
		ingestSecret := get(ingestSecretSetting)
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
			// Everything a provider daemon needs to call the ingest API.
			"ingest": map[string]any{
				"secret": ingestSecret,
				"header": IngestHeader,
				"url":    strings.TrimRight(cfg.Server.PublicURL, "/") + "/api/provider/{provider}/items/{id}",
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
	// External-provider ingest API — authenticated by a shared secret, not a session.
	// Registered on the PUBLIC mux on purpose: the "/api/" catch-all below hands
	// everything else under /api/ to the admin-session-protected mux, and a daemon has no
	// session to hand it. These patterns are strictly more specific than "/api/", so
	// ServeMux routes them here and everything else still reaches the protected mux.
	registerProviderIngest(mux, db, cfg, jfClient)

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

	protected := api.RequireAdmin(buildProtectedMux(db, cfg, assets, indexerClient, jfClient, livePicker(cfg), updater))

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

// ── External-provider ingest API ──────────────────────────────────────────────
//
// Everything else that puts a row in the library goes through the same pipeline: TMDB
// says what a title is, Prowlarr says what releases exist, the picker chooses one. That
// pipeline is the reason only TMDB identities can enter the library — /play is already
// provider-namespaced and will happily stream a non-TMDB row from its cached info hash,
// but nothing can CREATE such a row.
//
// This is the generic way in. A separate local process that has already done its own
// metadata lookup and its own release selection registers the finished result: here is
// an identity, here is what to call it, here is the magnet that plays it. JellyFreedom
// stores it, writes the .strm, and serves playback from the cached hash. It performs no
// metadata lookup and runs no indexer search for these rows — which is precisely what
// makes it work for a provider JellyFreedom knows nothing about, and precisely why a
// stale row is the CALLER's problem: re-register it, because JellyFreedom cannot
// re-resolve what it cannot search for.
//
// Nothing here names a provider or assumes anything about one. An anime database, a
// personal archive and a public-domain film collection are all the same shape to it.
//
// The threat model is the whole design. This endpoint writes files into a Jellyfin media
// root and inserts rows into the library, on behalf of a caller holding one shared
// secret, so every field is validated against the sink it actually reaches — see the
// validation block in helpers.go — and the two identity fields are validated with the
// SAME functions the /play routes use, not a second copy that could drift away from them.
func registerProviderIngest(mux *http.ServeMux, db *store.Store, cfg *config.Config, jf *jellyfin.Client) {
	// ingestProvider validates the {provider} path segment and writes the refusal itself.
	//
	// library.ValidProvider is the same allowlist the /play/p/{provider} routes and the
	// capability-token encoder use. Reusing it is not tidiness: the token that authorises
	// playback is an HMAC over a ':'-joined identity, and its unforgeability depends on no
	// field being able to contain the delimiter. A second, looser charset here would mint
	// .strm files whose identities collide with somebody else's under that encoding.
	ingestProvider := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		p := r.PathValue("provider")
		if !library.ValidProvider(p) {
			jsonErr(w, "bad provider", http.StatusBadRequest)
			return "", false
		}
		// The TMDB namespace is owned by the built-in resolve pipeline and is closed to
		// this API. Two independent reasons, either of which is sufficient:
		//
		// Its rows are the ones /play can genuinely re-resolve, and their identity is
		// mirrored into the legacy tmdb_id column — so a row written here would be found
		// by TMDB-shaped lookups all over the program as a title that TMDB never
		// described. And its .strm URLs are the frozen legacy shape carrying ~1,000 live
		// capability tokens; letting an external caller mint rows in that space means
		// letting it choose the identity those tokens authorise.
		if p == library.ProviderTMDB {
			jsonErr(w, "that provider namespace is not writable through this API", http.StatusBadRequest)
			return "", false
		}
		return p, true
	}

	// ingestItemID validates the {id} path segment. Same reasoning as the provider: this
	// is library.ValidProviderID, the function /play and the token encoder use, so an id
	// that can be registered is by construction an id that can be routed and signed.
	ingestItemID := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		id := r.PathValue("id")
		if !library.ValidProviderID(id) {
			jsonErr(w, "bad item id", http.StatusBadRequest)
			return "", false
		}
		return id, true
	}

	// ingestLibrary resolves and AUTHORISES the destination library.
	//
	// It returns errUnknownLibrary — the identical value, and therefore the identical
	// message, that the browser-facing request handlers return — for a name that does not
	// exist AND for a name whose type is wrong for the media. Indistinguishable answers
	// are the point: a caller that can tell those two apart can enumerate the operator's
	// library names one guess at a time, which is the exact knowledge the per-library
	// gate exists to withhold. There is no per-user check here because there is no user;
	// the secret authenticates a daemon, and "which libraries may this daemon use" is
	// answered by "the ones the operator configured".
	ingestLibrary := func(name, mediaType string) (*config.Library, error) {
		if name != "" {
			lib := cfg.FindLibrary(name)
			if lib == nil || lib.Type != mediaType {
				return nil, errUnknownLibrary
			}
			return lib, nil
		}
		// An empty name names nothing, so there is nothing to refuse: fall back to the
		// configured default for the media type, exactly as the queue worker would.
		lib := cfg.DefaultLibrary(mediaType)
		if lib == nil || lib.Path == "" {
			return nil, errNoLibraryAvailable
		}
		return lib, nil
	}

	// ingestRefusal mirrors libraryRefusal: a 400 about the body, never a 403 about the
	// caller. A 403 would confirm that the named library exists.
	ingestRefusal := func(w http.ResponseWriter, err error) {
		if errors.Is(err, errUnknownLibrary) || errors.Is(err, errNoLibraryAvailable) {
			jsonErr(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonErr(w, "could not resolve the destination library", http.StatusInternalServerError)
	}

	// ------------------------------------------------------------------ //
	// PUT /api/provider/{provider}/items/{id} — upsert one item
	// ------------------------------------------------------------------ //
	mux.HandleFunc("PUT /api/provider/{provider}/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !ingestAuthorised(db, r) {
			// No detail, and the same answer for a missing header as for a wrong one.
			slog.Warn("ingest: rejected an unauthenticated call", "remote", r.RemoteAddr)
			jsonErr(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		provider, ok := ingestProvider(w, r)
		if !ok {
			return
		}
		id, ok := ingestItemID(w, r)
		if !ok {
			return
		}
		var req struct {
			MediaType    string `json:"type"`
			Title        string `json:"title"`
			Year         string `json:"year"`
			PosterURL    string `json:"poster_url"`
			Season       int    `json:"season"`
			Episode      int    `json:"episode"`
			Library      string `json:"library"`
			Magnet       string `json:"magnet"`
			InfoHash     string `json:"info_hash"`
			ReleaseTitle string `json:"release_title"`
			FileIndex    int    `json:"file_index"`
		}
		// A tighter bound than the app-wide 1 MiB, for the same reason the WireGuard
		// upload has its own: the largest legitimate body here is a few hundred bytes, so
		// anything approaching the general limit is not a request this endpoint has a use
		// for. MaxBytesReader makes the decode fail rather than buffering the excess.
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIngestBody)).Decode(&req); err != nil {
			jsonErr(w, "malformed or oversized request body", http.StatusBadRequest)
			return
		}
		if !library.ValidMediaType(req.MediaType) {
			jsonErr(w, "type must be 'movie' or 'tv'", http.StatusBadRequest)
			return
		}
		// Season and episode are FORCED to zero for a movie rather than merely ignored.
		// The identity a movie is stored under has to be the identity
		// /play/p/{provider}/movie/{id} looks up, and that route has no season or episode
		// to pass, so it always asks for (0, 0). A row written with season 3 on a movie
		// would exist, be listed, and never once be found by playback.
		season, episode := 0, 0
		if req.MediaType == "tv" {
			season, episode = req.Season, req.Episode
			// Zero season is legal (specials); negative is not routable, since the /play
			// route parses these back out of a path and rejects a negative there.
			if season < 0 || episode < 0 || season > maxIngestSeasonOrEp || episode > maxIngestSeasonOrEp {
				jsonErr(w, "season and episode must be between 0 and 9999", http.StatusBadRequest)
				return
			}
		}
		title := strings.TrimSpace(req.Title)
		if title == "" || len([]rune(title)) > maxIngestTitleLen || hasControlChars(title) {
			jsonErr(w, "title is required and must be at most 200 printable characters", http.StatusBadRequest)
			return
		}
		if req.Year != "" && !yearRe.MatchString(req.Year) {
			jsonErr(w, "year must be four digits, or omitted", http.StatusBadRequest)
			return
		}
		if req.PosterURL != "" && !validPosterURL(req.PosterURL) {
			jsonErr(w, "poster_url must be an absolute http(s) URL", http.StatusBadRequest)
			return
		}
		if len(req.ReleaseTitle) > maxIngestReleaseLen || hasControlChars(req.ReleaseTitle) {
			jsonErr(w, "release_title is too long", http.StatusBadRequest)
			return
		}
		if req.FileIndex < 0 || req.FileIndex > maxIngestFileIndex {
			jsonErr(w, "file_index is out of range", http.StatusBadRequest)
			return
		}
		hash, magnet, err := ingestSource(req.InfoHash, req.Magnet)
		if err != nil {
			jsonErr(w, err.Error(), http.StatusBadRequest)
			return
		}
		lib, err := ingestLibrary(req.Library, req.MediaType)
		if err != nil {
			ingestRefusal(w, err)
			return
		}

		// Build the .strm contents BEFORE touching the disk. playURLFor returns "" for an
		// identity it cannot encode or sign, and a .strm holding a URL with no capability
		// token is a 403 at playback — a silent failure discovered by a user, days later.
		ref := playRef{provider: provider, mediaType: req.MediaType, providerID: id, season: season, episode: episode}
		streamURL := playURLFor(cfg.Server.PublicURL, ref)
		if streamURL == "" {
			slog.Error("ingest: could not build a play URL", "provider", provider, "type", req.MediaType)
			jsonErr(w, "could not build a play URL for that identity", http.StatusInternalServerError)
			return
		}

		// The row this identity already has, if any — needed to clean up after a RENAME.
		// The items table's conflict key is strm_path, so re-registering the same identity
		// under a different title inserts a second row and orphans the first file rather
		// than replacing it. Read it before the write; act on it after.
		existing, err := db.GetByProviderIdentity(ref.storeIdentity())
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
			return
		}

		var strmPath string
		if req.MediaType == "movie" {
			strmPath, err = library.WriteMovieStrm(lib.Path, title, req.Year, streamURL)
		} else {
			strmPath, err = library.WriteTVStrm(lib.Path, title, req.Year, season, episode, streamURL)
		}
		if err != nil {
			// The error is logged whole and answered generically: it is an *os.PathError
			// carrying an absolute server path, which is not a fact this caller is owed.
			httpFail(w, r, http.StatusInternalServerError, "could not write the library file", err)
			return
		}

		item := &store.Item{
			MediaType: req.MediaType, Title: title, Year: req.Year,
			InfoHash: hash, FileIndex: req.FileIndex, StrmPath: strmPath,
			LibraryName: lib.Name, Status: "ready", Updated: time.Now(),
			PosterURL: req.PosterURL, Magnet: magnet, ReleaseTitle: req.ReleaseTitle,
			Season: season, Episode: episode,
		}
		// SetProviderIdentity rather than assigning the fields: it also clears TMDBID, and
		// the store REFUSES a row that carries both a provider identity and a tmdb_id,
		// because such a row would be found by two different lookups as two different things.
		item.SetProviderIdentity(provider, id)
		if err := db.Upsert(item); err != nil {
			// Roll the .strm back, but ONLY if we just created it. Deleting the file when
			// the row already pointed at it would turn a failed refresh into a broken
			// library entry — strictly worse than the state we started in.
			if existing == nil || existing.StrmPath != strmPath {
				if rerr := library.RemoveStrm(strmPath); rerr != nil {
					slog.Error("ingest: could not roll back the .strm after a failed upsert", "err", rerr)
				}
			}
			httpFail(w, r, http.StatusInternalServerError, "could not record that item", err)
			return
		}

		// Renamed: the old file and row are now unreachable from this identity, so remove
		// them. Failures here are logged, not returned — the registration SUCCEEDED, and
		// reporting failure for a leftover file would make the caller retry a write that
		// already happened.
		if existing != nil && existing.StrmPath != "" && existing.StrmPath != strmPath {
			if rerr := library.RemoveStrm(existing.StrmPath); rerr != nil {
				slog.Error("ingest: could not remove the superseded .strm", "err", rerr)
			}
			if derr := db.DeleteItem(existing.StrmPath); derr != nil {
				slog.Error("ingest: could not remove the superseded library row", "err", derr)
			}
		}

		notifyJellyfinScan(jf)
		slog.Info("ingest: registered an item",
			"provider", provider, "id", id, "type", req.MediaType, "s", season, "e", episode, "library", lib.Name)
		// The response deliberately carries no strm_path and no play URL. The path is a
		// server filesystem path, and the play URL embeds a capability token — neither is
		// needed to register an item, and an endpoint that hands out capability tokens is
		// a larger thing than one that does not.
		jsonOK(w, map[string]any{
			"status": "ok", "provider": provider, "id": id, "type": req.MediaType,
			"season": season, "episode": episode, "library": lib.Name, "info_hash": hash,
		})
	})

	// ------------------------------------------------------------------ //
	// DELETE /api/provider/{provider}/items/{id} — unregister
	// ------------------------------------------------------------------ //
	//
	// Removes every row this provider registered under that id, plus each row's .strm.
	// Optional ?season=&episode= narrows it to a single episode, which is the delete that
	// matches a single PUT; without them, "delete this id" means the whole title, which is
	// what a caller retiring a series wants and is otherwise a loop of N requests.
	//
	// It is IDEMPOTENT: deleting something that is not there is a 200 with removed:0, not
	// a 404. A daemon reconciling its own state re-issues deletes freely, and an error for
	// "already gone" would only teach it to ignore errors.
	//
	// No TorrServer drop happens here. taskOrphanCleanup already drops every torrent that
	// no library row references, so the cleanup is covered — and doing it inline would put
	// a network call to another service inside a delete, which is how a delete starts
	// failing for reasons that have nothing to do with the delete.
	mux.HandleFunc("DELETE /api/provider/{provider}/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !ingestAuthorised(db, r) {
			slog.Warn("ingest: rejected an unauthenticated call", "remote", r.RemoteAddr)
			jsonErr(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		provider, ok := ingestProvider(w, r)
		if !ok {
			return
		}
		id, ok := ingestItemID(w, r)
		if !ok {
			return
		}
		// Both or neither. One alone would silently mean "season 0" or "episode 0" and
		// delete something the caller did not name.
		q := r.URL.Query()
		var season, episode int
		narrowed := q.Has("season") && q.Has("episode")
		if q.Has("season") != q.Has("episode") {
			jsonErr(w, "season and episode must be given together, or not at all", http.StatusBadRequest)
			return
		}
		if narrowed {
			var e1, e2 error
			season, e1 = strconv.Atoi(q.Get("season"))
			episode, e2 = strconv.Atoi(q.Get("episode"))
			if e1 != nil || e2 != nil || season < 0 || episode < 0 {
				jsonErr(w, "season and episode must be non-negative integers", http.StatusBadRequest)
				return
			}
		}
		items, err := db.ItemsByProviderID(provider, id)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
			return
		}
		removed := 0
		for _, it := range items {
			if narrowed && (it.Season != season || it.Episode != episode) {
				continue
			}
			if rerr := library.RemoveStrm(it.StrmPath); rerr != nil {
				// Log and carry on: leaving the ROW behind for a file we could not delete
				// would leave a library entry that plays nothing.
				slog.Error("ingest: could not remove a .strm", "provider", provider, "err", rerr)
			}
			if derr := db.DeleteItem(it.StrmPath); derr != nil {
				httpFail(w, r, http.StatusInternalServerError, "could not remove that item", derr)
				return
			}
			removed++
		}
		if removed > 0 {
			notifyJellyfinScan(jf)
			slog.Info("ingest: unregistered items", "provider", provider, "id", id, "count", removed)
		}
		jsonOK(w, map[string]any{"status": "ok", "removed": removed})
	})

	// ------------------------------------------------------------------ //
	// GET /api/provider/{provider}/items — what this provider has registered
	// ------------------------------------------------------------------ //
	//
	// Scoped to the provider in the path, so a secret holder sees its own namespace and
	// nothing else. The shape is purpose-built rather than store.Item: strm_path is a
	// server filesystem path and magnet carries a tracker list, and neither is needed to
	// answer "what have I already registered".
	mux.HandleFunc("GET /api/provider/{provider}/items", func(w http.ResponseWriter, r *http.Request) {
		if !ingestAuthorised(db, r) {
			slog.Warn("ingest: rejected an unauthenticated call", "remote", r.RemoteAddr)
			jsonErr(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		provider, ok := ingestProvider(w, r)
		if !ok {
			return
		}
		items, err := db.ItemsByProvider(provider)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read the library", err)
			return
		}
		type row struct {
			ID        string    `json:"id"`
			MediaType string    `json:"type"`
			Title     string    `json:"title"`
			Year      string    `json:"year"`
			Season    int       `json:"season"`
			Episode   int       `json:"episode"`
			Library   string    `json:"library"`
			InfoHash  string    `json:"info_hash"`
			PosterURL string    `json:"poster_url"`
			Status    string    `json:"status"`
			Updated   time.Time `json:"updated"`
		}
		out := make([]row, 0, len(items))
		for _, it := range items {
			out = append(out, row{
				ID: it.ProviderID, MediaType: it.MediaType, Title: it.Title, Year: it.Year,
				Season: it.Season, Episode: it.Episode, Library: it.LibraryName,
				InfoHash: it.InfoHash, PosterURL: it.PosterURL, Status: it.Status, Updated: it.Updated,
			})
		}
		jsonOK(w, out)
	})
}

func buildProtectedMux(db *store.Store, cfg *config.Config, assets fs.FS, indexerClient *indexer.Client,
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

	// ── Per-user library access (admin only) ────────────────────────────────
	//
	// These sit on this mux, so they inherit RequireAdmin along with the rest of
	// /api/users* — see SECURITY.md. That is not incidental: the response NAMES every
	// configured library, which is the single fact the gate exists to withhold from
	// everyone else, so nothing less than an admin session may reach it.
	//
	// The pair is read-then-replace rather than grant/revoke. The admin UI edits a whole
	// checkbox list, and a full replacement is the only shape that cannot lose a
	// concurrent revocation: two incremental edits interleave into a set neither admin
	// asked for, while two replacements leave the set one of them did.

	// GET /api/users/{id}/libraries — what this account may see, and what there is to
	// grant. "available" is included so the UI can render the checkbox list from one
	// call instead of joining this against GET /api/settings.
	mux.HandleFunc("GET /api/users/{id}/libraries", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonErr(w, "bad id", http.StatusBadRequest)
			return
		}
		user, err := db.GetUserByID(id)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read that user", err)
			return
		}
		if user == nil {
			jsonErr(w, "user not found", http.StatusNotFound)
			return
		}
		granted, err := db.LibraryAccess(id)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read library access", err)
			return
		}
		type libInfo struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		available := make([]libInfo, 0, len(cfg.Libraries))
		for _, l := range cfg.Libraries {
			available = append(available, libInfo{Name: l.Name, Type: l.Type})
		}
		jsonOK(w, map[string]any{
			"user_id":  id,
			"username": user.Username,
			// An admin bypasses the gate entirely, so their stored grants — which may be
			// none — say nothing about what they can see. The flag is here so the UI can
			// say "administrator: sees every library" rather than drawing an empty
			// checkbox list that looks like a lockout.
			"is_admin":  user.IsAdmin,
			"libraries": granted,
			"available": available,
		})
	})

	// PUT /api/users/{id}/libraries — replace this account's grants with exactly this set.
	mux.HandleFunc("PUT /api/users/{id}/libraries", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			jsonErr(w, "bad id", http.StatusBadRequest)
			return
		}
		user, err := db.GetUserByID(id)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read that user", err)
			return
		}
		if user == nil {
			jsonErr(w, "user not found", http.StatusNotFound)
			return
		}
		var req struct {
			Libraries []string `json:"libraries"`
		}
		if err := decodeBody(w, r, &req); err != nil {
			jsonErr(w, "bad request", http.StatusBadRequest)
			return
		}
		// Every name must be a library that actually exists. A typo would otherwise be
		// stored as a grant that grants nothing, and the admin would see a ticked box
		// beside a user who cannot see the library they just "granted" — a silent deny
		// is the worst possible outcome for a permissions UI, so it is a 400 instead.
		for _, name := range req.Libraries {
			if cfg.FindLibrary(name) == nil {
				jsonErr(w, fmt.Sprintf("no library named %q is configured", name), http.StatusBadRequest)
				return
			}
		}
		if err := db.SetLibraryAccess(id, req.Libraries); err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not save library access", err)
			return
		}
		granted, err := db.LibraryAccess(id)
		if err != nil {
			httpFail(w, r, http.StatusInternalServerError, "could not read library access back", err)
			return
		}
		slog.Info("set per-user library access", "user_id", id, "username", user.Username,
			"libraries", granted, "admin_account", user.IsAdmin)
		jsonOK(w, map[string]any{"user_id": id, "libraries": granted, "is_admin": user.IsAdmin})
	})

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
	cooldown *resolveCooldown
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
	// Runtime turns the size cap into a BITRATE judgement: size alone cannot tell a
	// 4.5Mbps WEB-DL from a 65Mbps remux, and only one of those streams out of a bounded
	// ring buffer. Zero when TMDB does not know the runtime, which leaves the rule
	// dormant rather than guessing.
	pc.RuntimeMinutes = details.RuntimeMinutes

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
	return playURLFor(publicURL, tmdbRef(mediaType, tmdbID, season, episode))
}

// playURLFor is the same thing for any provider's identity. The URL shape itself lives in
// internal/library so that the writer of a .strm and the reader of one cannot disagree
// about it, and so the frozen TMDB bytes are pinned by a test next to the code that emits
// them.
//
// It returns "" rather than a half-built URL when the identity cannot be encoded. There is
// no useful fallback: a URL without a valid token is a 403 the moment enforcement is on,
// so writing one would only convert a loud failure into a silent one.
func playURLFor(publicURL string, ref playRef) string {
	u, err := library.PlayURL(publicURL, ref.provider, ref.mediaType, ref.providerID,
		ref.season, ref.episode, ref.token())
	if err != nil {
		slog.Error("play url: refusing to build a URL for a malformed identity",
			"provider", ref.provider, "id", ref.providerID, "type", ref.mediaType, "err", err)
		return ""
	}
	return u
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

	// Episodes that failed recently are not retried on this pass.
	//
	// EpisodeActive treats only "ready in the library" and "pending/processing in the
	// queue" as reasons to skip — a FAILED row does not block, and unlike /request/season
	// this loop never clears terminal rows either. So an episode with no obtainable
	// release was re-enqueued on every run, forever: one subscribed show in this
	// deployment accumulated 501 failed rows for eight episodes, one per episode every six
	// hours from June to August, each costing a full indexer search that had already
	// failed a dozen times.
	//
	// A day's backoff keeps the self-healing property that matters (a release that appears
	// is still picked up automatically, just within 24h instead of 6h) while cutting the
	// wasted searches by 4x. Note this backs off on RECENCY, not on a failure count: the
	// queue keeps only the newest terminal row per identity, so the count is not available
	// to escalate on. If permanently-unobtainable episodes remain a problem, the next step
	// is a persisted failure count with a widening backoff, not a longer fixed window.
	const failedRetryBackoff = 24 * time.Hour
	recentlyFailed := map[string]bool{}
	if all, err := db.ListAllQueue(); err != nil {
		// Non-fatal: without the map every episode is simply eligible, which is the old
		// behaviour. Worth logging because it silently restores the wasteful path.
		slog.Warn("subscription check: could not read the queue for failure backoff; "+
			"recently-failed episodes will be retried this run", "err", err)
	} else {
		cutoff := time.Now().Add(-failedRetryBackoff)
		for _, q := range all {
			if q.Status == "failed" && q.UpdatedAt.After(cutoff) {
				recentlyFailed[fmt.Sprintf("%d:%d:%d", q.TMDBID, q.Season, q.Episode)] = true
			}
		}
	}
	skippedFailed := 0
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
			if recentlyFailed[fmt.Sprintf("%d:%d:%d", sub.TMDBID, sub.Season, ep.Number)] {
				skippedFailed++
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
	slog.Info("subscription check complete", "subscriptions", len(subs), "enqueued", enqueued,
		"retired", retired, "skipped_recently_failed", skippedFailed)
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
		TargetResolution:  cp.TargetResolution,
		RequireDirectPlay: cp.RequireDirectPlayValue(),
		MaxMbps:           cp.MaxMbps,
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
