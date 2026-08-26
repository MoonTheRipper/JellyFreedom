# Configuration

The config file lives at **`/etc/jellyfreedom/config.yaml`** (mode 640, owned by the
`jellyfreedom` user). The installer writes it from `release/config.sample.yaml` on a fresh
install and **never touches it again** — upgrades and repairs leave your settings alone.

After editing it:

```bash
sudo systemctl restart jellyfreedom
```

---

## Where a setting actually comes from

Three different rules apply, and mixing them up is a real source of confusion.

| Group | Rule |
|---|---|
| **Connections** — TMDB key, Prowlarr URL/key, Jellyfin URL/key, TorrServer URL | The file **seeds the database once**, on first run. After that the **database wins** and the dashboard edits it. Editing the file later has no effect. |
| **Quality and cache** — `picker.*`, `torrserver.cache.*` | The file is the base; any value ever saved in the dashboard **overrides it at every startup**. |
| **Everything else** — `server.*`, `libraries`, `vpn.config_dir` | **File only.** There is no dashboard equivalent; it takes effect on restart. |

Keys entered in the dashboard are stored server-side and are never sent back to the browser.
That is the recommended way to set them — the file is there so an automated deployment can
seed a box without a browser.

To take a connection setting back from the database, clear it in the dashboard, or delete the
row directly:

```bash
sudo apt-get install -y sqlite3        # not installed by default
sudo sqlite3 /var/lib/jellyfreedom/jellyfreedom.db \
  "delete from settings where key like 'conn.%';"
sudo systemctl restart jellyfreedom
```

### Environment overrides

Read at startup, and they override the **file value** — which means they only matter as the
first-run seed for connection settings.

| Variable | Overrides |
|---|---|
| `TMDB_API_KEY` | `tmdb.api_key` |
| `INDEXER_API_KEY` | `indexer.api_key` |
| `JELLYFIN_API_KEY` | `jellyfin.api_key` |
| `LISTEN` | `server.listen` |

---

## `server`

```yaml
server:
  listen: "0.0.0.0:1990"
  public_url: "http://192.168.1.50:1990"
  secure_cookies: false
```

| Key | Default | What it does |
|---|---|---|
| `listen` | `127.0.0.1:8080` if the key is absent; the shipped file sets `0.0.0.0:1990` | Address and port for the media UI, the dashboard and the streaming endpoints. Narrow it to a specific interface to shrink the surface; if you set `127.0.0.1` you need a reverse proxy for LAN clients to reach it. |
| `public_url` | `http://localhost:1990` if absent; **the shipped file has a placeholder you must replace** | The URL written **inside every `.strm` file**. Must be reachable from your Jellyfin server, and from clients on playback paths that fetch it directly. |
| `secure_cookies` | `false` | Sets the `Secure` flag on the session cookie. Leave false for plain HTTP on a LAN — turning it on without TLS stops the browser sending the cookie at all and logs everyone out instantly. Turn it on only behind a TLS-terminating proxy. |

> **`public_url` is the single most commonly missed setting.** Left at
> `http://CHANGE-ME-LAN-IP:1990` it produces a library full of items that will not play, and
> no error explains why. Existing `.strm` files are rewritten at startup when it changes, so
> fixing it later works.

---

## `tmdb`, `indexer`, `jellyfin`

```yaml
tmdb:
  api_key: ""
indexer:                              # Prowlarr
  base_url: "http://127.0.0.1:9696"
  api_key: ""
jellyfin:
  base_url: "http://127.0.0.1:8096"
  api_key: ""
```

| Key | Default | Consequence if unset |
|---|---|---|
| `tmdb.api_key` | empty | No search, no posters, no metadata. Everything else still runs. |
| `indexer.base_url` | `http://127.0.0.1:9696` | Prowlarr's address. Only Prowlarr is supported — the orchestrator uses its JSON API, not a generic Torznab feed. |
| `indexer.api_key` | empty | No releases are ever found. |
| `jellyfin.base_url` | `http://127.0.0.1:8096` | Where to trigger library scans and check active sessions. |
| `jellyfin.api_key` | empty | `.strm` files are written but Jellyfin is never told to scan, so new items may not appear until its own periodic scan. |

Leaving these blank is fine — the orchestrator starts anyway and the dashboard's health panel
shows what is missing. It will never refuse to boot over an empty key.

---

## `torrserver`

```yaml
torrserver:
  base_url: "http://10.42.0.2:8090"
  cache:
    mode: ram
    size_mb: 2048
    path: ""
    disconnect_timeout_s: 90
    connections_limit: 80
    retrackers_mode: 1
    upload_rate_limit_kb: 50
```

`base_url` defaults to `http://127.0.0.1:8090`, but the shipped config points at
**`10.42.0.2:8090`** — TorrServer runs inside the VPN network namespace and is reached across
the veth link. Only change this if you also change the namespace's addressing
(`VPNTORRENT_VETH_SUBNET`).

The whole `cache` block is applied to TorrServer at startup. **Omit the block entirely to
leave TorrServer's own settings untouched.**

| Key | Default in the shipped file | What it does |
|---|---|---|
| `mode` | `ram` | `ram` or `disk`. RAM is fastest and leaves nothing on disk. Empty means "do not manage TorrServer's settings". |
| `size_mb` | `2048` | The hard cap on the ring buffer. It physically cannot be exceeded. In RAM mode this memory is reserved — **lower it on a small box**. |
| `path` | empty | **Required when `mode: disk`.** Startup fails with a clear error otherwise. Avoid disk mode on Raspberry Pi SD or eMMC flash — use a small RAM cache or a USB SSD. |
| `disconnect_timeout_s` | `90` | How long an idle torrent stays warm. Higher means instant replay and a little more seeding; lower frees the cache sooner. |
| `connections_limit` | `80` | Peer connections. Higher buffers faster and uses more of your router's connection table. |
| `retrackers_mode` | `1` | `0` off, `1` add extra public trackers, `2` replace. `1` noticeably speeds up cold starts. |
| `upload_rate_limit_kb` | `50` | **Set a low nonzero value.** Zero gets you choked by peers under tit-for-tat and stalls your own stream. |

Also editable in **Dashboard → Settings**, which overrides these on every subsequent start.

---

## `libraries`

```yaml
libraries:
  - name: "Movies"
    type: movie
    path: /srv/jellyfreedom/movies
    default: true
  - name: "TV Shows"
    type: tv
    path: /srv/jellyfreedom/tv
    default: true
```

At least one library is required — the orchestrator refuses to start without one, and each
entry must have a `name`, a `type` of `movie` or `tv`, and a `path`.

| Key | Meaning |
|---|---|
| `name` | Shown in the UI and stored against each item. Case-sensitive when referenced. |
| `type` | `movie` or `tv`. |
| `path` | Directory the `.strm` files are written into. Must exist and be writable by the service user. Add this same path to Jellyfin as a library. |
| `default` | Used when a request does not name a library. First of that type wins if none is marked. |
| `adult` | A flag carried on the library for future filtering. It does not change behaviour today. |
| `picker` | Optional per-library overrides of the picker block below. Only non-zero and non-empty fields override. |

An older flat form (`library: { movies_dir:, tv_dir: }`) is still accepted and migrated
automatically into two named libraries.

---

## `picker`

```yaml
picker:
  min_seeders: 5
  prefer_video_codecs: ["h264", "h265", "hevc"]
  prefer_audio_codecs: ["aac", "ac3", "eac3"]
  prefer_containers: ["mp4", "mkv"]
  max_size_gb: 20
  reject_cam: true
  target_resolution: "1080p"
  require_direct_play: false
  max_mbps: 0
```

| Key | Default | Consequence |
|---|---|---|
| `min_seeders` | `5` | Releases below this are not considered. Raise it for reliability, lower it to see marginal releases (which will stream badly). |
| `max_size_gb` | `20` | Upper bound on release size. A blunt instrument — prefer `max_mbps`, which measures the thing that actually matters. |
| `reject_cam` | `true` | Sinks CAM, telesync and screener rips. Set false only if you genuinely want them. |
| `target_resolution` | `1080p` | The resolution the picker aims for. Hitting it is worth +300; one rung down +150, two down nothing, three+ down a penalty. Going *above* target is worth only +100 — 4K looks better but costs bitrate you may not be able to stream. A typo here fails startup rather than being silently ignored. |
| `require_direct_play` | `false` | When true, a release **known** to force a transcode (AV1, DTS/DTS-HD, TrueHD/Atmos, an AVI container) is rejected outright rather than merely ranked lower. Worth turning on for Apple TV. A release whose codecs the title does not state is *not* rejected — see below. |
| `max_mbps` | `0` (off) | Ceiling on **bitrate**, computed as size ÷ runtime. Dormant when TMDB does not know the runtime. |
| `prefer_video_codecs` | `h264, h265` | Tie-breakers for **direct play**. A codec your client cannot direct-play means a video transcode, which is exactly what you do not want while pulling from a swarm. |
| `prefer_audio_codecs` | `aac, ac3, eac3` | Same. DTS and TrueHD need a transcode on most clients. |
| `prefer_containers` | `mp4, mkv` | Same. |

**Resolution and direct play are now real signals, not tie-breakers.** Before this, neither
was scored at all: a 480p release with 520 seeders beat a 1080p HEVC with 210 seeders,
because seeder count was the only signal with any weight behind it. A release that direct-
plays is worth +400 and one at your target resolution +300, which is enough to outrank a
seeder bucket or two.

Seeder count still leads, and deliberately so — a dead release does not stream at all, no
matter how good its format. But it no longer wins on its own.

**Why bitrate rather than size.** A 60 GB remux and a 4 GB WEB-DL of the same film are
65 Mbps and 4.5 Mbps. Only one of them feeds a bounded ring buffer over a VPN link, and
`max_size_gb` cannot tell them apart from a 40 GB three-hour feature that streams fine.

**Why direct play matters more here than on a normal server.** Transcoding a torrent stream
is the worst case in this architecture: ffmpeg reads ahead faster than the swarm delivers,
so the ring buffer starves and playback stalls. On a 3rd-gen Apple TV 4K, DTS/TrueHD force
an audio transcode and AV1 forces a full video transcode.

**`direct_play` means "nothing known to force a transcode", not "confirmed direct play".**
That distinction is not pedantry. Release titles state a container almost never and an
audio codec about half the time — on a live search of 108 releases for one film, 106 had no
parseable container. Demanding positive proof of all three fields made the flag false for
every release, which would have made `require_direct_play` reject the entire swarm. The
formats that actually break playback are nearly always named in the title, because they are
selling points, so their *absence* is the reliable signal.

Also editable in **Dashboard → Settings**, which overrides these on every subsequent start.

---

## `vpn`

```yaml
vpn:
  config_dir: /var/lib/jellyfreedom/vpnconfigs
```

Where dashboard-uploaded WireGuard configs are stored (default
`/var/lib/jellyfreedom/vpnconfigs`, mode 700, owned by the service user). The active config is
the file `wg0-vpntorrent.conf` in that directory. The installer creates it and grants
`wg-quick` read access via an AppArmor override — if you move it, that override no longer
matches and activation will fail.

### What your VPN provider must give you

- **A WireGuard `.conf`.** Any provider, or self-hosted. Nothing here is provider-specific.
  OpenVPN is not supported.
- **A P2P-friendly server.** Not optional in practice: on a non-P2P or strict-NAT server
  torrents connect to seeders and transfer nothing at all.
- **Optional: NAT-PMP port forwarding.** Where the provider offers it (gateway `10.2.0.1` by
  default; override with `VPNTORRENT_PF_GATEWAY`), the port-forward service holds a mapping
  and keeps TorrServer's listen port in sync, which roughly doubles connectable seeders. Where
  it is not offered, that service idles and streaming still works.

Uploaded configs are rewritten before use: `PostUp`, `PostDown`, `PreUp`, `PreDown`, `Table`,
`SaveConfig` and `DNS` are stripped, and only a fixed allow-list of keys is passed through.
See [security.md](security.md).

---

## Environment variables read by the VPN plumbing

Set these in a systemd drop-in for the relevant unit if you need them.

| Variable | Unit | Default | Purpose |
|---|---|---|---|
| `VPNTORRENT_VETH_SUBNET` | `vpntorrent-netns` | `10.42.0.0/30` | Host↔namespace link. If you change it, update `torrserver.base_url` to match the namespace address. |
| `VPNTORRENT_DNS` | `vpntorrent-netns` | `1.1.1.1 9.9.9.9` | Resolvers used **inside** the namespace. They are reached through the tunnel, so they do not leak your real address. Set your provider's own resolver here if you prefer. |
| `VPNTORRENT_CONFIG_DIR` | `vpntorrent-netns` | `/var/lib/jellyfreedom/vpnconfigs` | Set by the installer to match `vpn.config_dir`. |
| `VPNTORRENT_PF_GATEWAY` | `vpntorrent-portforward` | `10.2.0.1` | NAT-PMP gateway address. |

---

## External-provider ingest

**There is no `config.yaml` key for this.** It is listed here so you do not go looking for
one.

A separate local daemon can register titles it has resolved itself, under its own provider
namespace, through `PUT /api/provider/{provider}/items/{id}`. JellyFreedom stores the row,
writes the `.strm` and plays it from the info hash the caller supplied; it performs no
metadata lookup and no indexer search for these items. The full contract and the validation
rules are in [`SECURITY.md`](../SECURITY.md#the-external-provider-ingest-api).

The one thing you need is the **shared secret**, which the caller presents in an
`X-JellyFreedom-Ingest` header. It follows the same rules as the Jellyfin webhook secret:

| | |
|---|---|
| Where it lives | the `settings` table in `/var/lib/jellyfreedom/jellyfreedom.db`, key `ingest.secret` |
| How it is created | 24 random bytes, generated automatically on first run. There is nothing to configure. |
| How to read it | `GET /api/settings` with an admin session returns it under `ingest`. The dashboard does not render it yet. |
| If it is missing | the endpoint is **closed**, not open — every call is refused. |

To rotate it, delete the row and restart; a new one is generated on the next start, and
every daemon using the old one stops working until you give it the new value.

```bash
sudo apt-get install -y sqlite3        # not installed by default
sudo sqlite3 /var/lib/jellyfreedom/jellyfreedom.db \
  "delete from settings where key = 'ingest.secret';"
sudo systemctl restart jellyfreedom
```

The libraries a daemon may write into are exactly the ones in `libraries` above, and the
library it names must match the media type — a `tv` item cannot be registered into a `movie`
library. Naming no library resolves to the `default: true` library for that type.

---

## Command-line flags

The service runs the orchestrator with all three set; you only need these for development.

```
--config <path>   config file            (default: config.yaml)
--db <path>       SQLite database        (default: jellyfreedom.db)
--assets <dir>    serve web assets from disk instead of the embedded copy
--version         print the version and exit
```

---

## A minimal working config

```yaml
server:
  listen: "0.0.0.0:1990"
  public_url: "http://192.168.1.50:1990"

torrserver:
  base_url: "http://10.42.0.2:8090"
  cache:
    mode: ram
    size_mb: 1024
    disconnect_timeout_s: 90
    connections_limit: 80
    retrackers_mode: 1
    upload_rate_limit_kb: 50

libraries:
  - name: "Movies"
    type: movie
    path: /srv/jellyfreedom/movies
    default: true
  - name: "TV Shows"
    type: tv
    path: /srv/jellyfreedom/tv
    default: true
```

Everything else — keys, quality, VPN — through the dashboard.
