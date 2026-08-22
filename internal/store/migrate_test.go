package store

import (
	"database/sql"
	"path/filepath"
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
