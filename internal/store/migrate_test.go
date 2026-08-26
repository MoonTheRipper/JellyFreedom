package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// oldSchema is the ORIGINAL schema, before any of the additive column migrations and
// before UNIQUE(strm_path) existed on items. Migrations run against live user data with
// no test coverage today, which makes this the riskiest untested code in the project.
const oldSchema = `
CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE sessions (
	token   TEXT     PRIMARY KEY,
	expires DATETIME NOT NULL
);
CREATE TABLE items (
	id         INTEGER  PRIMARY KEY AUTOINCREMENT,
	tmdb_id    INTEGER  NOT NULL,
	media_type TEXT     NOT NULL,
	title      TEXT     NOT NULL,
	year       TEXT     NOT NULL,
	info_hash  TEXT     NOT NULL DEFAULT '',
	file_index INTEGER  NOT NULL DEFAULT 0,
	strm_path  TEXT     NOT NULL DEFAULT '',
	status     TEXT     NOT NULL DEFAULT 'requested',
	seeders    INTEGER  NOT NULL DEFAULT 0,
	updated    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE queue (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	tmdb_id         INTEGER NOT NULL,
	media_type      TEXT    NOT NULL,
	title           TEXT    NOT NULL DEFAULT '',
	year            TEXT    NOT NULL DEFAULT '',
	poster_url      TEXT    NOT NULL DEFAULT '',
	season          INTEGER NOT NULL DEFAULT 0,
	episode         INTEGER NOT NULL DEFAULT 0,
	library_name    TEXT    NOT NULL DEFAULT '',
	requested_by    TEXT    NOT NULL DEFAULT '',
	magnet_override TEXT    NOT NULL DEFAULT '',
	status          TEXT    NOT NULL DEFAULT 'pending',
	progress        TEXT    NOT NULL DEFAULT '',
	error_msg       TEXT    NOT NULL DEFAULT '',
	info_hash       TEXT    NOT NULL DEFAULT '',
	strm_path       TEXT    NOT NULL DEFAULT '',
	created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

// buildOldDB writes a fixture database at the pre-migration schema, with real rows in it.
func buildOldDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	// A movie and two TV episodes whose season/episode live only in the title —
	// exactly what the backfill has to recover.
	rows := []struct {
		tmdb               int
		mediaType, title   string
		year, hash, strm   string
		status             string
		seeders, fileIndex int
	}{
		{1, "movie", "Inception", "2010", "aaa", "/lib/Inception (2010)/Inception (2010).strm", "ready", 40, 1},
		{2, "tv", "Show S01E08", "2019", "bbb", "/lib/Show/Season 01/Show S01E08.strm", "ready", 12, 3},
		{2, "tv", "Show S02E10", "2019", "ccc", "/lib/Show/Season 02/Show S02E10.strm", "stale", 0, 2},
		{3, "tv", "Untitled Thing", "2020", "ddd", "/lib/Other/x.strm", "ready", 5, 1},
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO items (tmdb_id,media_type,title,year,info_hash,file_index,strm_path,status,seeders,updated)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			r.tmdb, r.mediaType, r.title, r.year, r.hash, r.fileIndex, r.strm, r.status, r.seeders, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	// A queue row stuck in 'processing' — a crash mid-job. Migration must reset it.
	if _, err := db.Exec(
		`INSERT INTO queue (tmdb_id,media_type,title,status,progress) VALUES (9,'movie','Stuck','processing','Adding…')`); err != nil {
		t.Fatal(err)
	}
	// A done row, to check stage backfill.
	if _, err := db.Exec(
		`INSERT INTO queue (tmdb_id,media_type,title,status) VALUES (8,'movie','Finished','done')`); err != nil {
		t.Fatal(err)
	}
	// The legacy single-password setting the users-table migration promotes.
	if _, err := db.Exec(
		`INSERT INTO settings (key,value) VALUES ('dashboard_password_hash','$2a$10$legacyhash')`); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMigrateFromOldSchema(t *testing.T) {
	path := buildOldDB(t)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrating an old-schema database failed: %v", err)
	}
	defer s.Close()

	t.Run("items survive with the new columns defaulted", func(t *testing.T) {
		items, err := s.ListAllItems()
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 4 {
			t.Fatalf("kept %d items, want 4 — migration lost user data", len(items))
		}
		for _, it := range items {
			if it.RequestedBy != "" || it.IsPrivate || it.Magnet != "" {
				t.Errorf("new columns should default empty/false, got %+v", it)
			}
			if it.StaleSince != nil {
				t.Errorf("stale_since should default to NULL, got %v", it.StaleSince)
			}
		}
	})

	t.Run("season/episode are backfilled from the title", func(t *testing.T) {
		ep, err := s.GetEpisode(2, 1, 8)
		if err != nil {
			t.Fatal(err)
		}
		if ep == nil {
			t.Fatal("S01E08 was not backfilled out of the title")
		}
		ep2, _ := s.GetEpisode(2, 2, 10)
		if ep2 == nil {
			t.Fatal("S02E10 was not backfilled out of the title")
		}
		// A title with no SxxEyy token must be left at 0/0, not guessed.
		untitled, err := s.GetByStrmPath("/lib/Other/x.strm")
		if err != nil {
			t.Fatal(err)
		}
		if untitled.Season != 0 || untitled.Episode != 0 {
			t.Errorf("guessed season/episode for an untokenised title: %d/%d", untitled.Season, untitled.Episode)
		}
	})

	t.Run("an interrupted queue row is reset to pending", func(t *testing.T) {
		items, err := s.ListAllQueue()
		if err != nil {
			t.Fatal(err)
		}
		var stuck, finished *QueueItem
		for _, it := range items {
			switch it.Title {
			case "Stuck":
				stuck = it
			case "Finished":
				finished = it
			}
		}
		if stuck == nil {
			t.Fatal("the interrupted queue row disappeared")
		}
		if stuck.Status != "pending" {
			t.Errorf("interrupted row status = %q, want pending", stuck.Status)
		}
		if stuck.Stage != StageQueued {
			t.Errorf("interrupted row stage = %q, want %q", stuck.Stage, StageQueued)
		}
		if finished == nil {
			t.Fatal("the done queue row disappeared")
		}
		if finished.Stage != "done" {
			t.Errorf("done row stage = %q, want it backfilled to \"done\"", finished.Stage)
		}
	})

	t.Run("the legacy password becomes an admin user", func(t *testing.T) {
		u, err := s.GetUserByUsername("admin")
		if err != nil {
			t.Fatal(err)
		}
		if u == nil {
			t.Fatal("the legacy dashboard password was not promoted to a user")
		}
		if !u.IsAdmin || u.PasswordHash != "$2a$10$legacyhash" {
			t.Errorf("promoted user is wrong: %+v", u)
		}
		// And the legacy setting is gone, so it cannot be promoted twice.
		v, _ := s.GetSetting("dashboard_password_hash")
		if v != "" {
			t.Errorf("the legacy password setting survived: %q", v)
		}
	})

	t.Run("the strm_path UNIQUE constraint now exists", func(t *testing.T) {
		// Upsert relies on ON CONFLICT(strm_path); without the constraint it would
		// insert duplicates instead of updating.
		it := &Item{TMDBID: 1, MediaType: "movie", Title: "Inception", Year: "2010",
			StrmPath: "/lib/Inception (2010)/Inception (2010).strm", Status: "ready", Updated: time.Now()}
		if err := s.Upsert(it); err != nil {
			t.Fatal(err)
		}
		items, _ := s.ListAllItems()
		if len(items) != 4 {
			t.Fatalf("upsert duplicated a row: %d items, want 4", len(items))
		}
	})
}

// TestMigrateIsIdempotent: reopening an already-migrated database must be a no-op.
func TestMigrateIsIdempotent(t *testing.T) {
	path := buildOldDB(t)
	for i := 0; i < 3; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open #%d failed: %v", i+1, err)
		}
		items, err := s.ListAllItems()
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 4 {
			t.Fatalf("open #%d: %d items, want 4", i+1, len(items))
		}
		users, err := s.ListUsers()
		if err != nil {
			t.Fatal(err)
		}
		if len(users) != 1 {
			t.Fatalf("open #%d: %d users, want exactly 1 (the migration must not re-run)", i+1, len(users))
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestOpenOnAFreshPathCreatesEverything covers the first-run path.
func TestOpenOnAFreshPath(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("fresh Open: %v", err)
	}
	defer s.Close()
	if err := s.Ping(); err != nil {
		t.Fatal(err)
	}
	users, err := s.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Fatalf("a fresh database has %d users, want 0", len(users))
	}
}

// ── The provider dimension ────────────────────────────────────────────────────

// preProviderSchema is the shape a DEPLOYED database has immediately before the
// provider columns exist: every additive column migration up to that point already
// applied, plus the tmdb_id-only partial unique index that stops duplicate in-flight
// queue rows.
//
// That index is the interesting part. It already exists under exactly the name the new
// one wants, so a naive CREATE UNIQUE INDEX IF NOT EXISTS would leave the OLD definition
// in place — and it would do so only on databases that have been through the earlier
// migration, i.e. only on the ones with a user's data in them. A fresh test database
// would look perfectly fine. Hence this fixture.
const preProviderSchema = `
CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE sessions (
	token   TEXT     PRIMARY KEY,
	expires DATETIME NOT NULL,
	user_id INTEGER  NOT NULL DEFAULT 0
);
CREATE TABLE users (
	id               INTEGER  PRIMARY KEY AUTOINCREMENT,
	username         TEXT     NOT NULL UNIQUE,
	password_hash    TEXT     NOT NULL DEFAULT '',
	jellyfin_user_id TEXT     NOT NULL DEFAULT '',
	auth_source      TEXT     NOT NULL DEFAULT 'local',
	is_admin         INTEGER  NOT NULL DEFAULT 0,
	created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE items (
	id            INTEGER  PRIMARY KEY AUTOINCREMENT,
	tmdb_id       INTEGER  NOT NULL,
	media_type    TEXT     NOT NULL,
	title         TEXT     NOT NULL,
	year          TEXT     NOT NULL,
	info_hash     TEXT     NOT NULL DEFAULT '',
	file_index    INTEGER  NOT NULL DEFAULT 0,
	strm_path     TEXT     NOT NULL DEFAULT '',
	status        TEXT     NOT NULL DEFAULT 'requested',
	seeders       INTEGER  NOT NULL DEFAULT 0,
	updated       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	library_name  TEXT     NOT NULL DEFAULT '',
	requested_by  TEXT     NOT NULL DEFAULT '',
	is_private    INTEGER  NOT NULL DEFAULT 0,
	poster_url    TEXT     NOT NULL DEFAULT '',
	magnet        TEXT     NOT NULL DEFAULT '',
	release_title TEXT     NOT NULL DEFAULT '',
	stale_since   DATETIME,
	season        INTEGER  NOT NULL DEFAULT 0,
	episode       INTEGER  NOT NULL DEFAULT 0,
	UNIQUE(strm_path)
);
CREATE TABLE queue (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	tmdb_id         INTEGER NOT NULL,
	media_type      TEXT    NOT NULL,
	title           TEXT    NOT NULL DEFAULT '',
	year            TEXT    NOT NULL DEFAULT '',
	poster_url      TEXT    NOT NULL DEFAULT '',
	season          INTEGER NOT NULL DEFAULT 0,
	episode         INTEGER NOT NULL DEFAULT 0,
	library_name    TEXT    NOT NULL DEFAULT '',
	requested_by    TEXT    NOT NULL DEFAULT '',
	magnet_override TEXT    NOT NULL DEFAULT '',
	status          TEXT    NOT NULL DEFAULT 'pending',
	progress        TEXT    NOT NULL DEFAULT '',
	error_msg       TEXT    NOT NULL DEFAULT '',
	info_hash       TEXT    NOT NULL DEFAULT '',
	strm_path       TEXT    NOT NULL DEFAULT '',
	created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	stage           TEXT    NOT NULL DEFAULT '',
	diagnosis       TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE subscriptions (
	id           INTEGER  PRIMARY KEY AUTOINCREMENT,
	tmdb_id      INTEGER  NOT NULL,
	season       INTEGER  NOT NULL,
	title        TEXT     NOT NULL DEFAULT '',
	poster_url   TEXT     NOT NULL DEFAULT '',
	library_name TEXT     NOT NULL DEFAULT '',
	requested_by TEXT     NOT NULL DEFAULT '',
	is_airing    INTEGER  NOT NULL DEFAULT 1,
	last_checked DATETIME,
	created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(tmdb_id, season)
);
CREATE UNIQUE INDEX idx_queue_active_identity
	ON queue(tmdb_id,media_type,season,episode,requested_by)
	WHERE status IN ('pending','processing');
CREATE INDEX idx_queue_group
	ON queue(tmdb_id,media_type,season,requested_by,status);
`

// buildPreProviderDB writes a fixture at the pre-provider schema, populated with the
// kind of rows the live deployment actually holds: a real TMDB show (1622 is the id in
// the /play/tv/1622/14/1 URLs baked into the user's .strm files), several of its
// episodes, a movie, and queue rows in both in-flight and terminal states.
func buildPreProviderDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pre-provider.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(preProviderSchema); err != nil {
		t.Fatalf("create pre-provider schema: %v", err)
	}
	items := []struct {
		tmdb       int
		mediaType  string
		title      string
		season, ep int
		strm       string
		status     string
	}{
		{1622, "tv", "Supernatural S14E01", 14, 1, "/lib/Supernatural/Season 14/S14E01.strm", "ready"},
		{1622, "tv", "Supernatural S14E02", 14, 2, "/lib/Supernatural/Season 14/S14E02.strm", "ready"},
		{1622, "tv", "Supernatural S15E01", 15, 1, "/lib/Supernatural/Season 15/S15E01.strm", "stale"},
		{27205, "movie", "Inception", 0, 0, "/lib/Inception (2010)/Inception (2010).strm", "ready"},
	}
	for _, it := range items {
		if _, err := db.Exec(
			`INSERT INTO items (tmdb_id,media_type,title,year,info_hash,file_index,strm_path,status,seeders,updated,season,episode,requested_by)
			 VALUES (?,?,?,'2018','aaa',1,?,?,10,?,?,?,'alice')`,
			it.tmdb, it.mediaType, it.title, it.strm, it.status, time.Now(), it.season, it.ep); err != nil {
			t.Fatal(err)
		}
	}
	queue := []struct {
		tmdb       int
		mediaType  string
		title      string
		season, ep int
		status     string
		owner      string
	}{
		{1622, "tv", "Supernatural", 14, 3, "pending", "alice"},
		{1622, "tv", "Supernatural", 14, 4, "processing", "alice"},
		{1622, "tv", "Supernatural", 14, 1, "done", "alice"},
		{27205, "movie", "Inception", 0, 0, "failed", "bob"},
	}
	for _, q := range queue {
		if _, err := db.Exec(
			`INSERT INTO queue (tmdb_id,media_type,title,season,episode,status,stage,requested_by)
			 VALUES (?,?,?,?,?,?,?,?)`,
			q.tmdb, q.mediaType, q.title, q.season, q.ep, q.status, q.status, q.owner); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// TestMigrateBackfillsProviderIdentity is the headline migration assertion: a database
// full of TMDB rows must come out the other side with every row carrying the canonical
// identity ("tmdb", the decimal spelling of its old integer) — and with tmdb_id itself
// untouched, because live .strm files sign that integer into an HMAC.
func TestMigrateBackfillsProviderIdentity(t *testing.T) {
	path := buildPreProviderDB(t)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrating a pre-provider database failed: %v", err)
	}
	defer s.Close()

	items, err := s.ListAllItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("kept %d items, want 4 — the migration lost user data", len(items))
	}
	for _, it := range items {
		if it.Provider != ProviderTMDB {
			t.Errorf("item %q provider = %q, want %q", it.StrmPath, it.Provider, ProviderTMDB)
		}
		want := strconv.Itoa(it.TMDBID)
		if it.ProviderID != want {
			t.Errorf("item %q provider_id = %q, want %q", it.StrmPath, it.ProviderID, want)
		}
	}

	rows, err := s.ListAllQueue()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("kept %d queue rows, want 4", len(rows))
	}
	for _, q := range rows {
		if q.Provider != ProviderTMDB || q.ProviderID != strconv.Itoa(q.TMDBID) {
			t.Errorf("queue row %d identity = (%q,%q), want (%q,%q)",
				q.ID, q.Provider, q.ProviderID, ProviderTMDB, strconv.Itoa(q.TMDBID))
		}
	}

	// The literal case from the brief: an existing row must read ("tmdb", "1622").
	ep, err := s.GetByProviderIdentity(Identity{Provider: ProviderTMDB, ProviderID: "1622",
		MediaType: "tv", Season: 14, Episode: 1})
	if err != nil || ep == nil {
		t.Fatalf("GetByProviderIdentity(tmdb/1622 s14e1): %v %v", ep, err)
	}
	if ep.Provider != ProviderTMDB || ep.ProviderID != "1622" || ep.TMDBID != 1622 {
		t.Fatalf("identity = (%q,%q,%d), want (\"tmdb\",\"1622\",1622)", ep.Provider, ep.ProviderID, ep.TMDBID)
	}
	// And the TMDB-shaped lookup that /play/tv/1622/14/1 goes through still resolves
	// the very same row. This is the assertion that says the 1,026 live tokens survive.
	viaTMDB, err := s.GetByIdentity(1622, "tv", 14, 1)
	if err != nil || viaTMDB == nil {
		t.Fatalf("GetByIdentity(1622,tv,14,1): %v %v", viaTMDB, err)
	}
	if viaTMDB.ID != ep.ID {
		t.Fatalf("the TMDB lookup found row %d, the provider lookup found row %d", viaTMDB.ID, ep.ID)
	}
}

// TestMigrateRebuildsTheIdentityIndex checks the drop-and-recreate, because
// IF NOT EXISTS would have silently kept the old tmdb_id-only definition.
func TestMigrateRebuildsTheIdentityIndex(t *testing.T) {
	path := buildPreProviderDB(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var ddl string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_queue_active_identity'`).
		Scan(&ddl); err != nil {
		t.Fatalf("the identity index is gone entirely: %v", err)
	}
	if !containsStr(ddl, "provider") || !containsStr(ddl, "provider_id") {
		t.Fatalf("the index was not rebuilt with the provider dimension:\n%s", ddl)
	}
	if containsStr(ddl, "tmdb_id") {
		t.Fatalf("the rebuilt index still keys on tmdb_id, so two providers sharing a "+
			"numeric id would still collide:\n%s", ddl)
	}

	// It must still do its original job: one in-flight row per identity per requester.
	if _, err := s.db.Exec(
		`INSERT INTO queue (tmdb_id,provider,provider_id,media_type,season,episode,requested_by,status)
		 VALUES (1622,'tmdb','1622','tv',14,3,'alice','pending')`); err == nil {
		t.Fatal("the unique index let a duplicate in-flight row through")
	}
}

// TestMigrateProviderBackfillIsIdempotentAndScoped reopens an already-migrated database
// repeatedly. The backfill runs on EVERY startup, so it has to be a no-op the second
// time — and, critically, it must leave rows belonging to another provider alone. An
// unscoped `WHERE provider_id=”` would stamp such a row with CAST(tmdb_id AS TEXT) and
// silently hand it TMDB's identity.
func TestMigrateProviderBackfillIsIdempotentAndScoped(t *testing.T) {
	path := buildPreProviderDB(t)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// A second provider's row, inserted directly at its worst possible shape: a real
	// UUID id, and a sibling row that (through some future bug) has no id at all.
	if _, err := s.db.Exec(
		`INSERT INTO queue (tmdb_id,provider,provider_id,media_type,title,season,episode,requested_by,status)
		 VALUES (0,'anidb','cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c','tv','Other',1,1,'alice','done')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO queue (tmdb_id,provider,provider_id,media_type,title,season,episode,requested_by,status)
		 VALUES (0,'anidb','','tv','Broken',2,2,'alice','done')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("reopen #%d: %v", i+1, err)
		}
		rows, err := s.ListAllQueue()
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 6 {
			t.Fatalf("reopen #%d: %d queue rows, want 6 — a re-run of the migration moved rows", i+1, len(rows))
		}
		var uuid, broken *QueueItem
		for _, q := range rows {
			switch q.Title {
			case "Other":
				uuid = q
			case "Broken":
				broken = q
			}
		}
		if uuid == nil || uuid.ProviderID != "cc5a1adf-5ba4-441f-bcf0-6ade6fcd1e6c" {
			t.Fatalf("reopen #%d: the UUID row's provider_id was rewritten: %+v", i+1, uuid)
		}
		if uuid.Provider != "anidb" || uuid.TMDBID != 0 {
			t.Fatalf("reopen #%d: the UUID row's provider changed: %+v", i+1, uuid)
		}
		if broken == nil || broken.ProviderID != "" {
			t.Fatalf("reopen #%d: the backfill stamped a non-TMDB row with TMDB's id: %+v", i+1, broken)
		}
		// And the TMDB rows are still exactly where they were.
		for _, q := range rows {
			if q.Provider == ProviderTMDB && q.ProviderID != strconv.Itoa(q.TMDBID) {
				t.Fatalf("reopen #%d: TMDB row %d drifted to provider_id %q", i+1, q.ID, q.ProviderID)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestMigrateFromOldSchemaGivesEveryRowAnIdentity runs the SAME fixture the original
// migration test uses — a database with no provider columns at all — and checks the
// backfill reaches it too, not just the newer shape.
func TestMigrateFromOldSchemaGivesEveryRowAnIdentity(t *testing.T) {
	s, err := Open(buildOldDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	items, err := s.ListAllItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("%d items, want 4", len(items))
	}
	for _, it := range items {
		if it.Provider != ProviderTMDB || it.ProviderID != strconv.Itoa(it.TMDBID) {
			t.Errorf("item %q identity = (%q,%q), tmdb_id %d", it.StrmPath, it.Provider, it.ProviderID, it.TMDBID)
		}
	}
	rows, err := s.ListAllQueue()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range rows {
		if q.Provider != ProviderTMDB || q.ProviderID != strconv.Itoa(q.TMDBID) {
			t.Errorf("queue row %d identity = (%q,%q), tmdb_id %d", q.ID, q.Provider, q.ProviderID, q.TMDBID)
		}
	}
}
