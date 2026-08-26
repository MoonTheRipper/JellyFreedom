# Security Policy

## Reporting a vulnerability

Use **[GitHub's private vulnerability reporting](https://github.com/MoonTheRipper/JellyFreedom/security/advisories/new)**
— the *Report a vulnerability* button under the repository's **Security** tab. Include a
description, the affected version, and reproduction steps.

The report is visible only to the maintainer, so please use it rather than opening a public
issue for an unfixed vulnerability. It needs no email address from either of us, and it
keeps the whole exchange in one place with a private fork for the fix if one is needed.

This is a single-maintainer hobby project. Expect an acknowledgement within about a week
and a fix on a best-effort basis. There is no bug bounty.

### Supported versions

Only the latest release receives fixes. Older tags are not patched.

---

## Threat model — read this before you deploy

JellyFreedom is designed for a **single household on a trusted LAN**. It is not hardened
for hostile networks, and it is not multi-tenant software.

> **Do not expose JellyFreedom to the public internet.**
> Not on port 1990, not behind a naive port-forward. If you need remote access, put it
> behind a private overlay network (Tailscale, WireGuard, ZeroTier) or an authenticating
> reverse proxy that you control.

This document is the security *posture and policy*. For the operational side — what is and
is not tunnelled, how to verify the kill switch, seeding behaviour — see
[`docs/security.md`](docs/security.md).

### The orchestrator binds to all interfaces, over plain HTTP

`release/config.sample.yaml` ships `server.listen: "0.0.0.0:1990"`. The service listens on
every interface, and there is **no TLS anywhere in the orchestrator**. Anything that can
reach the host on port 1990 reaches the routes below.

Narrow `listen` to your LAN interface or to `127.0.0.1` (and front it with a proxy) if you
want a smaller surface. If you do put it behind an HTTPS proxy, also set
`server.secure_cookies: true` so the session cookie carries the `Secure` flag — it is off by
default because on plain HTTP a `Secure` cookie is simply never sent.

### Not every route requires authentication

Three tiers, implemented in `internal/api/auth.go` (`RequireAuth`, `RequireAdmin`,
`OptionalAuth`) and wired in `cmd/orchestrator/main.go`. **Any `/api/` path not explicitly
registered falls through to `RequireAdmin`** — the default is closed.

**Unauthenticated — anyone who can reach port 1990:**

- `GET /` and the media UI assets, `GET /search`
- `GET /healthz`, `GET /readyz`, `GET /api/version`, `GET /api/health/summary`,
  `GET /api/configured`, `GET /api/playback/active`
- TMDB read-through: `GET /api/tmdb/{id}/full`, `.../seasons`, `.../seasons/{n}/episodes`
- Browse and filtering: `GET /api/browse/trending`, `GET /api/browse/discover`,
  `GET /api/genres`, `GET /api/studios` — every one of these turns an anonymous request
  into an outbound TMDB call on this instance's API key, so all four share a fixed-window
  limit of **180 requests per minute per address** (the homepage alone fires one discover
  per carousel on load, so the budget is deliberately generous; what it stops is a script
  in a loop). `/api/browse/discover` accepts filters only from an explicit allowlist in
  `tmdb.DiscoverParams` — caller-supplied parameters are never forwarded to TMDB, so an
  anonymous caller cannot inject `api_key` or any other upstream parameter, and every
  value is validated (numeric ids, a fixed sort allowlist, bounded `page`/`min_votes`)
  with a `400` rather than being coerced.
- Library and queue reads: `GET /api/libraries`, `GET /api/library`,
  `GET /api/library/status`, `GET /api/queue`, `GET /api/queue/groups`,
  `GET /api/queue/count`,
  `GET /api/subscriptions`, `GET /api/calendar` — reachable without a session, but every
  one of them is now filtered by **per-library visibility** (below), and an anonymous
  caller holds no library grants. In practice that means an anonymous caller sees an
  empty library, an empty queue and an empty subscription list on any install that has
  libraries configured. `GET /api/queue/count` is the exception: it is a single global
  number of in-flight items and names no library.
- `GET /api/releases` — readable anonymously, but **magnet links and info hashes are
  stripped** for callers
  without a session, and anonymous use is rate-limited to 20 searches per minute per
  address. See the note below.
- Streaming: `GET /play/movie/{tmdb}`, `GET /play/tv/{tmdb}/{season}/{episode}`, and the
  legacy `GET /proxy/stream` — see *Streaming routes* below.
- `POST /webhook/jellyfin` — session-less by necessity, but secret-gated. See below.
- `POST /api/auth/login` — see *Sessions and login* below.

**Session required (`RequireAuth`):** `POST /request`, `POST /request/season`, the queue
cancel/delete/diagnosis routes, `DELETE /api/queue/finished` (bulk clear — an admin clears
everyone's terminal rows, a signed-in user only their own, and in-flight rows are never
touched), subscription create/delete, the `POST /api/library/.../drop`
routes, and `POST /api/auth/change-password`.

**Admin session required (`RequireAdmin`):** the dashboard, `GET /api/status`,
`GET /api/leak`, `/api/settings*`, `/api/vpn/configs*`, `/api/users*` — including
`GET /api/users/{id}/libraries` and `PUT /api/users/{id}/libraries`, the per-library
access controls described below — `/api/logs`, `/api/tasks`,
`/api/services/{name}/restart`, `/api/vpn`, `/api/debug/releases`, and every unregistered
`/api/` path.

> **Fixed:** `GET /api/releases` was registered with no auth wrapper at all and returned
> `picker.ScoredRelease`, which embeds `indexer.Release` and therefore its `magnet` field.
> Every magnet a search turned up was readable by anyone who could reach port 1990 — and
> the service listens on all interfaces, so that includes the whole LAN and any tailnet it
> is joined to. The endpoint also drove a live Prowlarr query with a 150-second budget and
> no throttle, which made it a free way to pin the indexers. It is now wrapped in
> `OptionalAuth`: signed-in callers get magnets, anonymous callers get the same list with
> `magnet` **and `info_hash`** blanked, and anonymous searches are rate-limited per
> address. Both fields go together — `magnet:?xt=urn:btih:<hash>` is a working magnet on
> its own for anything the DHT can find peers for, so stripping only one would be theatre. The web UI had
> already been written against this contract — it shows "sign in to force this exact
> release" when a magnet is absent — so only the server side was missing.

> **Fixed:** `GET /api/status` and `GET /api/leak` used to be public. They return the host's
> real public IPv4, the VPN exit IP, and the WireGuard peer public key — an unauthenticated
> caller could deanonymise the box. They are now `RequireAdmin`, and the UI's health dot uses
> the deliberately minimal public `GET /api/health/summary` instead.

### Per-library visibility

An admin decides which libraries each account may see (`GET` / `PUT
/api/users/{id}/libraries`, both admin-only). It is the ordinary parental-controls
feature: a household where the adults' libraries should not appear for a child's account.
A library an account cannot see is invisible to it **everywhere** — the library list, the
library itself, the search-result status badges, the flat queue, the grouped queue tree,
subscriptions, the release calendar, and the per-request diagnosis — not merely absent
from one list.

**The default is deny.** An account with no grants sees no library at all. The
alternative — visible-until-revoked — means that adding a library to `config.yaml`
exposes it to every account on the box, including the child's, until somebody remembers
to go and take it away. The cost of denying by default is that a newly created account
opens onto an empty app until the admin grants it something; the cost of allowing by
default is not recoverable after the fact.

The **default single-admin install is unaffected and needs no configuration**: an admin
bypasses the gate entirely, so the stock deployment never creates a grant and never needs
one.

How it is enforced, and what to check if you change it:

- **In the store queries, not in the handlers** (`internal/store/store.go`). Every method
  whose answer depends on who is asking takes a `store.Viewer`, and the library predicate
  is spliced into its `WHERE` clause. The zero `Viewer` is anonymous, non-admin and holds
  no grants, so a handler that forgets to fill it in denies rather than discloses.
  `internal/store/libaccess_test.go` walks `*Store` by reflection and fails if a method
  taking a `Viewer` is not in its audited read/mutation tables — a read path added later
  without a filter fails a test rather than leaking quietly.
- **Writes are gated too.** Requesting into a library you cannot see is refused, never
  silently redirected: `POST /request`, `POST /request/season` and `POST /api/subscriptions`
  authorise the destination before doing any work. A request that names no library
  resolves to the configured default when the caller may use it, otherwise to the first
  library of that type they can.
- **Responses are deliberately indistinguishable.** "No such library" and "that library
  exists but is not yours" are the same `400 unknown library`, because a caller who can
  tell them apart can enumerate every library on the box by guessing names. Likewise a
  queue row or library item in a hidden library answers `404`, and a cancel or delete
  aimed at one reports zero rows affected — the same answers as for something that never
  existed. Ownership is checked only *after* visibility, so the pre-existing `403 that
  item belongs to someone else` can never confirm the existence of an item in a hidden
  library.
- **It composes with the per-item `is_private` gate rather than replacing it.** Both
  predicates are ANDed: a library grant never exposes somebody else's private item, and a
  public item never escapes a hidden library.
- **An empty `library_name` is exempt.** `config.Load` refuses a library with an empty
  name, so no configured library is ever called `""`; a row carrying it is a legacy row
  from before libraries existed, or a request that named no destination, and it stays
  governed by the ownership and privacy rules that already covered it. Nothing a
  non-admin does can create one, because the request handlers resolve an empty library to
  a concrete one first.

Two limits worth understanding before you rely on this:

> **This gates JellyFreedom, not Jellyfin.** The `.strm` files live in Jellyfin's own
> libraries, and Jellyfin has its own per-user library permissions. Hiding a library here
> stops an account discovering, requesting into or managing it through JellyFreedom; it
> does **not** stop a Jellyfin user who can already see that Jellyfin library from playing
> what is in it. Configure both.

> **`/play/...` and `/proxy/stream` are not gated, and cannot be.** Jellyfin clients fetch
> `.strm` URLs with no session, so there is no viewer to filter by — possession of the URL
> is the credential (see *Streaming routes* below). A play URL for an item in a hidden
> library still plays. What keeps it out of a restricted account's hands is Jellyfin's own
> library permissions, not this feature.

### Streaming routes and capability tokens

`/play/...` cannot require a session: Jellyfin clients fetch `.strm` URLs anonymously and
have no cookie to present. Instead the identity is signed. `cmd/orchestrator/playtoken.go`
HMACs the identity (`movie:<tmdb>` / `tv:<tmdb>:<season>:<episode>`) with a server-side key
stored in the database, and the tag travels in the URL as `?t=`; `validPlayToken` compares
in constant time. Possession of a URL this server wrote **is** the credential.

Two caveats you should understand:

- **Enforcement is gated.** It switches on only after a startup migration has retokenised
  every existing `.strm` (`play.token_required`). Until that has run on your install,
  `/play/...` accepts untokenised requests, because rejecting them would break playback of
  every item already in the library.
- **A capability URL is a bearer credential.** It does not expire and is not bound to a
  user. Anyone who obtains a `.strm` file or a play URL can stream that item, and
  `/play/...` will resolve and start a torrent on demand.

`GET /proxy/stream` is the legacy hash-pinned path kept for `.strm` files written before
Resolve-at-Play. It validates the infohash and index but carries **no capability token** —
anyone who knows an infohash on your box can stream it.

### The Jellyfin webhook

`POST /webhook/jellyfin` requires the shared secret in an `X-JellyFreedom-Token` header,
compared in constant time, failing closed (`cmd/orchestrator/helpers.go`). The secret is
generated on first run and stored in the settings table.

> **Known gap:** the secret is **not surfaced anywhere a user can reach it.** It is not in
> `GET /api/settings`, not in the dashboard, and nothing in `web/` mentions it. The only way
> to read it is a direct SQLite query against `/var/lib/jellyfreedom/jellyfreedom.db` — and
> `sqlite3` is neither installed nor in the installer's package list, so you would have to
> `apt-get install sqlite3` first. **In practice the webhook cannot currently be configured**,
> which means it fails closed and playback-stopped torrent cleanup does not happen. This is
> tracked in `docs/dev/roadmap.md`. It is a usability bug, not a hole.

### First-run admin claim is a race

`/dashboard/setup` is public until the first user exists, and that first user is an admin.
**Whoever reaches the box first owns it.** Create the admin account immediately after
installing, before the host is reachable by anyone else.

### Sessions and login

- Cookie `jf_session`, 24-byte random token, 24-hour TTL, stored in SQLite.
- Flags: `HttpOnly`, `SameSite=Lax`, `Path=/`. `Secure` follows `server.secure_cookies`,
  which defaults to **false** for plain-HTTP LAN deployments.
- Passwords are bcrypt-hashed at `bcrypt.DefaultCost`.
- Login is rate-limited (`internal/api/ratelimit.go`, wired into both `LoginHandler` and
  `APILoginHandler`). The limiter is keyed independently by client IP **and** by username and
  both must have budget, so one host cannot walk every account and a botnet cannot lock a
  known user out.
- There is no CSRF token. `SameSite=Lax` is the only cross-site protection on state-changing
  `POST` requests.

---

## The privilege boundary

### What the scoped sudoers rules permit

`release/install.sh` writes `/etc/sudoers.d/jellyfreedom` (mode 440, validated with
`visudo -cf`). It grants the `jellyfreedom` service user, passwordless, **eleven
fixed-argument rules and no wildcards**:

```
/usr/bin/systemctl restart jellyfin.service
/usr/bin/systemctl restart torrserver-netns.service
/usr/bin/systemctl restart vpntorrent-netns.service
/usr/bin/systemctl restart prowlarr.service
/usr/bin/systemctl restart flaresolverr.service
/opt/vpntorrent/jf-netns-helper status
/opt/vpntorrent/jf-netns-helper exit-ip
/opt/vpntorrent/jf-netns-helper leakcheck
/opt/vpntorrent/jf-netns-helper routes
/opt/vpntorrent/jf-netns-helper vpn-up
/opt/vpntorrent/jf-netns-helper vpn-down
```

Every rule is a complete command line. None accepts a caller-supplied argument, so there is
nothing to inject across the sudo boundary — every dynamic value the helper needs (config
path, resolve address) is computed inside the helper, on the trusted side.

> **Fixed:** earlier releases granted `ip netns exec vpntorrent /usr/bin/curl *` and
> `... wg show *`. Both were root-equivalent: `curl -o /etc/sudoers.d/x` writes any root
> file, `curl file:///etc/shadow` reads any root file, and `wg show <if> private-key` prints
> the tunnel's private key. Compromising the orchestrator process meant root on the host.
> That is no longer the case.

The helper's security depends on **`/opt/vpntorrent/jf-netns-helper` and its containing
directory being root-owned and not writable by the service user** — the installer sets
`root:root` `0755` on both. If you relocate or edit it, preserve that.

### Uploaded WireGuard configs are sanitised twice

`wg-quick` executes `PostUp` / `PostDown` / `PreUp` / `PreDown` as **root shell commands**,
so an uploaded `.conf` is untrusted input on a path to root. Two independent layers handle
it:

1. **In the orchestrator** (`cmd/orchestrator/helpers.go`) before the file is stored.
2. **In the helper** (`vpntorrent/jf-netns-helper`, `vpn-up`) immediately before root's
   `wg-quick` reads it.

Both rebuild the file from an **allow-list** rather than blacklisting: only
`PrivateKey`, `ListenPort`, `MTU`, `FwMark`, `Address` in `[Interface]` and `PublicKey`,
`PresharedKey`, `Endpoint`, `AllowedIPs`, `PersistentKeepalive` in `[Peer]` survive.
Everything else — `PostUp`, `PostDown`, `PreUp`, `PreDown`, `Table`, `SaveConfig`, `DNS`,
unknown sections — is dropped by construction. Allow-listed values are additionally rejected
if they contain shell metacharacters, and the result must still have `[Interface]`,
`[Peer]`, a `PrivateKey` and an `Endpoint` or the config is refused.
`cmd/orchestrator/vpn_test.go` asserts this against a deliberately hostile config.

### FlareSolverr

FlareSolverr fetches arbitrary URLs with no authentication — an open SSRF proxy if exposed.
The installer runs it as a dedicated **`flaresolverr` system user** bound to
**`127.0.0.1`**.

> **Fixed:** it previously ran **as root on `0.0.0.0`**, publishing an unauthenticated
> URL-fetching proxy to the entire LAN, as root.

The installer no longer replaces or truncates FlareSolverr's bundled Chrome, and no longer
installs a system Chromium unconditionally; it uses the bundle as shipped and repairs a
bundle a previous installer had damaged. The `--no-sandbox` flag is passed by FlareSolverr
upstream, not by us — it is upstream's design, and a reason not to point FlareSolverr at
untrusted URLs.

### Where secrets live

| What | Path | Protection |
|---|---|---|
| TMDB / Prowlarr / Jellyfin API keys (file config) | `/etc/jellyfreedom/config.yaml` | mode `640`, owned by the service user |
| Keys set via the dashboard, password hashes, sessions, the play HMAC key, the webhook secret | `/var/lib/jellyfreedom/jellyfreedom.db` | directory owned by the service user |
| Uploaded WireGuard configs, **including private keys** | `/var/lib/jellyfreedom/vpnconfigs/` | mode `700`, owned by the service user |

Keys entered in the dashboard are stored server-side and never returned to the browser.
Nothing is encrypted at rest — anyone with root on the host, or a backup of these paths, has
the secrets. `.gitignore` excludes `config.yaml`, `*.db`, `*.conf` and `vpnconfigs/` so they
cannot be committed by accident; verify before publishing a fork.

### Privacy posture (the part that is deliberately strong)

Torrent traffic runs inside the `vpntorrent` network namespace, whose only default route is
the WireGuard interface (`vpntorrent/setup-netns.sh`). If the tunnel is down there is no
default route, so torrent traffic is **blocked, not leaked** — fail-closed by construction
rather than by firewall rule, with an `ip6tables` `OUTPUT DROP` policy and a disabled-IPv6
veth as the second layer. CI asserts the fail-closed behaviour on every run
(`installer-smoke`, assertion R2). Jellyfin-to-client traffic intentionally does **not** go
through the VPN; it stays on your LAN.

### Third-party components

JellyFreedom installs and drives Jellyfin, Prowlarr, FlareSolverr, and TorrServer. Their
security is their own; report issues in those projects upstream.

---

*Verified against the tree on 2026-08-26. If you are reading this after changes to
`internal/api/auth.go`, the route table in `cmd/orchestrator/main.go`, the sudoers block in
`release/install.sh`, or `vpntorrent/jf-netns-helper`, re-verify before trusting the details
above.*
