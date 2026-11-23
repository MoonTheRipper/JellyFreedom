package store

import (
	"database/sql"
	"errors"
	"strings"
)

// ── Cancelling a title's queue ────────────────────────────────────────────────
//
// A season request enqueues one row per episode, so "stop fetching this show" used to
// mean pressing Cancel on every row — and only on the PENDING ones, because the row
// the worker had already claimed could not be cancelled at all. Worse, removing the
// show from the library did not touch the queue: the worker kept resolving the rows
// still in it and wrote each finished episode straight back into the library. On the
// live box a series removed at 08:48 had S02E19 and S03E07–E16 rewritten into
// Jellyfin over the next fifteen minutes, which reads to the user as "the show I
// deleted came back".
//
// QueueScope is the unit both of those operations now speak: one title, optionally
// narrowed to one season or one episode.

// QueueScope selects the queue rows of one title. Season and Episode on the identity are
// only matched when the matching flag is set, because 0 is a real season (specials, and
// every movie row) and cannot double as "any".
type QueueScope struct {
	Identity   Identity
	SeasonSet  bool
	EpisodeSet bool
}

// ShowScope selects every queue row of a TMDB title — all seasons, all episodes.
func ShowScope(tmdbID int, mediaType string) QueueScope {
	return QueueScope{Identity: TMDBIdentity(tmdbID, mediaType, 0, 0)}
}

// SeasonScope selects every queue row of one TMDB season.
func SeasonScope(tmdbID, season int) QueueScope {
	return QueueScope{Identity: TMDBIdentity(tmdbID, "tv", season, 0), SeasonSet: true}
}

// EpisodeScope selects the queue rows of one TMDB episode.
func EpisodeScope(tmdbID, season, episode int) QueueScope {
	return QueueScope{Identity: TMDBIdentity(tmdbID, "tv", season, episode), SeasonSet: true, EpisodeSet: true}
}

// where renders the scope as a predicate. An EpisodeSet without SeasonSet is treated as
// both: an episode number means nothing without its season.
func (sc QueueScope) where() (string, []any) {
	id := sc.Identity.canonical()
	conds := []string{`provider=?`, `provider_id=?`, `media_type=?`}
	args := []any{id.Provider, id.ProviderID, id.MediaType}
	if sc.SeasonSet || sc.EpisodeSet {
		conds = append(conds, `season=?`)
		args = append(args, id.Season)
	}
	if sc.EpisodeSet {
		conds = append(conds, `episode=?`)
		args = append(args, id.Episode)
	}
	return strings.Join(conds, ` AND `), args
}

// CancelQueueScope cancels every in-flight (pending OR processing) row in the scope that
// the viewer may cancel, and returns the ids it cancelled so the caller can stop the
// worker if it is busy with one of them.
//
// Processing rows are included on purpose. Leaving them out is what let a cancelled
// show finish its current episode and write it into the library anyway; the row is
// marked here, and the worker re-reads the status before it commits anything (see
// UpdateQueue and QueueStatus).
//
// Ownership matches CancelQueueItem: an admin cancels any row, a signed-in user only
// their own rows in libraries they can see, and an anonymous viewer nothing.
func (s *Store) CancelQueueScope(v Viewer, sc QueueScope) ([]int64, error) {
	if !v.IsAdmin && v.Username == "" {
		return nil, nil
	}
	pred, args := sc.where()
	q := `UPDATE queue SET status='cancelled', stage=?, progress='', updated_at=CURRENT_TIMESTAMP
	      WHERE status IN ('pending','processing') AND ` + pred
	all := append([]any{StageCancelled}, args...)
	if !v.IsAdmin {
		scope, scopeArgs := v.libraryScope()
		q += ` AND requested_by=? AND ` + scope
		all = append(all, v.Username)
		all = append(all, scopeArgs...)
	}
	q += ` RETURNING id`
	rows, err := s.db.Query(q, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// QueueStatus returns a row's current status, or "" when the row no longer exists. The
// worker calls it immediately before committing a resolved release, under the same lock
// that cancellation and library removal take, so a row cancelled while it was being
// resolved is never written into the library.
func (s *Store) QueueStatus(id int64) (string, error) {
	var st string
	err := s.db.QueryRow(`SELECT status FROM queue WHERE id=?`, id).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return st, err
}

// CancelledEpisodes returns the TMDB episodes of one season whose newest queue row is
// 'cancelled'. The subscription check skips them: a cancelled row does not count as
// "active", so without this an airing show's cancelled episodes were re-enqueued on the
// next run — the same resurrection, six hours later.
//
// A fresh request clears the cancelled row (ClearTerminalQueue), which is how a user
// takes the episode back.
func (s *Store) CancelledEpisodes(tmdbID, season int) (map[int]bool, error) {
	id := TMDBIdentity(tmdbID, "tv", season, 0)
	rows, err := s.db.Query(
		`SELECT episode, status FROM queue
		 WHERE provider=? AND provider_id=? AND media_type='tv' AND season=?
		 ORDER BY created_at, id`, id.Provider, id.ProviderID, season)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Ordered oldest first, so the last status written for an episode is its newest.
	latest := map[int]string{}
	for rows.Next() {
		var ep int
		var st string
		if err := rows.Scan(&ep, &st); err != nil {
			return nil, err
		}
		latest[ep] = st
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for ep, st := range latest {
		if st == "cancelled" {
			out[ep] = true
		}
	}
	return out, nil
}

// DeleteSubscriptionsFor removes the caller's subscriptions to a show (season < 0: every
// season) or to one season of it. Removing a show from the library calls this, because a
// subscription that outlives the library entry re-enqueues the show on its next run.
//
// Ownership matches DeleteSubscription.
func (s *Store) DeleteSubscriptionsFor(v Viewer, tmdbID, season int) (int64, error) {
	if !v.IsAdmin && v.Username == "" {
		return 0, nil
	}
	q := `DELETE FROM subscriptions WHERE tmdb_id=?`
	args := []any{tmdbID}
	if season >= 0 {
		q += ` AND season=?`
		args = append(args, season)
	}
	if !v.IsAdmin {
		scope, scopeArgs := v.libraryScope()
		q += ` AND requested_by=? AND ` + scope
		args = append(args, v.Username)
		args = append(args, scopeArgs...)
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
