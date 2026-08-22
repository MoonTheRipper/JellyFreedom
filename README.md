# JellyFreedom

[![CI](https://github.com/MoonTheRipper/JellyFreedom/actions/workflows/ci.yml/badge.svg)](https://github.com/MoonTheRipper/JellyFreedom/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/MoonTheRipper/JellyFreedom)](https://github.com/MoonTheRipper/JellyFreedom/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**Torrent-streaming as a Jellyfin library.** A small Go orchestrator searches the indexers
*you* configure, picks a well-seeded release, and writes a tiny `.strm` pointer file into a
Jellyfin folder. Press play on your Apple TV, phone, or browser and it streams on demand
through a bounded RAM cache — behind a fail-closed WireGuard kill switch. Nothing is
downloaded to disk, so disk usage stays flat no matter how much you watch. No Docker, no
paid debrid service.

<!-- SCREENSHOT: add a dashboard/library screenshot here, e.g. docs/screenshot.png -->

## What this is NOT

- **Not a content source.** It ships no indexers, no trackers, no scrapers, no catalogue.
  With an empty Prowlarr it finds nothing at all.
- **Not a downloader.** There is no library on disk to keep. Streams are transient; the
  cache is a bounded ring buffer that physically cannot grow.
- **Not a *arr replacement.** Radarr and Sonarr are built around "a file exists". This is
  built around "a magnet with enough seeders is resolvable right now". They do not mix.
- **Not internet-facing software.** It binds `0.0.0.0:1990` over plain HTTP and several
  routes are unauthenticated. LAN or private overlay network only — see
  [SECURITY.md](SECURITY.md).
- **Not a Docker project**, and it will not become one.
- **Not multi-tenant.** Single household, single trusted network.

## How the pieces fit

| Component | Role | Provided by |
|---|---|---|
| **Jellyfin** | Player and library UI on every device | off-the-shelf |
| **Orchestrator** | TMDB search → pick a healthy release via Prowlarr → drive the stream → write `.strm` | **this repo** |
| **TorrServer** | Torrent → HTTP stream, bounded RAM cache | off-the-shelf binary |
| **Prowlarr + FlareSolverr** | Indexer aggregation and Cloudflare bypass | off-the-shelf |
| **WireGuard** | Privacy for torrent traffic, plus a fail-closed kill switch | your provider, or self-hosted |

## Prerequisites

- **Debian or Ubuntu** (22.04 / 24.04 tested). systemd required.
- **x86_64.** `arm64` (Raspberry Pi) is a work in progress — the orchestrator
  cross-compiles, but the installer's FlareSolverr step is x64-only today, so arm64
  installs currently fail.
- **RAM: 4 GB minimum, 8 GB comfortable.** The shipped default is a **2 GB RAM-backed
  streaming cache** (`torrserver.cache.mode: ram`, `size_mb: 2048` in
  `release/config.sample.yaml`) — that memory is reserved for streaming, on top of
  Jellyfin, which needs its own headroom if it has to transcode. Lower `size_mb` on a
  small box.
- **Disk: a few GB.** For the binaries, Jellyfin, and metadata — not for media.
- **A WireGuard config** from any provider, on a **P2P-friendly server**. This is the one
  thing you must bring. (A non-P2P server connects to peers and then downloads nothing.)
- **Optionally a TMDB API key** for metadata, and indexers to add to Prowlarr.

## Install

```bash
curl -fsSL https://github.com/MoonTheRipper/JellyFreedom/releases/latest/download/get.sh | sudo bash
```

`get.sh` picks the bundle for your architecture and **verifies it against the release's
`SHA256SUMS` before installing** — a mismatch or a missing entry aborts rather than
installing unverified bytes.

Piping a script to `sudo bash` is still a decision you should make deliberately. To read it
first, and to check the script itself:

```bash
base=https://github.com/MoonTheRipper/JellyFreedom/releases/latest/download
curl -fsSLO "$base/get.sh"
curl -fsSLO "$base/SHA256SUMS"
sha256sum -c SHA256SUMS --ignore-missing   # expect: get.sh: OK
less get.sh                                # read it
sudo bash get.sh
```

Releases from the automated workflow also carry signed build provenance:

```bash
gh attestation verify jellyfreedom-<version>-linux-amd64.tar.gz --repo MoonTheRipper/JellyFreedom
```

Or build from source (see [CONTRIBUTING.md](CONTRIBUTING.md) for the toolchain):

```bash
./release/build.sh "$(cat VERSION)"
sudo ./dist/jellyfreedom-"$(cat VERSION)"*/install.sh
```

The installer sets up the orchestrator, TorrServer, FlareSolverr, Jellyfin, and Prowlarr
under standard FHS paths (`/opt/jellyfreedom`, `/etc/jellyfreedom`, `/var/lib/jellyfreedom`)
with dedicated service users and the VPN namespace plumbing. Anything already installed is
detected and left alone.

## First run

1. Open `http://<host>:1990/dashboard/` and **create the admin account immediately** —
   until you do, the setup page is open to anyone who can reach the host.
2. **Settings** → enter your TMDB, Prowlarr, and Jellyfin URLs and keys.
3. **Prowlarr** (`http://<host>:9696`) → add the indexers you intend to use. JellyFreedom
   ships none.
4. **VPN → Configurations** → upload a WireGuard `.conf` from a P2P-friendly server →
   **Activate**. Until you do, torrent traffic is blocked, not leaked — that is the kill
   switch working.
5. In Jellyfin, add the `.strm` library folders (movies and TV) as libraries.

Then search in the media UI and press play. The first play of a title buffers for a few
seconds while the torrent connects.

Update in place with `sudo jellyfreedom --update`.

## Documentation

| | |
|---|---|
| [Install guide](docs/install.md) | Prerequisites, what the one-liner does, from source, updating, uninstalling |
| [First run](docs/first-run.md) | The ordered path from a fresh install to first playback |
| [Configuration](docs/configuration.md) | Every config key, its default, and what it changes |
| [Troubleshooting](docs/troubleshooting.md) | Symptom-to-fix, starting with `jellyfreedom doctor` |
| [FAQ](docs/faq.md) | Short answers to the recurring questions |
| [Security &amp; privacy](docs/security.md) | What is tunnelled, verifying the kill switch, what to expose |
| [SECURITY.md](SECURITY.md) | Threat model, route authentication tiers, the privilege boundary |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Build, test, lint, and how to test the installer safely |
| [CHANGELOG.md](CHANGELOG.md) | What changed in each release |
| [Architecture](docs/dev/architecture.md) | How the system is put together and why it works this way |
| [Decisions](docs/dev/decisions.md) | The reasoning behind every major choice |
| [Operations](docs/dev/operations.md) | VPN scoping, cache tuning, Jellyfin and Apple TV setup |
| [Roadmap](docs/dev/roadmap.md) | Genuinely open work |
| [Project website](https://moontheripper.github.io/JellyFreedom/) | Overview |

## Disclaimer

JellyFreedom ships **no indexers, no content, no trackers, and no sources**. It searches
the indexers you configure yourself and points your own Jellyfin instance at the result.
What you search for, what you stream, and whether either is permitted where you live is
your responsibility — you are responsible for complying with the laws and terms that apply
to you.

This project is **not affiliated with or endorsed by** the Jellyfin, Prowlarr, TorrServer,
FlareSolverr, TMDB, or WireGuard projects. All trademarks belong to their respective owners.

## Licence

[MIT](LICENSE). Provided **as is**, without warranty of any kind.
