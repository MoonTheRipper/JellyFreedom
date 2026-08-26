package store

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"
)

// ── Per-library visibility ────────────────────────────────────────────────────
//
// These tests are written NEGATIVE-FIRST on purpose. The interesting assertion is never
// "the admin can see the adults' library" — that one passes even with the whole feature
// deleted. It is "the restricted account cannot see it HERE, and here, and here", across
// every read path at once, which is why the bulk of this file is one table over the read
// surface rather than a test per method.
//
// The table is paired with TestEveryViewerScopedMethodIsInTheVisibilityTable below,
// which walks *Store by reflection and fails if a method that takes a Viewer is missing
// from it. That is the part that makes this suite hold up over time: a read path added
// later without a library predicate does not quietly leak, it fails a test with the name
// of the method nobody covered.

const (
	libKids   = "Kids"
	libAdults = "Adults"
)

// libFixture is the household this file keeps testing: two libraries, and four viewers
// standing for the four ways an account can relate to them.
type libFixture struct {
	s *Store

	admin Viewer // bypasses the gate entirely
	alice Viewer // granted Kids only — the case the feature exists for
	bob   Viewer // a real account with NO grants — the default-deny case
	anon  Viewer // nobody signed in

	// Per (owner, library) row identifiers, so a probe can point at a specific row in a
	// specific library and ask whether it was reachable.
	pendingQueue  map[string]map[string]int64 // owner → library → queue id (status pending)
	terminalQueue map[string]map[string]int64 // owner → library → queue id (status done)
	subs          map[string]map[string]int64 // owner → library → subscription id
	itemTMDB      map[string]int              // library → the tmdb id of its library item
}

// libraries returns the fixture's library names in a stable order.
func (f *libFixture) libraries() []string { return []string{libKids, libAdults} }

// seedLibFixture builds the household. Every row it writes is PUBLIC (is_private=0) and
// belongs to a real user, so nothing here is hidden by the pre-existing per-item privacy
// gate — anything a viewer fails to see, they fail to see because of the library gate
// and for no other reason. That isolation is what makes a failure in this file mean
// something specific.
func seedLibFixture(t *testing.T) *libFixture {
	t.Helper()
	s := newTestStore(t)
	f := &libFixture{
		s:             s,
		admin:         mustUser(t, s, "root", true),
		alice:         mustUser(t, s, "alice", false),
		bob:           mustUser(t, s, "bob", false),
		anon:          Viewer{},
		pendingQueue:  map[string]map[string]int64{},
		terminalQueue: map[string]map[string]int64{},
		subs:          map[string]map[string]int64{},
		itemTMDB:      map[string]int{},
	}

	// Alice may see Kids and nothing else. Bob is granted nothing at all, which is what
	// a brand-new account looks like: the default is DENY, so this is not a special
	// setup step, it is the absence of one.
	if err := s.SetLibraryAccess(f.alice.UserID, []string{libKids}); err != nil {
		t.Fatalf("SetLibraryAccess: %v", err)
	}

	// One library item per library, public and ready.
	for i, lib := range f.libraries() {
		tmdbID := 100 + i*100
		f.itemTMDB[lib] = tmdbID
		item := &Item{
			MediaType: "movie", Title: lib + " Movie", Year: "2024",
			InfoHash: fmt.Sprintf("%040d", tmdbID), StrmPath: "/srv/" + lib + "/movie.strm",
			LibraryName: lib, Status: "ready", Seeders: 10, Updated: time.Now(),
			RequestedBy: "alice", IsPrivate: false,
		}
		item.SetTMDBIdentity(tmdbID)
		if err := s.Upsert(item); err != nil {
			t.Fatalf("Upsert %s: %v", lib, err)
		}
	}

	// Queue rows and subscriptions for BOTH non-admin accounts in BOTH libraries. Alice
	// owning a row in Adults is the case that matters most: ownership alone used to be
	// the whole gate, so if the library predicate were missing she would still read her
	// own row straight back out of a library she was never granted.
	for _, owner := range []string{"alice", "bob"} {
		f.pendingQueue[owner] = map[string]int64{}
		f.terminalQueue[owner] = map[string]int64{}
		f.subs[owner] = map[string]int64{}
		for i, lib := range f.libraries() {
			base := 1000 + i*1000
			if owner == "bob" {
				base += 500
			}
			f.pendingQueue[owner][lib] = seedQueueRow(t, s, owner, lib, base+1, "pending")
			f.terminalQueue[owner][lib] = seedQueueRow(t, s, owner, lib, base+2, "done")
			// subscriptions is UNIQUE(tmdb_id, season), so the two owners cannot share a
			// tmdb id even across different libraries.
			f.subs[owner][lib] = seedSubscription(t, s, owner, lib, base+3)
		}
	}
	return f
}

func seedQueueRow(t *testing.T, s *Store, owner, library string, tmdbID int, status string) int64 {
	t.Helper()
	id, err := s.Enqueue(&QueueItem{
		TMDBID: tmdbID, MediaType: "movie", Title: fmt.Sprintf("%s %s", library, status),
		LibraryName: library, RequestedBy: owner,
	})
	if err != nil {
		t.Fatalf("Enqueue(%s,%s): %v", owner, library, err)
	}
	if status != "pending" {
		row, err := s.GetQueueItem(id)
		if err != nil || row == nil {
			t.Fatalf("GetQueueItem(%d): %v", id, err)
		}
		row.Status, row.Stage = status, StageDone
		if err := s.UpdateQueue(row); err != nil {
			t.Fatalf("UpdateQueue(%d): %v", id, err)
		}
	}
	return id
}

func seedSubscription(t *testing.T, s *Store, owner, library string, tmdbID int) int64 {
	t.Helper()
	if err := s.UpsertSubscription(&Subscription{
		TMDBID: tmdbID, Season: 1, Title: library + " Show",
		LibraryName: library, RequestedBy: owner,
	}); err != nil {
		t.Fatalf("UpsertSubscription(%s,%s): %v", owner, library, err)
	}
	subs, err := s.ListSubscriptions(Viewer{IsAdmin: true})
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	for _, sub := range subs {
		if sub.TMDBID == tmdbID {
			return sub.ID
		}
	}
	t.Fatalf("subscription %d not found after upsert", tmdbID)
	return 0
}

// readPath is one way of asking the store a question, reduced to the only answer this
// file cares about: which libraries did it just disclose the existence or contents of?
//
// Reducing every path to a set of library names is what lets one table cover methods
// that return items, queue rows, aggregate counts, plain booleans and lists of strings.
// A path that returns a title rather than a library name still discloses the library,
// because the fixture gives each library its own titles.
type readPath struct {
	name string
	// owner is the account whose rows this path can reach, or "" if the path is not
	// scoped by ownership. Paths scoped by ownership are probed as their owner.
	probe func(t *testing.T, f *libFixture, v Viewer) []string
}

// visibilityReadPaths is the audited read surface. EVERY entry must answer with the same
// set for the same viewer: the libraries that viewer is allowed to see, and no others.
var visibilityReadPaths = []readPath{
	{"ListVisible", func(t *testing.T, f *libFixture, v Viewer) []string {
		items, err := f.s.ListVisible(v)
		if err != nil {
			t.Fatalf("ListVisible: %v", err)
		}
		return libsOfItems(items)
	}},
	{"GetStatusByTMDBIDs", func(t *testing.T, f *libFixture, v Viewer) []string {
		ids := []int{f.itemTMDB[libKids], f.itemTMDB[libAdults]}
		got, err := f.s.GetStatusByTMDBIDs(ids, v)
		if err != nil {
			t.Fatalf("GetStatusByTMDBIDs: %v", err)
		}
		var out []string
		for _, item := range got {
			out = append(out, item.LibraryName)
		}
		return normalise(out)
	}},
	{"ListQueue", func(t *testing.T, f *libFixture, v Viewer) []string {
		rows, err := f.s.ListQueue(v)
		if err != nil {
			t.Fatalf("ListQueue: %v", err)
		}
		return libsOfQueue(rows)
	}},
	{"ListQueueFiltered", func(t *testing.T, f *libFixture, v Viewer) []string {
		// Probed through the SCOPED shape, because a filter is the obvious place to
		// accidentally rebuild the query without the gate. Naming a tmdb id must never
		// widen what the caller could already see.
		var out []string
		for _, lib := range f.libraries() {
			for _, owner := range []string{"alice", "bob"} {
				id := f.pendingQueue[owner][lib]
				row, err := f.s.GetQueueItem(id) // unfiltered, to learn the tmdb id
				if err != nil || row == nil {
					t.Fatalf("GetQueueItem(%d): %v", id, err)
				}
				rows, err := f.s.ListQueueFiltered(v, QueueFilter{TMDBID: row.TMDBID, MediaType: "movie"})
				if err != nil {
					t.Fatalf("ListQueueFiltered: %v", err)
				}
				out = append(out, libsOfQueue(rows)...)
			}
		}
		return normalise(out)
	}},
	{"ListQueueGroups", func(t *testing.T, f *libFixture, v Viewer) []string {
		groups, err := f.s.ListQueueGroups(v)
		if err != nil {
			t.Fatalf("ListQueueGroups: %v", err)
		}
		// A group carries no library_name — only a title and a count. The fixture titles
		// each row after its library precisely so that a leaked COUNT is still a leaked
		// library here, which is the disclosure the aggregate has to prevent.
		var out []string
		for _, g := range append(append([]QueueShowGroup{}, groups.Shows...), groups.Movies...) {
			for _, lib := range f.libraries() {
				if len(g.Title) >= len(lib) && g.Title[:len(lib)] == lib {
					out = append(out, lib)
				}
			}
		}
		return normalise(out)
	}},
	{"ListSubscriptions", func(t *testing.T, f *libFixture, v Viewer) []string {
		subs, err := f.s.ListSubscriptions(v)
		if err != nil {
			t.Fatalf("ListSubscriptions: %v", err)
		}
		var out []string
		for _, sub := range subs {
			out = append(out, sub.LibraryName)
		}
		return normalise(out)
	}},
	{"VisibleQueueItem", func(t *testing.T, f *libFixture, v Viewer) []string {
		var out []string
		for _, lib := range f.libraries() {
			for _, owner := range []string{"alice", "bob"} {
				row, err := f.s.VisibleQueueItem(f.pendingQueue[owner][lib], v)
				if err != nil {
					t.Fatalf("VisibleQueueItem: %v", err)
				}
				if row != nil {
					out = append(out, row.LibraryName)
				}
			}
		}
		return normalise(out)
	}},
	{"VisibleItemsByTMDB", func(t *testing.T, f *libFixture, v Viewer) []string {
		var out []string
		for _, lib := range f.libraries() {
			items, err := f.s.VisibleItemsByTMDB(f.itemTMDB[lib], "movie", v)
			if err != nil {
				t.Fatalf("VisibleItemsByTMDB: %v", err)
			}
			out = append(out, libsOfItems(items)...)
		}
		return normalise(out)
	}},
	{"VisibleItemsByTitle", func(t *testing.T, f *libFixture, v Viewer) []string {
		var out []string
		for _, lib := range f.libraries() {
			items, err := f.s.VisibleItemsByTitle(
				ProviderTMDB, fmt.Sprintf("%d", f.itemTMDB[lib]), "movie", v)
			if err != nil {
				t.Fatalf("VisibleItemsByTitle: %v", err)
			}
			out = append(out, libsOfItems(items)...)
		}
		return normalise(out)
	}},
	{"FilterLibraries", func(t *testing.T, f *libFixture, v Viewer) []string {
		got, err := f.s.FilterLibraries(v, f.libraries())
		if err != nil {
			t.Fatalf("FilterLibraries: %v", err)
		}
		return normalise(got)
	}},
	{"CanUseLibrary", func(t *testing.T, f *libFixture, v Viewer) []string {
		var out []string
		for _, lib := range f.libraries() {
			ok, err := f.s.CanUseLibrary(v, lib)
			if err != nil {
				t.Fatalf("CanUseLibrary: %v", err)
			}
			if ok {
				out = append(out, lib)
			}
		}
		return normalise(out)
	}},
}

// TestLibraryVisibilityAcrossEveryReadPath is the core assertion of the whole feature:
// a library a viewer may not see is invisible in EVERY read path, not merely in the one
// somebody remembered to filter.
func TestLibraryVisibilityAcrossEveryReadPath(t *testing.T) {
	viewers := []struct {
		name string
		pick func(f *libFixture) Viewer
		want []string
	}{
		{"admin sees every library", func(f *libFixture) Viewer { return f.admin }, []string{libAdults, libKids}},
		{"granted user sees only the granted library", func(f *libFixture) Viewer { return f.alice }, []string{libKids}},
		{"ungranted user sees nothing", func(f *libFixture) Viewer { return f.bob }, nil},
		{"anonymous sees nothing", func(f *libFixture) Viewer { return f.anon }, nil},
	}

	for _, path := range visibilityReadPaths {
		for _, vc := range viewers {
			t.Run(path.name+"/"+vc.name, func(t *testing.T) {
				f := seedLibFixture(t)
				got := path.probe(t, f, vc.pick(f))
				if !sameSet(got, vc.want) {
					t.Fatalf("%s disclosed libraries %v, want %v", path.name, got, vc.want)
				}
			})
		}
	}
}

// mutationPath is the write-side equivalent. A mutation discloses through its
// rows-affected count — "1 row cancelled" says the row is there — so the same table
// shape applies, reduced to "which libraries did this actually take effect in".
type mutationPath struct {
	name  string
	apply func(t *testing.T, f *libFixture, v Viewer, owner string) []string
}

var visibilityMutationPaths = []mutationPath{
	{"CancelQueueItem", func(t *testing.T, f *libFixture, v Viewer, owner string) []string {
		var out []string
		for _, lib := range f.libraries() {
			n, err := f.s.CancelQueueItem(f.pendingQueue[owner][lib], v)
			if err != nil {
				t.Fatalf("CancelQueueItem: %v", err)
			}
			if n > 0 {
				out = append(out, lib)
			}
		}
		return normalise(out)
	}},
	{"DeleteQueueItem", func(t *testing.T, f *libFixture, v Viewer, owner string) []string {
		var out []string
		for _, lib := range f.libraries() {
			n, err := f.s.DeleteQueueItem(f.terminalQueue[owner][lib], v)
			if err != nil {
				t.Fatalf("DeleteQueueItem: %v", err)
			}
			if n > 0 {
				out = append(out, lib)
			}
		}
		return normalise(out)
	}},
	{"DeleteSubscription", func(t *testing.T, f *libFixture, v Viewer, owner string) []string {
		var out []string
		for _, lib := range f.libraries() {
			n, err := f.s.DeleteSubscription(f.subs[owner][lib], v)
			if err != nil {
				t.Fatalf("DeleteSubscription: %v", err)
			}
			if n > 0 {
				out = append(out, lib)
			}
		}
		return normalise(out)
	}},
	{"DeleteFinishedQueue", func(t *testing.T, f *libFixture, v Viewer, owner string) []string {
		if _, err := f.s.DeleteFinishedQueue(v); err != nil {
			t.Fatalf("DeleteFinishedQueue: %v", err)
		}
		// The count it returns is not the interesting answer — which rows survive is.
		// Read them back as an admin, because only an admin can see all of them.
		var out []string
		for _, lib := range f.libraries() {
			row, err := f.s.VisibleQueueItem(f.terminalQueue[owner][lib], f.admin)
			if err != nil {
				t.Fatalf("VisibleQueueItem: %v", err)
			}
			if row == nil {
				out = append(out, lib) // deleted, therefore reached
			}
		}
		return normalise(out)
	}},
}

// TestLibraryVisibilityAcrossEveryMutationPath: a mutation aimed at a row in a hidden
// library must be a no-op, and must be indistinguishable from one aimed at a row that
// never existed. Both report zero rows affected.
//
// The viewers here act on THEIR OWN rows throughout, which is the point — ownership is
// satisfied, so the only thing that can stop them is the library gate.
func TestLibraryVisibilityAcrossEveryMutationPath(t *testing.T) {
	cases := []struct {
		name  string
		owner string
		pick  func(f *libFixture) Viewer
		want  []string
	}{
		{"admin reaches every library", "alice", func(f *libFixture) Viewer { return f.admin }, []string{libAdults, libKids}},
		{"granted user reaches only the granted library", "alice", func(f *libFixture) Viewer { return f.alice }, []string{libKids}},
		{"ungranted user reaches nothing, not even their own rows", "bob", func(f *libFixture) Viewer { return f.bob }, nil},
	}
	for _, path := range visibilityMutationPaths {
		for _, c := range cases {
			t.Run(path.name+"/"+c.name, func(t *testing.T) {
				f := seedLibFixture(t)
				got := path.apply(t, f, c.pick(f), c.owner)
				if !sameSet(got, c.want) {
					t.Fatalf("%s took effect in %v, want %v", path.name, got, c.want)
				}
			})
		}
	}
}

// TestEveryViewerScopedMethodIsInTheVisibilityTable is the guard that makes the two
// tables above age well.
//
// A Viewer parameter is the store's marker for "this answer depends on who is asking".
// Every method carrying one must therefore appear in the audited surface, and this test
// walks *Store by reflection to insist on it. Add a read path without a library
// predicate and it does not leak quietly — it fails here, by name, before anyone has to
// notice the missing WHERE clause in review.
func TestEveryViewerScopedMethodIsInTheVisibilityTable(t *testing.T) {
	covered := map[string]bool{}
	for _, p := range visibilityReadPaths {
		covered[p.name] = true
	}
	for _, p := range visibilityMutationPaths {
		covered[p.name] = true
	}

	viewerType := reflect.TypeOf(Viewer{})
	storeType := reflect.TypeOf(&Store{})
	found := 0
	for i := 0; i < storeType.NumMethod(); i++ {
		m := storeType.Method(i)
		takesViewer := false
		for j := 0; j < m.Type.NumIn(); j++ {
			if m.Type.In(j) == viewerType {
				takesViewer = true
				break
			}
		}
		if !takesViewer {
			continue
		}
		found++
		if !covered[m.Name] {
			t.Errorf("Store.%s takes a Viewer but is not in visibilityReadPaths or "+
				"visibilityMutationPaths — add it to the audited read surface, or it can "+
				"leak a hidden library with nothing to catch it", m.Name)
		}
	}
	if found == 0 {
		t.Fatal("reflection found no Viewer-scoped methods at all; this guard has stopped guarding anything")
	}
	for name := range covered {
		if _, ok := storeType.MethodByName(name); !ok {
			t.Errorf("the visibility table names Store.%s, which does not exist", name)
		}
	}
}

// TestLibraryAccessDefaultsToDeny states the default in one place, because it is the
// design decision most likely to be quietly reversed by someone who finds an empty app
// confusing. A brand-new account has no rows in user_library_access, and that absence is
// a denial rather than an omission.
func TestLibraryAccessDefaultsToDeny(t *testing.T) {
	s := newTestStore(t)
	fresh := mustUser(t, s, "newcomer", false)

	granted, err := s.LibraryAccess(fresh.UserID)
	if err != nil {
		t.Fatalf("LibraryAccess: %v", err)
	}
	if len(granted) != 0 {
		t.Fatalf("a new account starts with grants %v; it must start with none", granted)
	}
	for _, lib := range []string{libKids, libAdults, "Anything At All"} {
		ok, err := s.CanUseLibrary(fresh, lib)
		if err != nil {
			t.Fatalf("CanUseLibrary: %v", err)
		}
		if ok {
			t.Fatalf("a new account may use %q with no grant; the default must be deny", lib)
		}
	}
	visible, err := s.FilterLibraries(fresh, []string{libKids, libAdults})
	if err != nil {
		t.Fatalf("FilterLibraries: %v", err)
	}
	if len(visible) != 0 {
		t.Fatalf("a new account can see %v; it must see nothing until granted", visible)
	}
}

// TestSingleAdminInstallNeedsNoGrants is the "do not break the default install" test.
// The stock deployment is one admin and two libraries, and it must keep working with an
// empty user_library_access table and no configuration whatsoever.
func TestSingleAdminInstallNeedsNoGrants(t *testing.T) {
	f := seedLibFixture(t)
	// Deliberately wipe every grant in the database: the admin's access must not depend
	// on one, because the installer never creates one.
	if err := f.s.SetLibraryAccess(f.alice.UserID, nil); err != nil {
		t.Fatalf("SetLibraryAccess: %v", err)
	}
	for _, path := range visibilityReadPaths {
		got := path.probe(t, f, f.admin)
		if !sameSet(got, []string{libKids, libAdults}) {
			t.Fatalf("%s: the sole admin sees %v with no grants configured, want both libraries",
				path.name, got)
		}
	}
}

// TestEmptyLibraryNameIsExempt covers the compatibility rule that keeps an upgraded
// database usable: a row that names no library is not in a library, so the gate has
// nothing to apply and the row stays governed by the ownership and privacy rules that
// already covered it.
//
// This is not a hole. config.Load refuses a library with an empty name, so no configured
// library can ever be called "", and the request handlers resolve an empty library to a
// concrete one the caller may use before any row is written.
func TestEmptyLibraryNameIsExempt(t *testing.T) {
	s := newTestStore(t)
	carol := mustUser(t, s, "carol", false) // no grants at all

	item := &Item{
		MediaType: "movie", Title: "Legacy Row", Year: "2011",
		InfoHash: fmt.Sprintf("%040d", 7), StrmPath: "/srv/legacy.strm",
		LibraryName: "", Status: "ready", Updated: time.Now(), RequestedBy: "carol",
	}
	item.SetTMDBIdentity(7)
	if err := s.Upsert(item); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := s.Enqueue(&QueueItem{
		TMDBID: 8, MediaType: "movie", Title: "Legacy Queue Row", RequestedBy: "carol",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	items, err := s.ListVisible(carol)
	if err != nil {
		t.Fatalf("ListVisible: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("a user with no grants sees %d unassigned items, want 1 — an upgraded "+
			"database's pre-libraries rows must not vanish", len(items))
	}
	rows, err := s.ListQueue(carol)
	if err != nil {
		t.Fatalf("ListQueue: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("a user with no grants sees %d of their own unassigned queue rows, want 1", len(rows))
	}
	ok, err := s.CanUseLibrary(carol, "")
	if err != nil || !ok {
		t.Fatalf("CanUseLibrary(\"\") = %v, %v; an empty name is not a library to withhold", ok, err)
	}
}

// TestLoggingOutNeverRevealsMore is the bypass this feature would be worthless without.
//
// If an anonymous caller could see anything a signed-in restricted account could not,
// the child simply logs out. This codebase has already shipped that exact bug once, in
// the (viewerUsername, isAdmin) sentinel that treated "" as admin, so it is worth an
// explicit assertion rather than trusting that the zero Viewer stays restrictive.
func TestLoggingOutNeverRevealsMore(t *testing.T) {
	for _, path := range visibilityReadPaths {
		t.Run(path.name, func(t *testing.T) {
			f := seedLibFixture(t)
			anon := path.probe(t, f, f.anon)
			restricted := path.probe(t, f, f.alice)
			for _, lib := range anon {
				if !contains(restricted, lib) {
					t.Fatalf("%s: logging out reveals %q, which a signed-in restricted "+
						"account cannot see", path.name, lib)
				}
			}
			if len(anon) != 0 {
				t.Fatalf("%s: an anonymous caller sees libraries %v, want none", path.name, anon)
			}
		})
	}
}

// TestPrivacyAndLibraryGatesCompose: the two gates are ANDed, and neither may weaken the
// other. A private item does not escape into view because its library was granted, and a
// public item does not escape a hidden library because nobody marked it private.
func TestPrivacyAndLibraryGatesCompose(t *testing.T) {
	f := seedLibFixture(t)

	// Alice's own PRIVATE item, in the library she is granted.
	privateKids := &Item{
		MediaType: "movie", Title: "Kids Private", Year: "2024",
		InfoHash: fmt.Sprintf("%040d", 501), StrmPath: "/srv/Kids/private.strm",
		LibraryName: libKids, Status: "ready", Updated: time.Now(),
		RequestedBy: "alice", IsPrivate: true,
	}
	privateKids.SetTMDBIdentity(501)
	if err := f.s.Upsert(privateKids); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Bob's own PRIVATE item, in the library alice is granted. Alice owns the library
	// grant but not the item, so she must not see it.
	privateBob := &Item{
		MediaType: "movie", Title: "Kids Bob Private", Year: "2024",
		InfoHash: fmt.Sprintf("%040d", 502), StrmPath: "/srv/Kids/bob-private.strm",
		LibraryName: libKids, Status: "ready", Updated: time.Now(),
		RequestedBy: "bob", IsPrivate: true,
	}
	privateBob.SetTMDBIdentity(502)
	if err := f.s.Upsert(privateBob); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	items, err := f.s.ListVisible(f.alice)
	if err != nil {
		t.Fatalf("ListVisible: %v", err)
	}
	var titles []string
	for _, it := range items {
		titles = append(titles, it.Title)
	}
	if !contains(titles, "Kids Movie") {
		t.Errorf("alice lost the public item in her granted library: %v", titles)
	}
	if !contains(titles, "Kids Private") {
		t.Errorf("the library grant swallowed alice's own private item: %v", titles)
	}
	if contains(titles, "Kids Bob Private") {
		t.Errorf("a library grant exposed somebody else's private item: %v", titles)
	}
	if contains(titles, "Adults Movie") {
		t.Errorf("a public item escaped a library alice was never granted: %v", titles)
	}
}

// TestSetLibraryAccessReplaces: the admin API is a PUT of the whole set, so saving must
// mean "these and only these" rather than "these as well".
func TestSetLibraryAccessReplaces(t *testing.T) {
	s := newTestStore(t)
	u := mustUser(t, s, "dave", false)

	if err := s.SetLibraryAccess(u.UserID, []string{libKids, libAdults}); err != nil {
		t.Fatalf("SetLibraryAccess: %v", err)
	}
	if got, _ := s.LibraryAccess(u.UserID); !sameSet(got, []string{libKids, libAdults}) {
		t.Fatalf("after granting both, access = %v", got)
	}
	// Revoke Adults by sending the smaller set.
	if err := s.SetLibraryAccess(u.UserID, []string{libKids}); err != nil {
		t.Fatalf("SetLibraryAccess: %v", err)
	}
	if got, _ := s.LibraryAccess(u.UserID); !sameSet(got, []string{libKids}) {
		t.Fatalf("after replacing with Kids only, access = %v; a PUT must revoke what it omits", got)
	}
	// Duplicates are the same instruction said twice, not a failed save.
	if err := s.SetLibraryAccess(u.UserID, []string{libKids, libKids, ""}); err != nil {
		t.Fatalf("SetLibraryAccess with duplicates: %v", err)
	}
	if got, _ := s.LibraryAccess(u.UserID); !sameSet(got, []string{libKids}) {
		t.Fatalf("duplicates/blank stored as %v, want just Kids", got)
	}
	// Nothing at all is a full revocation, not a no-op.
	if err := s.SetLibraryAccess(u.UserID, nil); err != nil {
		t.Fatalf("SetLibraryAccess(nil): %v", err)
	}
	if got, _ := s.LibraryAccess(u.UserID); len(got) != 0 {
		t.Fatalf("after revoking everything, access = %v", got)
	}
}

// TestGrantsFollowTheAccountNotTheName: grants key on user_id, so renaming an account
// keeps them, and deleting one destroys them. The second half matters more — user ids
// are AUTOINCREMENT and never reused, but a stale grant row surviving a deletion would
// be a grant with no owner, waiting for one.
func TestGrantsFollowTheAccountNotTheName(t *testing.T) {
	s := newTestStore(t)
	u := mustUser(t, s, "erin", false)
	if err := s.SetLibraryAccess(u.UserID, []string{libKids}); err != nil {
		t.Fatalf("SetLibraryAccess: %v", err)
	}

	row, err := s.GetUserByID(u.UserID)
	if err != nil || row == nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	row.Username = "erin-renamed"
	if err := s.UpdateUser(row); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	renamed := ViewerOf(row)
	if ok, err := s.CanUseLibrary(renamed, libKids); err != nil || !ok {
		t.Fatalf("renaming an account revoked its library access (%v, %v)", ok, err)
	}

	if err := s.DeleteUser(u.UserID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	got, err := s.LibraryAccess(u.UserID)
	if err != nil {
		t.Fatalf("LibraryAccess: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("deleting the account left grants %v behind; ON DELETE CASCADE is not working", got)
	}
}

// TestZeroViewerIsTheMostRestrictive: the zero value is relied on as a fail-closed
// default all over the handlers, so it gets an assertion of its own rather than a
// comment.
func TestZeroViewerIsTheMostRestrictive(t *testing.T) {
	var v Viewer
	if v.IsAdmin {
		t.Fatal("the zero Viewer is an admin")
	}
	if !v.Anonymous() {
		t.Fatal("the zero Viewer is not anonymous")
	}
	if scope, args := v.libraryScope(); scope == "" || len(args) != 1 || args[0] != int64(0) {
		t.Fatalf("the zero Viewer bypasses the library gate: scope=%q args=%v", scope, args)
	}
	if got := ViewerOf(nil); got != (Viewer{}) {
		t.Fatalf("ViewerOf(nil) = %+v, want the zero Viewer", got)
	}
}

// ── small helpers ─────────────────────────────────────────────────────────────

func libsOfItems(items []*Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.LibraryName)
	}
	return normalise(out)
}

func libsOfQueue(rows []*QueueItem) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.LibraryName)
	}
	return normalise(out)
}

// normalise deduplicates and sorts, and drops the empty library name — an unassigned row
// is not a library and never counts as a disclosure.
func normalise(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func sameSet(a, b []string) bool {
	a, b = normalise(a), normalise(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
