# Decision log

Why the system is the way it is. These are settled: read the reasoning before proposing a
reversal, and if one is ever reversed, update this file in the same change.

Numbering is stable — code comments reference these identifiers.

---

## D1 — Jellyfin is the frontend; our code is a backend

**Decision:** Do not build a player or a per-device UI. Use Jellyfin everywhere and feed it
`.strm` files.

**Why:** An Apple TV cannot run an arbitrary web app well, but it runs Jellyfin natively. One
backend plus Jellyfin's existing clients means never rebuilding a player per device.

**Rejected:** A custom web app as the primary watch interface. Fine in a browser, a dead end
for the living room.

## D2 — TorrServer, not a hand-rolled engine

**Decision:** Use TorrServer as the torrent-to-HTTP engine.

**Why:** General torrent libraries persist every piece and seed whole files — the direct cause
of a previous build filling its disk. TorrServer is built for streaming, with a bounded
RAM-backed ring buffer that evicts behind the playhead. Storage stays flat and there is little
to seed. It is also a single Go binary, which fits the no-Docker rule.

**Rejected:** Re-using WebTorrent with a custom store, LRU eviction, sequential pieces and
destroy-on-stop. Possible in principle; it is exactly the fiddly plumbing TorrServer already
solved.

## D3 — No debrid service

**Decision:** Torrent streaming, not a paid debrid provider with an rclone mount.

**Why:** Cost. Debrid is the cleaner answer for both privacy and instant start, and it is ruled
out on budget alone.

**Consequence:** Privacy leans on WireGuard plus a kill switch plus minimised seeding instead
of debrid's HTTPS-direct model.

## D4 — No Docker

**Decision:** Native binaries and single-binary deploys only.

**Why:** Operator preference, and every component supports it: TorrServer is one Go binary,
Jellyfin has an apt repository, Prowlarr ships a self-contained tarball, and the orchestrator
builds to one static binary.

## D5 — Go for the orchestrator

**Decision:** Go.

**Why:** The workload is I/O-bound orchestration glue — HTTP calls, JSON, file writes,
webhooks, a few background loops. Go gives a single static binary, a cheap long-running daemon,
trivial concurrency, static types, and fast compiles. TorrServer is also Go, which helps when
debugging the integration.

**Rejected:** Rust (borrow-checker and async friction for performance the network guarantees
you will never notice); Node (its ecosystem advantage evaporates once TorrServer is the
engine); Python (needs an interpreter and a venv on the box).

## D6 — Availability is owned here, not by Radarr/Sonarr/Jellyseerr

**Decision:** Availability means **resolvable** — a healthy magnet exists right now. The *arr
stack is kept out of the streaming library's availability path entirely.

**Why:** *arr defines available as "a downloaded file exists", which streaming never satisfies,
producing perpetual missing states and faked import events. Jellyseerr inherits this through
its *arr coupling.

**Allowed:** A two-library hybrid — an *arr-managed library of real files alongside the
streaming library. Never both models in one library.

## D7 — Minimise seeding

**Decision:** A bounded RAM cache, a low but nonzero upload cap, drop-after-validate, and
drop-on-playback-stop.

**Why:** Most seeding is idle post-watch seeding of a completed file, and dropping kills it. A
bounded cache means there is never a complete file to share in the first place. Upload is low
but **not zero**: true zero gets you choked under tit-for-tat and wrecks your own stream.

**Evolved:** The queue worker now drops each torrent the moment it has validated the file and
written the `.strm`; playback re-adds on demand. Requesting a whole season therefore leaves
*zero* background torrent load.

## D8 — Seeders dominate the picker's score

**Decision:** Swarm health is the primary factor. Codec, audio and container are tie-breakers
that can never outrank a meaningfully better-seeded release.

**Why:** A low-seed release in a "better" codec stalls or is simply dead, which is useless for
streaming. An earlier picker weighted preferred codec at +300 and capped seeders at +40, and
duly chose a dead 16-seeder release over a live one with over a thousand. Seeder count is also
a rough liveness signal, so high-seed picks survive validation and playback far more often.

## D9 — Resolve-at-Play

**Status: implemented.** `/play/movie/{tmdb}` and `/play/tv/{tmdb}/{season}/{episode}` exist,
and `.strm` files contain those URLs.

**Decision:** The `.strm` points at a stable, identity-keyed orchestrator URL rather than a
frozen `link=<hash>`. At play time the orchestrator serves a cached hash if it is still
connectable, and otherwise searches again, ranks by current seeders, validates, and streams.

**Why:** A baked hash decays. Seeders die, releases vanish, and the pointer rots — forcing a
bulk re-request cycle. Identity-keyed resolution makes the library self-healing, and collapses
validation into the same on-demand step as playback so it stops competing with streaming for
TorrServer's single cache.

**Trade-off:** 5–15 seconds on a cold first play, mitigated by the per-item hash cache, the
indexer-warmup task, and next-episode pre-warming.

**Would change my mind:** only if sub-second cold start mattered more than self-healing — in
which case, baked hashes plus aggressive periodic re-validation. Not recommended.

## D10 — The VPN server must support P2P; port forwarding is an optional bonus

**Decision:** Require a server the provider flags for P2P. Where the provider offers NAT-PMP
port forwarding, a keeper service holds the mapping and keeps TorrServer's listen port in sync.

**Why:** On a non-P2P or strict-NAT server, tracker announces and peer *connections* succeed
but **piece data will not transfer** — torrents connect to dozens of seeders and download zero
bytes. It looks exactly like a bug and is not one. Port forwarding, where available, roughly
doubled connectable seeders in testing.

**Diagnose** with a stream read reaching HTTP 206, **not** with TorrServer's `downloaded`
counter, which stays at 0 until something actually reads the file.

**Scope:** NAT-PMP is provider-specific (a `10.2.0.1` gateway on some providers) and entirely
optional. Without it, leeching still works; only inbound connectivity is reduced.

## D11 — Remux and audio transcode on; video transcode off

**Decision:** On the Jellyfin user policy, enable playback remuxing and audio transcoding,
disable video transcoding.

**Why:** Disabling *everything* produces "media is not supported by this client" for an
h264-in-MKV release in a browser — the cheap container remux and audio transcode that fix it
were forbidden along with the expensive one. The thing that actually saturates a network is
**video** transcoding: a full re-encode while pulling from the swarm. Remux and audio transcode
pull at playback rate. A truly video-incompatible release now fails instead of re-encoding, and
the picker's direct-play preference keeps that rare.

## D12 — Playback pre-empts background work

**Decision:** Active playback is realtime priority. Background workers and daemons must yield.

**Why:** A bulk re-request worker churning torrents through TorrServer's single shared cache and
BT client **crashed the BT engine mid-session** — every subsequent add returned 500 — and
degraded playback badly.

**Implemented:** `GET /api/playback/active` exposes whether a Jellyfin session is playing. The
watchdog skips its TorrServer probe entirely while a stream is live, and requires two
consecutive failures before restarting anything. The port-forward keeper defers its
restart-on-port-change until playback stops. Resolve-at-Play removed the background bulk
resolve phase that caused the original problem.

**Residual:** a session *paused* during a genuine crash reads as playing and briefly delays
recovery. Acceptable.

## D13 — No user-specific paths; FHS, config and environment everywhere

**Status: applied.** The system installs and runs entirely under FHS paths with dedicated
service users.

**Decision:** Nothing hardcodes a developer's machine — no `/home/<user>` paths, no baked-in
usernames. Use `/opt/jellyfreedom`, `/etc/jellyfreedom`, `/var/lib/jellyfreedom`,
`/opt/vpntorrent`; read the service user dynamically; make paths overridable by config or
environment.

**Why:** A released stack installs on arbitrary machines. Identical-to-mine paths break every
install but one.

## D14 — VPN configs are owned by the orchestrator, not `/etc/wireguard`

**Decision:** WireGuard configs are uploaded, listed, activated, deleted and downloaded from
the admin dashboard. They live in an orchestrator-owned directory
(`/var/lib/jellyfreedom/vpnconfigs`, mode 700) so the app can manage them **without root**.
Activation materialises the chosen config there and restarts the namespace units.

**Why:** Chosen over a privileged file-writing helper that could write anywhere under
`/etc/wireguard`. `wg-quick` is granted read access through an AppArmor local override.
Uploading never activates: that is always an explicit click.

**Consequence:** because the service user controls the bytes root's `wg-quick` will parse,
sanitisation is not optional — see D16.

## D15 — Provider-agnostic WireGuard; the release ships zero secrets

**Decision:** The system requires only a valid WireGuard config, from any provider or
self-hosted. Nothing is tied to one vendor. The release bundle ships no API keys and no VPN
configs.

**How it is enforced:**
- The tunnel, kill switch and namespace DNS are provider-neutral. DNS uses public resolvers
  *through the tunnel*, so there is no real-IP leak and no dependency on a provider's own
  resolver (overridable with `VPNTORRENT_DNS`). The endpoint is derived from the config file.
  Upload validation checks only WireGuard structure.
- NAT-PMP port forwarding is optional and degrades silently to nothing (D10).
- The sample config has empty keys and a placeholder `public_url`; the build script copies only
  that sample and never a live config, database or VPN config; `.gitignore` excludes all of
  them.

## D16 — One root-owned helper with a closed verb set, and two-layer config sanitisation

**Decision:** All privileged operations go through `/opt/vpntorrent/jf-netns-helper`, root-owned
in a root-owned directory, reachable through sudo rules that name exact verbs with **no
free-form arguments**: `status`, `exit-ip`, `leakcheck`, `routes`, `vpn-up`, `vpn-down`, plus
five fixed `systemctl restart` commands.

**Why:** The previous policy granted `ip netns exec vpntorrent /usr/bin/curl *` as root. That is
root-equivalence: `curl -o` writes any file as root, `curl file://` reads any file, and
`wg show <if> private-key` prints the tunnel's private key. Every dynamic value that used to
cross the sudo boundary — the `--resolve` address, the config path — is now computed inside the
helper, on the trusted side.

The same policy previously used `/bin/systemctl`, which sudo never matches: it compares the
resolved path (`/usr/bin/systemctl`) and does not follow the merged-`/usr` symlink. Every
service restart from the dashboard was silently denied.

**Sanitisation, in two layers.** `wg-quick` runs `PostUp`/`PostDown`/`PreUp`/`PreDown` through
`bash` as root, and the config directory is writable by the service user (D14) — one uploaded
`PostUp` line would be a root shell. So: the upload handler strips those directives plus
`Table`, `SaveConfig` and `DNS`; and at bring-up the helper **rebuilds** the config from scratch
into a root-owned file under `/run`, emitting only allow-listed keys and refusing any value
carrying shell metacharacters. Rebuilding rather than filtering removes any parser differential.
`Table` is dropped specifically because it can suppress route installation and silently defeat a
routing-based kill switch.

**Both layers are required:** layer two makes the escalation impossible rather than merely
unlikely, and survives any future bug in layer one.

## D17 — Capability URLs for playback; a shared secret for the webhook

**Decision:** `/play/...` and `/proxy/stream` stay unauthenticated but carry an HMAC over the
item's identity, computed with a server-side key generated on first run. The Jellyfin webhook is
authenticated with a shared secret and fails closed.

**Why:** Jellyfin fetches a `.strm` URL with no session cookie, so requiring a login would break
every client. But an open `/play` used to accept any identity, and `/proxy/stream` accepted any
attacker-supplied info hash and would add that torrent — an unauthenticated stranger could make
the box download arbitrary content over the owner's VPN. Possession of a valid `.strm` is now
the credential. Enforcement switches on only after a startup pass has rewritten every existing
`.strm`, so it cannot break a pre-existing library.

The webhook is state-changing and cannot carry a session, so it uses the custom header the
Jellyfin plugin does support. With no secret configured it refuses every call rather than
accepting anonymous ones.

## D18 — The installer proves what it claims, and never destroys what it cannot rebuild

**Decision:** Four rules, each learned from a specific failure:

1. **Never report success for a component that has not been proved to work.** A file existing is
   not evidence. FlareSolverr is probed in three stages, and only a real HTTPS fetch counts —
   its own startup self-test reads the browser's version and navigates nowhere, so a browser
   that dies on its first real fetch passes the naive check and presents to the user as
   "searches return nothing".
2. **Never truncate; rename.** An earlier installer replaced FlareSolverr's bundled 462 MB
   Chrome with a 61-byte wrapper pointing at `/usr/bin/chromium-browser` — which on Ubuntu is a
   transitional package containing only a shim that execs the snap. `apt-get install
   chromium-browser` exits 0 while installing no browser, so the fallback never ran, there was
   no backup, and re-running reported "present — left alone". Unrecoverable without a 233 MB
   re-download.
3. **Never pin what upstream may retag.** The pinned TorrServer tag was removed upstream and
   began returning 404, so fresh installs got no streaming engine while the installer still
   printed "Installed". The current release is resolved at runtime, with a pin only as fallback.
4. **Never discard a third-party installer's output.** Jellyfin's upstream script deliberately
   reads `/dev/tty`, so piping it into `bash` *cannot* skip its confirmation prompt. With output
   redirected to `/dev/null` it hung forever, silently. It now runs with upstream's supported
   `SKIP_CONFIRM` and everything is teed to a log.

Corollary: re-running the installer is the supported repair path, so it must be idempotent and
must never touch user config, data or VPN configs. `--update` therefore runs the new bundle's
own installer rather than hand-swapping files — an update that only replaced the binary could
never deliver a new unit, a fixed sudoers path, or a new privileged helper.

## D19 — The bundled Chrome is fine; the sandbox is the problem

**Decision:** Run FlareSolverr's bundled browser as shipped. Do not substitute a system browser.

**Why:** The belief that bundled Chromium 142 crashes on newer kernels is **wrong** — verified
by strace. What actually happens without `--no-sandbox` is `FATAL … No usable sandbox!`, because
Ubuntu 24.04 and later restrict unprivileged user namespaces via AppArmor and FlareSolverr's
`chrome_sandbox` ships mode 0755 rather than setuid, leaving no fallback. Upstream already
passes `--no-sandbox` itself, so a wrapper adding it was always redundant and only obscured
which binary really ran.

**Consequence:** the browser runs with reduced isolation. Do not point FlareSolverr at untrusted
URLs.

**Note:** FlareSolverr publishes x64 binaries only, so it is skipped on arm64 with an explicit
message rather than installing a binary that cannot exec. Running it from source against a
distribution `chromium` is the arm64 path — that is what the official multi-arch container does
internally, and it needs no Docker to replicate.
