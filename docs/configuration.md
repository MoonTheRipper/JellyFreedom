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
```

| Key | Default | Consequence |
|---|---|---|
| `min_seeders` | `5` | Releases below this are not considered. Raise it for reliability, lower it to see marginal releases (which will stream badly). |
| `max_size_gb` | `20` | Upper bound on release size. Mostly excludes remuxes that will never stream well. |
| `reject_cam` | `true` | Sinks CAM, telesync and screener rips. Set false only if you genuinely want them. |
| `prefer_video_codecs` | `h264, h265, hevc` | Tie-breakers for **direct play**. A codec your client cannot direct-play means a video transcode, which is exactly what you do not want while pulling from a swarm. |
| `prefer_audio_codecs` | `aac, ac3, eac3` | Same. DTS and TrueHD need a transcode on most clients. |
| `prefer_containers` | `mp4, mkv` | Same. |

**Seeder count dominates the score; codec, audio and container are tie-breakers only.** A
well-seeded release in a slightly worse format beats a dead release in a perfect one, every
time — a dead release does not stream at all.

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
