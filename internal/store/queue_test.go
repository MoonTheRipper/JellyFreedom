package store

import (
	"testing"
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
