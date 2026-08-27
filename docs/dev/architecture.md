# Architecture

How the system is put together, and why it is put together that way. The reasoning behind each
choice is in [decisions.md](decisions.md); the runtime detail is in
[operations.md](operations.md).

## 1. The design principle

**Jellyfin is the frontend. Our code is a backend that feeds it.**

There is no custom player and no per-device UI. Apple TV, iPhone, browser — all use their
native Jellyfin client. Our job is to make torrent-streamable content *appear inside Jellyfin's
library* as playable items, on demand, without downloading whole files.

The glue is the **`.strm` file**: a tiny text file containing one URL. Jellyfin treats it as a
media item and fetches that URL on play.

## 2. Components

| Component | Role | Ours? |
|---|---|---|
| **Jellyfin** | Media server and player UI on every device | No |
| **TorrServer** | Torrent → HTTP stream engine with a bounded cache | No (single Go binary) |
| **Prowlarr** | Indexer aggregation, queried over its JSON API | No |
| **FlareSolverr** | Cloudflare bypass for indexers that need it | No |
| **TMDB** | Metadata and search | No (cloud API) |
| **WireGuard** | Privacy for torrent traffic, plus the kill switch | No (any provider) |
| **yt-dlp** | Extracts a media URL from a video page, for pasted web sources | No (single binary) |
| **Orchestrator** | Search → pick a release → drive TorrServer → write `.strm` → serve playback | **Yes (Go)** |
| **Namespace proxy** | Lends the VPN namespace to the orchestrator, which lives outside it | **Yes** — `orchestrator netns-proxy` |

Two notes that matter when reading the code:

- **Prowlarr only.** The orchestrator uses Prowlarr's JSON search API
  (`internal/indexer/client.go`), not a generic Torznab feed, so Jackett is not a drop-in
  substitute.
- **The VPN is provider-agnostic.** Anything that speaks WireGuard works, including a
  self-hosted endpoint. The only provider-specific piece anywhere is NAT-PMP port forwarding,
  which is optional and degrades to nothing.

### Why TorrServer and not a custom engine

General-purpose torrent libraries are downloaders that happen to stream: they persist every
piece and seed the whole file, and storage fills up. TorrServer is purpose-built for
streaming — a bounded **ring-buffer cache** keeps a window around the playhead and evicts the
rest. Storage stays flat, and there is never a complete file to seed. It is also a single Go
binary, which fits the no-Docker constraint.

### Two sources of bytes, one playback contract

Everything above describes the torrent path. There is a second one — a **web source**: a
video page URL somebody pasted into the dashboard, which becomes a library entry like any
other.

The two share more than they differ. Both are keyed on a provider identity, both write a
`.strm` holding `/play/…` with a capability token, both are gated by per-library visibility,
and both re-derive what actually delivers bytes at play time. They diverge only in what that
derivation is: the torrent path searches indexers and picks a release; the web path runs
yt-dlp against the saved page and takes the media URL it returns.

The reason the second one exists is coverage. Indexers are good at films and episodes and
poor at everything else, and a pasted link needs no index at all.

The reason it re-resolves rather than storing a URL is the same reason the torrent path does.
A frozen info hash rots because seeders leave; a frozen CDN link rots because the site signed
it with an expiry measured in hours. Same failure, same fix. See §4.

## 3. The data flow

```
User requests "Some Movie (2024)"
   │
   ▼
ORCHESTRATOR
   1. TMDB       → canonical title, year, ids, poster
   2. Prowlarr   → candidate releases (hashes, seeders, size, codecs)
   3. Pick       → seeders dominate; codec/audio/container are tie-breakers; CAM rejected
   4. TorrServer → add, wait for the file list, match the episode, validate, confirm a
                   connectable peer, then DROP the torrent again
   5. Write      → /srv/jellyfreedom/movies/Some Movie (2024)/Some Movie (2024).strm
                   containing  http://<public_url>/play/movie/<tmdb>?t=<token>
   6. Ask Jellyfin to scan
   │
   ▼
JELLYFIN shows the item on every device
   │
   ▼
User presses play → Jellyfin fetches the .strm URL → back into the ORCHESTRATOR
   │
   ▼
   /play resolves the best release that is live RIGHT NOW, adds it to TorrServer,
   and proxies the byte range from  <torrserver>/stream?index=<n>&link=<hash>&play
   │
   ▼
Playback stops → Jellyfin webhook → drop the torrent, free the cache
```

## 4. Resolve-at-Play

The `.strm` holds a **stable, identity-keyed** URL — `/play/movie/{tmdb}` or
`/play/tv/{tmdb}/{season}/{episode}` — not a frozen info hash.

This is the load-bearing design choice. A baked-in hash decays: seeders die, releases vanish,
and the pointer rots, forcing a "re-request the whole library" cycle. Because the URL is keyed
on *identity* rather than on a particular release, the orchestrator can choose again on every
play:

1. If a cached last-good hash exists and still has a connectable peer, stream it.
2. Otherwise search again, rank by **current** seeders, and commit the first candidate that
   resolves its file list, matches the episode, validates, and proves a real connected peer
   within a short window. Cache that choice.

Consequences worth knowing:

- The library **self-heals**. Jellyfin never needs a rescan when a release is swapped, because
  the URL never changes.
- Validation happens in the same on-demand step as playback, so it no longer competes with
  streaming for TorrServer's single cache.
- The cost is roughly 5–15 seconds on a cold first play. Replays and the next episode are fast
  via the cache, an indexer-warmup task keeps the search path warm, and the next episode is
  pre-warmed once you pass 80% of the current one.

### Capability tokens

`/play/...` and the legacy `/proxy/stream` **must** stay unauthenticated — Jellyfin fetches a
`.strm` URL with no session cookie. Instead, each URL carries an HMAC over the item's identity,
computed with a server-side key generated on first run. Possession of a valid `.strm` is the
credential. Without this, an unauthenticated stranger could make the box start downloading
arbitrary content over the owner's VPN.

Enforcement is switched on only after a startup pass has rewritten every existing `.strm` with
a tokenised URL, so enabling it can never break a library that predates it.

## 5. The availability model

**Availability means "resolvable", not "downloaded".**

Radarr, Sonarr and Jellyseerr define availability as *a finished file exists in the managed
folder*. A streaming setup never produces that, which is why bolting them onto a `.strm`
library produces perpetual "missing" states and faked imports. The orchestrator therefore owns
availability itself:

- An item is **`ready`** when a release with enough seeders can be resolved for it.
- An item is **`stale`** when nothing resolvable exists. It keeps its `.strm` and its stored
  magnet, and revives automatically the moment something is seeded again.
- A background **library health check** (every 30 minutes) is the *only* thing that moves items
  between those two states, and it does so purely from indexer results — it never loads
  TorrServer, because background load is exactly what the bounded-cache design exists to
  avoid.

**Finishing a video does not make an item stale.** The playback-stopped webhook drops the
torrent to free the cache, and the item stays `ready`; `/play` re-adds it on demand next time.

## 6. Storage

- TorrServer holds a **capped cache window**. It cannot exceed the cap.
- `.strm` files are a few hundred bytes.
- Torrents are dropped after validation and again after playback.

Disk usage is essentially constant regardless of how much is watched.

## 7. Direct play, and why the picker cares about codecs

If a release is in a format the client cannot direct-play, Jellyfin transcodes in real time —
*while simultaneously pulling the file from the swarm.* The source arrives at swarm speed, so
that double load stutters badly.

The policy that works:

| | |
|---|---|
| **Video transcoding** | **off** — a full re-encode while leeching is what saturates a network |
| **Audio transcoding** | **on** — cheap, and fixes AC3/DTS on clients that cannot handle it |
| **Remuxing** | **on** — repackages a container with no re-encode |

With all three off you get "media is not supported by this client" for an h264-in-MKV release
in a browser. With video transcoding on you get stutter. Remuxing and audio transcoding pull at
playback rate and are safe.

The picker prefers H.264/H.265 with AAC/AC3/E-AC3 in MP4/MKV so that a video-codec mismatch —
which now *fails* rather than re-encoding — stays rare. But those are only tie-breakers:
**seeder count dominates the score**, because a dead release in a perfect format does not play
at all.

## 8. Privacy and network model

- **Only torrent traffic is tunnelled.** TorrServer runs in a network namespace whose sole
  default route is the WireGuard interface, with an `OUTPUT DROP` policy as a second layer.
- **Jellyfin ↔ client stays on the LAN.** Tunnelling it would only add latency.
- **Indexer and metadata lookups leave over the real IP.** This is a deliberate scoping
  decision, not an oversight — see [security.md](../security.md) for what that means in
  practice.
- Remote access is the operator's problem: a private overlay network (Tailscale, WireGuard) or
  an authenticating proxy. Nothing here assumes a public IP, and nothing here should be given
  one.

## 9. Trust boundaries

- TMDB, Prowlarr, FlareSolverr and TorrServer are reached over plain HTTP on localhost or the
  namespace link. FlareSolverr in particular is bound to `127.0.0.1`: it fetches arbitrary URLs
  with no authentication, so exposing it publishes an open request proxy.
- The orchestrator itself binds to all interfaces by default and has no TLS. LAN or private
  overlay only.
- The service user reaches root through exactly one root-owned helper with a closed set of
  verbs and no free-form arguments.

## 10. Implemented shape

### Asynchronous request queue
`POST /request` and `POST /request/season` enqueue and return immediately. A single background
worker processes one item at a time (`pending → processing → done|failed`) with a live progress
string per stage. The queue is persisted, and rows interrupted by a restart are reset to
pending.

Both request paths are **idempotent**: a bare re-request for something already `ready` or
in flight does not enqueue a duplicate, an explicit magnet override always re-resolves, and a
fresh request supersedes stale terminal rows for the same identity so the queue never
contradicts the library.

### Observable background tasks
All recurring work is registered in a task registry (`internal/api/tasks.go`) rather than
hidden in anonymous goroutines, so each task is named, status-tracked and runnable by hand
from the dashboard:

| Task | Interval | What it does |
|---|---|---|
| `library-health-check` | 30 min | Re-checks resolvability via the indexer; marks stale and revives. Never touches TorrServer. |
| `orphan-cleanup` | 1 hour | Drops TorrServer torrents with no matching library entry. |
| `subscription-check` | 6 hours | Enqueues newly-aired episodes of subscribed seasons; retires ended shows. |
| `indexer-warmup` | 20 min | A lightweight search to keep FlareSolverr and the indexers warm. |
| `poster-backfill` | startup / manual | Fetches missing posters. |
| `session-cleanup` | 1 hour | Purges expired sessions. |
| `jellyfin-scan`, `torrserver-health` | manual | |

### Release selection
- **Hash recovery.** Prowlarr returns an explicit `infoHash` for only a minority of results;
  the rest carry it inside `guid`, `downloadUrl` or `infoUrl`. Recovering it with a 40-hex
  match instead of discarding those results roughly doubled the candidate pool and, in one
  measured case, moved the best available release from 15 seeders to over a thousand.
- **Scoring.** Seeders dominate; codec, audio and container break ties; CAM and telesync rips
  are sunk.
- **Validation.** The worker fails an item outright if no file list ever appears, rather than
  writing a bogus `index=0` pointer that falsely reads as ready. Episode matching returns the
  file id of the `SxxEyy`-matched file and hard-rejects multi-file packs with no match.
- **Connectability.** A ranked candidate is only committed once a *connected* peer is proven.
  An indexer's seeder count is a tracker scrape and can be a ghost: metadata resolves, peers are
  discovered, and nothing ever connects.

### Storage
SQLite (`internal/store`), a single file: `settings`, `sessions`, `users`, `items`, `queue`,
`subscriptions`. Migrations are additive.

### Interfaces
- **Media UI** (`web/public`) — search, library, queue and calendar, with a release picker.
- **Dashboard** (`web/dashboard`, admin only) — health, VPN configuration and leak check, logs,
  users, tasks, settings.

### VPN runtime
`setup-netns.sh` builds the namespace, the veth pair, the NAT and both kill-switch layers, then
calls the privileged helper to bring the tunnel up. A watchdog probes every 60 seconds and
escalates on its own. A port-forward keeper holds a NAT-PMP mapping where the provider offers
one. Details in [operations.md](operations.md).
