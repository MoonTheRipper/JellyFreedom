package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginLimiter throttles failed authentication attempts.
//
// Login had NO rate limiting at all: an attacker on the LAN could try passwords as
// fast as bcrypt would answer, against a username list they could enumerate from the
// old early-return timing difference (see authenticateWithDummyCompare).
//
// The limiter is keyed independently by client IP and by username. Keying only by IP lets
// a botnet spray one account; keying only by username lets one host walk every account.
//
// Blocking on EITHER bucket, which is what this used to do, hands an attacker a trivial
// lockout DoS: five failed POSTs for username=admin, from anywhere, no credentials needed,
// and the real admin cannot log in to their own machine for five minutes — renewable
// forever. On a single-admin install that is a total denial of the dashboard, and it is far
// easier to mount than the distributed guessing run the username bucket defends against.
//
// So the username bucket only binds an IP that has ALREADY failed here. A clean IP is never
// refused because of somebody else's attack on that name — which is the property the old
// comment claimed and the old code did not have — while an attacker's own address, which by
// definition accumulates failures, still hits the union of both budgets.
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	// tunables
	burst   int           // failures allowed before backoff begins
	window  time.Duration // how long a failure counts against the bucket
	maxWait time.Duration // cap on the computed backoff
}

type bucket struct {
	failures int
	last     time.Time
	blocked  time.Time // attempts before this instant are refused
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		buckets: make(map[string]*bucket),
		burst:   5,
		window:  15 * time.Minute,
		maxWait: 5 * time.Minute,
	}
}

var limiter = newLoginLimiter()

// Allow reports whether an attempt for this ip+username pair may proceed, and if not,
// how long the caller should be told to wait.
func (l *loginLimiter) Allow(ip, username string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gc(now)

	ipBucket := l.buckets["ip:"+ip]
	if ip != "" && ipBucket != nil && now.Before(ipBucket.blocked) {
		return false, ipBucket.blocked.Sub(now).Round(time.Second)
	}
	// Only an address that has failed here is subject to the username's backoff. Without
	// this condition, anyone could lock a known account out from any address.
	if username != "" && ipBucket != nil && ipBucket.failures > 0 {
		if b := l.buckets["user:"+strings.ToLower(username)]; b != nil && now.Before(b.blocked) {
			return false, b.blocked.Sub(now).Round(time.Second)
		}
	}
	return true, 0
}

// Fail records a failed attempt and applies exponential backoff past the burst.
func (l *loginLimiter) Fail(ip, username string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gc(now)
	for _, key := range l.keys(ip, username) {
		b := l.buckets[key]
		if b == nil || now.Sub(b.last) > l.window {
			b = &bucket{}
			l.buckets[key] = b
		}
		b.failures++
		b.last = now
		if b.failures > l.burst {
			// 1s, 2s, 4s, ... capped.
			d := time.Second << uint(min(b.failures-l.burst-1, 20))
			if d > l.maxWait || d <= 0 {
				d = l.maxWait
			}
			b.blocked = now.Add(d)
		}
	}
}

// Succeed clears the counters for a successful login.
func (l *loginLimiter) Succeed(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range l.keys(ip, username) {
		delete(l.buckets, key)
	}
}

func (l *loginLimiter) keys(ip, username string) []string {
	keys := make([]string, 0, 2)
	if ip != "" {
		keys = append(keys, "ip:"+ip)
	}
	if username != "" {
		keys = append(keys, "user:"+strings.ToLower(username))
	}
	return keys
}

// gc drops buckets that have aged out, so the map cannot grow without bound from
// spray traffic across many usernames. Caller holds the lock.
func (l *loginLimiter) gc(now time.Time) {
	if len(l.buckets) < 1024 {
		return
	}
	for k, b := range l.buckets {
		if now.Sub(b.last) > l.window && now.After(b.blocked) {
			delete(l.buckets, k)
		}
	}
}

// clientIP extracts the request's source IP. X-Forwarded-For is deliberately NOT
// trusted: this service is meant to be reached directly on the LAN, and honouring a
// client-supplied header would let an attacker mint a fresh rate-limit bucket per try.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
