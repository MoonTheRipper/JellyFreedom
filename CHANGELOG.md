# Changelog

All notable changes to JellyFreedom are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Entries for 0.1.0 – 0.2.1 are backfilled from the published GitHub release notes.

## [Unreleased]

## [0.4.2] - 2026-08-22

### Fixed
- **`--only` and `--repair` aborted before verifying anything.** A path used while enabling
  services was assigned inside a section those flags skip, so the run died on an unbound
  variable partway through — a targeted repair did less than it reported. Now covered by a
  test that exercises every component.
- **The dashboard's log panel was empty.** It renders logs by running `journalctl` as the
  orchestrator's account, and a plain system account can only read its own entries. A
  regression from moving the service off a human login that happened to be in `adm`. The
  installer now grants read-only `systemd-journal` membership and `doctor` checks for it.
- **Playback was invisible in the logs.** `/play` logged only rejections and errors, so
  watching the journal while pressing play showed nothing whether it worked or not. Every
  request and its outcome is now logged with elapsed time.
- CI readiness checks converge with a deadline instead of sampling the instant the installer
  returns. Prowlarr — a .NET app that can take tens of seconds to boot — was found
  `inactive` on a 22.04 runner and failed a build for healthy code.

## [0.4.1] - 2026-08-22

### Fixed
- **The dashboard's "restart" button for JellyFreedom itself could never work.** The service
  was listed as restartable, but the hardened sudoers policy deliberately withholds
  `restart jellyfreedom` — a service able to bounce itself through root is a persistence
  primitive. It now restarts itself with no privilege at all: it shuts down by exactly the
  same path as SIGTERM (workers cancelled, WAL checkpointed, database closed) and exits
  non-zero so systemd's `Restart=` brings it back. It first confirms the unit's restart
  policy is `always` or `on-failure` — the only two that revive on a non-zero exit — and
  refuses with a clear message otherwise, rather than shutting down permanently.
- **FlareSolverr leaked Chrome processes.** A live instance had accumulated eight orphaned
  browsers reparented to init, holding 1.19GB of RAM, plus stale profile directories. The
  unit now states `KillMode=control-group` explicitly and prunes leftover profile
  directories before starting, scoped by owner and age so it can only touch its own.

### Added
- The dashboard shows the running version, the update state, when it last checked, and a
  **Check now** button that forces a fresh check. Previously the only signal was a banner
  that appeared when an update happened to exist, so "no banner" was ambiguous between
  "up to date" and "the check silently failed". A source build reports `dev` and is
  correctly described as not updatable rather than as an error.

### Known issues
- **FlareSolverr becomes unreliable after its first solve.** Measured on a clean 22.04
  runner: two solves succeeded and eight failed in a single run, the successes being the
  installer's own gating probe and every later attempt returning *"invalid session id:
  session deleted as the browser has closed the connection"* — including three deliberate
  retries fifteen seconds apart. The same shape appears on a long-running host. It also
  leaks a browser per session: one live machine had eight orphaned Chrome processes holding
  1.19GB. Behaviour differs by host and is not yet fully characterised: on a clean 22.04
  runner the first solve after a start succeeds and later ones fail, whereas on a
  long-running 26.04 host with the dedicated service account and the bundled browser every
  solve fails, including the first after a restart. The unit changes in this release stop
  orphans accumulating across restarts but do not fix the per-session leak, which is
  upstream. The installer's probe chain is what finds a working combination on a given
  host, and it reports which one it settled on.
  Ruled out by direct test as causes: AppArmor's unprivileged-userns restriction (the
  service account can create namespaces), a missing `--no-sandbox` (Chrome runs fine
  without it as that user), GPU device-group membership, and the profile directory. Chrome
  itself is healthy — it launches under the service account with its own profile and
  answers on the DevTools port — so the fault is inside `undetected_chromedriver`'s session
  handling, which hides the browser's stderr.
- On Ubuntu 24.04 and later the installer's probe chain often ends up running FlareSolverr
  as root, and says so plainly in its component table rather than hiding it. It runs
  unprivileged on 22.04.

## [0.4.0] - 2026-08-22

### Added
- **Update from the dashboard.** JellyFreedom checks for a newer release and shows a banner
  with a short summary of what changed; one click installs it and restarts the service.
  The check is cached, degrades silently when GitHub is unreachable, and never prompts a
  developer running a source build.
- The self-updater now **verifies the release checksum before installing**. It runs a
  tarball's installer as root, and the dashboard can trigger it with one click, so
  unverified bytes are refused outright.

### Fixed
- **An upgrade did not apply changes to the network namespace.** `systemctl enable --now`
  does not restart an already-running unit, so a new `setup-netns.sh` sat on disk while the
  namespace kept running under the old one — leaving the previous routing-only kill switch
  in place while the installer reported success.
- **AppArmor blocked the sanitised WireGuard config.** The privileged helper hands `wg-quick`
  a sanitised copy under `/run/vpntorrent`, but the AppArmor override only covered the
  uploads directory. `wg-quick` runs as root and was still denied, so the tunnel failed to
  come up with only a bare "Permission denied" in a unit log. These two masked each other:
  the second only appears once the namespace is actually restarted.
- Jellyfin is installed from its apt repository directly instead of piping upstream's
  convenience script, which requires 2GB free on `/tmp` (a tmpfs-backed VPS does not have
  it), blocks on a tty prompt, and rejects unrecognised distro codenames. Verified on a
  clean Ubuntu 24.04 container, where the old path failed and the new one succeeds.
- The browser fallback chain no longer re-tests a binary it has already tried, no longer
  waits the full probe window on a crash-looping service, and no longer interpolates apt's
  progress output into its own status messages.
- FlareSolverr's service account is added to the `video`/`render` groups where they exist,
  and the chain falls back to root — loudly, and reported as such — when no browser can
  complete a real fetch under the dedicated account.
- `.strm` files with no library row are re-signed by identity recovered from their own URL,
  instead of silently returning 403 once capability tokens are enforced.
- Code under `/opt/jellyfreedom` is root-owned; only data and library directories belong to
  the service account, which also holds `systemctl restart jellyfreedom`.

## [0.3.0] - 2026-08-22

### Fixed
- **The installer destroyed FlareSolverr's browser on Ubuntu.** It overwrote the bundled
  462 MB Chrome with a 61-byte wrapper pointing at `/usr/bin/chromium-browser` — which on
  Ubuntu is a transitional package whose only executable is a shim to the chromium *snap*.
  `apt-get install chromium-browser` exits 0 while installing no browser, so the
  `|| snap install` fallback never ran. There was no backup, and re-running reported
  "present — left alone", making it unrecoverable. The installer now never truncates the
  bundle (it renames, reversibly), and chooses a browser by **probe**: it installs a
  candidate, restarts, and requires a real page fetch to succeed, walking a fallback chain
  until one works.
- **The pinned TorrServer version 404'd.** `MatriX.141.2` was removed upstream, so every
  fresh install ended up with no streaming engine while the installer still reported
  success. The release is now resolved at runtime with a pinned fallback.
- **The Jellyfin install hung forever with no output, or failed outright.** It was piped to
  `bash` with output sent to `/dev/null`, and upstream's convenience script deliberately
  reads `/dev/tty` so that piping into bash cannot skip its confirmation prompt. It also
  hard-requires 2GB free on **both** `/var/lib` and `/tmp` — a small VPS has a tmpfs `/tmp`
  sized at half of RAM and fails, which was reproduced on a clean Ubuntu 24.04 container —
  rejects distro codenames it has not been taught, and can exit 0 having installed nothing.
  JellyFreedom now adds the apt repository itself: it writes the keyring and a `.sources`
  file, and probes `repo.jellyfin.org` for a published suite, falling back to the newest
  available rather than silently skipping Jellyfin on an unrecognised codename.
- **Sudoers rules were written as `/bin/systemctl`,** which sudo never matches (it compares
  the resolved `/usr/bin/systemctl` and does not follow the merged-usr symlink), so every
  service restart from the dashboard was silently denied. The rules needed for VPN
  activation were missing entirely.
- **SQLite ran without a busy timeout or WAL, and ~35 write errors were discarded.** Under
  concurrency 1266 of 1280 writes failed silently — including session creation, so a login
  could set a cookie for a session row that was never written.
- **Anonymous callers saw more than authenticated ones.** A `viewerUsername == ""` sentinel
  meant for the background job was also passed by the public HTTP handler, so logging out
  granted access to private items that logging in denied.
- **Any authenticated user could delete any other user's library items**; the drop handlers
  had no ownership check.
- **Non-admins could not change their own password** (the handler was registered behind the
  admin-only mux).
- **API keys leaked to unauthenticated callers.** Keys travelled in query strings and Go's
  `url.Error` embeds the full URL, which was returned verbatim in error responses.
- Data race on the TorrServer client's base URL, reproduced under `-race`.
- `ReleaseQuality` classified every **WEBRip as bluray** (`"webrip"` contains `"brip"`, and
  bluray was tested first), silently degrading release selection.
- `server.public_url` shipped as the literal `CHANGE-ME-LAN-IP` and is written into every
  `.strm`; the installer now fills it in from the detected LAN address, and `doctor` fails
  if it is still a placeholder.
- The library folders the installer told users to add to Jellyfin did not exist until the
  first successful request.
- `jellyfreedom --update` hand-swapped files and never refreshed units, sudoers, or new
  files, so no fix at that layer could ever reach an existing install. It now re-runs the
  bundle's installer, which is idempotent and preserves config, database and VPN configs.
- The service account could overwrite its own binary and restart itself. Code is now
  root-owned; only data and library directories belong to the service user.

### Added
- **`jellyfreedom doctor`** — diagnoses the whole stack with a concrete remediation for each
  finding, including a three-rung FlareSolverr check, because the upstream self-test
  resolves the browser and reads its version but navigates nowhere: a browser that dies on
  its first real fetch passes it.
- **`jellyfreedom repair`** and **`jellyfreedom uninstall`**; `uninstall.sh` is now installed
  to `/opt/jellyfreedom` so one-liner users can actually remove the software, with `--all`
  and `--purge` levels.
- Installer preflight (architecture, distro, disk, RAM, port conflicts, dpkg-lock wait) and
  a full transcript at `/var/log/jellyfreedom-install.log`.
- A hermetic installer test suite (`tests/install/`, 57 checks) and `tests/doctor/`
  (14 checks) that encode each shipped regression as a named test.
- Web assets are embedded with `go:embed`, so the binary no longer depends on a matching
  sibling directory; `--assets` remains as a development override.

### Added (project)
- Continuous integration (`.github/workflows/ci.yml`): build, `go vet`, tests, `gofmt`,
  ShellCheck over every shipped script, cross-compilation for `linux/amd64` and
  `linux/arm64`, and an end-to-end installer smoke test on clean Ubuntu runners.
- Automated release workflow (`.github/workflows/release.yml`): multi-arch bundles,
  `SHA256SUMS`, and signed build provenance attestations.
- Project documentation: `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`,
  `docs/RELEASING.md`, issue and pull-request templates.
- Releases now publish **arm64 bundles** alongside amd64, and `get.sh` selects the right
  one for your machine and verifies its SHA-256 checksum before installing.

### Changed
- `README.md` rewritten around the working install one-liner and checksum verification.
- Documentation restructured and published: user guides under `docs/` (install, first-run,
  configuration, troubleshooting, security, FAQ) and design docs under `docs/dev/`
  (architecture, decisions, operations, roadmap). They were previously excluded from the
  repository entirely.

### Security
- **Sudoers policy no longer grants a wildcard.** The `ip netns exec vpntorrent
  /usr/bin/curl *` and `wg show *` rules were root-equivalent — `curl` can read and write
  any file as root, and `wg show <if> private-key` prints the tunnel's private key. They
  are replaced by eleven fixed-argument rules against a root-owned helper
  (`/opt/vpntorrent/jf-netns-helper`) with a closed verb set and no caller-supplied
  arguments.
- **FlareSolverr no longer runs as root on `0.0.0.0`.** It fetches arbitrary URLs with no
  authentication, so this published an open SSRF proxy to the whole LAN, as root. It now
  runs as a dedicated `flaresolverr` user bound to `127.0.0.1`.
- **`GET /api/status` and `GET /api/leak` now require an admin session.** They return the
  host's real public IPv4, the VPN exit IP, and the WireGuard peer public key, and were
  reachable unauthenticated. A minimal public `GET /api/health/summary` replaces them for
  the UI health indicator.
- **Uploaded WireGuard configs are sanitised by an allow-list** before root's `wg-quick`
  reads them, in both the orchestrator and the privileged helper. `PostUp`, `PostDown`,
  `PreUp`, `PreDown`, `Table`, `SaveConfig` and anything else not explicitly allowed are
  dropped by construction.
- **`POST /webhook/jellyfin` requires a shared secret** (`X-JellyFreedom-Token`, compared
  in constant time, failing closed). The URL, header name and secret are shown read-only in
  Settings → Jellyfin webhook, with the secret masked until revealed.
- **Login is rate-limited**, keyed independently by client IP and by username.
- **`/play/...` URLs carry an HMAC capability token**, so a `.strm` URL this server wrote
  is the credential for streaming that item.

## [0.2.1] - 2026-06-04

### Added
- Redesigned GUI — a cinematic, auto-rotating featured hero on Home, floating glass
  navigation with the brand purple-to-cyan gradient, and richer poster cards (hover
  zoom, ratings, library state).
- Mobile overhaul — fixed bottom tab bar, full safe-area (notch/home-bar) handling,
  larger touch targets, and a responsive dashboard.
- Smarter "More like this" — the related list in a title's details is now ranked and
  grouped: **Franchise** (same TMDB collection) → **More Like This** (recommendations
  and similar) → **Starring _lead_** (the top-billed actor's other notable titles).

### Fixed
- Library `ready`/`stale` state now reflects real resolvability (an indexer search)
  rather than whether a torrent happens to be loaded in TorrServer. Watching a title no
  longer marks it "expired", and the health check no longer re-adds torrents in the
  background.
- The request queue stays consistent with the library: single requests are idempotent
  and stale `failed`/`done` rows are superseded, so a resolved title no longer shows a
  stale failure.
- Closing a title's details no longer scrolls the page back to the top.
- `/healthz` now reports the correct version.

## [0.2.0] - 2026-06-03

Additive release on top of 0.1.0 — no breaking changes.

### Added
- `jellyfreedom` control CLI (`/usr/local/bin/jellyfreedom`): `--update` downloads the
  latest release and swaps the binary and web assets in place, then restarts the
  service. Also `--version`, `status`, `restart`, `logs`. It only acts on an installed
  instance and refuses to run against a source checkout.
- Browser favicon for the server UI and the project website.
- `release/migrate-local.sh` — move an instance that was running out of a source
  checkout into standard FHS paths (`/opt/jellyfreedom`, `/etc/jellyfreedom`,
  `/var/lib/jellyfreedom`) without disturbing config, database, VPN netns, or media
  libraries.

## [0.1.0] - 2026-06-01

First public release.

### Added
- Self-hosted search and streaming of movies and TV from torrent sources straight into
  Jellyfin, watchable on any device, behind a fail-closed WireGuard kill switch.
  No Docker, no paid debrid.
- Debian/Ubuntu installer with a one-line bootstrap
  (`curl -fsSL .../get.sh | sudo bash`) and a downloadable release bundle.
- Dashboard at `http://<host>:1990/dashboard/` for admin account creation, connection
  keys, and WireGuard configuration upload.
- MIT licence.

[Unreleased]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/MoonTheRipper/JellyFreedom/releases/tag/v0.1.0
