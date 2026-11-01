package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"jellyfreedom/internal/indexer"
	"jellyfreedom/internal/picker"
	"jellyfreedom/internal/store"
)

func TestRotateAfter(t *testing.T) {
	mk := func(ids ...int64) []*store.Item {
		out := make([]*store.Item, len(ids))
		for i, id := range ids {
			out[i] = &store.Item{ID: id}
		}
		return out
	}
	ids := func(items []*store.Item) []int64 {
		out := make([]int64, len(items))
		for i, it := range items {
			out[i] = it.ID
		}
		return out
	}
	cases := []struct {
		name   string
		cursor int64
		want   []int64
	}{
		{"no cursor starts at the lowest id", 0, []int64{3, 5, 9, 12}},
		{"continues after the last checked item", 5, []int64{9, 12, 3, 5}},
		{"cursor between ids", 10, []int64{12, 3, 5, 9}},
		{"cursor on the last id wraps to the start", 12, []int64{3, 5, 9, 12}},
		{"cursor past the end (item deleted) wraps", 99, []int64{3, 5, 9, 12}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Input deliberately unordered: the store gives no ordering guarantee.
			got := ids(rotateAfter(mk(9, 3, 12, 5), c.cursor))
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
	if got := rotateAfter(nil, 7); len(got) != 0 {
		t.Fatalf("empty input: got %v", ids(got))
	}
}

// Successive runs that each stop after n items must, between them, reach every item —
// the property the old "always start from the top" loop lacked.
func TestRotateAfterCoversEverything(t *testing.T) {
	var items []*store.Item
	for id := int64(1); id <= 100; id++ {
		items = append(items, &store.Item{ID: id})
	}
	seen := map[int64]bool{}
	var cursor int64
	for run := 0; run < 3; run++ {
		for _, it := range rotateAfter(items, cursor)[:42] {
			seen[it.ID] = true
			cursor = it.ID
		}
	}
	if len(seen) != 100 {
		t.Fatalf("three runs of 42 reached %d of 100 items", len(seen))
	}
}

// Two real runs against a real store: the second must pick up where the first stopped.
// Before the fix every run searched the same first healthCheckMaxItems titles.
func TestHealthCheckResumesAcrossRuns(t *testing.T) {
	defer func(p time.Duration) { healthCheckItemPause = p }(healthCheckItemPause)
	healthCheckItemPause = 0

	db, err := store.Open(filepath.Join(t.TempDir(), "hc.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const total = 100
	for i := 1; i <= total; i++ {
		// Already stale and still unresolvable: the check reads them and writes nothing,
		// so the only thing that can move the next run along is the saved position.
		if err := db.Upsert(&store.Item{
			TMDBID: i, MediaType: "movie", Title: fmt.Sprintf("Title %03d", i), Year: "2000",
			StrmPath: fmt.Sprintf("/lib/t%03d.strm", i), LibraryName: "Movies", Status: "stale",
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	var mu sync.Mutex
	var searched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/search" {
			mu.Lock()
			searched = append(searched, r.URL.Query().Get("query"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/search" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	idx := indexer.New(srv.URL, "test-key")
	cfg := picker.Config{MinSeeders: 1, MaxSizeGB: 100}

	run := func() []string {
		mu.Lock()
		searched = nil
		mu.Unlock()
		if err := taskLibraryHealthCheck(context.Background(), db, idx, cfg); err != nil {
			t.Fatalf("health check: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), searched...)
	}

	first, second := run(), run()
	if len(first) != healthCheckMaxItems || len(second) != healthCheckMaxItems {
		t.Fatalf("runs searched %d and %d titles, want %d each", len(first), len(second), healthCheckMaxItems)
	}
	if first[0] != "Title 001" || first[len(first)-1] != "Title 060" {
		t.Fatalf("first run covered %q..%q, want Title 001..Title 060", first[0], first[len(first)-1])
	}
	if second[0] != "Title 061" {
		t.Fatalf("second run started at %q, want Title 061 (it restarted from the top)", second[0])
	}
	seen := map[string]bool{}
	for _, q := range append(first, second...) {
		seen[q] = true
	}
	if len(seen) != total {
		t.Fatalf("two runs reached %d of %d titles", len(seen), total)
	}
}
