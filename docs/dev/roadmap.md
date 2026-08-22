# Roadmap

Only genuinely open work. Anything that already exists is described in
[architecture.md](architecture.md) and [operations.md](operations.md), not here.

---

## Rough edges in setup

These are the things a new user hits, in the order they hit them. Each is a small change with a
large effect on whether the system works for a stranger.

**`server.public_url` is a placeholder with no safety net.** The installer writes
`http://CHANGE-ME-LAN-IP:1990`, nothing validates it, `jellyfreedom doctor` does not check it,
and there is no dashboard field for it. Left unchanged, every `.strm` is unplayable and no error
explains why. The obvious fixes, in increasing order of ambition: a `doctor` check; a dashboard
field; the installer detecting the primary LAN address and writing it.

**The FlareSolverr proxy must be configured in Prowlarr by hand.** Installing FlareSolverr does
nothing on its own — Prowlarr needs an indexer proxy pointing at it, and that proxy's tag on each
indexer that needs it. Prowlarr has an API; this could be offered as a one-click action, or at
least detected and reported by `doctor`.

**The Jellyfin webhook secret is generated but never shown.** It is only readable by querying
SQLite directly, and `sqlite3` is not among the installed packages. It should appear in the
dashboard's settings, alongside the exact webhook URL to paste into Jellyfin.

**Jellyfin's own configuration is entirely manual** — adding the two libraries, enabling
metadata providers, disabling the three scheduled tasks that probe whole files, and setting the
playback policy. All of it is reachable through Jellyfin's API with the key the orchestrator
already holds, so a "configure Jellyfin for me" action is feasible.

**A Prowlarr with zero indexers is invisible in the UI.** `doctor` reports it and the installer's
closing banner warns about it, but the media UI itself still just returns nothing. It should say
so.

---

## Streaming throughput on a cold start

Independent of release selection: even a release with connectable peers ramps slowly on first
play — tens of KB/s climbing — because the torrent is cold and initially connected to few
seeders. Fresh popular content connects to dozens quickly and is fine; old or niche content may
never get there.

Levers worth exploring:

- Keep a small warm cache of recently played or likely-next items, relaxing strict
  drop-after-validate.
- Raise the per-torrent connection floor for the first N seconds of a stream.
- Tune TorrServer's preload behaviour.
- Investigate why connected-seeder counts stay low on some VPN paths even with port forwarding
  active.

There is also a known slow path: when an item's top candidates are all ghosts — a healthy scrape
count with no connectable peers, common for old episodes — resolution walks the candidate list
serially and can take tens of seconds before failing. Failing is correct, but it is slow and the
UI does not explain what is happening.

---

## Retire the `stale` lifecycle

Resolve-at-Play chooses a release at play time, which makes a persisted "this item has gone bad"
state much less meaningful than it was when a hash was frozen into each `.strm`. The health check
still marks items stale and revives them, and the UI still renders that state.

Worth deciding deliberately: either keep it as a *useful signal* ("nothing has been seeded for
this in a while") and say so in the UI, or remove it and let `/play` be the only judge. Half of
each is the current position and is the worst of both.

---

## Platform coverage

**arm64 has no FlareSolverr.** Upstream publishes x64 binaries only. The installer skips it and
says so, which is honest but leaves Cloudflare-protected indexers broken on every Raspberry Pi
and ARM server. The official multi-arch container runs FlareSolverr from source against a distro
`chromium`, and nothing about that approach requires Docker — the installer could do the same on
arm64.

**32-bit ARM** has no published bundle at all. The installer's preflight accepts `armv7l`, but
`get.sh` correctly refuses, so those users must build from source. Either publish an `armhf`
bundle or make the preflight say so plainly.

---

## Dependencies to tidy

`vpntorrent/portforward.sh` parses TorrServer's JSON with `python3`, which the installer does not
install and which is not guaranteed present on a minimal Debian. `jq` **is** installed and is
already used elsewhere. Switching would remove an undeclared dependency.

`sqlite3` is referenced by the documentation for the two values that are only readable from the
database. Either install it, or stop needing it by surfacing those values properly.

---

## Deliberately not planned

- **Docker.** No.
- **Radarr/Sonarr integration for the streaming library.** Their availability model is
  fundamentally incompatible; see [decisions.md](decisions.md) D6.
- **A paid debrid backend as the primary path.** Ruled out on cost; see D3.
- **Jackett support.** The indexer client targets Prowlarr's JSON API. Prowlarr aggregates the
  same indexers.
- **Exposing the orchestrator to the internet.** There is no TLS and several routes are
  necessarily unauthenticated. A private overlay network is the answer, and hardening this for
  hostile networks is not a goal.
