# 07 — Security

A five-reviewer audit ran on 2026-08-31 against v0.6.3: authentication and capability tokens,
the privilege boundary, web sources and SSRF, the browser surface, and secrets at rest.
Thirteen findings were fixed across 0.7.0–0.7.2. **Assume every one of them has a Windows
analogue until you have checked.**

## Fixed — and what the Windows equivalent must be

### 1. The database was readable by every local account — HIGH

`/var/lib/jellyfreedom` was `0755` and SQLite wrote the file `0644`. Confirmed by reading it
as an unrelated user with no sudo.

That file holds `sessions.token` **in plaintext** — a copied row *is* a logged-in admin, no
password, no rate limit — plus `play.hmac_key` (forge a token for any identity),
`ingest.secret`, `webhook.secret`, and the TMDB, Prowlarr and Jellyfin API keys. And an admin
session reaches `POST /api/update/apply`, which runs the root-owned updater. File-read became
root without exploiting anything.

The privileged design around it is genuinely good — exact-path sudoers, an argument-free
helper, root-owned code the service cannot rewrite. This walked past all of it through a
missing `-m` flag.

**Fixed:** directory `0700`, file `0600`, unit `UMask=0077` — three locks, because any one
can be undone by hand.

> **Windows:** `os.Chmod` is a no-op for this purpose. You must set ACLs at install time
> (`icacls`) and **verify them at runtime**, or this protection silently does not exist.
> Consider hashing session tokens as well, so a file leak is not directly replayable.

### 2. `/play` enforcement failed open on restart — MEDIUM-HIGH

Whether tokens were required was decided from scratch every boot, and the persisted marker
`play.token_required` was written but never read. One `.strm` that could not be re-signed
turned the entire capability system **off** for that process, on a machine that had been
enforcing the day before.

**Fixed:** a ratchet. Once enforced, always enforced; an unsignable file breaks that file.

> **Windows:** keep the ratchet. The failure mode is identical.

### 3. `/proxy/stream` was an unauthenticated existence oracle — MEDIUM

The legacy hash-pinned route validated an infohash and looked it up, so `200` vs `404` told
any unauthenticated caller whether that torrent was in this library — including libraries
they had been denied — and then streamed it. `Item.Redacted()` leaves `info_hash` in
`/api/library`, so the hashes were published.

**Fixed:** a capability token over `hash:<infohash>:<index>`, in its own key space.

### 4. Web-source thumbnails leaked the home IP — MEDIUM

The extractor returns a thumbnail on the source site's CDN, and that URL was rendered as
`<img src>` directly. Opening the library **or merely previewing a pasted link** made the
browser connect to the tube site from the home address, outside the tunnel, with the signed
CDN path intact.

**Fixed:** relayed server-side through the same dialler as the video; allow-lists
JPEG/PNG/WebP/GIF (an SVG can carry script), caps at 5 MB, logs the host not the URL. Existing
rows migrated.

> **Windows:** the same trap exists for any third-party image. If you keep TMDB artwork
> loading directly, be deliberate about it — that is a different threat model from a tube site.

### 5. Prowlarr handed its API key to the LAN — HIGH

Stock Prowlarr binds every interface with `AuthenticationRequired=DisabledForLocalAddresses`,
and the installer only ever *read* the key out of that file. Any host on the network could
`GET /initialize.json` and receive the key in cleartext; that key lists every indexer with its
private-tracker credentials. Verified live.

**Fixed:** the installer now requires the login for every address where one exists, or binds
to localhost where none does, and chmods the config.

> **Windows: your installer must do this too.** It is the same Prowlarr with the same default.

### 6–13, briefly

- **No CSP or security headers.** Now `script-src 'self'` with no `'unsafe-inline'` (there is
  not one inline `<script>` in the UI), plus `connect-src 'self'`, `frame-ancestors 'none'`,
  `nosniff`, `no-referrer`. The UI builds HTML with template strings and `innerHTML`
  throughout; the escaping is currently perfect but enforced only by convention, so the CSP
  is what makes a future missed `esc()` cost nothing.
- **CSRF rested entirely on `SameSite=Lax`** — which is *site*-scoped, so Jellyfin (`:8096`),
  Prowlarr (`:9696`) and TorrServer (`:8090`) are the **same site** as the dashboard. Now
  also checks `Sec-Fetch-Site`/`Origin` on state-changing requests, while letting
  header-less machine callers through.
- **`/api/logs` returned journal lines verbatim**, including from third-party units that log
  full request URLs. Now redacted.
- **Anyone could lock the admin out**: the login limiter blocked on *either* the IP or the
  username bucket, so five failed POSTs for `admin` from anywhere locked the real admin out.
  Now the username backoff binds only an address that has itself failed.
- **Poster URLs were unvalidated** on the user-facing paths — a stored beacon revealing an
  admin's real IP when they opened the queue.
- **The `jf-update` sudoers rule accepted arbitrary arguments.**
- **`uninstall --purge` left `/var/lib/prowlarr`** with the API key in it.
- **A renamed Jellyfin-backed account repointed its password** at whoever now held that name.

## Still open

Both were deferred for the same reason: they are unverifiable without being able to bring the
tunnel up and check it immediately afterwards.

1. **TorrServer binds `*:8090` inside the namespace**, so it is reachable over the tunnel if
   the VPN provider does not isolate clients. Fix is an INPUT rule in the namespace.
   **On Windows this is probably moot or completely different** — re-derive it rather than
   porting it.
2. **`jellyfreedom.service` has no systemd hardening.** The obvious directives are not free:
   the unit spawns privileged helpers that enter a netns and run `wg-quick`, so
   `RestrictNamespaces` breaks `setns`, `ProtectKernelTunables` blocks the sysctls `wg-quick`
   sets, and `ProtectHome` breaks a library under `/home`. Every failure reads as "the VPN
   stopped working". **Windows equivalent:** run the service as a least-privileged account,
   not LocalSystem, and give the elevated helper a closed verb set.

## Judged sound — do not re-litigate without new evidence

The reviewers specifically confirmed: no SQL injection (all placeholders; the one
`fmt.Sprintf` builds a `?,?,?` run); no path traversal (`safeName`/`containedPath` probed with
`..`, NUL, zero-width, BiDi, 240-dot inputs — **but on Linux only**, so re-audit for Windows
device names); the yt-dlp argv is shell-free with a `--` terminator; the SOCKS proxy refused
every obfuscated-IP form (`2130706433`, `0177.0.0.1`, `127.1`, IPv4-mapped IPv6) because it
resolves first and checks the result; `hmac.Equal` everywhere; and no collision exists in the
play-token identity encoding.
