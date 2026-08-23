package store

import (
	"testing"
	"time"
)

func enqueue(t *testing.T, s *Store, owner string, tmdb, season, ep int) int64 {
	t.Helper()
	id, err := s.Enqueue(&QueueItem{
		TMDBID: tmdb, MediaType: "tv", Title: "Show", Season: season, Episode: ep,
		RequestedBy: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEnqueueSetsInitialStage(t *testing.T) {
	s := newTestStore(t)
	id := enqueue(t, s, "alice", 1, 1, 1)
	item, err := s.GetQueueItem(id)
	if err != nil || item == nil {
		t.Fatalf("GetQueueItem: %v %v", item, err)
	}
	if item.Status != "pending" {
		t.Errorf("status = %q, want pending", item.Status)
	}
	if item.Stage != StageQueued {
		t.Errorf("stage = %q, want %q", item.Stage, StageQueued)
	}
}

func TestNextPendingClaimsAndAdvancesStage(t *testing.T) {
	s := newTestStore(t)
	enqueue(t, s, "alice", 1, 1, 1)

	claimed, err := s.NextPending()
	if err != nil || claimed == nil {
		t.Fatalf("NextPending: %v %v", claimed, err)
	}
	if claimed.Status != "processing" {
		t.Errorf("claimed status = %q, want processing", claimed.Status)
	}
	if claimed.Stage != StageIndexing {
		t.Errorf("claimed stage = %q, want %q", claimed.Stage, StageIndexing)
	}

	// The claim must be exclusive: a second call sees nothing pending.
	again, err := s.NextPending()
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Fatalf("NextPending returned an already-claimed item: %+v", again)
	}
}

func TestUpdateQueueRoundTrip(t *testing.T) {
	s := newTestStore(t)
	id := enqueue(t, s, "alice", 1, 1, 1)
	item, _ := s.GetQueueItem(id)

	item.Status = "failed"
	item.Stage = StageFailed
	item.Progress = "gave up"
	item.ErrorMsg = "no release"
	item.Diagnosis = `{"reason":"no_release"}`
	item.InfoHash = "abc"
	item.StrmPath = "/x.strm"
	if err := s.UpdateQueue(item); err != nil {
		t.Fatal(err)
	}

	got, _ := s.GetQueueItem(id)
	if got.Status != "failed" || got.Stage != StageFailed || got.ErrorMsg != "no release" {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if got.Diagnosis != `{"reason":"no_release"}` {
		t.Fatalf("diagnosis = %q, want it persisted", got.Diagnosis)
	}
}

// TestActiveQueueItemIdempotency: a repeat request must find the in-flight row rather
// than enqueue a duplicate that shows up alongside the library entry.
func TestActiveQueueItemIdempotency(t *testing.T) {
	s := newTestStore(t)
	id := enqueue(t, s, "alice", 10, 2, 3)

	active, err := s.ActiveQueueItem(10, "tv", 2, 3, "alice")
	if err != nil || active == nil {
		t.Fatalf("ActiveQueueItem: %v %v", active, err)
	}
	if active.ID != id {
		t.Fatalf("found id %d, want %d", active.ID, id)
	}

	// A different identity must NOT match.
	other, err := s.ActiveQueueItem(10, "tv", 2, 4, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if other != nil {
		t.Fatalf("a different episode matched: %+v", other)
	}

	// Once terminal, it is no longer "active".
	active.Status = "done"
	active.Stage = StageDone
	if err := s.UpdateQueue(active); err != nil {
		t.Fatal(err)
	}
	done, err := s.ActiveQueueItem(10, "tv", 2, 3, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if done != nil {
		t.Fatalf("a done row still reported as active: %+v", done)
	}
}

// TestClearTerminalQueueLeavesInFlightAlone is the important half: clearing history must
// never delete a row that is still being worked on.
func TestClearTerminalQueueLeavesInFlightAlone(t *testing.T) {
	s := newTestStore(t)

	doneID := enqueue(t, s, "alice", 20, 1, 1)
	d, _ := s.GetQueueItem(doneID)
	d.Status = "done"
	d.Stage = StageDone
	if err := s.UpdateQueue(d); err != nil {
		t.Fatal(err)
	}

	failedID := enqueue(t, s, "alice", 20, 1, 1)
	f, _ := s.GetQueueItem(failedID)
	f.Status = "failed"
	if err := s.UpdateQueue(f); err != nil {
		t.Fatal(err)
	}

	inFlightID := enqueue(t, s, "alice", 20, 1, 1)
	p, _ := s.GetQueueItem(inFlightID)
	p.Status = "processing"
	if err := s.UpdateQueue(p); err != nil {
		t.Fatal(err)
	}

	// bob's row, not alice's: idx_queue_active_identity permits only ONE in-flight row
	// per (identity, requester), so a second alice row here would silently collapse into
	// inFlightID and this case would re-assert the processing row instead of a pending
	// one. ClearTerminalQueue is scoped by identity alone, so bob's row is still in range.
	pendingID := enqueue(t, s, "bob", 20, 1, 1)
	if pendingID == inFlightID {
		t.Fatal("pending row collapsed into the in-flight row; this case proves nothing")
	}

	if err := s.ClearTerminalQueue(20, "tv", 1, 1); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		id       int64
		name     string
		wantGone bool
	}{
		{doneID, "done", true},
		{failedID, "failed", true},
		{inFlightID, "processing", false},
		{pendingID, "pending", false},
	} {
		got, err := s.GetQueueItem(tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if (got == nil) != tc.wantGone {
			t.Errorf("%s row: gone=%v, want gone=%v", tc.name, got == nil, tc.wantGone)
		}
	}
}

// TestQueueOwnership: cancel and delete must respect ownership AND report how many rows
// actually changed, so the handler can tell "cancelled" from "not yours".
func TestQueueOwnership(t *testing.T) {
	s := newTestStore(t)

	t.Run("another user cannot cancel", func(t *testing.T) {
		id := enqueue(t, s, "alice", 30, 1, 1)
		n, err := s.CancelQueueItem(id, "bob", false)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("bob cancelled alice's row (%d affected)", n)
		}
	})

	t.Run("anonymous cannot cancel", func(t *testing.T) {
		id := enqueue(t, s, "alice", 31, 1, 1)
		n, err := s.CancelQueueItem(id, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("an anonymous caller cancelled a row (%d affected)", n)
		}
	})

	t.Run("owner can cancel", func(t *testing.T) {
		id := enqueue(t, s, "alice", 32, 1, 1)
		n, err := s.CancelQueueItem(id, "alice", false)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("owner cancel affected %d rows, want 1", n)
		}
		got, _ := s.GetQueueItem(id)
		if got.Status != "cancelled" || got.Stage != StageCancelled {
			t.Fatalf("row not cancelled: %+v", got)
		}
	})

	t.Run("admin can cancel anyone's", func(t *testing.T) {
		id := enqueue(t, s, "alice", 33, 1, 1)
		n, err := s.CancelQueueItem(id, "root", true)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("admin cancel affected %d rows, want 1", n)
		}
	})

	t.Run("cancelling a non-pending row affects nothing", func(t *testing.T) {
		id := enqueue(t, s, "alice", 34, 1, 1)
		it, _ := s.GetQueueItem(id)
		it.Status = "processing"
		if err := s.UpdateQueue(it); err != nil {
			t.Fatal(err)
		}
		n, err := s.CancelQueueItem(id, "alice", false)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("cancelled an in-flight row (%d affected)", n)
		}
	})

	t.Run("a non-owner cannot delete", func(t *testing.T) {
		id := enqueue(t, s, "alice", 35, 1, 1)
		it, _ := s.GetQueueItem(id)
		it.Status = "failed"
		if err := s.UpdateQueue(it); err != nil {
			t.Fatal(err)
		}
		n, err := s.DeleteQueueItem(id, "bob", false)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("bob deleted alice's row (%d affected)", n)
		}
	})

	t.Run("a non-admin cannot delete an in-flight row", func(t *testing.T) {
		id := enqueue(t, s, "alice", 36, 1, 1)
		n, err := s.DeleteQueueItem(id, "alice", false)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("deleted a pending row as a non-admin (%d affected)", n)
		}
	})
}

func TestEpisodeActiveAndPendingCount(t *testing.T) {
	s := newTestStore(t)

	active, err := s.EpisodeActive(40, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("nothing enqueued yet, EpisodeActive should be false")
	}

	enqueue(t, s, "alice", 40, 1, 1)
	active, err = s.EpisodeActive(40, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("a pending row must count as active")
	}

	n, err := s.QueuePendingCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pending count = %d, want 1", n)
	}
}

func TestSubscriptionOwnership(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertSubscription(&Subscription{TMDBID: 1, Season: 1, Title: "S", RequestedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	subs, _ := s.ListSubscriptions("alice", false)
	if len(subs) != 1 {
		t.Fatalf("owner sees %d subs, want 1", len(subs))
	}
	id := subs[0].ID

	if n, err := s.DeleteSubscription(id, "bob", false); err != nil || n != 0 {
		t.Fatalf("bob deleted alice's subscription (n=%d, err=%v)", n, err)
	}
	if n, err := s.DeleteSubscription(id, "", false); err != nil || n != 0 {
		t.Fatalf("anonymous deleted a subscription (n=%d, err=%v)", n, err)
	}
	if n, err := s.DeleteSubscription(id, "alice", false); err != nil || n != 1 {
		t.Fatalf("owner delete affected %d rows (err=%v), want 1", n, err)
	}

	// UpsertSubscription must be idempotent per (tmdb_id, season).
	for i := 0; i < 3; i++ {
		if err := s.UpsertSubscription(&Subscription{TMDBID: 2, Season: 3, Title: "T", RequestedBy: "alice"}); err != nil {
			t.Fatal(err)
		}
	}
	subs, _ = s.ListSubscriptions("alice", false)
	if len(subs) != 1 {
		t.Fatalf("upsert created %d rows, want 1", len(subs))
	}
	exists, err := s.SubscriptionExists(2, 3)
	if err != nil || !exists {
		t.Fatalf("SubscriptionExists = %v, %v", exists, err)
	}
}

// TestEnqueueIsIdempotentPerRequester: the storage layer itself must refuse a second
// in-flight row for the same identity+requester, even if a caller skips the handler's
// checks — this is the backstop for the magnet-override path that had none.
func TestEnqueueIsIdempotentPerRequester(t *testing.T) {
	s := newTestStore(t)
	first := enqueue(t, s, "alice", 10, 2, 3)

	again, err := s.Enqueue(&QueueItem{
		TMDBID: 10, MediaType: "tv", Season: 2, Episode: 3,
		RequestedBy: "alice", MagnetOverride: "magnet:?xt=urn:btih:deadbeef",
	})
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	if again != first {
		t.Fatalf("duplicate row created: got id %d, want the incumbent %d", again, first)
	}
	all, err := s.ListAllQueue()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("queue holds %d rows, want 1", len(all))
	}

	// A different person requesting the same title keeps their OWN row, because
	// ListQueue filters on requested_by and would otherwise hide bob's request.
	bob, err := s.Enqueue(&QueueItem{
		TMDBID: 10, MediaType: "tv", Season: 2, Episode: 3, RequestedBy: "bob",
	})
	if err != nil {
		t.Fatalf("bob Enqueue: %v", err)
	}
	if bob == first {
		t.Fatal("bob was handed alice's row")
	}
}

// TestRequeueWithMagnet: re-picking a release repoints the in-flight row instead of
// inserting a second one beside it.
func TestRequeueWithMagnet(t *testing.T) {
	s := newTestStore(t)
	id := enqueue(t, s, "alice", 10, 2, 3)

	item, err := s.GetQueueItem(id)
	if err != nil {
		t.Fatal(err)
	}
	item.Status = "processing"
	item.Stage = StagePicking
	if err := s.UpdateQueue(item); err != nil {
		t.Fatal(err)
	}

	const magnet = "magnet:?xt=urn:btih:cafebabe"
	if err := s.RequeueWithMagnet(id, magnet); err != nil {
		t.Fatalf("RequeueWithMagnet: %v", err)
	}
	got, err := s.GetQueueItem(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.MagnetOverride != magnet {
		t.Errorf("magnet = %q, want %q", got.MagnetOverride, magnet)
	}
	if got.Status != "pending" || got.Stage != StageQueued {
		t.Errorf("status/stage = %q/%q, want pending/%s", got.Status, got.Stage, StageQueued)
	}
	all, err := s.ListAllQueue()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("queue holds %d rows, want 1", len(all))
	}
}

// ── The grouped queue tree ────────────────────────────────────────────────────

func enqueueMovie(t *testing.T, s *Store, owner string, tmdb int, title string) int64 {
	t.Helper()
	id, err := s.Enqueue(&QueueItem{
		TMDBID: tmdb, MediaType: "movie", Title: title, RequestedBy: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func setQueueStatus(t *testing.T, s *Store, id int64, status string) {
	t.Helper()
	item, err := s.GetQueueItem(id)
	if err != nil || item == nil {
		t.Fatalf("GetQueueItem(%d): %v %v", id, item, err)
	}
	item.Status = status
	if err := s.UpdateQueue(item); err != nil {
		t.Fatal(err)
	}
}

func findGroup(t *testing.T, groups []QueueShowGroup, tmdb int) QueueShowGroup {
	t.Helper()
	for _, g := range groups {
		if g.TMDBID == tmdb {
			return g
		}
	}
	t.Fatalf("no group for tmdb %d in %+v", tmdb, groups)
	return QueueShowGroup{}
}

// TestListQueueGroupsRollsUpShowsAndMovies: the shape the queue page renders. A show is
// one entry with its seasons nested underneath it; a movie is one entry with none.
func TestListQueueGroupsRollsUpShowsAndMovies(t *testing.T) {
	s := newTestStore(t)
	for _, ep := range []int{1, 2, 3} {
		enqueue(t, s, "alice", 42, 1, ep)
	}
	for _, ep := range []int{1, 2} {
		enqueue(t, s, "alice", 42, 2, ep)
	}
	enqueue(t, s, "alice", 7, 1, 1)
	enqueueMovie(t, s, "alice", 99, "Inception")

	g, err := s.ListQueueGroups("alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if g.Total != 7 || g.Active != 7 {
		t.Errorf("total/active = %d/%d, want 7/7", g.Total, g.Active)
	}
	if len(g.Shows) != 2 {
		t.Fatalf("shows = %d, want 2", len(g.Shows))
	}
	if len(g.Movies) != 1 {
		t.Fatalf("movies = %d, want 1", len(g.Movies))
	}

	show := findGroup(t, g.Shows, 42)
	if show.Counts.Total != 5 || show.Counts.Pending != 5 {
		t.Errorf("show counts = %+v, want 5 pending of 5", show.Counts)
	}
	if len(show.Seasons) != 2 {
		t.Fatalf("seasons = %d, want 2", len(show.Seasons))
	}
	// Seasons read 1, 2 — ascending — even though the query orders groups newest-first.
	if show.Seasons[0].Season != 1 || show.Seasons[1].Season != 2 {
		t.Errorf("season order = %d,%d, want 1,2", show.Seasons[0].Season, show.Seasons[1].Season)
	}
	if show.Seasons[0].Counts.Total != 3 || show.Seasons[1].Counts.Total != 2 {
		t.Errorf("season totals = %d,%d, want 3,2",
			show.Seasons[0].Counts.Total, show.Seasons[1].Counts.Total)
	}

	movie := findGroup(t, g.Movies, 99)
	if movie.Title != "Inception" {
		t.Errorf("movie title = %q, want Inception", movie.Title)
	}
	if movie.Counts.Total != 1 {
		t.Errorf("movie counts = %+v, want 1 row", movie.Counts)
	}
	// Never nil: the UI iterates this without a guard.
	if movie.Seasons == nil || len(movie.Seasons) != 0 {
		t.Errorf("movie seasons = %+v, want an empty non-nil slice", movie.Seasons)
	}
}

// TestListQueueGroupsCountsEveryStatus: the histogram is the whole point of the grouped
// view — a season ring showing "3 of 5 done" is drawn from these numbers.
func TestListQueueGroupsCountsEveryStatus(t *testing.T) {
	s := newTestStore(t)
	for i, status := range []string{"pending", "processing", "done", "failed", "cancelled"} {
		id := enqueue(t, s, "alice", 42, 1, i+1)
		setQueueStatus(t, s, id, status)
	}

	g, err := s.ListQueueGroups("alice", false)
	if err != nil {
		t.Fatal(err)
	}
	want := QueueCounts{Pending: 1, Processing: 1, Done: 1, Failed: 1, Cancelled: 1, Total: 5, Active: 2}
	show := findGroup(t, g.Shows, 42)
	if show.Counts != want {
		t.Errorf("show counts = %+v, want %+v", show.Counts, want)
	}
	if show.Seasons[0].Counts != want {
		t.Errorf("season counts = %+v, want %+v", show.Seasons[0].Counts, want)
	}
	if g.Total != 5 || g.Active != 2 {
		t.Errorf("overall total/active = %d/%d, want 5/2", g.Total, g.Active)
	}
	if g.Counts != want {
		t.Errorf("overall counts = %+v, want %+v", g.Counts, want)
	}
	// Newest must be a real instant. MAX(created_at) is an expression, so the driver
	// hands it back as TEXT rather than as the time.Time a plain DATETIME column gives;
	// if that parse ever regresses this is the zero time and every group sorts last.
	if show.Newest.IsZero() {
		t.Fatal("Newest is the zero time: MAX(created_at) was not parsed")
	}
	if d := time.Since(show.Newest); d > time.Hour || d < -time.Hour {
		t.Errorf("Newest = %v, %v away from now — wrong timezone or layout", show.Newest, d)
	}
}

// TestListQueueGroupsSeeRowsTheFlatListCannot is the regression test for the user's
// actual complaint ("I do not see the list of complete shows and series"). The flat
// list is capped at 100 newest rows, so a queue larger than that can hide whole titles
// from it. The grouped view must account for every row regardless.
func TestListQueueGroupsSeeRowsTheFlatListCannot(t *testing.T) {
	s := newTestStore(t)
	enqueueMovie(t, s, "alice", 99, "Buried")
	for ep := 1; ep <= 150; ep++ {
		enqueue(t, s, "alice", 42, 1, ep)
	}

	flat, err := s.ListQueue("alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(flat) != queueListLimit {
		t.Fatalf("flat list = %d rows, want the %d-row cap", len(flat), queueListLimit)
	}

	g, err := s.ListQueueGroups("alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if g.Total != 151 {
		t.Errorf("grouped total = %d, want all 151 rows counted", g.Total)
	}
	// 151 rows collapse to two groups: one show (one season) and one movie.
	if len(g.Shows) != 1 || len(g.Movies) != 1 {
		t.Fatalf("groups = %d shows / %d movies, want 1/1", len(g.Shows), len(g.Movies))
	}
	if show := findGroup(t, g.Shows, 42); show.Counts.Total != 150 {
		t.Errorf("show total = %d, want 150", show.Counts.Total)
	}
	if movie := findGroup(t, g.Movies, 99); movie.Title != "Buried" {
		t.Errorf("movie title = %q, want Buried", movie.Title)
	}
}

// TestListQueueGroupsPrefersAPopulatedPoster: title and poster_url are denormalised
// copies, and MAX picks a real string over the ” an older row may carry.
func TestListQueueGroupsPrefersAPopulatedPoster(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Enqueue(&QueueItem{
		TMDBID: 42, MediaType: "tv", Title: "Show", Season: 1, Episode: 1, RequestedBy: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(&QueueItem{
		TMDBID: 42, MediaType: "tv", Title: "Show", Season: 1, Episode: 2, RequestedBy: "alice",
		PosterURL: "https://image.tmdb.org/p/poster.jpg",
	}); err != nil {
		t.Fatal(err)
	}

	g, err := s.ListQueueGroups("alice", false)
	if err != nil {
		t.Fatal(err)
	}
	show := findGroup(t, g.Shows, 42)
	if show.PosterURL != "https://image.tmdb.org/p/poster.jpg" {
		t.Errorf("poster = %q, want the populated one", show.PosterURL)
	}
	if show.Title != "Show" {
		t.Errorf("title = %q, want Show", show.Title)
	}
}

// ── Scoped queue fetches ──────────────────────────────────────────────────────

// TestListQueueFilteredScopesToOneSeason: expanding a season in the tree fetches that
// season's leaf rows — all of them, in episode order, past the flat list's cap.
func TestListQueueFilteredScopesToOneSeason(t *testing.T) {
	s := newTestStore(t)
	for ep := 1; ep <= 120; ep++ {
		enqueue(t, s, "alice", 42, 1, ep)
	}
	for ep := 1; ep <= 3; ep++ {
		enqueue(t, s, "alice", 42, 2, ep)
	}
	enqueue(t, s, "alice", 7, 1, 1)

	rows, err := s.ListQueueFiltered("alice", false, QueueFilter{
		TMDBID: 42, MediaType: "tv", Season: 1, SeasonSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 120 > queueListLimit: a season must not be truncated by the flat list's cap.
	if len(rows) != 120 {
		t.Fatalf("scoped rows = %d, want all 120 of the season", len(rows))
	}
	for i, row := range rows {
		if row.TMDBID != 42 || row.Season != 1 {
			t.Fatalf("row %d escaped the scope: tmdb %d season %d", i, row.TMDBID, row.Season)
		}
		if row.Episode != i+1 {
			t.Fatalf("row %d is episode %d, want episode order", i, row.Episode)
		}
	}
}

// TestListQueueFilterSeasonZeroIsARealSeason: season 0 is a value (movies carry it, and
// so do TV specials), so it cannot double as "unset" — that is what SeasonSet is for.
func TestListQueueFilterSeasonZeroIsARealSeason(t *testing.T) {
	s := newTestStore(t)
	enqueue(t, s, "alice", 42, 0, 1) // a special
	enqueue(t, s, "alice", 42, 1, 1)

	all, err := s.ListQueueFiltered("alice", false, QueueFilter{TMDBID: 42})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unset season filtered anyway: got %d rows, want 2", len(all))
	}
	specials, err := s.ListQueueFiltered("alice", false, QueueFilter{TMDBID: 42, Season: 0, SeasonSet: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(specials) != 1 || specials[0].Season != 0 {
		t.Fatalf("season 0 fetch = %d rows, want the one special", len(specials))
	}
}

// TestListQueueFilterActiveOnly: the tree's "in flight" view drops finished rows.
func TestListQueueFilterActiveOnly(t *testing.T) {
	s := newTestStore(t)
	pending := enqueue(t, s, "alice", 42, 1, 1)
	finished := enqueue(t, s, "alice", 42, 1, 2)
	setQueueStatus(t, s, finished, "done")

	rows, err := s.ListQueueFiltered("alice", false, QueueFilter{TMDBID: 42, ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != pending {
		t.Fatalf("active-only = %+v, want just the pending row %d", rows, pending)
	}
}

// TestListQueueUnfilteredIsUnchanged: the existing callers pass no filter and must get
// exactly what they always got — the newest rows, capped, newest-first.
func TestListQueueUnfilteredIsUnchanged(t *testing.T) {
	s := newTestStore(t)
	for ep := 1; ep <= 3; ep++ {
		enqueue(t, s, "alice", 42, 1, ep)
	}
	plain, err := s.ListQueue("alice", false)
	if err != nil {
		t.Fatal(err)
	}
	zero, err := s.ListQueueFiltered("alice", false, QueueFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 3 || len(zero) != len(plain) {
		t.Fatalf("ListQueue = %d rows, zero filter = %d, want 3 and equal", len(plain), len(zero))
	}
	for i := range plain {
		if plain[i].ID != zero[i].ID {
			t.Fatalf("row %d differs: %d vs %d", i, plain[i].ID, zero[i].ID)
		}
	}
}
