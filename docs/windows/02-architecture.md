# 02 — Architecture

```
 Apple TV / iPhone / Browser
        │  (LAN, no VPN)
        ▼
    JELLYFIN  ── universal frontend for every device
        │
        ▼
  Library of .strm pointer files  ◄── written by the ORCHESTRATOR (Go — the only thing we write)
        │       each contains a stable /play/... URL, not a frozen info hash
        ▼
   ORCHESTRATOR /play  ── resolves the best LIVE release at play time, proxies the stream
        │
        ▼
   TorrServer  ── streaming engine, bounded RAM ring buffer → no disk bloat
        │       (runs INSIDE the vpntorrent network namespace)
        ▼
   WireGuard + fail-closed kill switch
        │
        ▼
    torrent swarm
```

**The orchestrator is the only thing we write.** Everything else is off the shelf and all of
it has a Windows build. That is the good news for the port.

## Resolve-at-Play, in detail

This is the core idea and the thing most likely to be broken by a well-meaning refactor.

A `.strm` file is a one-line text file that Jellyfin treats as a playable item. Ours contains
an **identity URL**, never a media URL:

```
http://192.168.178.2:1990/play/movie/550?t=<capability token>
http://192.168.178.2:1990/play/tv/1622/1/4?t=<token>
http://192.168.178.2:1990/play/p/web/movie/ZvDA7Dz11ymui5yKVxka3Q?t=<token>
```

When Jellyfin fetches that URL, the orchestrator:

1. validates the capability token (an HMAC over the identity string);
2. searches Prowlarr for that identity **now**;
3. picks a release (`internal/picker`);
4. hands the magnet to TorrServer, which begins streaming;
5. proxies the bytes back, honouring `Range` so seeking works.

So a title in the library never goes stale. If the release it played last month is dead, it
picks a different one this month, and nothing in Jellyfin changes.

### Identity encoding

Two shapes, and they live in separate key spaces on purpose:

```
TMDB (frozen — .strm files in the wild carry it):
    movie:<id>                       tv:<id>:<season>:<episode>
Any other provider:
    p:<provider>:movie:<id>          p:<provider>:tv:<id>:<season>:<episode>
Legacy hash-pinned route:
    hash:<infohash>:<index>
```

`p` and `hash` occupy field 0 in their own right, so a provider literally named `p` or `tv`
cannot produce a colliding identity. Provider ids are `^[A-Za-z0-9_-]{1,64}$` and providers
`^[a-z0-9]{1,16}$` — anchored, and neither charset contains `:`. This was audited
specifically for collisions and none was found. **Do not change this encoding casually**;
`.strm` files already written depend on the TMDB shape byte for byte.

## Capability tokens

`/play/...` and `/proxy/stream` cannot require a login — Jellyfin fetches them with no
session cookie. Instead each URL carries an HMAC over its identity, keyed by `play.hmac_key`
in the database. Possession of a valid `.strm` is the credential.

Consequences you must preserve:

- Enforcement is a **ratchet**. Once an install has enforced, it enforces. It used to be
  re-derived each boot, which meant one unreadable `.strm` disabled the whole control (see
  [07](07-security.md#2-play-enforcement-failed-open-on-restart--medium-high)).
- Tokens never expire and are not bound to a user, so the only revocation is rotating the
  key: `jellyfreedom rotate-play-key`, after which every `.strm` is re-signed on restart.

## The VPN namespace (Linux)

A network namespace called `vpntorrent`:

- has WireGuard (`wg0-vpntorrent`) inside it and **no other default route**;
- is joined to the host by a veth pair on `10.42.0.0/30` — host `10.42.0.1`, namespace
  `10.42.0.2`;
- runs `iptables -P OUTPUT DROP`, allowing only `lo`, the tunnel, and the veth subnet;
- hosts **TorrServer** (`:8090`) and **jf-netnsproxy**, a small SOCKS5 proxy on `10.42.0.2:1080`.

The orchestrator lives *outside* the namespace, on the LAN, and reaches TorrServer over the
veth. Anything that must egress through the tunnel — web-source extraction, thumbnail
fetching, the media stream itself — dials the SOCKS proxy.

**This is the part that does not exist on Windows.** See [04](04-the-vpn-problem.md).

## Data model

SQLite (`modernc.org/sqlite`, pure Go — no cgo, which matters for the Windows build).

- `items` — the library. Identity is `(provider, provider_id)`; `tmdb_id` is the TMDB
  spelling of it and stays for `.strm` URLs and the web UI.
- `queue` — in-flight requests, with a `stage` stepper for the UI.
- `web_sources` — paste-a-link entries; id is `base64url(sha256(canonical page url))[:16]`.
- `users`, `sessions`, `user_library_access` — accounts and per-library visibility.
- `settings` — key/value: API keys, `play.hmac_key`, `ingest.secret`, `webhook.secret`.

Library visibility is **default-deny** and enforced in the SQL of every viewer-scoped read,
not in handlers — "a predicate in a handler is a predicate the next handler forgets". Admins
bypass the gate entirely, so a single-admin install never sees it.
