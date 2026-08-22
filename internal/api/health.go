package api

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"
)

// ── Public health summary ─────────────────────────────────────────────────────
//
// The header health dot used to be driven by GET /api/status, which was PUBLIC and
// returned the WireGuard peer public key, the VPN endpoint IP, and every service's
// bind address to any unauthenticated LAN visitor. /api/leak, also public, returned
// the host's real public IPv4 and the VPN exit IP. On a project whose entire purpose
// is not leaking exactly those, that was the worst finding in the audit.
//
// Both moved behind RequireAdmin. This endpoint replaces what the dot actually needed:
// a boolean and a list of component names from a CLOSED vocabulary. No hostnames, no
// keys, no IPs, no unit names, no counts.

// Component names — the complete public vocabulary. (API contract §4.)
const (
	CompTMDB       = "tmdb"
	CompProwlarr   = "prowlarr"
	CompJellyfin   = "jellyfin"
	CompTorrServer = "torrserver"
	CompVPN        = "vpn"
)

// HealthProbe reports whether one named component is healthy.
type HealthProbe struct {
	Component string
	Check     func(ctx context.Context) bool
}

var (
	healthMu     sync.RWMutex
	healthProbes []HealthProbe

	healthCacheMu sync.Mutex
	healthCacheAt time.Time
	healthCache   healthSummary
	healthRefresh sync.Mutex
)

const healthTTL = 5 * time.Second

// SetHealthProbes installs the component checks used by /api/health/summary and /readyz.
func SetHealthProbes(p []HealthProbe) {
	healthMu.Lock()
	healthProbes = p
	healthMu.Unlock()
}

type healthSummary struct {
	OK       bool     `json:"ok"`
	Degraded []string `json:"degraded"`
}

func computeHealth(ctx context.Context) healthSummary {
	healthMu.RLock()
	probes := make([]HealthProbe, len(healthProbes))
	copy(probes, healthProbes)
	healthMu.RUnlock()

	degraded := []string{}
	for _, p := range probes {
		if !p.Check(ctx) {
			degraded = append(degraded, p.Component)
		}
	}
	sort.Strings(degraded)
	return healthSummary{OK: len(degraded) == 0, Degraded: degraded}
}

// Health returns the cached component summary, refreshing at most once per healthTTL.
// The cache is what keeps a public, unauthenticated endpoint from being a way to drive
// repeated probes of the VPN helper and the upstream services.
func Health(ctx context.Context) (bool, []string) {
	healthCacheMu.Lock()
	if time.Since(healthCacheAt) < healthTTL {
		c := healthCache
		healthCacheMu.Unlock()
		return c.OK, c.Degraded
	}
	healthCacheMu.Unlock()

	healthRefresh.Lock()
	defer healthRefresh.Unlock()
	healthCacheMu.Lock()
	if time.Since(healthCacheAt) < healthTTL {
		c := healthCache
		healthCacheMu.Unlock()
		return c.OK, c.Degraded
	}
	healthCacheMu.Unlock()

	sum := computeHealth(ctx)

	healthCacheMu.Lock()
	healthCache = sum
	healthCacheAt = time.Now()
	healthCacheMu.Unlock()
	return sum.OK, sum.Degraded
}

// HealthSummaryHandler — GET /api/health/summary (PUBLIC).
// Returns only {"ok":bool,"degraded":[...]} from the fixed component vocabulary.
func HealthSummaryHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ok, degraded := Health(ctx)
	if degraded == nil {
		degraded = []string{}
	}
	jsonOK(w, healthSummary{OK: ok, Degraded: degraded})
}
