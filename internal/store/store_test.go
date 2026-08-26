package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestConcurrentWrites is the regression test for the SQLite DSN.
//
// With the old configuration — sql.Open("sqlite", path) with no parameters, no
// busy_timeout, and an unbounded connection pool — the overwhelming majority of these
// writes fail with SQLITE_BUSY. With busy_timeout + WAL + SetMaxOpenConns(1) every one
// must succeed. Any failure here means the DSN regressed.
func TestConcurrentWrites(t *testing.T) {
	s := newTestStore(t)

	const goroutines, perGoroutine = 32, 40

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				err := s.Upsert(&Item{
					TMDBID:    g*1000 + i,
					MediaType: "movie",
					Title:     fmt.Sprintf("Movie %d-%d", g, i),
					Year:      "2020",
					StrmPath:  fmt.Sprintf("/lib/m-%d-%d.strm", g, i),
					Status:    "ready",
					Updated:   time.Now(),
				})
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}
		}(g)
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d of %d concurrent writes failed; first error: %v",
			len(errs), goroutines*perGoroutine, errs[0])
	}
	items, err := s.ListAllItems()
	if err != nil {
		t.Fatalf("ListAllItems: %v", err)
	}
	if len(items) != goroutines*perGoroutine {
		t.Fatalf("stored %d items, want %d", len(items), goroutines*perGoroutine)
	}
}

// TestConcurrentMixedReadWrite exercises the read-vs-write contention WAL is there for.
func TestConcurrentMixedReadWrite(t *testing.T) {
	s := newTestStore(t)
	if err := s.Upsert(&Item{TMDBID: 1, MediaType: "movie", Title: "A", StrmPath: "/a.strm", Status: "ready", Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 256)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if _, err := s.ListVisible(vw("", false)); err != nil {
					errCh <- err
				}
				if err := s.SetSetting(fmt.Sprintf("k%d", i), fmt.Sprint(j)); err != nil {
					errCh <- err
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("mixed read/write failed: %v", err)
	}
}

// ── Visibility ────────────────────────────────────────────────────────────────

// TestVisibility is the highest-value test in the project: it pins down exactly who can
// see what.
//
// The bug it guards against: viewerUsername=="" was treated as ADMIN-EQUIVALENT, but the
// HTTP layer passed "" for an UNAUTHENTICATED caller — so an anonymous visitor saw
// private items that a logged-in non-admin correctly could not. Logging out granted
// access that logging in denied.
func TestVisibility(t *testing.T) {
	s := newTestStore(t)

	mustUpsert := func(strm, owner string, private bool) {
		t.Helper()
		if err := s.Upsert(&Item{
			TMDBID: len(strm), MediaType: "movie", Title: "T" + strm, Year: "2020",
			StrmPath: strm, Status: "ready", RequestedBy: owner, IsPrivate: private,
			Updated: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	mustUpsert("/pub-alice.strm", "alice", false)
	mustUpsert("/priv-alice.strm", "alice", true)
	mustUpsert("/pub-bob.strm", "bob", false)
	mustUpsert("/priv-bob.strm", "bob", true)
	// An item with no owner at all (enqueued by a background job).
	mustUpsert("/pub-orphan.strm", "", false)
	mustUpsert("/priv-orphan.strm", "", true)

	cases := []struct {
		name    string
		viewer  string
		isAdmin bool
		want    []string
	}{
		{
			name: "anonymous sees only public items and NEVER a private one",
			want: []string{"/pub-alice.strm", "/pub-bob.strm", "/pub-orphan.strm"},
		},
		{
			name:   "owner sees public items plus their own private one",
			viewer: "alice",
			want:   []string{"/pub-alice.strm", "/priv-alice.strm", "/pub-bob.strm", "/pub-orphan.strm"},
		},
		{
			name:   "other user does not see someone else's private item",
			viewer: "bob",
			want:   []string{"/pub-alice.strm", "/pub-bob.strm", "/priv-bob.strm", "/pub-orphan.strm"},
		},
		{
			name: "unknown user is treated like any other non-owner", viewer: "mallory",
			want: []string{"/pub-alice.strm", "/pub-bob.strm", "/pub-orphan.strm"},
		},
		{
			name: "admin sees everything", viewer: "root", isAdmin: true,
			want: []string{"/pub-alice.strm", "/priv-alice.strm", "/pub-bob.strm",
				"/priv-bob.strm", "/pub-orphan.strm", "/priv-orphan.strm"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, err := s.ListVisible(vw(tc.viewer, tc.isAdmin))
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, it := range items {
				got[it.StrmPath] = true
			}
			if len(got) != len(tc.want) {
				t.Fatalf("saw %d items %v, want %d %v", len(got), keys(got), len(tc.want), tc.want)
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Errorf("missing %s", w)
				}
			}
		})
	}
}

// TestVisibilityAnonymousNeverBeatsAuthenticated is the exact inversion the audit found:
// whatever anonymous can see must be a SUBSET of what any authenticated non-admin sees.
func TestVisibilityAnonymousNeverBeatsAuthenticated(t *testing.T) {
	s := newTestStore(t)
	if err := s.Upsert(&Item{
		TMDBID: 7, MediaType: "movie", Title: "Secret", StrmPath: "/secret.strm",
		Status: "ready", RequestedBy: "alice", IsPrivate: true, Updated: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	anon, err := s.ListVisible(vw("", false))
	if err != nil {
		t.Fatal(err)
	}
	if len(anon) != 0 {
		t.Fatalf("anonymous saw %d items, want 0 — a private item leaked to an unauthenticated caller", len(anon))
	}

	bob, err := s.ListVisible(vw("bob", false))
	if err != nil {
		t.Fatal(err)
	}
	if len(bob) != 0 {
		t.Fatalf("non-owner saw %d items, want 0", len(bob))
	}
}

func TestGetStatusByTMDBIDsFiltersPrivacy(t *testing.T) {
	s := newTestStore(t)
	if err := s.Upsert(&Item{
		TMDBID: 42, MediaType: "movie", Title: "Private Movie", StrmPath: "/p.strm",
		Status: "ready", RequestedBy: "alice", IsPrivate: true, Updated: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		viewer  string
		isAdmin bool
		want    bool
	}{
		{"anonymous", "", false, false},
		{"other user", "bob", false, false},
		{"owner", "alice", false, true},
		{"admin", "root", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.GetStatusByTMDBIDs([]int{42}, vw(tc.viewer, tc.isAdmin))
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := got[42]; ok != tc.want {
				t.Fatalf("visible=%v, want %v", ok, tc.want)
			}
		})
	}
}

func TestListQueueAndSubscriptionsVisibility(t *testing.T) {
	s := newTestStore(t)
	for _, owner := range []string{"alice", "bob"} {
		if _, err := s.Enqueue(&QueueItem{TMDBID: 1, MediaType: "movie", Title: owner, RequestedBy: owner}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertSubscription(&Subscription{
			TMDBID: len(owner), Season: 1, Title: owner, RequestedBy: owner,
		}); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name      string
		viewer    string
		isAdmin   bool
		wantQueue int
		wantSubs  int
	}{
		// A queue row and a subscription both name a real person's viewing request, so
		// an anonymous caller gets nothing rather than everything.
		{"anonymous sees nothing", "", false, 0, 0},
		{"owner sees only their own", "alice", false, 1, 1},
		{"other user sees only their own", "bob", false, 1, 1},
		{"unknown user sees nothing", "mallory", false, 0, 0},
		{"admin sees all", "root", true, 2, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := s.ListQueue(vw(tc.viewer, tc.isAdmin))
			if err != nil {
				t.Fatal(err)
			}
			if len(q) != tc.wantQueue {
				t.Errorf("queue: got %d, want %d", len(q), tc.wantQueue)
			}
			subs, err := s.ListSubscriptions(vw(tc.viewer, tc.isAdmin))
			if err != nil {
				t.Fatal(err)
			}
			if len(subs) != tc.wantSubs {
				t.Errorf("subscriptions: got %d, want %d", len(subs), tc.wantSubs)
			}
		})
	}
}

// TestListQueueGroupsVisibility is TestListQueueAndSubscriptionsVisibility for the
// grouped view, and it exists because the grouped view is the easier of the two to get
// wrong. ListQueue's WHERE requested_by=? is right there in the query; an aggregate
// invites you to group first and filter the groups afterwards, which would already
// have leaked — the counts would be everyone's, and a count is enough to reveal that
// somebody else requested a title. So this asserts both halves: the caller sees their
// own rows, and NOTHING of anyone else's, right down to the numbers.
func TestListQueueGroupsVisibility(t *testing.T) {
	s := newTestStore(t)
	// A movie only alice asked for, a movie only bob asked for, and a show they both
	// asked for — which is the case that catches a leak in the counts rather than in
	// the group list.
	seed := []QueueItem{
		{TMDBID: 1, MediaType: "movie", Title: "Alice Only", RequestedBy: "alice"},
		{TMDBID: 2, MediaType: "movie", Title: "Bob Only", RequestedBy: "bob"},
		{TMDBID: 5, MediaType: "tv", Title: "Shared Show", Season: 1, Episode: 1, RequestedBy: "alice"},
		{TMDBID: 5, MediaType: "tv", Title: "Shared Show", Season: 1, Episode: 2, RequestedBy: "alice"},
		{TMDBID: 5, MediaType: "tv", Title: "Shared Show", Season: 1, Episode: 1, RequestedBy: "bob"},
		// A row with NO requester. requested_by defaults to '', so these exist — and
		// they are why the anonymous case cannot be left to the WHERE clause: a query
		// built as requested_by='' would MATCH this row and hand it to a passer-by.
		{TMDBID: 3, MediaType: "movie", Title: "Ownerless", RequestedBy: ""},
	}
	for i := range seed {
		if _, err := s.Enqueue(&seed[i]); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name       string
		viewer     string
		isAdmin    bool
		wantTotal  int
		wantMovies []int // tmdb ids, and no others
		wantShowN  int   // rows counted under the shared show
	}{
		// A queue row names a real person's viewing request, so an anonymous caller
		// gets an empty view rather than everyone's.
		{"anonymous sees nothing", "", false, 0, nil, 0},
		{"owner sees only their own", "alice", false, 3, []int{1}, 2},
		{"other user sees only their own", "bob", false, 2, []int{2}, 1},
		{"unknown user sees nothing", "mallory", false, 0, nil, 0},
		{"admin sees all", "root", true, 6, []int{1, 2, 3}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := s.ListQueueGroups(vw(tc.viewer, tc.isAdmin))
			if err != nil {
				t.Fatal(err)
			}
			if g == nil {
				t.Fatal("ListQueueGroups returned nil; the handler marshals this directly")
			}
			if g.Total != tc.wantTotal {
				t.Errorf("total = %d, want %d", g.Total, tc.wantTotal)
			}
			if g.Active != tc.wantTotal {
				t.Errorf("active = %d, want %d (every seeded row is pending)", g.Active, tc.wantTotal)
			}
			// Compared as a set: the group order is by recency, and the seeded rows
			// share a timestamp, so asserting positions here would be asserting a
			// tiebreak rather than the visibility rule this case is about.
			got := map[int]bool{}
			for _, m := range g.Movies {
				got[m.TMDBID] = true
			}
			if len(got) != len(g.Movies) || len(g.Movies) != len(tc.wantMovies) {
				t.Fatalf("movies = %+v, want tmdb ids %v", g.Movies, tc.wantMovies)
			}
			for _, want := range tc.wantMovies {
				if !got[want] {
					t.Errorf("movie tmdb %d missing from %v", want, keys2(got))
				}
			}
			if tc.wantShowN == 0 {
				if len(g.Shows) != 0 {
					t.Fatalf("shows = %+v, want none", g.Shows)
				}
				return
			}
			if len(g.Shows) != 1 {
				t.Fatalf("shows = %d, want 1", len(g.Shows))
			}
			show := g.Shows[0]
			// The leak this guards: alice and bob both requested this show, so a
			// missing predicate shows each of them the other's episode in the count.
			if show.Counts.Total != tc.wantShowN {
				t.Errorf("show counts = %+v, want %d rows", show.Counts, tc.wantShowN)
			}
			if len(show.Seasons) != 1 || show.Seasons[0].Counts.Total != tc.wantShowN {
				t.Errorf("season counts = %+v, want %d rows", show.Seasons, tc.wantShowN)
			}
		})
	}
}

// TestListQueueFilteredCannotWidenVisibility: a filter is a NARROWING device. Naming
// another person's title in one must not reach their rows, and an anonymous caller
// stays at nothing no matter how specific the filter is.
func TestListQueueFilteredCannotWidenVisibility(t *testing.T) {
	s := newTestStore(t)
	seed := []QueueItem{
		{TMDBID: 1, MediaType: "movie", Title: "Alice Only", RequestedBy: "alice"},
		// Ownerless (requested_by defaults to ''), so "anonymous" must be an explicit
		// early return and not a requested_by='' predicate that would match this row.
		{TMDBID: 9, MediaType: "movie", Title: "Ownerless", RequestedBy: ""},
	}
	for i := range seed {
		if _, err := s.Enqueue(&seed[i]); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name    string
		viewer  string
		isAdmin bool
		tmdb    int
		want    int
	}{
		{"another user naming the title explicitly", "bob", false, 1, 0},
		{"anonymous naming the title explicitly", "", false, 1, 0},
		{"anonymous naming an ownerless row", "", false, 9, 0},
		{"the owner", "alice", false, 1, 1},
		{"an admin", "root", true, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.ListQueueFiltered(vw(tc.viewer, tc.isAdmin), QueueFilter{TMDBID: tc.tmdb, MediaType: "movie"})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tc.want {
				t.Fatalf("got %d rows, want %d", len(rows), tc.want)
			}
		})
	}
}

func TestRedactedStripsSecrets(t *testing.T) {
	it := Item{Magnet: "magnet:?xt=urn:btih:abc&tr=secret", StrmPath: "/srv/media/x.strm", RequestedBy: "alice", Title: "Keep"}
	r := it.Redacted()
	if r.Magnet != "" || r.StrmPath != "" || r.RequestedBy != "" {
		t.Fatalf("Item.Redacted left a secret: %+v", r)
	}
	if r.Title != "Keep" {
		t.Fatalf("Item.Redacted dropped a public field")
	}

	q := QueueItem{MagnetOverride: "magnet:?x", StrmPath: "/srv/x.strm", RequestedBy: "alice", Title: "Keep"}
	rq := q.Redacted()
	if rq.MagnetOverride != "" || rq.StrmPath != "" || rq.RequestedBy != "" {
		t.Fatalf("QueueItem.Redacted left a secret: %+v", rq)
	}
	if sub := (Subscription{RequestedBy: "alice"}).Redacted(); sub.RequestedBy != "" {
		t.Fatalf("Subscription.Redacted left requested_by")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keys2(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
