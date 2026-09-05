# Changelog

All notable changes to JellyFreedom are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Entries for 0.1.0 – 0.2.1 are backfilled from the published GitHub release notes.

## [Unreleased]

### Fixed
- **Links added since 0.7.0 were invisible in Jellyfin.** The orchestrator's unit set
  `UMask=0077` — added to keep the database private — and the same service writes the
  **library**. So every `.strm` came out `0600` and every folder `0700`, owned by the service
  account. Jellyfin runs as a *different* user and simply could not read them: no error
  anywhere, the files present and correct on disk, and Jellyfin's count quietly lower than
  reality. On the affected box, 21 of 23 entries were visible.

  `UMask` is now `0022`, and the library writer sets `0755`/`0644` **explicitly** — `MkdirAll`
  and `WriteFile` take a mode the umask then masks off, `chmod` does not. Privacy was never
  this knob's job: the data directory, the VPN configs and the database each carry their own
  explicit restrictive mode.

  Existing libraries are repaired with:
  `sudo find /srv/jellyfreedom -type d -exec chmod 755 {} + && sudo find /srv/jellyfreedom -type f -exec chmod 644 {} +`
  then a Jellyfin library scan.
- **`doctor` now checks that Jellyfin can read the library**, because nothing else can detect
  this: a title that Jellyfin cannot read is not an error, it is an absence.
- **The `/tmp` reaper never worked.** Its `find` expressions were written with shell escaping
  (`\(`), but a systemd `ExecStart` is not a shell, so `find` received a literal `\(` and
  exited with *"paths must precede expression"* on every run since 0.6.2. The `-` prefix on
  the lines meant systemd ignored the failure and reported success, and the unit test asserted
  the file *contained* the right strings rather than that it worked. Both fixed: the reaper is
  now a script that is executed by the test.

## [0.7.3] - 2026-09-01

### Fixed
- **Every page could return 403 while every file was present and correct.** The installer
  inherited the umask of whatever shell invoked it, so running `--update` from a shell with a
  restrictive umask (`077` is common in hardened profiles) produced a `0700 root` asset tree
  under `/opt/jellyfreedom/web`. The service does not run as root, so it could not read its own
  web assets, and the media UI answered 403 for everything.

  The installer now pins `umask 022` and sets the asset tree mode explicitly rather than
  inheriting it. Anything that is meant to be private — the data directory, the VPN configs,
  the extractor scratch — is given an explicit `-m 700`, so nothing that was deliberately tight
  is loosened.
- **`doctor` said everything checked out while the site was down.** Its web-assets check asked
  only whether the directory existed, not whether the service account could read it. It now
  asks the question the service asks, and names the fix.
- **A flaky assertion in the installer harness.** Pipelines ending in `grep -q` exit as soon as
  they match, which SIGPIPEs the `sed` feeding them; under `set -o pipefail` the pipeline then
  reports 141 or 0 depending on scheduling, so the check failed at random. Replaced with
  `grep -c … >/dev/null`, which drains its input, and the reason is recorded in the harness so
  it is not reintroduced.

## [0.7.2] - 2026-09-01

### Added
- **`sudo jellyfreedom rotate-play-key`.** A play URL is a permanent bearer credential: it
  never expires, is bound to no user, and deleting the library item does not revoke it, because
  `/play` resolves the identity straight out of the URL. So a `.strm` that leaks — a backup, a
  screenshot, a shared library path — kept working forever, and the only way to revoke it was
  hand-editing SQLite. Rotating re-signs every `.strm` on the restart that follows, so your
  library keeps playing and Jellyfin needs no rescan; every URL already copied out stops working.

### Security
- **A renamed Jellyfin-backed account no longer repoints its password at someone else.**
  Authentication went to Jellyfin by *username*, while the account's `jellyfin_user_id` was
  never used — so renaming such an account silently pointed its credential at whoever now held
  that name in Jellyfin, handing them the row's admin flag and library grants. The ID Jellyfin
  returns is now compared against the stored one. Rows with no stored ID, and servers that do
  not return one, are unaffected.

### Fixed
- **A dead link no longer re-runs the extractor on every play request.** The torrent path has
  had a resolve cooldown since a measured incident — 7,813 ffprobe re-requests of one
  unplayable title in five minutes — but the web branch returns before that check, and only
  *successes* were cached. So a broken link spent a 90-second budget and unpacked ~76MB of
  yt-dlp bundle on every single request. Failures now enter a 60-second cooldown; a client that
  hung up mid-resolve does not count, and "check again" skips it.
- **Extractions are bounded server-side.** `preview`, `add`, `status` and `play` can each start
  a yt-dlp process, and nothing limited how many ran at once — the dashboard's `MAX_CONCURRENT`
  is browser-side, which `curl` in a loop with an admin cookie ignored entirely. All four paths
  now share a budget of three, because every run reaches the internet through one SOCKS proxy in
  one namespace: the tunnel is the bottleneck long before the CPU is.
- **Strings from the extractor are bounded.** The add handler checked the length and control
  characters of the title the *admin* typed, but when they supplied one it was the extractor's
  title that reached the database — and the uploader and extractor names were never checked at
  all. All three come from a third-party page, bounded only by yt-dlp's 8MiB stdout cap. Not
  XSS, since the dashboard escapes them; unbounded storage and control bytes in a JSON response,
  under a guard that read as though it covered them.

## [0.7.1] - 2026-08-31

### Security
- **Anyone could lock the admin out of their own dashboard.** The login limiter refused an
  attempt when *either* the IP bucket or the username bucket was blocked — so five failed POSTs
  for `username=admin`, from any address, with no credentials, locked the real admin out for up
  to five minutes, renewable forever. On a single-admin install that is a total denial of the
  dashboard, and far cheaper to mount than the distributed guessing the rule defends against.

  The username backoff now binds only an address that has *itself* failed here. A clean address
  is never refused because of somebody else's attack on that name — which is the property the
  code comment already claimed — while an attacker's own address still hits both budgets. The
  test that pinned the old behaviour was asserting the defect and has been rewritten.
- **Poster URLs are validated on the user-facing request paths.** `validPosterURL` existed but
  was applied only to the provider-ingest API; `/request`, `/request/season` and
  `/api/subscriptions` stored whatever they were handed. A poster is rendered as `<img src>` to
  *other* viewers, so an unvalidated one is a stored beacon — a low-privileged account learns
  when an admin looks, from that admin's real address, outside the VPN. Not XSS, which is why it
  read as harmless. Invalid values are dropped rather than failing the request.

### Fixed
- **The extractor's scratch directory is reaped too.** yt-dlp's official build unpacks ~76MB per
  run and is SIGKILLed whenever an extraction times out or the client disconnects mid-resolve —
  and a killed PyInstaller bundle cleans up nothing. `jf-tmpreaper` now covers
  `/var/lib/jellyfreedom/tmp` as well as the browser scratch in `/tmp`, so a repeatedly failing
  link cannot fill the data partition.

### Security
- **The installer now locks down Prowlarr instead of documenting that you should.** Stock
  Prowlarr binds every interface with authentication disabled for local addresses, and this
  installer only ever *read* the API key out of that file — so on a default install any host on
  the LAN could `GET /initialize.json` and receive the key in cleartext, which then lists every
  indexer definition including private-tracker credentials. Verified on a live box. Meanwhile
  `docs/security.md` said "Keep on localhost: Prowlarr (9696)".

  Where a Prowlarr login exists, it is now required for every address — reachability is
  unchanged, so a remote or Tailscale user keeps their UI and simply signs in. Where **no**
  login is configured, requiring one would lock the operator out, so Prowlarr is bound to
  localhost instead. `config.xml` is also chmod'd `600`; it holds the key.
- **`/api/logs` redacts.** The dashboard's log view returns journal lines verbatim, and the
  allow-listed units include `prowlarr` and `flaresolverr` — third-party services whose logging
  this project does not control and which routinely log full request URLs. Lines are now passed
  through `redact` on the way out.
- **The signed CDN URL is no longer written to the log** when a web-source fetch fails. The
  error wrapped the time-limited media address; only the host is logged now.
- **`uninstall --purge` removes `/var/lib/prowlarr`.** The installer created it; uninstall never
  removed it, so a user who purged believing their secrets were gone still had `config.xml`
  (the API key) and `prowlarr.db` (indexer credentials) sitting at `0755`.

## [0.7.0] - 2026-08-31

### Security
- **Web-source thumbnails were fetched by your browser, from the source site, outside the VPN.**
  The extractor returns a thumbnail on the site's own CDN, and that URL was stored and rendered
  as `<img src>` directly — so opening the library or the Links page made your browser connect
  to the tube site from your **home address**, with the signed CDN path intact. The site learned
  the household IP and exactly which video was in the library. That is the de-anonymisation the
  whole VPN design exists to prevent, arriving through an image tag, in the one feature whose
  own module header promises the browser only ever gets "a page URL, a title and a thumbnail".

  Thumbnails are now fetched server-side over the same namespace dialler as the video and served
  from this server. The relay takes a web-source **id**, never a URL, so it cannot be used as a
  general image proxy; it refuses anything that is not a plain JPEG, PNG, WebP or GIF, caps the
  relay at 5 MB, and logs only the host on failure rather than the signed URL.

  **Existing entries are repaired on the next start** — a migration repoints their `poster_url`
  at the relay. TMDB artwork is untouched.
- **A Content-Security-Policy, and the other browser headers, on every response.** The UI builds
  HTML with template strings and `innerHTML` throughout; the escaping discipline is currently
  intact but enforced by convention alone. A CSP does not fix a missed escape — it changes what
  one costs, from "full admin compromise, and from there the root-owned updater" to "nothing
  happens". `script-src 'self'` carries no `'unsafe-inline'`, so injected script cannot run, and
  `connect-src 'self'` means it could not exfiltrate anywhere if it did. Verified in a browser:
  all 11 dashboard modules and the media UI load with **zero** policy violations.
- **Cross-origin state-changing requests are refused.** CSRF defence rested entirely on
  `SameSite=Lax`, and SameSite is *site*-scoped: Jellyfin (`:8096`), Prowlarr (`:9696`) and
  TorrServer (`:8090`) are the **same site** as the dashboard, so an XSS or open redirect in any
  of that third-party code could drive the whole admin API — upload and activate a WireGuard
  config, repoint Prowlarr at an attacker's server, create an admin, trigger the updater. Requests
  are now checked against `Sec-Fetch-Site`/`Origin`; machine callers that send neither (the
  Jellyfin webhook, the provider ingest API) are unaffected and keep their shared-secret auth.
- **Playback authorisation failed open on restart.** Whether `/play` required a capability
  token was decided from scratch on every boot, and the persisted marker (`play.token_required`)
  was written but never read. One `.strm` that could not be read or re-signed — a library mount
  not up yet, a permission change, an identity the signer refuses — turned the **whole**
  capability system off for that process, on a machine that had been enforcing the day before.
  The only signal was a single log line, and `/play` would then resolve and start a torrent for
  any unauthenticated caller who could reach the port.

  Enforcement is now a ratchet: once an install has enforced, it stays enforced, and an
  unsignable file degrades that file rather than the control.
- **`GET /proxy/stream` was an unauthenticated existence oracle over the whole library.** The
  legacy hash-pinned route carried no capability token, so anyone who could reach the port and
  had an infohash learned 200-versus-404 whether that torrent was here — the exact question
  every other route goes out of its way not to answer, including for libraries the caller has
  been denied — and could then stream it. Infohashes were easy to come by: `Item.Redacted()`
  leaves `info_hash` in `/api/library`.

  It now carries a token over `hash:<infohash>:<index>`, in its own key space, so a token for a
  hash cannot validate an identity-based `/play` request or the reverse. The startup sweep signs
  surviving legacy `.strm` files in place, so they keep working rather than turning into 403s.
- **The database was world-readable, and session tokens are stored in plaintext.**
  `/var/lib/jellyfreedom` was created `0755` and SQLite wrote `jellyfreedom.db` at `0644`, so
  **every local account on the machine** — `torrserver`, `prowlarr`, `flaresolverr`, any human
  shell — could read it. A session token is the bearer credential, stored unhashed, so copying
  a row is being a logged-in admin; the same file also holds `play.hmac_key` (forge any
  playback token), the ingest and webhook secrets, and the TMDB, Prowlarr and Jellyfin API
  keys. From a dashboard admin session, `POST /api/update/apply` reaches the root-owned update
  helper — so file-read became root without exploiting anything.

  Confirmed on a live install by reading the database as an unrelated user. The data directory
  is now `0700`, the database file `0600`, and the service runs with `UMask=0077` so
  everything it writes later is private by default.
- **The `jf-update` sudoers rule accepted arbitrary arguments.** The file's own comment says it
  takes none, and the script ignores them — but the policy permitted any, so the safety came
  from the script rather than the boundary. Constrained to `""`.

### Fixed
- **Existing installs are not repaired automatically.** `sudo jellyfreedom --update` applies the
  new permissions to the directory, but a database file created before this release keeps its
  old mode until then. If you cannot update immediately:
  `sudo chmod 700 /var/lib/jellyfreedom && sudo find /var/lib/jellyfreedom -maxdepth 1 -type f -name '*.db*' -exec chmod 600 {} +`

### Added
- **Two release channels.** **Stable** is the versions that have been explicitly promoted —
  unchanged, and still what you get by default. **Nightly** is whatever is on `main`, built
  and published on every merge that touches code.

  ```bash
  curl -fsSL .../get.sh | sudo bash -s -- --channel nightly   # install nightly
  sudo jellyfreedom channel nightly                           # switch an install
  sudo jellyfreedom channel                                   # what am I on?
  ```

  The separation rests on GitHub's own rule rather than ours: a prerelease is never "the
  latest release", nightlies are prereleases, and the stable channel resolves through
  `releases/latest`. So no number of nightlies can move a stable install onto one.

  The channel lives in `/opt/jellyfreedom/CHANNEL` and survives updates — an upgrade that
  says nothing keeps the channel it is on, rather than resetting a nightly box to stable
  behind the user's back. Nightly tags are immutable `nightly-<date>-<sha>`; the workflow
  prunes to the ten most recent rather than moving a pointer, because moving a tag
  invalidates the provenance attestation and checksums of whatever it pointed at.

## [0.6.3] - 2026-08-30

### Fixed
- **Pasted links stopped playing after a restart.** Every `.strm` that did not come from
  TMDB was rewritten, at startup, to `/play/movie/0` — one URL and one shared token for all
  of them — and playback answered `bad tmdb id`.

  `migrateStrmTokens` rewrites every library `.strm` when the orchestrator starts, and built
  each URL from the row's `TMDBID` alone. A web source has no TMDB id, so that field is `0`
  for all of them. The rows carried their real identity in `provider` / `provider_id` the
  whole time; the rewriter simply never read it.

  The damage landed on an ordinary restart, days after the links were added and playing,
  which is why nothing caught it: adding a link never touches this path. Two rows of
  different identity also ended up sharing one capability token, since both hashed
  `movie:0`.

  **Entries repair themselves on the next start** — the rewriter now produces the correct
  URL and replaces the broken one. Nothing needs re-adding.

  The same TMDB-only assumption is fixed in the queue worker's library write, and both now
  refuse to write a `.strm` at all rather than write an empty one when an identity cannot be
  encoded.

## [0.6.2] - 2026-08-30

### Added
- **`/tmp` no longer fills up.** A new `jf-tmpreaper.timer` clears browser scratch
  directories hourly. FlareSolverr drives a browser per request and each launch leaves a
  directory behind; on a snap Chromium those land in `/tmp/snap-private-tmp/snap.chromium/tmp`,
  which is root-owned `0700` — so FlareSolverr's own `ExecStartPre` cleanup cannot see them,
  let alone delete them. A live box accumulated 3,894 directories, 7.7GB and 741k inodes,
  filling a 7.8GB tmpfs three times in four days. A full `/tmp` then breaks package installs,
  the updater (which stages its download there), and the installer's own preflight — none of
  which say `/tmp` in their error.

  The match is deliberately narrow: an entry must sit inside a snap's `tmp`, be untouched for
  an hour, **and** carry a browser's name. A stale directory costs a little memory; a wrong
  glob would delete somebody's work.
- **`doctor` reports free space on `/tmp`,** separately from `/`. It is usually a tmpfs sized
  at half of RAM and fills from a direction nothing else warns about.

## [0.6.1] - 2026-08-30

### Fixed
- **`jellyfreedom repair` deleted the installed web assets.** Repair re-runs the installer
  out of `/opt/jellyfreedom`, so its source directory and its destination were the same one.
  The web step is `rm -rf "$APP_DIR/web"` followed by a copy *from* that path, so it removed
  the assets and then reported `cannot stat '/opt/jellyfreedom/web'` for the directory it had
  just deleted. It also printed `install: ... are the same file` for every other component,
  and a red `✗ the bundle is missing vpntorrent/jf-netns-helper` about a helper that was
  installed and working the whole time.

  Nothing was permanently broken, because the orchestrator serves its assets from inside the
  binary — but the one command both `doctor` and the installer recommend for "most problems"
  was deleting files and reporting a failure that had not happened. The installer now detects
  that it is running from the copy it would be writing to, leaves those files alone, says so,
  and points at `jellyfreedom --update` for the case where the binary itself needs replacing.

  Covered by a new hermetic scenario that runs the installed copy the way repair does. No
  test exercised that path before, which is how this and the two `jf-netnsproxy` bugs before
  it all reached a release.

## [0.6.0] - 2026-08-30

### Added
- **Paste many links at once.** The Links box now takes a whole list — one per line, or
  separated by commas or spaces — and reads them in the background, two at a time, while you
  carry on pasting. Each row shows where it has got to, so a link that fails is visible next
  to the ones that worked instead of stopping the batch. Duplicates within a paste are
  dropped, anything that is not a link is ignored, and a failed row can be retried on its own.
  The library is chosen once, at the end, for the whole batch.

  Splitting is on whitespace and commas only. Not on colons, however reasonable a separator
  they look: every URL contains `://`, so splitting there would cut each link in half.

### Changed
- **The library is chosen from a list rather than typed.** A mistyped library name was
  refused with the same "unknown library" the server gives for one you are not allowed to
  use — correct, deliberately indistinguishable, and useless as a spelling correction. The
  list offers movie-type libraries only, because a web source is a single video with no
  season or episode and the server refuses anything else.

## [0.5.5] - 2026-08-27

### Fixed
- **Upgrading from 0.5.3 left the VPN proxy dead even with the fix installed.** A machine
  running the broken 0.5.3 has a `jf-netnsproxy.service` that failed five times in a row,
  and systemd then refuses every further start with *"Start request repeated too quickly"* —
  including the one that would have run the corrected unit. The update wrote the right file,
  reported honestly that the proxy did not stay up, and web sources stayed unavailable until
  someone ran `systemctl reset-failed` by hand. The installer now clears the start limit
  before restarting.

## [0.5.4] - 2026-08-27

### Fixed
- **Web sources did not work at all on a real install.** `jf-netnsproxy.service` — the VPN
  proxy every paste-a-link extraction and stream is dialled through — exited immediately on
  every start. Its sandbox granted `AF_INET AF_INET6 AF_UNIX`, but the service works out its
  own listen address from the namespace's veth using `net.Interfaces()`, and that is a
  `NETLINK_ROUTE` socket. Without `AF_NETLINK` the lookup failed with "address family not
  supported by protocol", the proxy could not find `10.42.0.2`, and systemd gave up after
  five restarts. Adding the family fixes it.
- **The installer reported the proxy as started when it had already died.** `systemctl
  restart` returns once the process has forked, not once it has stayed up, so a service that
  failed 200ms later still printed a tick — and `websources` was summarised as *ready* on a
  system where it could not work. The installer now re-checks with `is-active` a moment
  later and says plainly when the proxy did not stay up.

## [0.5.3] - 2026-08-27

### Added
- **Paste a link to a video page and it becomes a library entry.** A new **Links** section in
  the dashboard takes a video page URL, extracts what the video is, shows you the title,
  thumbnail, duration and resolution, and writes a `.strm` that Jellyfin plays like anything
  else. It needs no indexer, which is the point: indexers are good at films and episodes and
  poor at everything else.

  The link is re-resolved **at play time**, exactly as the torrent path re-resolves a
  release. Sites sign their video addresses with an expiry measured in hours, so anything
  frozen into a `.strm` would stop working by Friday — saving the *page* is what makes the
  entry last. Nothing but the page URL is written to disk.

  Both the extraction and the stream go **through the VPN**, with the same fail-closed kill
  switch as torrent traffic, and there is no direct-connection fallback: with no proxy
  configured the feature switches itself off rather than fetching anything outside the
  tunnel. See [security.md](docs/security.md) for how that works and what it does and does
  not hide.

  New: `web_sources` config section, `jf-netnsproxy.service`, `/play/p/web/{id}`,
  `/api/websources*` (admin only), a `web_sources` table, and a `jellyfreedom doctor
  websources` section.

- **`orchestrator netns-proxy`** — a minimal SOCKS5 proxy that runs inside the `vpntorrent`
  namespace and lends it to the orchestrator, which has to stay outside to serve the LAN. It
  refuses every non-public destination, so it cannot be turned around into a route back into
  the host or the LAN, and it inherits the namespace's kill switch rather than having one of
  its own.

- **yt-dlp** is installed by the installer to `/usr/local/bin`, with its own scratch
  directory at `/var/lib/jellyfreedom/tmp`. It is the only optional third-party component: a
  failed download is a warning, and the install still completes.

### Fixed
- A movie with no year no longer produces a folder and file called `Title ()`. Only reachable
  since something other than TMDB could create a movie entry — a pasted link has an upload
  date, not a release year.

### Upgrading
- **Web sources are off until you switch them on.** An upgrade never rewrites your existing
  `config.yaml`, so the new `web_sources` block is not added for you. Run `sudo jellyfreedom
  doctor websources` — it prints the exact block to paste, then restart the orchestrator.
- Do **not** point `web_sources.temp_dir` at `/tmp`. The yt-dlp binary unpacks ~76 MB of
  itself there on every run, and `/tmp` is a RAM-backed tmpfs on stock Ubuntu.

## [0.5.2] - 2026-08-27

### Added
- **Browse and filter by genre, studio and type.** The home page previously offered five
  fixed rows and the search box took a query and nothing else. There is now a filter panel —
  media type, genres with an any/all toggle, studio, year, sort and a vote floor — plus a
  Movies/TV/All tab on search results. New endpoints `GET /api/genres` and
  `GET /api/studios` back it.
- **Per-user library visibility.** An admin chooses which libraries each account can see, so
  a household can keep some libraries off a child's account. Absence of a grant is a denial,
  admins bypass it, and the stock single-admin install needs no configuration.
- **An ingest API for external providers.** A local process that has resolved something
  itself can register it under its own provider namespace; JellyFreedom stores it, writes the
  `.strm` and streams it, without doing any metadata lookup of its own. This is what lets a
  third-party source — an anime database, a personal archive — put titles in the library.
- **Identity is keyed on a provider**, not on a TMDB id alone, and play URLs can name one:
  `/play/p/{provider}/movie/{id}`. Existing TMDB URLs, identity strings and capability tokens
  are unchanged and frozen.
- `release/bump.sh` — cuts a release in one step: moves `VERSION`, closes off the changelog
  section, repoints the comparison links, and commits on a `release/<version>` branch.
  Takes `patch`, `minor`, `major`, or an exact version. It refuses a dirty tree, a version
  that is already tagged, and an empty `## [Unreleased]`, and it re-runs the release
  workflow's own note extraction against the file it just wrote — so a mistake surfaces
  before the tag is pushed rather than after.

### Fixed
- **The genre rows on the home page were showing the wrong thing.** TMDB reads a comma in a
  genre filter as AND, so "Action & Adventure" was asking for titles that are *both* — 8,422
  of them instead of 69,246, and 2,725 instead of 54,598 for "Sci-Fi & Fantasy". Both rows
  had been quietly serving the narrow intersection since they were written.
- **A malformed release title could no longer confuse the filename sanitiser.** It filtered
  characters but permitted `..` and an empty result, so the safety of every file this program
  writes rested on a format string in the caller rather than on the sanitiser. Paths are now
  checked after resolution, and a write that would escape its directory is refused outright.
- TMDB requests are built with proper URL encoding instead of string concatenation, and the
  TMDB key is registered with the log-redaction filter on every configuration path rather
  than only one.

### Security
- **The library was readable by anyone who could reach the port.** `GET /api/library`
  returned every item — titles and library names — to callers with no session, and the
  service listens on all interfaces. Signed-out callers now see nothing. This is the change
  most likely to be noticed: see *Upgrading*.
- Search parameters sent to TMDB are built from a strict allowlist, so nothing a caller
  supplies is forwarded upstream as written.

### Upgrading
**Signed-out visitors no longer see the library.** If you browse without signing in, the
library and queue pages will be empty until you do. This is deliberate — a per-library
permission that a user can sidestep by signing out is not a permission — but it is a visible
change to how the app behaves on a shared network.

**Existing non-admin accounts see nothing until granted.** Admins are unaffected and the
single-admin install needs no action. If you have other accounts, grant their libraries
before or immediately after upgrading, via `PUT /api/users/{id}/libraries`. The dashboard
does not yet render this as a checkbox list.

## [0.5.1] - 2026-08-24

### Fixed
- **The release picker looked like it had hung.** Searching your indexers takes real time —
  43 seconds against 16 indexers in one measurement — and the picker filled that with a bare
  shimmer, which is indistinguishable from a frozen screen. It now says what it is doing,
  counts the seconds, and after twenty of them adds that a slow answer is normal rather than
  a fault. The styles for this shipped in 0.5.0 but the code that renders them did not, so
  0.5.0 carried the rules with nothing to use them.


## [0.5.0] - 2026-08-23

A queue that could bury your whole library under one title, a Request button that had never
worked for a signed-in user, and a picker that ignored resolution entirely.

### Fixed
- **The queue could fill with thousands of copies of one title.** A request carrying an
  explicit magnet skipped every idempotency check, and nothing else stood in the way — the
  queue table had no unique constraint and the endpoint no rate limit. One client re-firing
  put 19,430 identical rows in a live queue in ten minutes, and the worker then spent a day
  re-resolving the same magnet every five seconds: an indexer search, a TorrServer add and
  drop, an identical `.strm` rewrite and a Jellyfin library scan, per row.
- **Your shows could vanish from the queue page entirely.** The queue list is capped at 100
  rows, newest first, so a burst of duplicates at the head of the list pushed every real
  title out of view. Requesting a release is now idempotent regardless of magnet, a partial
  unique index makes a duplicate unrepresentable, and re-picking a release repoints the row
  you are already watching instead of adding one beside it.
- **Request, Request Season, Subscribe and every Remove button did nothing when signed in.**
  The sign-in guard ran its own callback, so each of these called back into itself until the
  stack overflowed; because the callers are async the error surfaced as an unhandled
  rejection and the button simply failed in silence.
- **The wrong episode could be grabbed and cached as correct.** The episode matcher tested
  `s1e5` as a substring, which also matches S1E50 through S1E59, and the same test chose the
  file inside a season pack. Every file in a `Show.S01E01-E10.COMPLETE/` folder also matched
  a request for E01, so the largest file in the pack was returned with confidence.
- **The 2018 film *Cam* could never be picked.** The camera-rip filter ran against the whole
  release title, film name included, so every release of it was scored as a cinema rip. The
  same flaw hit `.ts` as a container and `TC` as a Traditional-Chinese tag.
- **A per-library `reject_cam` setting was silently ignored** and the global value used.
- **Airing seasons were labelled "Complete".** A season holding every *aired* episode of a
  running show rendered as a finished one, leaving no signal when the next episode dropped.
  "Complete" and "Up to date" are now different states, as are "aired but not acquired" and
  "not out yet".
- **Subscribed shows re-searched permanently unobtainable episodes every six hours, forever.**
  A failed attempt did not block a retry and nothing cleared it; one show had accumulated 501
  failed rows since June.
- **"Clear finished" cleared at most 100 items** however many there were, because it deleted
  row by row over one page of results.
- **Signed-out visitors were shown the sign-in dialog on arrival.** The first "who am I"
  request is expected to be unauthenticated; it was being treated as a session expiring.
- Release titles no longer have words mangled during matching — "Ghosts" was being reduced to
  "ghos" and "Cutthroat Island" to "throat island" — and `DD+`-style audio written as `DDP`
  is now recognised.

### Added
- **The queue and the library are now trees: show → season → episode.** Seasons load on
  demand, so an episode is reachable whether or not it falls inside the newest hundred rows.
  Anything needing attention sorts to the top, and "jump to active" goes straight to the
  episode being worked on. Failed outranks in-progress at every level, because a season with
  failures needs a person even while other episodes are still fetching.
- **Resolution and direct play are now ranking signals.** Neither was scored at all, and the
  codec, audio and container bonuses combined were worth less than a single step on the
  seeder ladder — so a 480p release with many seeders beat a 1080p one with fewer, every
  time. New picker settings: `target_resolution` (default 1080p), `require_direct_play`
  (default off) and `max_mbps`, which bounds *bitrate* rather than raw size. A 60 GB remux
  and a 4 GB WEB-DL of the same film are 65 Mbps and 4.5 Mbps; only one of them streams.
- The VPN watchdog now checks that torrent traffic is **anonymous**, not merely working. It
  reads the address the tunnel actually exits on and refuses to report healthy if that is
  this machine's own public address or the default route has left the tunnel — stopping
  TorrServer if so, and restarting it once anonymity returns.

### Security
- `GET /api/releases` was unauthenticated and returned magnet links and info hashes to
  anyone who could reach the port — on a service that listens on all interfaces. It now
  strips both for callers without a session and rate-limits anonymous searches. The two
  fields go together: a bare info hash is a working magnet for anything the DHT can find.
- `/play` ran the full 90-second resolver for metadata probes, and repeated a failed resolve
  without limit — one episode was probed 7,813 times in five minutes. Probes are answered
  from the library or declined, and a failed resolve is remembered briefly.

### Upgrading
The database migration **deletes duplicate queue rows**, keeping the oldest in-flight row per
title and the most recent outcome per title. On the deployment this was found on, that took
the queue from 26,187 rows to 1,591. Back up `/var/lib/jellyfreedom/jellyfreedom.db` first if
you want the history.

Automatic picks will change. Resolution and direct play now carry real weight, so a
well-seeded low-resolution release that used to win will often lose to a better one.

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

[Unreleased]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.7.3...HEAD
[0.7.3]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.7.2...v0.7.3
[0.7.2]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.7.1...v0.7.2
[0.7.1]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.7.0...v0.7.1
[0.7.0]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.6.3...v0.7.0
[0.6.3]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.6.2...v0.6.3
[0.6.2]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.6.1...v0.6.2
[0.6.1]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.5.5...v0.6.0
[0.5.5]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.5.4...v0.5.5
[0.5.4]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.5.3...v0.5.4
[0.5.3]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.5.2...v0.5.3
[0.5.2]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.5.1...v0.5.2
[0.5.1]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.4.2...v0.5.0
[0.4.2]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.4.1...v0.4.2
[0.4.1]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.2.1...v0.4.0
[0.2.1]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/MoonTheRipper/JellyFreedom/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/MoonTheRipper/JellyFreedom/releases/tag/v0.1.0
