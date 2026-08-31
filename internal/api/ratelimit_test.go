package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoginLimiterBackoff(t *testing.T) {
	l := newLoginLimiter()

	// Within the burst, attempts are allowed.
	for i := 0; i < l.burst; i++ {
		if ok, _ := l.Allow("10.0.0.1", "bob"); !ok {
			t.Fatalf("attempt %d blocked while still inside the burst of %d", i+1, l.burst)
		}
		l.Fail("10.0.0.1", "bob")
	}
	// One past the burst triggers backoff.
	l.Fail("10.0.0.1", "bob")
	ok, wait := l.Allow("10.0.0.1", "bob")
	if ok {
		t.Fatal("attempts past the burst should be blocked")
	}
	if wait <= 0 {
		t.Fatalf("wait = %v, want a positive backoff", wait)
	}
	if wait > l.maxWait {
		t.Fatalf("wait = %v, exceeds the cap %v", wait, l.maxWait)
	}
}

// Keyed by BOTH ip and username: a different IP guessing the SAME account is still
// throttled, which is what stops a distributed guessing run.
// A CLEAN address is never refused because of somebody else's attack on that username.
//
// This test used to assert the opposite, and the opposite is a denial of service: five
// failed POSTs for username=admin, from anywhere, no credentials required, and the real
// admin cannot reach their own dashboard for five minutes — renewable indefinitely. On a
// single-admin install that locks the operator out of their own machine, and it is far
// cheaper to mount than the distributed guessing the rule was meant to stop.
func TestUsernameBackoffDoesNotLockOutAnUnrelatedAddress(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i <= l.burst; i++ {
		l.Fail("10.0.0.1", "bob")
	}
	if ok, _ := l.Allow("10.0.0.99", "bob"); !ok {
		t.Fatal("an address that has never failed here was locked out of bob's account " +
			"because a different address attacked it — that is a trivial admin lockout")
	}
}

// ...but an address that HAS failed here is subject to the username's backoff too, so an
// attacker cannot escape their own budget by guessing one account from one host.
func TestUsernameBackoffStillBindsAnAddressThatHasFailed(t *testing.T) {
	l := newLoginLimiter()
	// Spread the failures so neither bucket alone would trip on the attacker's own address
	// before the username bucket does.
	for i := 0; i <= l.burst; i++ {
		l.Fail("10.0.0.1", "bob")
	}
	l.Fail("10.0.0.2", "bob") // this address now has a failure of its own
	if ok, _ := l.Allow("10.0.0.2", "bob"); ok {
		t.Fatal("an address that has failed here was not bound by the username backoff")
	}
}

// ...and a different account from the same IP is also throttled, so one host cannot
// walk every username.
func TestLoginLimiterBlocksAcrossUsersForOneIP(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i <= l.burst; i++ {
		l.Fail("10.0.0.1", "bob")
	}
	if ok, _ := l.Allow("10.0.0.1", "alice"); ok {
		t.Fatal("one IP was allowed to move on to another account")
	}
}

// An unrelated user on an unrelated IP must be unaffected — a lockout must not become
// a denial of service against everyone else.
func TestLoginLimiterDoesNotBlockUnrelatedTraffic(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i <= l.burst; i++ {
		l.Fail("10.0.0.1", "bob")
	}
	if ok, _ := l.Allow("10.0.0.99", "alice"); !ok {
		t.Fatal("an unrelated user on an unrelated IP was blocked")
	}
}

func TestLoginLimiterSucceedClearsCounters(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < l.burst; i++ {
		l.Fail("10.0.0.1", "bob")
	}
	l.Succeed("10.0.0.1", "bob")
	for i := 0; i < l.burst; i++ {
		if ok, _ := l.Allow("10.0.0.1", "bob"); !ok {
			t.Fatalf("attempt %d blocked after a successful login reset the counters", i+1)
		}
		l.Fail("10.0.0.1", "bob")
	}
}

func TestLoginLimiterGCBoundsMemory(t *testing.T) {
	l := newLoginLimiter()
	l.window = time.Nanosecond // everything ages out immediately
	for i := 0; i < 3000; i++ {
		l.Fail("10.0.0.1", string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n > 2000 {
		t.Fatalf("the limiter map grew to %d entries; GC is not bounding it", n)
	}
}

// The HTTP handler must return 429 with Retry-After once the limiter trips.
func TestAPILoginHandlerRateLimits(t *testing.T) {
	s := setupAPI(t)
	mkUser(t, s, "bob", "password123", false)

	var last *httptest.ResponseRecorder
	for i := 0; i < limiter.burst+3; i++ {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/login",
			strings.NewReader(`{"username":"bob","password":"wrong"}`))
		r.RemoteAddr = "10.0.0.5:1234"
		last = httptest.NewRecorder()
		APILoginHandler(last, r)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures status = %d, want 429", limiter.burst+3, last.Code)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Error("a 429 should carry Retry-After")
	}

	// Even the CORRECT password is refused while the backoff is in effect — otherwise
	// the limiter would not actually slow an attacker down.
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"username":"bob","password":"password123"}`))
	r.RemoteAddr = "10.0.0.5:1234"
	w := httptest.NewRecorder()
	APILoginHandler(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the backoff to apply regardless of correctness", w.Code)
	}
}

func TestClientIPIgnoresForwardedHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	r.RemoteAddr = "10.0.0.7:5555"
	// Trusting this header would let an attacker mint a fresh bucket for every attempt.
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := clientIP(r); got != "10.0.0.7" {
		t.Fatalf("clientIP = %q, want the real socket address", got)
	}
}

// TestUnknownUserStillCostsABcryptCompare guards against the username-enumeration
// timing oracle: the old code returned immediately for an unknown user, skipping bcrypt.
func TestUnknownUserStillCostsABcryptCompare(t *testing.T) {
	s := setupAPI(t)
	mkUser(t, s, "bob", "password123", false)

	measure := func(username string) time.Duration {
		start := time.Now()
		_, _ = checkCredentials(username, "somepassword")
		return time.Since(start)
	}
	known := measure("bob")
	unknown := measure("definitely-not-a-user")

	// Both must do real work. A skipped bcrypt is orders of magnitude faster; requiring
	// the unknown path to be at least a fifth of the known one catches that without
	// being flaky on a loaded machine.
	if unknown < known/5 {
		t.Fatalf("unknown-user path took %v vs %v for a known user — the bcrypt compare is being skipped", unknown, known)
	}
}
