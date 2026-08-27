# Installing JellyFreedom

This page covers what the machine needs, exactly what the installer does to it, and how to
update or remove it later. If you want the ordered "get to first playback" path, read
[first-run.md](first-run.md) after this.

---

## 1. What you need before you start

### Operating system

**Debian or Ubuntu with systemd.** The installer is apt-based and refuses to run without
`apt-get`. Ubuntu 22.04 and 24.04 are the tested targets. Other distributions can run the
orchestrator, but you would have to install the surrounding stack yourself — nothing here
will do it for you.

You need root (the installer creates users, systemd units and a sudoers policy).

### Architecture

| Architecture | Published bundle | Notes |
|---|---|---|
| `x86_64` / `amd64` | yes | Fully supported. |
| `aarch64` / `arm64` | yes | Everything works **except FlareSolverr** — see below. |
| `armv7l` / anything else | no | Build from source. `get.sh` refuses rather than installing a binary that cannot exec. |

**The arm64 caveat you need to know up front:** FlareSolverr publishes **x64 binaries only**.
On arm64 the installer detects this, skips FlareSolverr, and says so. Everything else works.
The consequence is that **Cloudflare-protected indexers will not work** on arm64 unless you
run FlareSolverr yourself from source against a distro `chromium`. That is what the official
multi-arch FlareSolverr container does internally, so the approach is proven — you do not
need Docker to copy it. Indexers that are not behind Cloudflare are unaffected.

### Memory

The default streaming cache is a **2 GB RAM ring buffer** (`torrserver.cache.size_mb: 2048`
in the shipped config). That memory is reserved for streaming and sits on top of Jellyfin,
which needs its own headroom.

- **4 GB RAM**: workable.
- **8 GB**: comfortable.
- **Under 2 GB**: the installer warns you. Lower `cache.size_mb` — see
  [configuration.md](configuration.md) — or the box will thrash. `jellyfreedom doctor` fails
  the check outright if the configured cache does not fit in RAM.

### Disk

The installer **refuses to run with less than 4 GB free on `/opt`**. FlareSolverr alone
downloads ~233 MB and extracts to roughly 705 MB, and Jellyfin plus Prowlarr add several
hundred MB more.

Separately, **Jellyfin's own upstream installer requires 2 GB free on both `/var/lib` and
`/tmp`**, and only supports specific distribution codenames. If it fails, that is usually
why; the installer prints its full output to the log rather than swallowing it.

You do **not** need disk for media. Nothing is written to disk to be watched — see
[faq.md](faq.md).

### Things you have to bring yourself

The release ships no keys, no indexers and no VPN configuration. Before it can find or play
anything you need:

1. **A WireGuard `.conf` from some VPN provider**, or a self-hosted endpoint. Any provider
   works — nothing here is Proton-specific. **Pick a server the provider flags as P2P.** On a
   non-P2P server torrents connect to seeders and then transfer nothing at all, which looks
   exactly like a broken install.
2. **A TMDB API key** (free, from themoviedb.org). Without it there is no search, no posters
   and no metadata.
3. **At least one indexer to add to Prowlarr.** JellyFreedom ships none, and a Prowlarr with
   zero indexers returns empty results forever without ever printing an error. This is the
   single most common "it's broken" report.

---

## 2. Install

```bash
curl -fsSL https://github.com/MoonTheRipper/JellyFreedom/releases/latest/download/get.sh | sudo bash
```

`get.sh` selects the bundle for your CPU, **verifies it against the release's `SHA256SUMS`**,
extracts it and runs `install.sh`. A checksum mismatch, or a missing `SHA256SUMS` entry,
aborts the install rather than proceeding with unverified bytes.

### Verifying by hand first

Piping a script into `sudo bash` is a decision worth making deliberately. To read everything
before running it:

```bash
base=https://github.com/MoonTheRipper/JellyFreedom/releases/latest/download
curl -fsSLO "$base/get.sh"
curl -fsSLO "$base/SHA256SUMS"
sha256sum -c SHA256SUMS --ignore-missing   # expect: get.sh: OK
less get.sh
sudo bash get.sh
```

Or download the bundle yourself and run its installer:

```bash
curl -fsSLO "$base/jellyfreedom-linux-amd64.tar.gz"
curl -fsSLO "$base/SHA256SUMS"
sha256sum -c SHA256SUMS --ignore-missing
tar xzf jellyfreedom-linux-amd64.tar.gz
sudo ./jellyfreedom-*/install.sh
```

Releases built by the GitHub Actions workflow also carry signed build provenance:

```bash
gh attestation verify jellyfreedom-<version>-linux-amd64.tar.gz --repo MoonTheRipper/JellyFreedom
```

### Escape hatches in `get.sh`

| Variable | Effect |
|---|---|
| `JELLYFREEDOM_URL=<url>` | Download this exact bundle. Skips architecture selection **and checksum verification**. |
| `JELLYFREEDOM_BASE=<url>` | Use a different release base URL (forks, staging). |
| `JELLYFREEDOM_SKIP_VERIFY=1` | Continue when `SHA256SUMS` cannot be fetched. Understand what you are giving up. |

---

## 3. What the installer actually does

Expect **5–15 minutes** and a few hundred MB of downloads on a normal connection. Everything
it prints is also written to **`/var/log/jellyfreedom-install.log`**.

It is **idempotent**. Anything already present is detected and left alone, and your config,
database and uploaded VPN configs are never touched. Re-running it is the supported repair
path.

**1. Preflight** — architecture, distribution, free space on `/opt`, RAM against the
configured cache size, port conflicts on 1990/8090/8096/8191/8192/9696, and it waits (up to 5
minutes) for `unattended-upgrades` to release the dpkg lock instead of dying at step one.

**2. apt packages** — `wireguard-tools natpmpc iproute2 iptables jq curl ca-certificates tar
xvfb`. (`xvfb` is required by FlareSolverr, which starts its own X server and does not bundle
one.)

**3. Service users** — system accounts with no home and no shell: `jellyfreedom`,
`torrserver`, `flaresolverr`, and `prowlarr` if Prowlarr is installed.

**4. Files** — the layout below.

| What | Path |
|---|---|
| Orchestrator binary | `/opt/jellyfreedom/bin/orchestrator` |
| Web assets | `/opt/jellyfreedom/web/` |
| Bundled installer, uninstaller, doctor, sample config | `/opt/jellyfreedom/{install,uninstall,doctor}.sh`, `config.sample.yaml` |
| Control CLI | `/usr/local/bin/jellyfreedom` |
| VPN namespace scripts | `/opt/vpntorrent/{setup-netns,watchdog,portforward}.sh` |
| Privileged helper (root-owned) | `/opt/vpntorrent/jf-netns-helper` |
| Config | `/etc/jellyfreedom/config.yaml` (mode 640) |
| Database + uploaded VPN configs | `/var/lib/jellyfreedom/`, `vpnconfigs/` mode 700 |
| Library folders (`.strm` files) | `/srv/jellyfreedom/movies`, `/srv/jellyfreedom/tv` |
| Install log | `/var/log/jellyfreedom-install.log` |

**5. Third-party components**, each skipped if already installed:

| Component | Where it goes | Port |
|---|---|---|
| TorrServer | `/usr/local/bin/torrserver`, data in `/var/lib/torrserver` | 8090, inside the VPN namespace |
| yt-dlp | `/usr/local/bin/yt-dlp`, scratch in `/var/lib/jellyfreedom/tmp` | — (used by the Links feature) |
| FlareSolverr | `/opt/flaresolverr`, home `/var/lib/flaresolverr` | 8191, bound to `127.0.0.1` |
| Jellyfin | via the official upstream apt installer | 8096 |
| Prowlarr | `/opt/Prowlarr`, data in `/var/lib/prowlarr` | 9696 |

TorrServer's version is resolved from the upstream "latest release" API rather than a pin,
because a pinned tag was removed upstream once and every fresh install silently ended up with
no streaming engine.

yt-dlp is the only **optional** one: nothing else depends on it, so a failed download is a
warning and the install still completes. A box without it simply has the dashboard's Links
section switched off, with one sentence saying why. Keep it current with `sudo yt-dlp -U` —
a pasted link that used to work and stopped is nearly always a stale extractor.

**6. systemd units** — `jellyfreedom`, `vpntorrent-netns`, `torrserver-netns`,
`jf-netnsproxy`, `vpntorrent-portforward`, `vpntorrent-watchdog.{service,timer}`, plus
`flaresolverr` and `prowlarr` where installed.

**7. A scoped sudoers policy** at `/etc/sudoers.d/jellyfreedom` — five fixed `systemctl
restart` commands and six verbs on the root-owned helper. **No wildcards.** See
[security.md](security.md).

**8. An AppArmor override** at `/etc/apparmor.d/local/wg-quick`, so `wg-quick` may read the
uploaded VPN configs.

**9. Verification** — it does not report success on the strength of a file existing. It probes
the orchestrator's `/healthz`, TorrServer, Jellyfin, Prowlarr, and FlareSolverr in three
stages (up → browser launches → a real HTTPS page was actually fetched). Anything that cannot
be proved working is printed as `degraded`, not `ready`.

### What it deliberately does not do

- It does not add any indexer to Prowlarr.
- It does not supply a VPN config, and **on a fresh install the namespace comes up with a
  deny-all kill switch and no tunnel** — TorrServer runs but cannot reach the internet. That
  is the intended safe state, not a failure.
- It does not create your admin account.
- It does not set `server.public_url`, which you **must** edit — see
  [first-run.md](first-run.md).

### Useful flags

```bash
sudo ./install.sh --only flaresolverr     # install just one component
sudo ./install.sh --repair flaresolverr   # force-reinstall one component
sudo ./install.sh --repair all            # force-reinstall everything
sudo ./install.sh --skip-preflight        # skip the environment checks
```
Valid component names: `torrserver`, `flaresolverr`, `jellyfin`, `prowlarr`.

---

## 4. Install from source

Requires the Go toolchain matching `go.mod` (Go 1.25 or newer). The orchestrator
cross-compiles cleanly, including to arm64.

```bash
git clone https://github.com/MoonTheRipper/JellyFreedom
cd JellyFreedom
./release/build.sh "$(cat VERSION)"
sudo ./dist/jellyfreedom-"$(cat VERSION)"*/install.sh
```

Cross-compile for another machine:

```bash
GOARCH=arm64 ./release/build.sh "$(cat VERSION)"
# copy dist/jellyfreedom-<version>-linux-arm64.tar.gz to the target, extract, run install.sh
```

`build.sh` produces exactly one tarball per invocation, plus a `.sha256`. It refuses to ship a
binary whose architecture does not match `GOARCH`, and it never copies your live
`config.yaml`, database or VPN configs into the bundle.

---

## 5. Updating

```bash
sudo jellyfreedom --update
```

This downloads the latest release **for this machine's architecture**, compares versions, and
runs the new bundle's own installer. It does not hand-swap files: an update that only replaced
the binary could never deliver a new systemd unit, a fixed sudoers path, or a new privileged
helper, and would leave a new binary calling a helper that was never installed.

Your `config.yaml`, database and uploaded VPN configs survive. If the version is unchanged it
exits without doing anything; `JELLYFREEDOM_FORCE=1` reinstalls anyway.

```bash
jellyfreedom --version    # what is installed
sudo jellyfreedom repair  # re-run the installer over this instance (safe, keeps everything)
jellyfreedom doctor       # full diagnostic with per-check remediation
jellyfreedom status       # systemctl status
jellyfreedom logs         # journalctl -u jellyfreedom -f
sudo jellyfreedom restart
```

The CLI refuses to operate on a source checkout — it only ever touches a deployed instance
under `/opt/jellyfreedom`.

---

## 6. Uninstalling

Three levels, least to most destructive:

```bash
sudo jellyfreedom uninstall           # our app, units, sudoers, AppArmor, namespace
sudo jellyfreedom uninstall --all     # also TorrServer, FlareSolverr and Prowlarr
sudo jellyfreedom uninstall --purge   # --all, plus the database, config and VPN configs
```

`--purge` asks you to type `PURGE` before it deletes anything (`--yes` skips the prompt).

Jellyfin is never removed automatically — `sudo apt-get remove jellyfin` if you want it gone.
Your `.strm` library folders under `/srv/jellyfreedom` are never deleted. Without `--purge`
the uninstaller prints exactly what it kept and where.

---

## 7. It finished — now what?

Run `sudo jellyfreedom doctor`, then follow [first-run.md](first-run.md). If something is
already wrong, go to [troubleshooting.md](troubleshooting.md).
