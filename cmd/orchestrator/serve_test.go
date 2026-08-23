package main

import (
	"context"
	"testing"
	"time"
)

// TestResolveCooldownBlocksAfterFailure covers the probe-storm guard: a failed slow
// resolve must be remembered briefly, and a success must clear it immediately so a title
// that becomes playable is playable on the very next request.
func TestResolveCooldownBlocksAfterFailure(t *testing.T) {
	c := newResolveCooldown(time.Minute)
	const key = "movie:1:0:0"

	if _, blocked := c.blocked(key); blocked {
		t.Fatal("a fresh identity must not be blocked")
	}

	c.fail(key)
	left, blocked := c.blocked(key)
	if !blocked {
		t.Fatal("identity must be blocked immediately after a failure")
	}
	if left <= 0 || left > time.Minute {
		t.Fatalf("remaining = %v, want (0, 1m]", left)
	}

	// A different identity is unaffected — the cooldown is per title, not global.
	if _, blocked := c.blocked("movie:2:0:0"); blocked {
		t.Fatal("an unrelated identity was blocked")
	}

	c.succeed(key)
	if _, blocked := c.blocked(key); blocked {
		t.Fatal("a success must clear the cooldown at once")
	}
}

// TestResolveCooldownExpires: the block is temporary, not permanent.
func TestResolveCooldownExpires(t *testing.T) {
	c := newResolveCooldown(20 * time.Millisecond)
	c.fail("k")
	if _, blocked := c.blocked("k"); !blocked {
		t.Fatal("want blocked right after the failure")
	}
	time.Sleep(40 * time.Millisecond)
	if _, blocked := c.blocked("k"); blocked {
		t.Fatal("the cooldown outlived its TTL")
	}
}

// TestSearchLimiterBudget: the anonymous release search is capped per address, and one
// caller exhausting its budget must not affect another.
func TestSearchLimiterBudget(t *testing.T) {
	l := newSearchLimiter(3, time.Minute)
	for i := range 3 {
		if !l.allow("10.0.0.1") {
			t.Fatalf("request %d was refused inside the budget", i+1)
		}
	}
	if l.allow("10.0.0.1") {
		t.Fatal("the 4th request exceeded a budget of 3 and should have been refused")
	}
	if !l.allow("10.0.0.2") {
		t.Fatal("a different address must have its own budget")
	}
}

// TestSearchLimiterWindowSlides: budget is per window, not for the process lifetime.
func TestSearchLimiterWindowSlides(t *testing.T) {
	l := newSearchLimiter(1, 20*time.Millisecond)
	if !l.allow("ip") {
		t.Fatal("first request refused")
	}
	if l.allow("ip") {
		t.Fatal("second request inside the window should be refused")
	}
	time.Sleep(40 * time.Millisecond)
	if !l.allow("ip") {
		t.Fatal("the window did not slide")
	}
}

// TestResolveGroupSingleFlight guards the property the cooldown sits next to: two
// concurrent resolves of one identity must not both run.
func TestResolveGroupSingleFlight(t *testing.T) {
	g := newResolveGroup()
	release, ok := g.lock(context.Background(), "k")
	if !ok {
		t.Fatal("first lock failed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, ok := g.lock(ctx, "k"); ok {
		t.Fatal("a second holder acquired the same key while it was held")
	}
	release()

	release2, ok := g.lock(context.Background(), "k")
	if !ok {
		t.Fatal("the key was not reusable after release")
	}
	release2()
}
