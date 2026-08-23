package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// sxxeyy extracts season/episode numbers from a string like "Show S01E08".
var sxxeyy = regexp.MustCompile(`(?i)s(\d{1,2})[\s._-]*e(\d{1,3})`)

// Item is one library entry. JSON field names are snake_case and LOCKED by the API
// contract — the web UI reads exactly these names.
type Item struct {
	ID           int64      `json:"id"`
	TMDBID       int        `json:"tmdb_id"`
	MediaType    string     `json:"media_type"` // "movie" | "tv"
	Title        string     `json:"title"`
	Year         string     `json:"year"`
	InfoHash     string     `json:"info_hash"`
	FileIndex    int        `json:"file_index"`
	StrmPath     string     `json:"strm_path"`
	LibraryName  string     `json:"library_name"`
	Status       string     `json:"status"` // "requested" | "ready" | "stale" (expired, revivable)
	Seeders      int        `json:"seeders"`
	Updated      time.Time  `json:"updated"`
	RequestedBy  string     `json:"requested_by"`
	IsPrivate    bool       `json:"is_private"`
	PosterURL    string     `json:"poster_url"`
	Magnet       string     `json:"magnet"`        // the magnet used — kept so a dropped torrent can be re-added
	ReleaseTitle string     `json:"release_title"` // the chosen release name, e.g. "Inception.2010.1080p.BluRay.x264"
	StaleSince   *time.Time `json:"stale_since"`   // when it went stale; nil while ready
	Season       int        `json:"season"`        // 0 for movies
	Episode      int        `json:"episode"`       // 0 for movies
}

// Redacted returns a copy of the item with server-side/secret fields blanked, for
// serialisation to an UNAUTHENTICATED caller. Magnets carry tracker lists, strm_path
// is a server filesystem path, and requested_by is another user's identity — none of
// which an anonymous LAN visitor may see. (API contract §2.)
func (i Item) Redacted() Item {
	i.Magnet = ""
	i.StrmPath = ""
	i.RequestedBy = ""
	return i
}

// itemCols is the canonical SELECT column list for Item rows.
const itemCols = `id,tmdb_id,media_type,title,year,info_hash,file_index,strm_path,library_name,status,seeders,updated,requested_by,is_private,poster_url,magnet,release_title,stale_since,season,episode`

// Queue stage tokens — a CLOSED, ordered set the UI renders as a stepper.
// Progress stays free-text human prose; Stage is the machine-readable position.
// (API contract §6.)
const (
	StageQueued    = "queued"
	StageIndexing  = "indexing"
	StagePicking   = "picking"
	StageAdding    = "adding"
	StageVerifying = "verifying"
	StageWriting   = "writing"
	StageDone      = "done"
	StageFailed    = "failed"
	StageCancelled = "cancelled"
)

type QueueItem struct {
	ID             int64     `json:"id"`
	TMDBID         int       `json:"tmdb_id"`
	MediaType      string    `json:"media_type"`
	Title          string    `json:"title"`
	Year           string    `json:"year"`
	PosterURL      string    `json:"poster_url"`
	Season         int       `json:"season"`
	Episode        int       `json:"episode"`
	LibraryName    string    `json:"library_name"`
	RequestedBy    string    `json:"requested_by"`
	MagnetOverride string    `json:"magnet_override"`
	Status         string    `json:"status"` // pending|processing|done|failed|cancelled
	Progress       string    `json:"progress"`
	Stage          string    `json:"stage"`
	ErrorMsg       string    `json:"error_msg"`
	InfoHash       string    `json:"info_hash"`
	StrmPath       string    `json:"strm_path"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	// Diagnosis is a JSON blob explaining a "no release found" failure. It is served
	// only from GET /api/queue/{id}/diagnosis, never inline in the queue list.
	Diagnosis string `json:"-"`
}

// Redacted blanks the fields an unauthenticated caller must not see. (API contract §2.)
func (q QueueItem) Redacted() QueueItem {
	q.MagnetOverride = ""
	q.StrmPath = ""
	q.RequestedBy = ""
	return q
}

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	// PasswordHash MUST never be serialised. Tagged json:"-" so no handler can leak
	// the bcrypt hash by marshalling a User directly.
	PasswordHash   string    `json:"-"`
	JellyfinUserID string    `json:"jellyfin_user_id"`
	AuthSource     string    `json:"auth_source"` // "local" | "jellyfin"
	IsAdmin        bool      `json:"is_admin"`
	CreatedAt      time.Time `json:"created_at"`
}

// Subscription auto-fetches newly-aired episodes of a TV season for airing shows.
type Subscription struct {
	ID          int64      `json:"id"`
	TMDBID      int        `json:"tmdb_id"`
	Season      int        `json:"season"`
	Title       string     `json:"title"`
	PosterURL   string     `json:"poster_url"`
	LibraryName string     `json:"library_name"`
	RequestedBy string     `json:"requested_by"`
	IsAiring    bool       `json:"is_airing"`
	LastChecked *time.Time `json:"last_checked"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Redacted blanks the fields an unauthenticated caller must not see. (API contract §2.)
func (s Subscription) Redacted() Subscription {
	s.RequestedBy = ""
	return s
}

type Store struct {
	db *sql.DB
}

// DSN builds the modernc.org/sqlite connection string for a database file.
//
// Every pragma here is load-bearing:
//
//   - busy_timeout   — without it SQLite returns SQLITE_BUSY *immediately* on any write
//     contention. With a 3s worker tick, HTTP handlers, and background
//     tasks all writing, that meant the overwhelming majority of
//     concurrent writes simply failed.
//   - journal_mode=WAL — readers no longer block the writer (and vice versa), which is the
//     actual fix for a read-heavy dashboard polling next to a writer.
//   - foreign_keys     — enforced rather than silently ignored, so referential bugs surface.
//   - _txlock=immediate — take the write lock at BEGIN instead of upgrading mid-transaction,
//     which is what turns a deadlock into a retriable busy wait.
func DSN(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(on)" +
		"&_txlock=immediate"
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// SQLite permits exactly one writer. Serialising at the pool removes write
	// contention entirely rather than relying on every caller to retry; busy_timeout
	// above still covers contention from *other processes* (e.g. a `sqlite3` shell).
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// sql.Open is lazy — force a real connection now so a bad path/permission fails
	// at startup instead of on the first request.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Ping verifies the database is actually usable. /healthz calls it so liveness means
// "the process is up AND its own storage answers", not just "the process is up".
func (s *Store) Ping() error { return s.db.Ping() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
	CREATE TABLE IF NOT EXISTS settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS sessions (
		token   TEXT    PRIMARY KEY,
		expires DATETIME NOT NULL
	);
	CREATE TABLE IF NOT EXISTS users (
		id               INTEGER  PRIMARY KEY AUTOINCREMENT,
		username         TEXT     NOT NULL UNIQUE,
		password_hash    TEXT     NOT NULL DEFAULT '',
		jellyfin_user_id TEXT     NOT NULL DEFAULT '',
		auth_source      TEXT     NOT NULL DEFAULT 'local',
		is_admin         INTEGER  NOT NULL DEFAULT 0,
		created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS items (
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
		updated    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(strm_path)
	);
	CREATE TABLE IF NOT EXISTS queue (
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
	CREATE TABLE IF NOT EXISTS subscriptions (
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
	);`)
	if err != nil {
		return err
	}
	if err := s.migrateItemsConstraint(); err != nil {
		return err
	}
	// Additive column migrations. addColumn tolerates ONLY "duplicate column name"
	// (the column already exists from a previous run); any other failure is a real
	// migration error and aborts startup rather than leaving a half-migrated schema
	// that the rest of the code will then read wrong.
	adds := []string{
		`ALTER TABLE items    ADD COLUMN library_name  TEXT     NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN user_id       INTEGER  NOT NULL DEFAULT 0`,
		`ALTER TABLE items    ADD COLUMN requested_by  TEXT     NOT NULL DEFAULT ''`,
		`ALTER TABLE items    ADD COLUMN is_private    INTEGER  NOT NULL DEFAULT 0`,
		`ALTER TABLE items    ADD COLUMN poster_url    TEXT     NOT NULL DEFAULT ''`,
		`ALTER TABLE items    ADD COLUMN magnet        TEXT     NOT NULL DEFAULT ''`,
		`ALTER TABLE items    ADD COLUMN release_title TEXT     NOT NULL DEFAULT ''`,
		`ALTER TABLE items    ADD COLUMN stale_since   DATETIME`,
		`ALTER TABLE items    ADD COLUMN season        INTEGER  NOT NULL DEFAULT 0`,
		`ALTER TABLE items    ADD COLUMN episode       INTEGER  NOT NULL DEFAULT 0`,
		`ALTER TABLE queue    ADD COLUMN stage         TEXT     NOT NULL DEFAULT ''`,
		`ALTER TABLE queue    ADD COLUMN diagnosis     TEXT     NOT NULL DEFAULT ''`,
	}
	for _, stmt := range adds {
		if err := s.addColumn(stmt); err != nil {
			return err
		}
	}
	// One-time collapse of duplicate queue rows, then the constraint that makes them
	// impossible. A magnet-override request used to bypass every idempotency check
	// (POST /request), so a client that re-fired on each response could insert tens of
	// thousands of identical rows for one title — the queue list, which is capped at 100
	// rows ordered newest-first, then showed nothing but that one title. The DELETEs run
	// before the index because CREATE UNIQUE INDEX fails outright while duplicates exist.
	//
	// Scope is (identity + requester), not identity alone: ListQueue filters on
	// requested_by, so collapsing two people's requests for the same title into one row
	// would erase the second person's request from their own queue. The flood came from a
	// single account, so per-requester scoping closes it just as completely.
	//
	// In-flight rows keep the OLDEST per identity (it holds the queue position the user
	// actually waited for); terminal rows keep the NEWEST per identity+status (the most
	// recent outcome is the true one).
	if _, err := s.db.Exec(
		`DELETE FROM queue WHERE status IN ('pending','processing') AND id NOT IN (
             SELECT MIN(id) FROM queue WHERE status IN ('pending','processing')
             GROUP BY tmdb_id,media_type,season,episode,requested_by)`); err != nil {
		return fmt.Errorf("collapse duplicate in-flight queue rows: %w", err)
	}
	if _, err := s.db.Exec(
		`DELETE FROM queue WHERE status IN ('done','failed','cancelled') AND id NOT IN (
             SELECT MAX(id) FROM queue WHERE status IN ('done','failed','cancelled')
             GROUP BY tmdb_id,media_type,season,episode,status,requested_by)`); err != nil {
		return fmt.Errorf("collapse duplicate terminal queue rows: %w", err)
	}
	if _, err := s.db.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_queue_active_identity
             ON queue(tmdb_id,media_type,season,episode,requested_by)
             WHERE status IN ('pending','processing')`); err != nil {
		return fmt.Errorf("create queue identity index: %w", err)
	}
	// The queue tree reads the whole visible queue in ONE aggregate over
	// (tmdb_id, media_type, season) — ListQueueGroups — and then fetches a single
	// title's or season's leaf rows back out — ListQueueFiltered. Before this index the
	// table had no non-unique index at all, so both shapes were a full table scan, and
	// the aggregate paid a temp b-tree on top to group its rows. That is cheap at the
	// healthy 1,591 rows and grows with the table — and the table grows fastest exactly
	// when something has gone wrong, which is when the queue page most needs to load.
	//
	// The column ORDER earns its keep twice. It matches the GROUP BY term order, so
	// SQLite walks the index in group order and drops the temp b-tree for the GROUP BY
	// entirely; and (tmdb_id,media_type,season) is a usable prefix for the scoped leaf
	// query, which filters on precisely those. requested_by sits FOURTH rather than
	// first on purpose: leading with it would index a single user's view well and leave
	// the admin aggregate — which carries no requester predicate at all — back on a full
	// scan. Fourth, it still narrows the leaf query and keeps status adjacent for the
	// active-only filter.
	//
	// Measured on synthetic data at both shapes that matter. At the live one (894 rows,
	// 28 shows and 54 movies) the aggregate is 3.2ms against 3.7ms unindexed — a wash,
	// because at that size nothing is slow. At the flood shape (25,894 rows over the
	// same ~80 titles) it is 40ms against 66ms, and the scoped leaf fetch is where the
	// index really pays: 0.46ms against 3.4ms at 6,800 rows, and it only widens. Adding
	// title/poster_url/created_at to make the index genuinely COVERING was tried and
	// rejected — 60ms against 64ms at 29,800 rows, for a second copy of every title and
	// poster URL on disk.
	if _, err := s.db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_queue_group
             ON queue(tmdb_id,media_type,season,requested_by,status)`); err != nil {
		return fmt.Errorf("create queue grouping index: %w", err)
	}

	// Reset interrupted queue items on restart.
	if _, err := s.db.Exec(
		`UPDATE queue SET status='pending', progress='', stage=? WHERE status='processing'`,
		StageQueued); err != nil {
		return fmt.Errorf("reset interrupted queue items: %w", err)
	}
	// Backfill the stage token for rows written before the column existed, so the UI
	// stepper has something to render for historical items.
	if _, err := s.db.Exec(`UPDATE queue SET stage=status WHERE stage='' AND status IN ('done','failed','cancelled')`); err != nil {
		return fmt.Errorf("backfill queue stage: %w", err)
	}
	if _, err := s.db.Exec(`UPDATE queue SET stage=? WHERE stage='' AND status='pending'`, StageQueued); err != nil {
		return fmt.Errorf("backfill queue stage: %w", err)
	}
	// Backfill season/episode for TV items created before those columns existed.
	if err := s.backfillEpisodeNumbers(); err != nil {
		return err
	}
	// Migrate old single-password setting into the users table.
	return s.migrateAdminUser()
}

// addColumn runs an ALTER TABLE ... ADD COLUMN, treating "column already exists" as
// success and every other error as fatal.
func (s *Store) addColumn(stmt string) error {
	_, err := s.db.Exec(stmt)
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
		return nil // already migrated
	}
	return fmt.Errorf("migration %q: %w", stmt, err)
}

// migrateAdminUser promotes the old dashboard_password_hash setting into a proper
// admin User row so the new multi-user system is backward-compatible.
func (s *Store) migrateAdminUser() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if n > 0 {
		return nil // users table already populated
	}
	hash, err := s.GetSetting("dashboard_password_hash")
	if err != nil {
		return fmt.Errorf("read legacy password hash: %w", err)
	}
	if hash == "" {
		return nil // fresh install — setup page will create the first user
	}
	if _, err := s.db.Exec(
		`INSERT INTO users (username, password_hash, auth_source, is_admin) VALUES ('admin', ?, 'local', 1)`,
		hash,
	); err != nil {
		return fmt.Errorf("migrate admin user: %w", err)
	}
	// Only drop the legacy setting once the user row above committed — otherwise a
	// failure here would leave the install with no credentials at all.
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key='dashboard_password_hash'`); err != nil {
		return fmt.Errorf("clear legacy password hash: %w", err)
	}
	return nil
}

func (s *Store) migrateItemsConstraint() error {
	// SQLite UNIQUE table constraints create autoindexes with sql=NULL in sqlite_master,
	// so we check the table DDL directly instead of looking for an explicit index entry.
	var ddl string
	err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='items'`).Scan(&ddl)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read items DDL: %w", err)
	}
	if strings.Contains(ddl, "UNIQUE(strm_path)") || strings.Contains(ddl, "UNIQUE (strm_path)") {
		return nil
	}
	_, err = s.db.Exec(`
	CREATE TABLE IF NOT EXISTS items_new (
		id           INTEGER  PRIMARY KEY AUTOINCREMENT,
		tmdb_id      INTEGER  NOT NULL,
		media_type   TEXT     NOT NULL,
		title        TEXT     NOT NULL,
		year         TEXT     NOT NULL,
		info_hash    TEXT     NOT NULL DEFAULT '',
		file_index   INTEGER  NOT NULL DEFAULT 0,
		strm_path    TEXT     NOT NULL DEFAULT '',
		status       TEXT     NOT NULL DEFAULT 'requested',
		seeders      INTEGER  NOT NULL DEFAULT 0,
		updated      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		library_name TEXT     NOT NULL DEFAULT '',
		UNIQUE(strm_path)
	);
	INSERT OR IGNORE INTO items_new
		SELECT id,tmdb_id,media_type,title,year,info_hash,file_index,strm_path,status,seeders,updated,'' FROM items;
	DROP TABLE items;
	ALTER TABLE items_new RENAME TO items;
	`)
	return err
}

// ── Users ────────────────────────────────────────────────────────────────────

func (s *Store) CreateUser(u *User) error {
	_, err := s.db.Exec(
		`INSERT INTO users (username, password_hash, jellyfin_user_id, auth_source, is_admin)
		 VALUES (?, ?, ?, ?, ?)`,
		u.Username, u.PasswordHash, u.JellyfinUserID, u.AuthSource, boolToInt(u.IsAdmin),
	)
	return err
}

func (s *Store) GetUserByUsername(username string) (*User, error) {
	row := s.db.QueryRow(
		`SELECT id, username, password_hash, jellyfin_user_id, auth_source, is_admin, created_at
		 FROM users WHERE username=?`, username)
	return scanUser(row)
}

func (s *Store) GetUserByID(id int64) (*User, error) {
	row := s.db.QueryRow(
		`SELECT id, username, password_hash, jellyfin_user_id, auth_source, is_admin, created_at
		 FROM users WHERE id=?`, id)
	return scanUser(row)
}

func (s *Store) ListUsers() ([]*User, error) {
	rows, err := s.db.Query(
		`SELECT id, username, password_hash, jellyfin_user_id, auth_source, is_admin, created_at
		 FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) UpdateUser(u *User) error {
	_, err := s.db.Exec(
		`UPDATE users SET username=?, password_hash=?, jellyfin_user_id=?, auth_source=?, is_admin=? WHERE id=?`,
		u.Username, u.PasswordHash, u.JellyfinUserID, u.AuthSource, boolToInt(u.IsAdmin), u.ID,
	)
	return err
}

func (s *Store) DeleteUser(id int64) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE id=?`, id)
	return err
}

// ── Sessions ─────────────────────────────────────────────────────────────────

func (s *Store) CreateSession(token string, userID int64, expires time.Time) error {
	_, err := s.db.Exec(`INSERT INTO sessions(token, user_id, expires) VALUES(?,?,?)`, token, userID, expires)
	return err
}

// GetSessionUser returns the User associated with a valid (non-expired) session token.
func (s *Store) GetSessionUser(token string) (*User, bool) {
	var userID int64
	var exp time.Time
	err := s.db.QueryRow(`SELECT user_id, expires FROM sessions WHERE token=?`, token).Scan(&userID, &exp)
	if err != nil || time.Now().After(exp) {
		return nil, false
	}
	user, err := s.GetUserByID(userID)
	if err != nil || user == nil {
		return nil, false
	}
	return user, true
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token=?`, token)
	return err
}

func (s *Store) PurgeSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires < ?`, time.Now())
	return err
}

// DeleteSessionsForUser invalidates every session belonging to a user. Called on a
// password change so a stolen cookie cannot outlive the credential it was issued for.
// keepToken (may be empty) is preserved so the user changing their own password is
// not logged out of the tab they are using.
func (s *Store) DeleteSessionsForUser(userID int64, keepToken string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user_id=? AND token<>?`, userID, keepToken)
	return err
}

// ── Items ─────────────────────────────────────────────────────────────────────

func (s *Store) Upsert(item *Item) error {
	_, err := s.db.Exec(`
	INSERT INTO items (tmdb_id, media_type, title, year, info_hash, file_index, strm_path, library_name, status, seeders, updated, requested_by, is_private, poster_url, magnet, release_title, stale_since, season, episode)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(strm_path) DO UPDATE SET
		tmdb_id=excluded.tmdb_id, media_type=excluded.media_type,
		title=excluded.title, year=excluded.year, info_hash=excluded.info_hash,
		file_index=excluded.file_index, library_name=excluded.library_name,
		status=excluded.status, seeders=excluded.seeders, updated=excluded.updated,
		requested_by=COALESCE(NULLIF(excluded.requested_by,''), requested_by),
		is_private=excluded.is_private,
		poster_url=COALESCE(NULLIF(excluded.poster_url,''), poster_url),
		magnet=COALESCE(NULLIF(excluded.magnet,''), magnet),
		release_title=COALESCE(NULLIF(excluded.release_title,''), release_title),
		stale_since=excluded.stale_since,
		season=excluded.season, episode=excluded.episode`,
		item.TMDBID, item.MediaType, item.Title, item.Year,
		item.InfoHash, item.FileIndex, item.StrmPath, item.LibraryName,
		item.Status, item.Seeders, item.Updated, item.RequestedBy, boolToInt(item.IsPrivate),
		item.PosterURL, item.Magnet, item.ReleaseTitle, item.StaleSince, item.Season, item.Episode,
	)
	return err
}

// DeleteItem removes an item row entirely (user-initiated removal, not expiry).
func (s *Store) DeleteItem(strmPath string) error {
	_, err := s.db.Exec(`DELETE FROM items WHERE strm_path=?`, strmPath)
	return err
}

// MarkStale flags an item as expired, recording stale_since if not already set.
func (s *Store) MarkStale(strmPath string) error {
	_, err := s.db.Exec(
		`UPDATE items SET status='stale', stale_since=COALESCE(stale_since, ?), updated=? WHERE strm_path=?`,
		time.Now(), time.Now(), strmPath)
	return err
}

// ItemsByTMDB returns all items for a show/movie (used for series removal).
func (s *Store) ItemsByTMDB(tmdbID int, mediaType string) ([]*Item, error) {
	rows, err := s.db.Query(`SELECT `+itemCols+` FROM items WHERE tmdb_id=? AND media_type=?`, tmdbID, mediaType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []*Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// GetEpisode returns a specific TV episode item (used for per-episode removal).
func (s *Store) GetEpisode(tmdbID, season, episode int) (*Item, error) {
	row := s.db.QueryRow(
		`SELECT `+itemCols+` FROM items WHERE tmdb_id=? AND season=? AND episode=? AND media_type='tv'`,
		tmdbID, season, episode)
	return scanItem(row)
}

// CountByHash returns how many item rows (any status) reference a given info hash.
// Used to decide whether dropping a torrent is safe (season packs share a hash).
func (s *Store) CountByHash(hash string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM items WHERE info_hash=?`, hash).Scan(&n)
	return n, err
}

// backfillEpisodeNumbers parses S##E## out of the title for TV items that have
// season/episode still at 0 (rows created before those columns existed).
func (s *Store) backfillEpisodeNumbers() error {
	rows, err := s.db.Query(`SELECT id, title FROM items WHERE media_type='tv' AND season=0 AND episode=0`)
	if err != nil {
		return fmt.Errorf("backfill episode numbers: %w", err)
	}
	type upd struct {
		id            int64
		season, epNum int
	}
	var updates []upd
	for rows.Next() {
		var id int64
		var title string
		if err := rows.Scan(&id, &title); err != nil {
			rows.Close()
			return fmt.Errorf("backfill episode numbers: %w", err)
		}
		if m := sxxeyy.FindStringSubmatch(title); m != nil {
			sn, _ := strconv.Atoi(m[1])
			en, _ := strconv.Atoi(m[2])
			if sn > 0 && en > 0 {
				updates = append(updates, upd{id, sn, en})
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("backfill episode numbers: %w", err)
	}
	rows.Close()
	for _, u := range updates {
		if _, err := s.db.Exec(`UPDATE items SET season=?, episode=? WHERE id=?`, u.season, u.epNum, u.id); err != nil {
			return fmt.Errorf("backfill episode numbers: %w", err)
		}
	}
	return nil
}

// EpisodeActive reports whether a TV episode is already ready in the library or
// already pending/processing in the queue — used to skip redundant enqueues.
// On a DB error it reports true (fail CLOSED): skipping a possible duplicate is
// strictly safer than enqueuing a second copy of an episode we may already have.
func (s *Store) EpisodeActive(tmdbID, season, episode int) (bool, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM items WHERE tmdb_id=? AND season=? AND episode=? AND status='ready'`,
		tmdbID, season, episode).Scan(&n); err != nil {
		return true, err
	}
	if n > 0 {
		return true, nil
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM queue WHERE tmdb_id=? AND season=? AND episode=? AND status IN ('pending','processing')`,
		tmdbID, season, episode).Scan(&n); err != nil {
		return true, err
	}
	return n > 0, nil
}

// SetPrivate toggles the is_private flag on an item by strm path.
func (s *Store) SetPrivate(strmPath string, private bool) error {
	_, err := s.db.Exec(`UPDATE items SET is_private=? WHERE strm_path=?`, boolToInt(private), strmPath)
	return err
}

// GetStatusByTMDBIDs returns the best-status item per TMDB id, filtered to what the
// viewer may see. This endpoint feeds the search-result badges and was previously
// UNFILTERED, so a private item's existence, title and library leaked to anyone who
// guessed its TMDB id.
func (s *Store) GetStatusByTMDBIDs(ids []int, viewer string, isAdmin bool) (map[int]Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+2)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	where := ""
	if !isAdmin {
		where = ` AND (is_private=0 OR (requested_by=? AND ?<>''))`
		args = append(args, viewer, viewer)
	}
	q := fmt.Sprintf(
		`SELECT `+itemCols+` FROM items WHERE tmdb_id IN (%s)%s
		 ORDER BY CASE status WHEN 'ready' THEN 0 WHEN 'stale' THEN 1 WHEN 'error' THEN 2 ELSE 3 END`,
		strings.Join(placeholders, ","), where,
	)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int]Item)
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			continue
		}
		if _, exists := out[item.TMDBID]; !exists {
			out[item.TMDBID] = *item
		}
	}
	return out, rows.Err()
}

// ItemsNeedingPosters returns items whose poster_url is empty, so a backfill
// goroutine can fetch them from TMDB and call SetPosterURL.
func (s *Store) ItemsNeedingPosters() ([]*Item, error) {
	rows, err := s.db.Query(`SELECT ` + itemCols + ` FROM items WHERE poster_url='' AND status IN ('ready','stale') LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []*Item
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// SetPosterURL updates only the poster_url field for a given item ID.
func (s *Store) SetPosterURL(id int64, url string) error {
	_, err := s.db.Exec(`UPDATE items SET poster_url=? WHERE id=?`, url, id)
	return err
}

// CountReadyByHashExcept counts ready items sharing an info hash, excluding one strm path.
// Used on playback-stop to decide whether ANY OTHER live item (e.g. another episode from
// the same season pack) still needs the torrent before we drop it from TorrServer. The
// stopped item itself stays 'ready', so it must be excluded from this count.
func (s *Store) CountReadyByHashExcept(hash, strmPath string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM items WHERE info_hash=? AND status='ready' AND strm_path<>?`,
		hash, strmPath).Scan(&n)
	return n, err
}

func (s *Store) GetByHash(hash string) (*Item, error) {
	row := s.db.QueryRow(`SELECT `+itemCols+` FROM items WHERE info_hash=?`, hash)
	return scanItem(row)
}

// GetByIdentity looks an item up by its stable identity (TMDB id + type + season/episode;
// season/episode are 0 for movies). This is the key the Resolve-at-Play handler uses to map
// a /play/... URL back to its library row.
func (s *Store) GetByIdentity(tmdbID int, mediaType string, season, episode int) (*Item, error) {
	row := s.db.QueryRow(
		`SELECT `+itemCols+` FROM items WHERE tmdb_id=? AND media_type=? AND season=? AND episode=?`,
		tmdbID, mediaType, season, episode)
	return scanItem(row)
}

func (s *Store) GetByStrmPath(path string) (*Item, error) {
	row := s.db.QueryRow(`SELECT `+itemCols+` FROM items WHERE strm_path=?`, path)
	return scanItem(row)
}

// ── Visibility ────────────────────────────────────────────────────────────────
//
// There used to be ONE parameter pair (viewerUsername, isAdmin) with the rule
// "empty username == admin". That sentinel was meant only for the background job
// caller, but the HTTP layer passed "" for an *unauthenticated* viewer too — so an
// anonymous LAN visitor was treated as an admin and could read private items that
// a logged-in non-admin correctly could not. Logging out granted access that
// logging in denied.
//
// The sentinel is gone. The job path calls the explicit ListAll* methods; the HTTP
// path calls the List* methods, where an empty viewer means ANONYMOUS and always
// takes the most restrictive branch.

// ListAllItems returns every managed item regardless of privacy — the BACKGROUND JOB
// view. It must never be reached from an HTTP handler.
func (s *Store) ListAllItems() ([]*Item, error) {
	return s.queryItems(`SELECT ` + itemCols + ` FROM items WHERE status IN ('ready','stale')`)
}

// ListVisible returns library items visible to the given viewer. Includes both
// 'ready' (live) and 'stale' (expired but revivable) items — stale items are the
// user's request history and surface in the UI with an "Expired" badge.
//
// viewer=="" means ANONYMOUS: public items only, never private ones.
func (s *Store) ListVisible(viewer string, isAdmin bool) ([]*Item, error) {
	if isAdmin {
		return s.ListAllItems()
	}
	// The `?<>''` guard makes the anonymous case (viewer=="") collapse to
	// "public items only" instead of matching rows whose requested_by is blank.
	return s.queryItems(
		`SELECT `+itemCols+` FROM items
		 WHERE status IN ('ready','stale') AND (is_private=0 OR (requested_by=? AND ?<>''))`,
		viewer, viewer)
}

func (s *Store) queryItems(q string, args ...any) ([]*Item, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []*Item
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ── Settings ──────────────────────────────────────────────────────────────────

func (s *Store) GetSetting(key string) (string, error) {
	var val string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return val, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ── Scanners ──────────────────────────────────────────────────────────────────

type scanner interface {
	Scan(dest ...any) error
}

func scanItem(s scanner) (*Item, error) {
	var item Item
	var isPrivate int
	var staleSince sql.NullTime
	err := s.Scan(&item.ID, &item.TMDBID, &item.MediaType, &item.Title, &item.Year,
		&item.InfoHash, &item.FileIndex, &item.StrmPath, &item.LibraryName,
		&item.Status, &item.Seeders, &item.Updated, &item.RequestedBy, &isPrivate, &item.PosterURL,
		&item.Magnet, &item.ReleaseTitle, &staleSince, &item.Season, &item.Episode)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	item.IsPrivate = isPrivate == 1
	if staleSince.Valid {
		item.StaleSince = &staleSince.Time
	}
	return &item, nil
}

func scanUser(s scanner) (*User, error) {
	var u User
	var isAdmin int
	err := s.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.JellyfinUserID, &u.AuthSource, &isAdmin, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin == 1
	return &u, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ── Queue ─────────────────────────────────────────────────────────────────────

// queueCols is the canonical SELECT column list for QueueItem rows.
const queueCols = `id,tmdb_id,media_type,title,year,poster_url,season,episode,library_name,
                   requested_by,magnet_override,status,progress,stage,error_msg,info_hash,strm_path,
                   diagnosis,created_at,updated_at`

func scanQueueItem(sc scanner) (*QueueItem, error) {
	var item QueueItem
	err := sc.Scan(
		&item.ID, &item.TMDBID, &item.MediaType, &item.Title, &item.Year, &item.PosterURL,
		&item.Season, &item.Episode, &item.LibraryName, &item.RequestedBy, &item.MagnetOverride,
		&item.Status, &item.Progress, &item.Stage, &item.ErrorMsg, &item.InfoHash, &item.StrmPath,
		&item.Diagnosis, &item.CreatedAt, &item.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// Enqueue inserts a queue row, or returns the id of the in-flight row that already
// covers this identity. The ON CONFLICT clause pairs with idx_queue_active_identity,
// so a duplicate can never be created even if a caller skips the handler-level checks
// — this is the backstop that the magnet-override path used to have no equivalent of.
func (s *Store) Enqueue(item *QueueItem) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO queue (tmdb_id,media_type,title,year,poster_url,season,episode,library_name,requested_by,magnet_override,stage)
         VALUES (?,?,?,?,?,?,?,?,?,?,?)
         ON CONFLICT DO NOTHING`,
		item.TMDBID, item.MediaType, item.Title, item.Year, item.PosterURL,
		item.Season, item.Episode, item.LibraryName, item.RequestedBy, item.MagnetOverride, StageQueued,
	)
	if err != nil {
		return 0, err
	}
	// DO NOTHING leaves LastInsertId pointing at some earlier insert, so it must never
	// be trusted without checking that a row was actually written.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		existing, err := s.ActiveQueueItem(item.TMDBID, item.MediaType, item.Season, item.Episode, item.RequestedBy)
		if err != nil {
			return 0, fmt.Errorf("read incumbent queue row: %w", err)
		}
		if existing != nil {
			return existing.ID, nil
		}
		return 0, fmt.Errorf("queue insert conflicted but no in-flight row found for tmdb %d", item.TMDBID)
	}
	return res.LastInsertId()
}

// RequeueWithMagnet repoints an in-flight queue row at a different release and resets
// it to pending, so "pick this exact release instead" re-resolves the row the user is
// already watching rather than inserting a second one beside it.
func (s *Store) RequeueWithMagnet(id int64, magnet string) error {
	_, err := s.db.Exec(
		`UPDATE queue SET magnet_override=?, status='pending', stage=?, progress='', error_msg='',
             updated_at=CURRENT_TIMESTAMP WHERE id=?`, magnet, StageQueued, id)
	return err
}

// NextPending atomically claims the next pending queue item by marking it processing.
func (s *Store) NextPending() (*QueueItem, error) {
	item, err := scanQueueItem(s.db.QueryRow(
		`SELECT ` + queueCols + ` FROM queue WHERE status='pending' ORDER BY created_at LIMIT 1`))
	if err != nil || item == nil {
		return nil, err
	}
	if _, err := s.db.Exec(
		`UPDATE queue SET status='processing', stage=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		StageIndexing, item.ID); err != nil {
		// The claim did not commit. Returning the item anyway would let a restart
		// double-process it, so report the failure and let the worker retry later.
		return nil, fmt.Errorf("claim queue item %d: %w", item.ID, err)
	}
	item.Status = "processing"
	item.Stage = StageIndexing
	return item, nil
}

func (s *Store) UpdateQueue(item *QueueItem) error {
	_, err := s.db.Exec(
		`UPDATE queue SET status=?,progress=?,stage=?,error_msg=?,info_hash=?,strm_path=?,diagnosis=?,
		 updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		item.Status, item.Progress, item.Stage, item.ErrorMsg, item.InfoHash, item.StrmPath,
		item.Diagnosis, item.ID,
	)
	return err
}

func (s *Store) GetQueueItem(id int64) (*QueueItem, error) {
	return scanQueueItem(s.db.QueryRow(`SELECT `+queueCols+` FROM queue WHERE id=?`, id))
}

const (
	// queueListLimit caps the flat, unscoped queue list. It is a DISPLAY cap and never
	// a safety one — and it is worth being precise about what it does not do. When the
	// duplicate-insert bug (see idx_queue_active_identity in migrate) put 26,187 rows
	// in the table for a single movie, the newest hundred were all that movie, so none
	// of the user's 28 shows could appear on the queue page at all. A cap on a flat
	// newest-first list cannot survive a lopsided queue; the grouped tree below is the
	// actual answer, because it summarises EVERY row — 894 of them on the live
	// deployment, or 25,894 during the flood — in the same 82 titles either way.
	queueListLimit = 100
	// queueScopedLimit caps a fetch narrowed to one title. It can afford to be far
	// higher because such a result is bounded by an episode count rather than by the
	// size of the table.
	queueScopedLimit = 500
)

// QueueFilter narrows ListQueueFiltered to one branch of the grouped tree, so the UI
// can expand a single season and pull just that season's rows instead of the whole
// queue. The zero value filters nothing and yields exactly the list the queue page has
// always shown.
type QueueFilter struct {
	TMDBID    int    // 0 = any title
	MediaType string // "" = any; otherwise "movie" | "tv"
	// Season is consulted ONLY when SeasonSet is true. Season 0 is a REAL value — every
	// movie row carries it and so do TV specials — so "no season filter" cannot be
	// spelled as Season == 0 and needs the companion flag.
	Season     int
	SeasonSet  bool
	ActiveOnly bool // pending|processing only
}

// scoped reports whether the filter pins a single title. That, and only that, bounds a
// result by an episode count instead of by the size of the queue, which is what makes
// the higher row limit safe. ActiveOnly alone does not qualify: a queue can hold any
// number of pending rows.
func (f QueueFilter) scoped() bool { return f.TMDBID != 0 }

// ListQueue returns queue rows visible to the caller. requester=="" means ANONYMOUS,
// which sees nothing — a queue row is a named person's viewing request.
func (s *Store) ListQueue(requester string, isAdmin bool) ([]*QueueItem, error) {
	return s.ListQueueFiltered(requester, isAdmin, QueueFilter{})
}

// ListQueueFiltered is ListQueue with the grouped tree's filters applied. Both go
// through this one body deliberately, so the visibility rule cannot drift between
// them: ANONYMOUS (requester=="") sees nothing, a named user sees only their own rows,
// an admin sees everything. A filter can only ever NARROW what the caller was already
// allowed to see — passing a TMDBID never reaches another person's rows.
func (s *Store) ListQueueFiltered(requester string, isAdmin bool, f QueueFilter) ([]*QueueItem, error) {
	var (
		where []string
		args  []any
	)
	if !isAdmin {
		if requester == "" {
			return nil, nil
		}
		where = append(where, `requested_by=?`)
		args = append(args, requester)
	}
	if f.TMDBID != 0 {
		where = append(where, `tmdb_id=?`)
		args = append(args, f.TMDBID)
	}
	if f.MediaType != "" {
		where = append(where, `media_type=?`)
		args = append(args, f.MediaType)
	}
	if f.SeasonSet {
		where = append(where, `season=?`)
		args = append(args, f.Season)
	}
	if f.ActiveOnly {
		where = append(where, `status IN ('pending','processing')`)
	}
	q := `SELECT ` + queueCols + ` FROM queue`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	// Ordering follows the caller's intent. The unscoped list is a feed and stays
	// newest-first exactly as before; a scoped fetch is one season being expanded in
	// the tree, where the user expects episode order rather than request order.
	if f.scoped() {
		q += ` ORDER BY season, episode, created_at DESC LIMIT ?`
		args = append(args, queueScopedLimit)
	} else {
		q += ` ORDER BY created_at DESC LIMIT ?`
		args = append(args, queueListLimit)
	}
	return s.queryQueue(q, args...)
}

// ListAllQueue returns every queue row — the BACKGROUND JOB view, never an HTTP one.
func (s *Store) ListAllQueue() ([]*QueueItem, error) {
	return s.queryQueue(`SELECT ` + queueCols + ` FROM queue ORDER BY created_at DESC`)
}

func (s *Store) queryQueue(q string, args ...any) ([]*QueueItem, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []*QueueItem
	for rows.Next() {
		item, err := scanQueueItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ── Queue: the grouped tree ───────────────────────────────────────────────────
//
// The queue page cannot be a flat list. The user runs 28 shows and 844 episodes; at
// the peak of the duplicate-insert bug the table held 26,187 rows, and serving them
// flat was 15.7MB of JSON. Ordering them newest-first and cutting at a hundred does
// not help either — that is precisely how a single flooding movie hid every show the
// user owned. So the aggregation happens HERE, in one GROUP BY, and what crosses the
// wire is roughly one row per title-and-season no matter how large the table gets.
//
// JSON names are snake_case and LOCKED by the API contract (§1), same as Item and
// QueueItem above.

// QueueCounts is one group's status histogram. The five named buckets are the closed
// set of queue statuses (see QueueItem.Status).
type QueueCounts struct {
	Pending    int `json:"pending"`
	Processing int `json:"processing"`
	Done       int `json:"done"`
	Failed     int `json:"failed"`
	Cancelled  int `json:"cancelled"`
	// Total is COUNT(*) over the group and Active is pending+processing. Both are
	// derived, and the store fills them in so the UI never has to sum five numbers to
	// draw a progress ring. Total is deliberately the raw COUNT rather than the sum of
	// the five buckets: should a row ever carry a status outside the closed set, the
	// group's chips will visibly fail to add up instead of the row disappearing.
	Total  int `json:"total"`
	Active int `json:"active"`
}

func (c *QueueCounts) add(o QueueCounts) {
	c.Pending += o.Pending
	c.Processing += o.Processing
	c.Done += o.Done
	c.Failed += o.Failed
	c.Cancelled += o.Cancelled
	c.Total += o.Total
	c.Active += o.Active
}

// QueueSeasonGroup is one season of one show: the second level of the tree.
type QueueSeasonGroup struct {
	Season int         `json:"season"`
	Counts QueueCounts `json:"counts"`
	Newest time.Time   `json:"newest"` // most recent created_at in the season
}

// QueueShowGroup is one title: a show with its seasons nested, or a movie with none.
type QueueShowGroup struct {
	TMDBID    int         `json:"tmdb_id"`
	MediaType string      `json:"media_type"`
	Title     string      `json:"title"`
	PosterURL string      `json:"poster_url"`
	Counts    QueueCounts `json:"counts"` // rolled up across every season
	Newest    time.Time   `json:"newest"`
	// Seasons is empty for a movie and never nil, so the UI can iterate it blind.
	Seasons []QueueSeasonGroup `json:"seasons"`
}

// QueueGroups is the whole grouped view. Total and Active are Counts.Total and
// Counts.Active hoisted to the top level for the page's summary line.
type QueueGroups struct {
	Total  int              `json:"total"`
	Active int              `json:"active"`
	Counts QueueCounts      `json:"counts"`
	Shows  []QueueShowGroup `json:"shows"`
	Movies []QueueShowGroup `json:"movies"`
}

// ListQueueGroups summarises every queue row visible to the caller as one entry per
// movie and one per show, each show carrying its seasons. It is a single GROUP BY, so
// its cost tracks the number of distinct titles rather than the number of rows, and it
// makes ZERO TMDB calls: title and poster_url are already denormalised onto the queue
// rows themselves.
//
// PRIVACY — the one line in here that must not be got wrong. This reads the very same
// rows ListQueue does, and a queue row is a named person's viewing request: the list
// of titles someone asked for is exactly the sort of thing they did not publish. So
// the identical gate applies — ANONYMOUS (requester=="") sees nothing, a named user
// sees only their own rows, an admin sees everything — and the requester predicate
// goes INSIDE the aggregate. Filtering groups after the fact would be far too late:
// the counts would already be everyone's, and a count is enough to reveal that
// another user requested a title. A consequence worth knowing at the UI: a non-admin's
// "8 of 12 done" counts only their own requests for that show, exactly as their flat
// queue list only ever showed their own rows.
func (s *Store) ListQueueGroups(requester string, isAdmin bool) (*QueueGroups, error) {
	// Always a real object with real slices, even when the answer is "nothing" — the
	// handler marshals this straight through and the UI iterates both lists blind.
	groups := &QueueGroups{Shows: []QueueShowGroup{}, Movies: []QueueShowGroup{}}
	if !isAdmin && requester == "" {
		return groups, nil
	}
	q := `SELECT tmdb_id, media_type, season,
                 MAX(title), MAX(poster_url), MAX(created_at), COUNT(*),
                 SUM(status='pending'), SUM(status='processing'),
                 SUM(status='done'), SUM(status='failed'), SUM(status='cancelled')
          FROM queue`
	var args []any
	if !isAdmin {
		q += ` WHERE requested_by=?`
		args = append(args, requester)
	}
	// MAX(title) and MAX(poster_url) are not a heuristic. Both columns are DENORMALISED
	// copies of one TMDB lookup, written identically onto every row of a title, so any
	// row's value is the group's value and MAX simply picks one deterministically
	// without a correlated subquery. Where the copies do differ it is because an older
	// row predates the field being filled in, and MAX prefers a real string over '' —
	// which is the value we wanted anyway. (For a TV row, title holds the SHOW title,
	// not the episode title; every enqueue path writes it that way.)
	//
	// The GROUP BY term order matches idx_queue_group's leading columns, so SQLite walks
	// the index in group order and never sorts ROWS. The one temp b-tree that remains
	// sorts the finished GROUPS by recency — tens of them, not tens of thousands.
	q += ` GROUP BY tmdb_id, media_type, season
           ORDER BY MAX(created_at) DESC, tmdb_id, season`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type groupKey struct {
		mediaType string
		tmdbID    int
	}
	// The map holds an INDEX into the slice, not a pointer into it: append reallocates
	// the backing array and would leave every stored pointer writing into the old one.
	index := make(map[groupKey]int)
	for rows.Next() {
		var (
			tmdbID, season, total    int
			mediaType, title, poster string
			newestRaw                any
			c                        QueueCounts
		)
		if err := rows.Scan(&tmdbID, &mediaType, &season, &title, &poster, &newestRaw, &total,
			&c.Pending, &c.Processing, &c.Done, &c.Failed, &c.Cancelled); err != nil {
			return nil, err
		}
		c.Total, c.Active = total, c.Pending+c.Processing
		newest := sqliteTime(newestRaw)

		// Anything that is not a movie is treated as a show, so a media_type this code
		// has never heard of still surfaces in the tree rather than vanishing from it.
		dst := &groups.Shows
		if mediaType == "movie" {
			dst = &groups.Movies
		}
		key := groupKey{mediaType, tmdbID}
		i, ok := index[key]
		if !ok {
			*dst = append(*dst, QueueShowGroup{
				TMDBID: tmdbID, MediaType: mediaType, Title: title, PosterURL: poster,
				Seasons: []QueueSeasonGroup{},
			})
			i = len(*dst) - 1
			index[key] = i
		}
		g := &(*dst)[i]
		// A later season may carry the poster or title an earlier one was missing.
		if g.Title == "" {
			g.Title = title
		}
		if g.PosterURL == "" {
			g.PosterURL = poster
		}
		g.Counts.add(c)
		if newest.After(g.Newest) {
			g.Newest = newest
		}
		// A movie's season column is 0 on every row, so its one group already IS the
		// whole title and a nested season would just repeat it.
		if mediaType != "movie" {
			g.Seasons = append(g.Seasons, QueueSeasonGroup{Season: season, Counts: c, Newest: newest})
		}
		groups.Counts.add(c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Newest-first is right for the shows themselves — it is the order the flat queue
	// used — but wrong inside one: a show's seasons read 1, 2, 3.
	for i := range groups.Shows {
		seasons := groups.Shows[i].Seasons
		sort.Slice(seasons, func(a, b int) bool { return seasons[a].Season < seasons[b].Season })
	}
	groups.Total, groups.Active = groups.Counts.Total, groups.Counts.Active
	return groups, nil
}

// sqliteTime converts a timestamp read through an `any` into a time.Time. The grouped
// query has to scan MAX(created_at) that way rather than straight into a time.Time,
// because the driver types a result column from its DECLARED type: `created_at` is
// declared DATETIME and does arrive as a time.Time, but MAX(created_at) is an
// EXPRESSION, has no declared type, and arrives as the raw TEXT SQLite stores. A
// value that cannot be parsed comes back as the zero time, which sorts a group to the
// bottom of the tree but never drops it — the counts matter more than the ordering.
func sqliteTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		return parseSQLiteTimestamp(t)
	case []byte:
		return parseSQLiteTimestamp(string(t))
	}
	return time.Time{}
}

// sqliteTimeLayouts covers what actually lands in a DATETIME column here: CURRENT_TIMESTAMP
// writes "2006-01-02 15:04:05" in UTC, while a Go time.Time bound as a parameter is
// stored by the driver with an offset.
var sqliteTimeLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02 15:04:05.999999999-07:00",
	time.RFC3339Nano,
	"2006-01-02",
}

func parseSQLiteTimestamp(s string) time.Time {
	for _, layout := range sqliteTimeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t
		}
	}
	return time.Time{}
}

// CancelQueueItem cancels a pending row. Returns the number of rows affected so the
// handler can distinguish "cancelled" from "not yours / not pending / gone" instead of
// reporting success on an unchanged row.
func (s *Store) CancelQueueItem(id int64, requester string, isAdmin bool) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if isAdmin {
		res, err = s.db.Exec(
			`UPDATE queue SET status='cancelled', stage=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND status='pending'`,
			StageCancelled, id)
	} else {
		if requester == "" {
			return 0, nil
		}
		res, err = s.db.Exec(
			`UPDATE queue SET status='cancelled', stage=?, updated_at=CURRENT_TIMESTAMP
			 WHERE id=? AND status='pending' AND requested_by=?`,
			StageCancelled, id, requester)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteQueueItem removes a finished row. Returns rows affected (see CancelQueueItem).
func (s *Store) DeleteQueueItem(id int64, requester string, isAdmin bool) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if isAdmin {
		res, err = s.db.Exec(`DELETE FROM queue WHERE id=?`, id)
	} else {
		if requester == "" {
			return 0, nil
		}
		res, err = s.db.Exec(
			`DELETE FROM queue WHERE id=? AND requested_by=? AND status IN ('done','failed','cancelled')`,
			id, requester)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) QueuePendingCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM queue WHERE status IN ('pending','processing')`).Scan(&n)
	return n, err
}

// ActiveQueueItem returns the newest in-flight (pending|processing) queue row for an
// identity AND requester, or nil if none. Used to make a repeat request idempotent — we
// hand back the existing row instead of enqueuing a duplicate that would show up
// alongside the library entry. Scoped by requester to match ListQueue's own filter.
func (s *Store) ActiveQueueItem(tmdbID int, mediaType string, season, episode int, requester string) (*QueueItem, error) {
	return scanQueueItem(s.db.QueryRow(
		`SELECT `+queueCols+` FROM queue
         WHERE tmdb_id=? AND media_type=? AND season=? AND episode=? AND requested_by=?
           AND status IN ('pending','processing')
         ORDER BY created_at DESC LIMIT 1`,
		tmdbID, mediaType, season, episode, requester))
}

// ClearTerminalQueue deletes finished (done|failed|cancelled) queue rows for an identity.
// Called when a fresh request supersedes them, so the queue never shows a stale "Failed"
// or duplicate "Completed" entry for a title the library has since resolved. In-flight rows
// (pending|processing) are left untouched.
func (s *Store) ClearTerminalQueue(tmdbID int, mediaType string, season, episode int) error {
	_, err := s.db.Exec(
		`DELETE FROM queue
         WHERE tmdb_id=? AND media_type=? AND season=? AND episode=? AND status IN ('done','failed','cancelled')`,
		tmdbID, mediaType, season, episode)
	return err
}

// ── Subscriptions ─────────────────────────────────────────────────────────────

const subCols = `id,tmdb_id,season,title,poster_url,library_name,requested_by,is_airing,last_checked,created_at`

func scanSubscription(sc scanner) (*Subscription, error) {
	var s Subscription
	var airing int
	var lastChecked sql.NullTime
	err := sc.Scan(&s.ID, &s.TMDBID, &s.Season, &s.Title, &s.PosterURL, &s.LibraryName,
		&s.RequestedBy, &airing, &lastChecked, &s.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.IsAiring = airing == 1
	if lastChecked.Valid {
		s.LastChecked = &lastChecked.Time
	}
	return &s, nil
}

// UpsertSubscription creates or refreshes a subscription (idempotent per tmdb_id+season).
func (s *Store) UpsertSubscription(sub *Subscription) error {
	_, err := s.db.Exec(`
	INSERT INTO subscriptions (tmdb_id, season, title, poster_url, library_name, requested_by, is_airing)
	VALUES (?, ?, ?, ?, ?, ?, 1)
	ON CONFLICT(tmdb_id, season) DO UPDATE SET
		title=COALESCE(NULLIF(excluded.title,''), title),
		poster_url=COALESCE(NULLIF(excluded.poster_url,''), poster_url),
		library_name=COALESCE(NULLIF(excluded.library_name,''), library_name),
		is_airing=1`,
		sub.TMDBID, sub.Season, sub.Title, sub.PosterURL, sub.LibraryName, sub.RequestedBy)
	return err
}

func (s *Store) SubscriptionExists(tmdbID, season int) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM subscriptions WHERE tmdb_id=? AND season=?`, tmdbID, season).Scan(&n)
	return n > 0, err
}

// ListSubscriptions returns subscriptions visible to the caller. requester=="" means
// ANONYMOUS, which sees nothing — a subscription names who requested it.
func (s *Store) ListSubscriptions(requester string, isAdmin bool) ([]*Subscription, error) {
	if isAdmin {
		return s.querySubs(`SELECT ` + subCols + ` FROM subscriptions ORDER BY created_at DESC`)
	}
	if requester == "" {
		return nil, nil
	}
	return s.querySubs(`SELECT `+subCols+` FROM subscriptions WHERE requested_by=? ORDER BY created_at DESC`, requester)
}

// ListAiringSubscriptions returns subscriptions still flagged as airing (for the checker task).
func (s *Store) ListAiringSubscriptions() ([]*Subscription, error) {
	return s.querySubs(`SELECT ` + subCols + ` FROM subscriptions WHERE is_airing=1`)
}

func (s *Store) querySubs(q string, args ...any) ([]*Subscription, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []*Subscription
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

func (s *Store) MarkSubscriptionChecked(id int64, isAiring bool) error {
	_, err := s.db.Exec(`UPDATE subscriptions SET last_checked=?, is_airing=? WHERE id=?`,
		time.Now(), boolToInt(isAiring), id)
	return err
}

// DeleteSubscription removes a subscription the caller owns (or any, for an admin).
// Returns rows affected so the handler can report "not yours" rather than success.
func (s *Store) DeleteSubscription(id int64, requester string, isAdmin bool) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if isAdmin {
		res, err = s.db.Exec(`DELETE FROM subscriptions WHERE id=?`, id)
	} else {
		if requester == "" {
			return 0, nil
		}
		res, err = s.db.Exec(`DELETE FROM subscriptions WHERE id=? AND requested_by=?`, id, requester)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
