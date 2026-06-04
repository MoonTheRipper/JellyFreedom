package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// sxxeyy extracts season/episode numbers from a string like "Show S01E08".
var sxxeyy = regexp.MustCompile(`(?i)s(\d{1,2})[\s._-]*e(\d{1,3})`)

type Item struct {
	ID           int64
	TMDBID       int
	MediaType    string // "movie" | "tv"
	Title        string
	Year         string
	InfoHash     string
	FileIndex    int
	StrmPath     string
	LibraryName  string
	Status       string // "requested" | "ready" | "stale" (expired, revivable)
	Seeders      int
	Updated      time.Time
	RequestedBy  string
	IsPrivate    bool
	PosterURL    string
	Magnet       string     // the magnet used — kept so a dropped torrent can be re-added
	ReleaseTitle string     // the chosen release name, e.g. "Inception.2010.1080p.BluRay.x264"
	StaleSince   *time.Time // when it went stale; nil while ready
	Season       int        // 0 for movies
	Episode      int        // 0 for movies
}

// itemCols is the canonical SELECT column list for Item rows.
const itemCols = `id,tmdb_id,media_type,title,year,info_hash,file_index,strm_path,library_name,status,seeders,updated,requested_by,is_private,poster_url,magnet,release_title,stale_since,season,episode`

type QueueItem struct {
	ID             int64
	TMDBID         int
	MediaType      string
	Title          string
	Year           string
	PosterURL      string
	Season         int
	Episode        int
	LibraryName    string
	RequestedBy    string
	MagnetOverride string
	Status         string // pending|processing|done|failed|cancelled
	Progress       string
	ErrorMsg       string
	InfoHash       string
	StrmPath       string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type User struct {
	ID             int64
	Username       string
	PasswordHash   string
	JellyfinUserID string
	AuthSource     string // "local" | "jellyfin"
	IsAdmin        bool
	CreatedAt      time.Time
}

// Subscription auto-fetches newly-aired episodes of a TV season for airing shows.
type Subscription struct {
	ID          int64
	TMDBID      int
	Season      int
	Title       string
	PosterURL   string
	LibraryName string
	RequestedBy string
	IsAiring    bool
	LastChecked *time.Time
	CreatedAt   time.Time
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

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
	// Additive column migrations — ignore errors if column already exists.
	s.db.Exec(`ALTER TABLE items    ADD COLUMN library_name  TEXT    NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE sessions ADD COLUMN user_id       INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE items    ADD COLUMN requested_by  TEXT    NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE items    ADD COLUMN is_private     INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE items ADD COLUMN poster_url TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE items ADD COLUMN magnet        TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE items ADD COLUMN release_title TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE items ADD COLUMN stale_since   DATETIME`)
	s.db.Exec(`ALTER TABLE items ADD COLUMN season        INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE items ADD COLUMN episode       INTEGER NOT NULL DEFAULT 0`)
	// Reset interrupted queue items on restart.
	s.db.Exec(`UPDATE queue SET status='pending', progress='' WHERE status='processing'`)
	// Backfill season/episode for TV items created before those columns existed.
	s.backfillEpisodeNumbers()
	// Migrate old single-password setting into the users table.
	return s.migrateAdminUser()
}

// migrateAdminUser promotes the old dashboard_password_hash setting into a proper
// admin User row so the new multi-user system is backward-compatible.
func (s *Store) migrateAdminUser() error {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n > 0 {
		return nil // users table already populated
	}
	hash, _ := s.GetSetting("dashboard_password_hash")
	if hash == "" {
		return nil // fresh install — setup page will create the first user
	}
	_, err := s.db.Exec(
		`INSERT INTO users (username, password_hash, auth_source, is_admin) VALUES ('admin', ?, 'local', 1)`,
		hash,
	)
	if err != nil {
		return err
	}
	s.db.Exec(`DELETE FROM settings WHERE key='dashboard_password_hash'`)
	return nil
}

func (s *Store) migrateItemsConstraint() error {
	// SQLite UNIQUE table constraints create autoindexes with sql=NULL in sqlite_master,
	// so we check the table DDL directly instead of looking for an explicit index entry.
	var ddl string
	s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='items'`).Scan(&ddl)
	if strings.Contains(ddl, "UNIQUE(strm_path)") || strings.Contains(ddl, "UNIQUE (strm_path)") {
		return nil
	}
	_, err := s.db.Exec(`
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

// ValidSession is kept for compatibility but delegates to GetSessionUser.
func (s *Store) ValidSession(token string) bool {
	_, ok := s.GetSessionUser(token)
	return ok
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token=?`, token)
	return err
}

func (s *Store) PurgeSessions() {
	s.db.Exec(`DELETE FROM sessions WHERE expires < ?`, time.Now())
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
func (s *Store) backfillEpisodeNumbers() {
	rows, err := s.db.Query(`SELECT id, title FROM items WHERE media_type='tv' AND season=0 AND episode=0`)
	if err != nil {
		return
	}
	type upd struct {
		id            int64
		season, epNum int
	}
	var updates []upd
	for rows.Next() {
		var id int64
		var title string
		if rows.Scan(&id, &title) != nil {
			continue
		}
		if m := sxxeyy.FindStringSubmatch(title); m != nil {
			sn, _ := strconv.Atoi(m[1])
			en, _ := strconv.Atoi(m[2])
			if sn > 0 && en > 0 {
				updates = append(updates, upd{id, sn, en})
			}
		}
	}
	rows.Close()
	for _, u := range updates {
		s.db.Exec(`UPDATE items SET season=?, episode=? WHERE id=?`, u.season, u.epNum, u.id)
	}
}

// EpisodeActive reports whether a TV episode is already ready in the library or
// already pending/processing in the queue — used to skip redundant enqueues.
func (s *Store) EpisodeActive(tmdbID, season, episode int) bool {
	var n int
	s.db.QueryRow(
		`SELECT COUNT(*) FROM items WHERE tmdb_id=? AND season=? AND episode=? AND status='ready'`,
		tmdbID, season, episode).Scan(&n)
	if n > 0 {
		return true
	}
	s.db.QueryRow(
		`SELECT COUNT(*) FROM queue WHERE tmdb_id=? AND season=? AND episode=? AND status IN ('pending','processing')`,
		tmdbID, season, episode).Scan(&n)
	return n > 0
}

// SetPrivate toggles the is_private flag on an item by strm path.
func (s *Store) SetPrivate(strmPath string, private bool) error {
	_, err := s.db.Exec(`UPDATE items SET is_private=? WHERE strm_path=?`, boolToInt(private), strmPath)
	return err
}

func (s *Store) GetStatusByTMDBIDs(ids []int) (map[int]Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	q := fmt.Sprintf(
		`SELECT `+itemCols+` FROM items WHERE tmdb_id IN (%s)
		 ORDER BY CASE status WHEN 'ready' THEN 0 WHEN 'stale' THEN 1 WHEN 'error' THEN 2 ELSE 3 END`,
		strings.Join(placeholders, ","),
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

func (s *Store) CountReadyByHash(hash string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM items WHERE info_hash=? AND status='ready'`, hash).Scan(&n)
	return n, err
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

func (s *Store) GetByTMDB(tmdbID int, mediaType string) (*Item, error) {
	row := s.db.QueryRow(`SELECT `+itemCols+` FROM items WHERE tmdb_id=? AND media_type=?`, tmdbID, mediaType)
	return scanItem(row)
}

func (s *Store) GetByStrmPath(path string) (*Item, error) {
	row := s.db.QueryRow(`SELECT `+itemCols+` FROM items WHERE strm_path=?`, path)
	return scanItem(row)
}

// ListReady returns all managed items (ready + stale) — background-job view.
// The health check uses this to both verify ready items and attempt to revive
// stale (expired) ones.
func (s *Store) ListReady() ([]*Item, error) {
	return s.ListVisible("", true)
}

// ListVisible returns library items visible to the given viewer. Includes both
// 'ready' (live) and 'stale' (expired but revivable) items — stale items are the
// user's request history and surface in the UI with an "Expired" badge.
// Admins (or empty viewerUsername) see all. Others see their own + public items.
func (s *Store) ListVisible(viewerUsername string, isAdmin bool) ([]*Item, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if isAdmin || viewerUsername == "" {
		rows, err = s.db.Query(`SELECT ` + itemCols + ` FROM items WHERE status IN ('ready','stale')`)
	} else {
		rows, err = s.db.Query(
			`SELECT `+itemCols+` FROM items WHERE status IN ('ready','stale') AND (is_private=0 OR requested_by=?)`,
			viewerUsername,
		)
	}
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

func (s *Store) Enqueue(item *QueueItem) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO queue (tmdb_id,media_type,title,year,poster_url,season,episode,library_name,requested_by,magnet_override)
         VALUES (?,?,?,?,?,?,?,?,?,?)`,
		item.TMDBID, item.MediaType, item.Title, item.Year, item.PosterURL,
		item.Season, item.Episode, item.LibraryName, item.RequestedBy, item.MagnetOverride,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// NextPending atomically claims the next pending queue item by marking it processing.
func (s *Store) NextPending() (*QueueItem, error) {
	var item QueueItem
	err := s.db.QueryRow(
		`SELECT id,tmdb_id,media_type,title,year,poster_url,season,episode,library_name,
                requested_by,magnet_override,status,progress,error_msg,info_hash,strm_path,created_at,updated_at
         FROM queue WHERE status='pending' ORDER BY created_at LIMIT 1`).Scan(
		&item.ID, &item.TMDBID, &item.MediaType, &item.Title, &item.Year, &item.PosterURL,
		&item.Season, &item.Episode, &item.LibraryName, &item.RequestedBy, &item.MagnetOverride,
		&item.Status, &item.Progress, &item.ErrorMsg, &item.InfoHash, &item.StrmPath,
		&item.CreatedAt, &item.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_, err = s.db.Exec(`UPDATE queue SET status='processing', updated_at=CURRENT_TIMESTAMP WHERE id=?`, item.ID)
	item.Status = "processing"
	return &item, err
}

func (s *Store) UpdateQueue(item *QueueItem) error {
	_, err := s.db.Exec(
		`UPDATE queue SET status=?,progress=?,error_msg=?,info_hash=?,strm_path=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		item.Status, item.Progress, item.ErrorMsg, item.InfoHash, item.StrmPath, item.ID,
	)
	return err
}

func (s *Store) GetQueueItem(id int64) (*QueueItem, error) {
	var item QueueItem
	err := s.db.QueryRow(
		`SELECT id,tmdb_id,media_type,title,year,poster_url,season,episode,library_name,
                requested_by,magnet_override,status,progress,error_msg,info_hash,strm_path,created_at,updated_at
         FROM queue WHERE id=?`, id).Scan(
		&item.ID, &item.TMDBID, &item.MediaType, &item.Title, &item.Year, &item.PosterURL,
		&item.Season, &item.Episode, &item.LibraryName, &item.RequestedBy, &item.MagnetOverride,
		&item.Status, &item.Progress, &item.ErrorMsg, &item.InfoHash, &item.StrmPath,
		&item.CreatedAt, &item.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &item, err
}

func (s *Store) ListQueue(requester string, isAdmin bool) ([]*QueueItem, error) {
	var rows *sql.Rows
	var err error
	if isAdmin || requester == "" {
		rows, err = s.db.Query(
			`SELECT id,tmdb_id,media_type,title,year,poster_url,season,episode,library_name,
                    requested_by,magnet_override,status,progress,error_msg,info_hash,strm_path,created_at,updated_at
             FROM queue ORDER BY created_at DESC LIMIT 100`)
	} else {
		rows, err = s.db.Query(
			`SELECT id,tmdb_id,media_type,title,year,poster_url,season,episode,library_name,
                    requested_by,magnet_override,status,progress,error_msg,info_hash,strm_path,created_at,updated_at
             FROM queue WHERE requested_by=? ORDER BY created_at DESC LIMIT 100`, requester)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []*QueueItem
	for rows.Next() {
		var item QueueItem
		if err := rows.Scan(
			&item.ID, &item.TMDBID, &item.MediaType, &item.Title, &item.Year, &item.PosterURL,
			&item.Season, &item.Episode, &item.LibraryName, &item.RequestedBy, &item.MagnetOverride,
			&item.Status, &item.Progress, &item.ErrorMsg, &item.InfoHash, &item.StrmPath,
			&item.CreatedAt, &item.UpdatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, &item)
	}
	return items, rows.Err()
}

func (s *Store) CancelQueueItem(id int64, requester string, isAdmin bool) error {
	if isAdmin || requester == "" {
		_, err := s.db.Exec(`UPDATE queue SET status='cancelled', updated_at=CURRENT_TIMESTAMP WHERE id=? AND status='pending'`, id)
		return err
	}
	_, err := s.db.Exec(`UPDATE queue SET status='cancelled', updated_at=CURRENT_TIMESTAMP WHERE id=? AND status='pending' AND requested_by=?`, id, requester)
	return err
}

func (s *Store) DeleteQueueItem(id int64, requester string, isAdmin bool) error {
	if isAdmin {
		_, err := s.db.Exec(`DELETE FROM queue WHERE id=?`, id)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM queue WHERE id=? AND requested_by=? AND status IN ('done','failed','cancelled')`, id, requester)
	return err
}

func (s *Store) QueuePendingCount() int {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM queue WHERE status IN ('pending','processing')`).Scan(&n)
	return n
}

// ActiveQueueItem returns the newest in-flight (pending|processing) queue row for an identity,
// or nil if none. Used to make a repeat request idempotent — we hand back the existing row
// instead of enqueuing a duplicate that would show up alongside the library entry.
func (s *Store) ActiveQueueItem(tmdbID int, mediaType string, season, episode int) (*QueueItem, error) {
	var item QueueItem
	err := s.db.QueryRow(
		`SELECT id,tmdb_id,media_type,title,year,poster_url,season,episode,library_name,
                requested_by,magnet_override,status,progress,error_msg,info_hash,strm_path,created_at,updated_at
         FROM queue
         WHERE tmdb_id=? AND media_type=? AND season=? AND episode=? AND status IN ('pending','processing')
         ORDER BY created_at DESC LIMIT 1`,
		tmdbID, mediaType, season, episode).Scan(
		&item.ID, &item.TMDBID, &item.MediaType, &item.Title, &item.Year, &item.PosterURL,
		&item.Season, &item.Episode, &item.LibraryName, &item.RequestedBy, &item.MagnetOverride,
		&item.Status, &item.Progress, &item.ErrorMsg, &item.InfoHash, &item.StrmPath,
		&item.CreatedAt, &item.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &item, err
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

func (s *Store) SubscriptionExists(tmdbID, season int) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM subscriptions WHERE tmdb_id=? AND season=?`, tmdbID, season).Scan(&n)
	return n > 0
}

func (s *Store) ListSubscriptions(requester string, isAdmin bool) ([]*Subscription, error) {
	var rows *sql.Rows
	var err error
	if isAdmin || requester == "" {
		rows, err = s.db.Query(`SELECT ` + subCols + ` FROM subscriptions ORDER BY created_at DESC`)
	} else {
		rows, err = s.db.Query(`SELECT `+subCols+` FROM subscriptions WHERE requested_by=? ORDER BY created_at DESC`, requester)
	}
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

// ListAiringSubscriptions returns subscriptions still flagged as airing (for the checker task).
func (s *Store) ListAiringSubscriptions() ([]*Subscription, error) {
	rows, err := s.db.Query(`SELECT ` + subCols + ` FROM subscriptions WHERE is_airing=1`)
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

func (s *Store) DeleteSubscription(id int64, requester string, isAdmin bool) error {
	if isAdmin || requester == "" {
		_, err := s.db.Exec(`DELETE FROM subscriptions WHERE id=?`, id)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM subscriptions WHERE id=? AND requested_by=?`, id, requester)
	return err
}
