package main

import (
	"context"
	"sync"
	"time"

	"jellyfreedom/internal/torrserver"
)

// ── Judging a release by its live swarm ───────────────────────────────────────
//
// The resolver used to accept the first release that connected to a single peer. The
// picker had already ranked releases by the seeder count the indexer reported, and that
// count can be far from the truth. On the live box on 2026-11-23:
//
//   - Bohemian Rhapsody: listed with 84 seeders; TorrServer connected to 2 and read 0 KB/s.
//   - Tully: listed with 31; it connected to 6 or 7 and streamed 100–240 KB/s, against
//     the ~600 KB/s its bitrate needs.
//   - Breeder: 35 connected seeders streamed at 2.5 MB/s.
//
// Both of the first two passed the one-peer check and were written into the library. The
// player then sat on its first frames. So after a release connects, the resolver now
// watches how many seeders TorrServer actually reaches, and moves on to the next release
// when it is fewer than liveSeedersWanted.
//
// It never makes something unplayable that resolved before. When no candidate reaches the
// bar, the strongest swarm seen is used, just as the first connectable one would have
// been. A release the user picked by hand is never second-guessed.

// liveSeedersWanted is the live-seeder count at which a release is accepted outright. At
// the ~70 KB/s per seeder measured above, ten seeders sustain roughly a 1080p WEB-DL's
// bitrate, and a healthy swarm reaches it within seconds (Breeder: 11 after 5 s, 19 after
// 10 s). These are package variables so tests can shorten the window.
var (
	liveSeedersWanted = 10
	liveSeedersWindow = 20 * time.Second
	liveSeedersEvery  = time.Second
)

// swarmGate decides, release by release, whether to commit to one. It holds at most one
// weak release as the fallback, and says which torrents are no longer needed so the
// caller can drop them.
type swarmGate struct {
	want     int
	best     *resolveResult
	bestLive int
}

// offer judges one release that has passed every other check. accept is non-nil when its
// live swarm is strong enough to commit to now. release lists the torrents the gate no
// longer needs, which never includes the one it returns or still holds.
func (g *swarmGate) offer(res *resolveResult, live int) (accept *resolveResult, release []string) {
	if live >= g.want {
		if g.best != nil && g.best.hash != res.hash {
			release = append(release, g.best.hash)
		}
		g.best, g.bestLive = nil, 0
		return res, release
	}
	switch {
	case g.best == nil:
		g.best, g.bestLive = res, live
	case res.hash == g.best.hash:
		// The same torrent listed twice. Dropping it would drop the fallback.
		if live > g.bestLive {
			g.bestLive = live
		}
	case live > g.bestLive:
		release = append(release, g.best.hash)
		g.best, g.bestLive = res, live
	default:
		release = append(release, res.hash)
	}
	return nil, release
}

// holds reports whether hash is the fallback the gate is keeping.
func (g *swarmGate) holds(hash string) bool { return g.best != nil && g.best.hash == hash }

// take hands over the fallback, if there is one. The gate no longer holds it afterwards.
func (g *swarmGate) take() (*resolveResult, int) {
	res, live := g.best, g.bestLive
	g.best, g.bestLive = nil, 0
	return res, live
}

// ── Measurement cache ─────────────────────────────────────────────────────────
//
// A season request resolves every episode separately, and they usually land on the same
// season pack. Without a cache, a weak pack would cost the full window again for every
// episode of the season. A strong reading is reused for swarmSeenTTL. A weak one is kept
// for only weakSwarmTTL, because a swarm also reads weak for the first ~45 s after a
// TorrServer restart (DHT warm-up; see docs/dev/operations.md), and that reading should
// not hold a good release back for half an hour.

const (
	swarmSeenTTL = 30 * time.Minute
	weakSwarmTTL = 5 * time.Minute
)

type swarmReading struct {
	live int
	at   time.Time
}

func (r swarmReading) ttl() time.Duration {
	if r.live < liveSeedersWanted {
		return weakSwarmTTL
	}
	return swarmSeenTTL
}

var (
	swarmSeenMu sync.Mutex
	swarmSeen   = map[string]swarmReading{}
)

// measureSwarm returns the live seeders of a loaded torrent, from the cache when a recent
// reading exists. A reading cut short by ctx is not cached.
func measureSwarm(ctx context.Context, ts *torrserver.Client, hash string) int {
	swarmSeenMu.Lock()
	if r, ok := swarmSeen[hash]; ok && time.Since(r.at) < r.ttl() {
		swarmSeenMu.Unlock()
		return r.live
	}
	swarmSeenMu.Unlock()

	live := ts.LiveSeeders(ctx, hash, liveSeedersWanted, liveSeedersWindow, liveSeedersEvery)
	if ctx.Err() != nil {
		return live
	}
	swarmSeenMu.Lock()
	defer swarmSeenMu.Unlock()
	if len(swarmSeen) >= 256 {
		for h, r := range swarmSeen {
			if time.Since(r.at) >= r.ttl() {
				delete(swarmSeen, h)
			}
		}
	}
	swarmSeen[hash] = swarmReading{live: live, at: time.Now()}
	return live
}
