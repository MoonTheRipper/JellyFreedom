package update

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// releaseJSON is a minimal GitHub releases/latest payload.
func releaseJSON(tag string) string {
	return fmt.Sprintf(`{"tag_name":%q,"html_url":"https://github.com/MoonTheRipper/JellyFreedom/releases/tag/%s",
	  "body":"## What's Changed\n- Fixed the thing\n- Added the other thing\n","published_at":"2026-08-22T00:00:00Z",
	  "draft":false,"prerelease":false}`, tag, tag)
}

// newTestChecker returns a Checker pointed at a stub GitHub plus the request counter.
func newTestChecker(t *testing.T, current string, h http.HandlerFunc) (*Checker, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := NewChecker(current)
	c.SetAPIURL(srv.URL)
	return c, &hits
}

func okHandler(tag string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, releaseJSON(tag))
	}
}

func TestCheckAvailable(t *testing.T) {
	c, _ := newTestChecker(t, "0.3.0", okHandler("v0.4.0"))
	res := c.Check(context.Background(), false)
	if res.Error != "" {
		t.Fatalf("unexpected error: %q", res.Error)
	}
	if !res.Available {
		t.Errorf("available = false, want true")
	}
	if res.Current != "0.3.0" || res.Latest != "v0.4.0" {
		t.Errorf("current/latest = %q/%q", res.Current, res.Latest)
	}
	if res.URL == "" || res.PublishedAt != "2026-08-22T00:00:00Z" || res.CheckedAt == "" {
		t.Errorf("metadata missing: %+v", res)
	}
	if len(res.Notes) != 2 || res.Notes[0] != "Fixed the thing" {
		t.Errorf("notes = %q", res.Notes)
	}
}

func TestCheckNotAvailable(t *testing.T) {
	for _, tag := range []string{"v0.3.0", "v0.2.9", "v0.3.0-rc1"} {
		c, _ := newTestChecker(t, "0.3.0", okHandler(tag))
		res := c.Check(context.Background(), false)
		if res.Available {
			t.Errorf("latest=%s over current=0.3.0: available = true, want false", tag)
		}
		if res.Error != "" {
			t.Errorf("latest=%s: unexpected error %q", tag, res.Error)
		}
	}
}

// A source build must never be offered a release that would overwrite it.
func TestCheckDevBuildNeverAvailable(t *testing.T) {
	c, _ := newTestChecker(t, "dev", okHandler("v99.0.0"))
	res := c.Check(context.Background(), false)
	if res.Available {
		t.Fatalf("a dev build was offered an update: %+v", res)
	}
	if res.Current != "dev" || res.Latest != "v99.0.0" {
		t.Errorf("dev result should still report both versions: %+v", res)
	}
	if res.Error != "" {
		t.Errorf("a dev build is not an error state, got %q", res.Error)
	}
}

// The dashboard calls this on every load: the cache must stop the second outbound call.
func TestCheckCaches(t *testing.T) {
	c, hits := newTestChecker(t, "0.3.0", okHandler("v0.4.0"))
	for i := 0; i < 5; i++ {
		if res := c.Check(context.Background(), false); !res.Available {
			t.Fatalf("call %d: available = false", i)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("outbound calls = %d, want 1 (the cache did not hold)", n)
	}

	// ?refresh=1 bypasses it.
	c.Check(context.Background(), true)
	if n := hits.Load(); n != 2 {
		t.Errorf("after refresh: outbound calls = %d, want 2", n)
	}

	// And it expires.
	c.mu.Lock()
	c.cachedAt = time.Now().Add(-CacheTTL - time.Minute)
	c.mu.Unlock()
	c.Check(context.Background(), false)
	if n := hits.Load(); n != 3 {
		t.Errorf("after expiry: outbound calls = %d, want 3", n)
	}
}

// A failure is cached only briefly, so the banner recovers when the network does.
func TestCheckFailureIsCachedBriefly(t *testing.T) {
	c, hits := newTestChecker(t, "0.3.0", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	c.Check(context.Background(), false)
	c.Check(context.Background(), false)
	if n := hits.Load(); n != 1 {
		t.Errorf("outbound calls = %d, want 1 (failures are negative-cached)", n)
	}
	c.mu.Lock()
	c.cachedAt = time.Now().Add(-failureTTL - time.Second)
	c.mu.Unlock()
	if c.Check(context.Background(), false); hits.Load() != 2 {
		t.Errorf("a stale failure should be retried, calls = %d", hits.Load())
	}
	if CacheTTL <= failureTTL {
		t.Errorf("the failure TTL must be shorter than the success TTL")
	}
}

// Every failure mode: 200 with available:false and a readable error, never a panic.
func TestCheckFailuresAreSoft(t *testing.T) {
	cases := []struct {
		name string
		h    http.HandlerFunc
	}{
		{"rate limited", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }},
		{"too many requests", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }},
		{"not found", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }},
		{"server error", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }},
		{"malformed json", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "{not json") }},
		{"empty body", func(w http.ResponseWriter, r *http.Request) {}},
		{"html error page", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>nope</html>") }},
		{"no tag", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"tag_name":"","body":"x"}`) }},
		{"garbage tag", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"tag_name":"latest","body":"x"}`) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestChecker(t, "0.3.0", tc.h)
			res := c.Check(context.Background(), false)
			if res.Available {
				t.Errorf("available = true on a failed check")
			}
			if res.Error == "" {
				t.Errorf("no error message on a failed check: %+v", res)
			}
			if res.Notes == nil {
				t.Errorf("notes must be a non-nil slice so the JSON is [] not null")
			}
			if res.Current != "0.3.0" || res.CheckedAt == "" {
				t.Errorf("a failed check must still report current/checked_at: %+v", res)
			}
		})
	}
}

// Offline: the endpoint is unreachable, not merely unhappy.
func TestCheckOffline(t *testing.T) {
	c := NewChecker("0.3.0")
	// Reserved TEST-NET-1 address that cannot route, plus a short client timeout.
	c.SetAPIURL("http://192.0.2.1:9/latest")
	c.client = &http.Client{Timeout: 250 * time.Millisecond}
	res := c.Check(context.Background(), false)
	if res.Available || res.Error == "" {
		t.Errorf("offline check = %+v, want available:false with an error", res)
	}
}

// The cache is shared mutable state; this codebase has had a race on exactly that.
func TestCheckConcurrent(t *testing.T) {
	c, hits := newTestChecker(t, "0.3.0", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, releaseJSON("v0.4.0"))
	})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := c.Check(context.Background(), i%8 == 0)
			_ = res.Available
		}(i)
	}
	wg.Wait()
	if hits.Load() == 0 {
		t.Errorf("no outbound calls at all")
	}
}
