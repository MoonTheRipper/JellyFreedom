package main

import (
	"os"
	"path/filepath"
	"testing"

	"jellyfreedom/internal/config"
	"jellyfreedom/internal/store"
	"jellyfreedom/internal/torrserver"
)

// A removed show came back on the live box because the worker wrote each episode it was
// still resolving into the library after the removal had run. commitResolved is the one
// place a resolved row becomes a library entry; these drive it directly, with a real
// store, a real library directory and a fake TorrServer, and judge it by what ended up
// on disk and in the database.

const commitHash = "2222222222222222222222222222222222222222"

func commitFixture(t *testing.T) (*queueWorker, *store.Store, *fakeTorrServer, *store.QueueItem, *resolveResult) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "commit.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ts := &fakeTorrServer{}
	cfg := &config.Config{}
	cfg.Server.PublicURL = "http://host:1990"
	w := &queueWorker{db: db, ts: torrserver.New(ts.serve(t).URL), cfg: cfg}

	if _, err := db.Enqueue(&store.QueueItem{TMDBID: 61923, MediaType: "tv", Title: "Show",
		Season: 3, Episode: 7, LibraryName: "Shows", RequestedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	item, err := db.NextPending()
	if err != nil || item == nil {
		t.Fatalf("NextPending: %v %v", item, err)
	}
	lib := &config.Library{Name: "Shows", Type: "tv", Path: t.TempDir()}
	res := &resolveResult{hash: commitHash, fileIndex: 1, title: "Show", displayTitle: "Show S03E07",
		year: "2015", lib: lib}
	return w, db, ts, item, res
}

func noFail(t *testing.T) func(string, string) {
	return func(msg, _ string) { t.Errorf("fail(%q) called", msg) }
}

func TestCommitResolvedWritesAnUncancelledRow(t *testing.T) {
	w, db, _, item, res := commitFixture(t)
	if !w.commitResolved(item, res, noFail(t)) {
		t.Fatal("commitResolved refused a row nobody cancelled")
	}
	got, err := db.GetEpisode(61923, 3, 7)
	if err != nil || got == nil {
		t.Fatalf("library row after commit: %v %v", got, err)
	}
	if _, err := os.Stat(got.StrmPath); err != nil {
		t.Fatalf(".strm not written: %v", err)
	}
	if st, _ := db.QueueStatus(item.ID); st != "done" {
		t.Fatalf("queue status = %q, want done", st)
	}
}

func TestCommitResolvedRefusesACancelledRow(t *testing.T) {
	w, db, ts, item, res := commitFixture(t)
	// The user removed the show while this episode was being resolved.
	if ids, err := db.CancelQueueScope(store.Viewer{IsAdmin: true}, store.ShowScope(61923, "tv")); err != nil || len(ids) != 1 {
		t.Fatalf("CancelQueueScope = %v, %v", ids, err)
	}
	if w.commitResolved(item, res, noFail(t)) {
		t.Fatal("commitResolved wrote a cancelled row into the library")
	}
	if got, _ := db.GetEpisode(61923, 3, 7); got != nil {
		t.Fatalf("a cancelled episode has a library row: %+v", got)
	}
	var strms []string
	_ = filepath.Walk(res.lib.Path, func(p string, _ os.FileInfo, _ error) error {
		if filepath.Ext(p) == ".strm" {
			strms = append(strms, p)
		}
		return nil
	})
	if len(strms) != 0 {
		t.Fatalf("a cancelled episode left .strm files behind: %v", strms)
	}
	if st, _ := db.QueueStatus(item.ID); st != "cancelled" {
		t.Fatalf("queue status = %q, want it to stay cancelled", st)
	}
	if d := ts.drops(); len(d) != 1 || d[0] != commitHash {
		t.Fatalf("drops = %v, want the resolved torrent released", d)
	}
}

// Removal now deletes the title's queue rows outright, the one the worker is resolving
// included. A row that no longer exists must be refused like a cancelled one.
func TestCommitResolvedRefusesARowRemovedWithItsTitle(t *testing.T) {
	w, db, ts, item, res := commitFixture(t)
	admin := store.Viewer{IsAdmin: true}
	if _, err := db.CancelQueueScope(admin, store.ShowScope(61923, "tv")); err != nil {
		t.Fatal(err)
	}
	if n, err := db.ForgetQueueScope(admin, store.ShowScope(61923, "tv"), false); err != nil || n != 1 {
		t.Fatalf("ForgetQueueScope = %d, %v; want the in-flight row gone", n, err)
	}
	if w.commitResolved(item, res, noFail(t)) {
		t.Fatal("commitResolved wrote a row that had been removed with its title")
	}
	if got, _ := db.GetEpisode(61923, 3, 7); got != nil {
		t.Fatalf("a removed title has a library row again: %+v", got)
	}
	if d := ts.drops(); len(d) != 1 || d[0] != commitHash {
		t.Fatalf("drops = %v, want the resolved torrent released", d)
	}
}

// A re-pick (RequeueWithMagnet) while the old release was still resolving resets the row
// to pending. Committing the old result would mark it done and lose the user's choice.
func TestCommitResolvedYieldsToARepick(t *testing.T) {
	w, db, _, item, res := commitFixture(t)
	if err := db.RequeueWithMagnet(item.ID, "magnet:?xt=urn:btih:3333333333333333333333333333333333333333"); err != nil {
		t.Fatal(err)
	}
	if w.commitResolved(item, res, noFail(t)) {
		t.Fatal("commitResolved committed a release the user had just replaced")
	}
	if st, _ := db.QueueStatus(item.ID); st != "pending" {
		t.Fatalf("queue status = %q, want pending (so the re-pick runs)", st)
	}
}

func TestAbortInterruptsOnlyTheMatchingJob(t *testing.T) {
	w := &queueWorker{}
	cancelled := false
	w.curID, w.curCancel = 42, func() { cancelled = true }
	w.abort([]int64{7, 8})
	if cancelled {
		t.Fatal("abort interrupted a job that was not in the list")
	}
	w.abort([]int64{7, 42})
	if !cancelled {
		t.Fatal("abort did not interrupt the job being resolved")
	}
	idle := &queueWorker{}
	idle.abort([]int64{1}) // no job running: must not panic
}
