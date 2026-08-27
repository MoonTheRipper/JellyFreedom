package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ── Web sources ───────────────────────────────────────────────────────────────
//
// A web source is a video page somebody pasted into the dashboard. The library row it
// produces lives in `items` like every other one, under provider "web"; this table holds
// the one fact `items` has no column for — WHICH PAGE it came from — plus the extracted
// description and the outcome of the last resolve.
//
// It is a separate table rather than a column on `items` for two reasons. `items.magnet`
// is the obvious place and is the wrong one: it is validated as a magnet by everything
// that touches it, and a URL living there would be a bug waiting for the first piece of
// code that trusts the column name. And a nullable page_url column would be empty on
// every one of the ~1,000 torrent rows, which is a column that means nothing for 99% of
// the table.
//
// WHAT IS NOT STORED HERE, EVER
//
// The MEDIA url. Not in this table, not in the .strm file, not anywhere on disk. Tube
// sites sign their CDN links with a short expiry, so a stored media URL is a library
// entry that plays today and 403s on Friday — the same rot that made a frozen info hash
// unusable, with the same fix. What is stored is the page; the media URL is resolved at
// play time and lives only in memory. See internal/websource.

// WebSource is one pasted video page.
type WebSource struct {
	// ID is the identity the rest of the system uses: it is the {id} in
	// /play/p/web/{id}, the provider_id on the items row, and the thing a capability
	// token signs. It is DERIVED from the page URL (see WebSourceID), not random, so
	// pasting the same link twice updates one entry instead of creating a second one
	// pointing at the same video.
	ID      string `json:"id"`
	PageURL string `json:"page_url"`

	// Everything the extractor said about the video, as of the last successful probe.
	Title     string `json:"title"`
	Uploader  string `json:"uploader"`
	Extractor string `json:"extractor"`
	Duration  int    `json:"duration_seconds"`
	Thumbnail string `json:"thumbnail_url"`

	AddedBy string    `json:"added_by"`
	AddedAt time.Time `json:"added_at"`

	// LastOK and LastError are the health of the link, and they are the whole
	// troubleshooting story for this feature. A web source dies in ways a torrent does
	// not — the uploader deletes it, the site changes its player, yt-dlp's extractor
	// breaks — and all three look identical from Jellyfin ("it won't play"). Recording
	// which one happened, and when it last worked, is the difference between a
	// diagnosable failure and a mystery.
	LastOK    *time.Time `json:"last_ok"`
	LastError string     `json:"last_error"`
}

// ErrWebSourceNotFound is returned by the update helpers for an id with no row. The
// getters return (nil, nil) instead, matching every other getter in this package.
var ErrWebSourceNotFound = errors.New("no such web source")

// WebSourceID derives the stable identity of a page URL.
//
// Three properties, all required:
//
//   - Deterministic, so re-pasting a link finds the existing entry rather than making a
//     twin that Jellyfin would show as a duplicate.
//   - Opaque, so the identity carries no part of the URL. This value appears in the
//     .strm file, in Jellyfin's database, in the orchestrator's logs and in any HTTP
//     access log along the way; a URL-derived-but-readable id would put the name of the
//     site into all of them.
//   - Drawn from [A-Za-z0-9_-], because library.ValidProviderID accepts exactly that set
//     — the id has to survive being a path segment AND being a field in the ':'-joined
//     string the play token signs.
//
// 16 bytes of SHA-256, base64url, is 22 characters and 128 bits. Collisions are not a
// practical concern, and the input is not secret, so the truncation is about length in a
// path rather than about strength.
func WebSourceID(pageURL string) string {
	sum := sha256.Sum256([]byte(canonicalPageURL(pageURL)))
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// canonicalPageURL normalises just enough that the same link pasted twice hashes the
// same way, and no more.
//
// Deliberately conservative: it lowercases the scheme and host and drops a trailing
// slash, and it does NOT touch the query string. Stripping query parameters would be an
// improvement for one site and a catastrophe for the next, because a tube site's video
// id very often IS a query parameter — "?v=abc" and "?v=def" are different videos, and a
// canonicaliser clever enough to drop tracking parameters would sooner or later drop
// that one and collapse an entire site into a single entry.
func canonicalPageURL(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "://"); i > 0 {
		s = strings.ToLower(s[:i]) + s[i:]
	}
	// Lowercase the authority only — everything from "://" to the first "/", "?" or "#".
	if i := strings.Index(s, "://"); i > 0 {
		rest := s[i+3:]
		end := len(rest)
		for j, r := range rest {
			if r == '/' || r == '?' || r == '#' {
				end = j
				break
			}
		}
		s = s[:i+3] + strings.ToLower(rest[:end]) + rest[end:]
	}
	// A trailing slash on a path with nothing after it is the same page.
	if !strings.ContainsAny(s, "?#") {
		s = strings.TrimRight(s, "/")
	}
	return s
}

// webSourceCols is the canonical column list, so the SELECTs and scanWebSource cannot
// drift apart.
const webSourceCols = `id, page_url, title, uploader, extractor, duration_s, thumbnail,
	added_by, added_at, last_ok_at, last_error`

// UpsertWebSource inserts or refreshes one entry, keyed on its id.
//
// added_by and added_at are preserved on update: re-probing an entry must not rewrite
// who added it or when, and a caller refreshing metadata has no reason to be carrying
// either. last_error is CLEARED on every upsert, because an upsert only happens after a
// successful extraction — the entry demonstrably works right now.
func (s *Store) UpsertWebSource(ws *WebSource) error {
	if ws.ID == "" {
		return errors.New("web source has no id")
	}
	if ws.PageURL == "" {
		return errors.New("web source has no page URL")
	}
	if ws.AddedAt.IsZero() {
		ws.AddedAt = time.Now()
	}
	_, err := s.db.Exec(`
	INSERT INTO web_sources (id, page_url, title, uploader, extractor, duration_s, thumbnail,
	                         added_by, added_at, last_ok_at, last_error)
	VALUES (?,?,?,?,?,?,?,?,?,?,'')
	ON CONFLICT(id) DO UPDATE SET
		page_url=excluded.page_url,
		title=excluded.title, uploader=excluded.uploader, extractor=excluded.extractor,
		duration_s=excluded.duration_s, thumbnail=excluded.thumbnail,
		last_ok_at=excluded.last_ok_at,
		last_error=''`,
		ws.ID, ws.PageURL, ws.Title, ws.Uploader, ws.Extractor, ws.Duration, ws.Thumbnail,
		ws.AddedBy, ws.AddedAt, ws.LastOK)
	return err
}

// GetWebSource returns one entry, or (nil, nil) when there is no such id.
func (s *Store) GetWebSource(id string) (*WebSource, error) {
	row := s.db.QueryRow(`SELECT `+webSourceCols+` FROM web_sources WHERE id=?`, id)
	return scanWebSource(row)
}

// WebSourceByPageURL finds an entry by the page it came from. Used to answer "you have
// already added this" before an add rather than after it.
func (s *Store) WebSourceByPageURL(pageURL string) (*WebSource, error) {
	return s.GetWebSource(WebSourceID(pageURL))
}

// ListWebSources returns every entry, newest first.
func (s *Store) ListWebSources() ([]*WebSource, error) {
	rows, err := s.db.Query(`SELECT ` + webSourceCols + ` FROM web_sources ORDER BY added_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WebSource
	for rows.Next() {
		ws, err := scanWebSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// DeleteWebSource removes one entry. It does NOT touch the items row or the .strm file:
// those are owned by the caller that created them, and deleting a library entry is a
// larger operation than forgetting where it came from.
func (s *Store) DeleteWebSource(id string) error {
	_, err := s.db.Exec(`DELETE FROM web_sources WHERE id=?`, id)
	return err
}

// MarkWebSourceOK records a successful resolve and clears any previous failure.
func (s *Store) MarkWebSourceOK(id string) error {
	res, err := s.db.Exec(`UPDATE web_sources SET last_ok_at=?, last_error='' WHERE id=?`, time.Now(), id)
	return affectedOne(res, err)
}

// MarkWebSourceFailed records why a resolve failed, leaving last_ok_at alone so "it
// worked at 09:00 and has failed since" stays readable.
//
// The message is truncated rather than rejected: it comes from an extractor, it is
// diagnostic text and not data, and losing the tail of a long one is better than losing
// the record that anything went wrong.
func (s *Store) MarkWebSourceFailed(id, reason string) error {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	res, err := s.db.Exec(`UPDATE web_sources SET last_error=? WHERE id=?`, reason, id)
	return affectedOne(res, err)
}

func affectedOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w", ErrWebSourceNotFound)
	}
	return nil
}

func scanWebSource(sc scanner) (*WebSource, error) {
	var ws WebSource
	var lastOK sql.NullTime
	err := sc.Scan(&ws.ID, &ws.PageURL, &ws.Title, &ws.Uploader, &ws.Extractor,
		&ws.Duration, &ws.Thumbnail, &ws.AddedBy, &ws.AddedAt, &lastOK, &ws.LastError)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastOK.Valid {
		ws.LastOK = &lastOK.Time
	}
	return &ws, nil
}
