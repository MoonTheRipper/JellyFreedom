package store

import (
	"sort"
	"testing"
)

func statusOf(t *testing.T, s *Store, id int64) string {
	t.Helper()
	st, err := s.QueueStatus(id)
	if err != nil {
		t.Fatalf("QueueStatus(%d): %v", id, err)
	}
	return st
}

func sortedIDs(ids []int64) []int64 {
	out := append([]int64(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// The bug this exists for: a season request enqueues one row per episode, the worker has
// one of them claimed, and the user wants the whole show stopped. Every in-flight row —
// including the claimed one — must be cancelled; finished rows and other shows must not.
func TestCancelQueueScopeShowCancelsPendingAndProcessing(t *testing.T) {
	s := newTestStore(t)
	e1 := enqueue(t, s, "alice", 100, 1, 1)
	e2 := enqueue(t, s, "alice", 100, 1, 2)
	e3 := enqueue(t, s, "alice", 100, 2, 1)
	done := enqueue(t, s, "alice", 100, 2, 2)
	other := enqueue(t, s, "alice", 200, 1, 1)
	setQueueStatus(t, s, done, "done")

	claimed, err := s.NextPending()
	if err != nil || claimed == nil || claimed.ID != e1 {
		t.Fatalf("NextPending = %+v, %v; want row %d claimed", claimed, err, e1)
	}

	ids, err := s.CancelQueueScope(vw("alice", false), ShowScope(100, "tv"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedIDs(ids), []int64{e1, e2, e3}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("cancelled ids = %v, want %v", got, want)
	}
	for _, id := range []int64{e1, e2, e3} {
		if st := statusOf(t, s, id); st != "cancelled" {
			t.Errorf("row %d status = %q, want cancelled", id, st)
		}
	}
	if st := statusOf(t, s, done); st != "done" {
		t.Errorf("finished row status = %q, want done (a cancel must not rewrite history)", st)
	}
	if st := statusOf(t, s, other); st != "pending" {
		t.Errorf("other show's row status = %q, want pending", st)
	}
}

func TestCancelQueueScopeSeasonAndEpisode(t *testing.T) {
	s := newTestStore(t)
	s1e1 := enqueue(t, s, "alice", 100, 1, 1)
	s1e2 := enqueue(t, s, "alice", 100, 1, 2)
	s2e1 := enqueue(t, s, "alice", 100, 2, 1)
	// Season 0 is a real season (specials). It must not be swept up by "season unset".
	s0e1 := enqueue(t, s, "alice", 100, 0, 1)

	ids, err := s.CancelQueueScope(vw("alice", false), SeasonScope(100, 1))
	if err != nil || len(ids) != 2 {
		t.Fatalf("season cancel = %v, %v; want 2 rows", ids, err)
	}
	if statusOf(t, s, s2e1) != "pending" || statusOf(t, s, s0e1) != "pending" {
		t.Fatal("a season cancel reached another season")
	}

	ids, err = s.CancelQueueScope(vw("alice", false), EpisodeScope(100, 0, 1))
	if err != nil || len(ids) != 1 || ids[0] != s0e1 {
		t.Fatalf("episode cancel = %v, %v; want [%d]", ids, err, s0e1)
	}
	if statusOf(t, s, s2e1) != "pending" {
		t.Fatal("an episode cancel reached another episode")
	}
	_ = s1e1
	_ = s1e2
}

func TestCancelQueueScopeOwnership(t *testing.T) {
	s := newTestStore(t)
	mine := enqueue(t, s, "alice", 100, 1, 1)
	theirs := enqueue(t, s, "bob", 100, 1, 2)

	if ids, err := s.CancelQueueScope(Viewer{}, ShowScope(100, "tv")); err != nil || len(ids) != 0 {
		t.Fatalf("anonymous cancel = %v, %v; want nothing", ids, err)
	}
	ids, err := s.CancelQueueScope(vw("alice", false), ShowScope(100, "tv"))
	if err != nil || len(ids) != 1 || ids[0] != mine {
		t.Fatalf("alice's cancel = %v, %v; want only her own row %d", ids, err, mine)
	}
	if statusOf(t, s, theirs) != "pending" {
		t.Fatal("a non-admin cancelled somebody else's request")
	}
	ids, err = s.CancelQueueScope(vw("root", true), ShowScope(100, "tv"))
	if err != nil || len(ids) != 1 || ids[0] != theirs {
		t.Fatalf("admin cancel = %v, %v; want bob's row %d", ids, err, theirs)
	}
}

// The worker holds its own copy of a row while it resolves, and used to write that copy
// back unconditionally — turning a cancelled row into 'processing' on the next progress
// tick and into 'done' at the end, with the episode written into the library.
func TestUpdateQueueCannotResurrectACancelledRow(t *testing.T) {
	s := newTestStore(t)
	id := enqueue(t, s, "alice", 100, 1, 1)
	item, err := s.NextPending()
	if err != nil || item == nil {
		t.Fatalf("NextPending: %v %v", item, err)
	}
	if n, err := s.CancelQueueItem(id, vw("alice", false)); err != nil || n != 1 {
		t.Fatalf("CancelQueueItem on a PROCESSING row = %d, %v; want 1", n, err)
	}

	item.Progress = "Checking peers…"
	if err := s.UpdateQueue(item); err != nil {
		t.Fatal(err)
	}
	item.Status = "done"
	if err := s.UpdateQueue(item); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, s, id); st != "cancelled" {
		t.Fatalf("status after the worker's stale writes = %q, want cancelled", st)
	}
}

func TestQueueStatusOfAMissingRowIsEmpty(t *testing.T) {
	s := newTestStore(t)
	if st := statusOf(t, s, 98765); st != "" {
		t.Fatalf("status of a row that never existed = %q, want empty", st)
	}
}

func TestCancelledEpisodesUsesTheNewestRow(t *testing.T) {
	s := newTestStore(t)
	a := enqueue(t, s, "alice", 100, 1, 1)
	b := enqueue(t, s, "alice", 100, 1, 2)
	enqueue(t, s, "alice", 100, 1, 3)
	setQueueStatus(t, s, a, "cancelled")
	setQueueStatus(t, s, b, "cancelled")
	// Episode 2 was re-requested after the cancel: its newest row is in flight again.
	if err := s.ClearTerminalQueue(100, "tv", 1, 2); err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, "alice", 100, 1, 2)

	got, err := s.CancelledEpisodes(100, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !got[1] || got[2] || got[3] || len(got) != 1 {
		t.Fatalf("CancelledEpisodes = %v, want only episode 1", got)
	}
}

func TestDeleteSubscriptionsFor(t *testing.T) {
	s := newTestStore(t)
	for _, sub := range []*Subscription{
		{TMDBID: 100, Season: 1, Title: "Show", RequestedBy: "alice"},
		{TMDBID: 100, Season: 2, Title: "Show", RequestedBy: "alice"},
		{TMDBID: 200, Season: 1, Title: "Other", RequestedBy: "alice"},
	} {
		if err := s.UpsertSubscription(sub); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.DeleteSubscriptionsFor(vw("bob", false), 100, -1); err != nil || n != 0 {
		t.Fatalf("bob removed %d of alice's subscriptions (err %v)", n, err)
	}
	if n, err := s.DeleteSubscriptionsFor(vw("alice", false), 100, 2); err != nil || n != 1 {
		t.Fatalf("season delete = %d, %v; want 1", n, err)
	}
	if n, err := s.DeleteSubscriptionsFor(vw("alice", false), 100, -1); err != nil || n != 1 {
		t.Fatalf("show delete = %d, %v; want the remaining 1", n, err)
	}
	left, err := s.ListSubscriptions(vw("alice", false))
	if err != nil || len(left) != 1 || left[0].TMDBID != 200 {
		t.Fatalf("remaining subscriptions = %+v, %v; want only the other show", left, err)
	}
}

// A re-request made by hand must not wait behind a season backlog.
func TestNextPendingTakesHandMadeRequestsFirst(t *testing.T) {
	s := newTestStore(t)
	for ep := 1; ep <= 3; ep++ {
		enqueue(t, s, "alice", 100, 2, ep) // a season request: bulk, priority 0
	}
	byHand, err := s.Enqueue(&QueueItem{TMDBID: 100, MediaType: "tv", Title: "Show", Season: 1, Episode: 1,
		RequestedBy: "alice", Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.NextPending()
	if err != nil || next == nil || next.ID != byHand {
		t.Fatalf("NextPending = %+v, %v; want the hand-made request %d first", next, err, byHand)
	}
	// Then the backlog, in the order it was queued.
	for ep := 1; ep <= 3; ep++ {
		next, err = s.NextPending()
		if err != nil || next == nil || next.Episode != ep {
			t.Fatalf("backlog claim %d = %+v, %v; want episode %d", ep, next, err, ep)
		}
	}
}

// A show removed season by season used to stay in the queue page with every episode it
// had fetched. Removal now clears the finished rows of exactly the removed scope.
func TestForgetQueueScopeClearsOnlyTheScopesFinishedRows(t *testing.T) {
	s := newTestStore(t)
	s1done := enqueue(t, s, "alice", 100, 1, 1)
	s1failed := enqueue(t, s, "alice", 100, 1, 2)
	s1cancelled := enqueue(t, s, "alice", 100, 1, 3)
	s1pending := enqueue(t, s, "alice", 100, 1, 4)
	s2done := enqueue(t, s, "alice", 100, 2, 1)
	otherDone := enqueue(t, s, "alice", 200, 1, 1)
	setQueueStatus(t, s, s1done, "done")
	setQueueStatus(t, s, s1failed, "failed")
	setQueueStatus(t, s, s1cancelled, "cancelled")
	setQueueStatus(t, s, s2done, "done")
	setQueueStatus(t, s, otherDone, "done")

	n, err := s.ForgetQueueScope(vw("alice", false), SeasonScope(100, 1), false)
	if err != nil || n != 3 {
		t.Fatalf("season forget = %d, %v; want the 3 finished season-1 rows", n, err)
	}
	for _, id := range []int64{s1done, s1failed, s1cancelled} {
		if st := statusOf(t, s, id); st != "" {
			t.Errorf("row %d still in the queue as %q after its season was removed", id, st)
		}
	}
	// In flight is CancelQueueScope's job, not this one's: it is never deleted from
	// under the worker here.
	if st := statusOf(t, s, s1pending); st != "pending" {
		t.Errorf("in-flight row = %q, want it left pending", st)
	}
	if statusOf(t, s, s2done) != "done" || statusOf(t, s, otherDone) != "done" {
		t.Fatal("a season forget reached another season or another show")
	}

	if n, err := s.ForgetQueueScope(vw("alice", false), ShowScope(100, "tv"), false); err != nil || n != 1 {
		t.Fatalf("show forget = %d, %v; want season 2's finished row", n, err)
	}
	if statusOf(t, s, otherDone) != "done" {
		t.Fatal("a show forget reached another show")
	}
}

// Removing one episode of a season a subscription still follows must leave a cancelled
// row, or the next subscription check fetches the episode straight back.
func TestForgetQueueScopeTombstoneKeepsTheEpisodeCancelled(t *testing.T) {
	s := newTestStore(t)
	ep := enqueue(t, s, "alice", 100, 1, 5)
	setQueueStatus(t, s, ep, "done")
	if n, err := s.ForgetQueueScope(vw("alice", false), EpisodeScope(100, 1, 5), true); err != nil || n != 1 {
		t.Fatalf("tombstone = %d, %v; want 1", n, err)
	}
	if st := statusOf(t, s, ep); st != "cancelled" {
		t.Fatalf("tombstoned row = %q, want cancelled", st)
	}
	got, err := s.CancelledEpisodes(100, 1)
	if err != nil || !got[5] {
		t.Fatalf("CancelledEpisodes = %v, %v; the subscription check would re-fetch episode 5", got, err)
	}
}

func TestForgetQueueScopeOwnership(t *testing.T) {
	s := newTestStore(t)
	mine := enqueue(t, s, "alice", 100, 1, 1)
	theirs := enqueue(t, s, "bob", 100, 1, 2)
	setQueueStatus(t, s, mine, "done")
	setQueueStatus(t, s, theirs, "done")

	if n, err := s.ForgetQueueScope(Viewer{}, ShowScope(100, "tv"), false); err != nil || n != 0 {
		t.Fatalf("anonymous forget = %d, %v; want nothing", n, err)
	}
	if n, err := s.ForgetQueueScope(vw("alice", false), ShowScope(100, "tv"), false); err != nil || n != 1 {
		t.Fatalf("alice's forget = %d, %v; want only her own row", n, err)
	}
	if statusOf(t, s, theirs) != "done" {
		t.Fatal("a non-admin cleared somebody else's queue history")
	}
	if n, err := s.ForgetQueueScope(vw("root", true), ShowScope(100, "tv"), false); err != nil || n != 1 {
		t.Fatalf("admin forget = %d, %v; want bob's row", n, err)
	}
}
