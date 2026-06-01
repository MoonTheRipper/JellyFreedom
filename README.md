# JellyFreedom

**🌐 Website: https://moontheripper.github.io/JellyFreedom/**

Search and stream movies & TV from torrent sources **straight into Jellyfin** — watch on any
device (Apple TV, phone, browser) through one clean UI, without filling your disk or leaking
your IP. No Docker, no paid debrid.

---

## What it is

A small Go **orchestrator** that makes torrent-streamable content appear as normal, playable
items in your Jellyfin library. It writes tiny `.strm` pointer files; pressing play streams the
torrent on demand through a bounded-cache engine, behind a VPN kill switch.

| Component | Role | Provided by |
|---|---|---|
| **Jellyfin** | The player/library UI on every device | off-the-shelf |
| **Orchestrator** | Search (TMDB) → pick a healthy release (Prowlarr) → drive the stream → write `.strm` | **this repo** |
| **TorrServer** | Torrent → HTTP stream, bounded RAM cache (no disk bloat) | off-the-shelf binary |
| **Prowlarr + FlareSolverr** | Indexer search / Cloudflare bypass | off-the-shelf |
| **WireGuard** | Privacy for torrent traffic + a fail-closed kill switch | any provider or self-hosted |

The installer sets up everything; the only thing you bring is a **WireGuard config** (from any
provider) and, optionally, a TMDB key.

## How

**Install** (Debian/Ubuntu):

```bash
# one-liner (once a release is hosted — see release/get.sh):
curl -fsSL https://<host>/get.sh | sudo bash

# or from source:
./release/build.sh && sudo ./dist/jellyfreedom-*/install.sh
```

It installs the orchestrator + TorrServer + FlareSolverr + Jellyfin + Prowlarr under standard
paths (`/opt/jellyfreedom`, `/var/lib/jellyfreedom`, `/etc/jellyfreedom`), a dedicated service
user, and the VPN netns plumbing. Anything already installed is left untouched.

**First run:**

1. Open `http://<host>:1990/dashboard/` and create the admin account.
2. **Settings** → enter your TMDB / Prowlarr / Jellyfin keys (stored server-side, never shown back).
3. **VPN → Configurations** → upload a WireGuard `.conf` (any provider; pick a torrent-friendly
   server) → **Activate**.
4. Add the `.strm` library folders (movies + tv) to Jellyfin.

Then search in the media UI and press play. First play of an item buffers for a few seconds
while the torrent connects, then streams.

## Why

- **Torrent-streaming, not downloading** — no paid debrid, and TorrServer's bounded RAM cache
  means disk usage stays flat no matter how much you watch.
- **Availability = "resolvable", not "a file exists"** — the orchestrator owns this, so you
  don't fight Radarr/Sonarr's download-complete model.
- **Resolve-at-play** — `.strm` files point at a stable identity URL, not a frozen torrent hash,
  so the library self-heals when releases/seeders decay (it re-picks a live one on play).
- **Privacy is fail-closed** — torrent traffic is confined to a network namespace whose only
  route is the WireGuard tunnel; if the VPN drops, that traffic is **blocked, not leaked**.
  Jellyfin↔client stays on the LAN (no VPN).
- **No Docker** — native single binaries and systemd units.

## What if… (troubleshooting)

| Symptom | Likely cause / fix |
|---|---|
| **"Playback failed — not supported by this client"** | A release whose container/audio can't direct-play. Jellyfin remux + audio-transcode are enabled by default; if it persists, pick another release via **Choose release** (prefer h264 + AAC/AC3 in MP4/MKV). |
| **First play is slow / buffers** | Normal cold-start — the torrent is re-added on demand and connects to peers. It ramps up; well-seeded releases are fast. |
| **Connects to seeders but downloads nothing** | Your VPN server doesn't allow P2P. Use a **torrent/P2P-friendly** WireGuard server. |
| **Everything torrent-related is dead** | The VPN tunnel is down (kill switch blocking — working as intended). Check `systemctl status vpntorrent-netns`; the watchdog auto-re-establishes within ~60s, or upload a fresh config. |
| **Old/niche titles won't stream** | Some content simply isn't well-seeded right now; the picker prefers connectable releases but can't conjure peers that don't exist. |
| **FlareSolverr / Prowlarr searches fail** | First search after idle can be slow (cold indexers); retry. Ensure `flaresolverr` and `prowlarr` are `active`. |
| **Dashboard won't load** | `journalctl -u jellyfreedom -f`. The orchestrator starts even without keys configured — set them in the dashboard. |

## License

[MIT](LICENSE) — free to use, modify, and distribute. Provided **as is**, without warranty.

---

*Self-hosted, single-user oriented. You are responsible for what you stream and for complying
with the laws and terms that apply to you.*
