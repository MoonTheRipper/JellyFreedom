# 03 — Components, and their Windows equivalents

Every component, what it does, why it was chosen, and what it becomes on Windows.

## The one we write

### Orchestrator (`cmd/orchestrator`, Go)

The whole product. Serves the media UI (`:1990/`), the admin dashboard (`/dashboard/`), the
JSON API, and the `/play` streaming path. Writes `.strm` files. Owns the database.

**Windows: builds essentially as-is.** `GOOS=windows go build` already works for most of it.
SQLite is `modernc.org/sqlite`, pure Go, no cgo. Web assets are `go:embed`ed.

What does *not* compile or does not make sense on Windows:

| File | Why it is Linux-only | What to do |
|---|---|---|
| `internal/netnsproxy/` | SOCKS5 proxy that binds a veth inside a netns | Keep the SOCKS server; replace how it is *placed* on the network — see [04](04-the-vpn-problem.md) |
| `cmd/orchestrator/netnsproxy.go` | Derives its listen address from the veth via `net.Interfaces()` | Rework addressing |
| `cmd/orchestrator/selfrestart.go`, `internal/api/selfrestart.go` | systemd restart semantics | Windows Service Control Manager |
| `internal/api/privileged.go` | `sudo` + a fixed allow-list of `systemctl` verbs | A privileged helper service, or an elevated task |
| `internal/api/dashboard.go` (`journalLines`) | shells out to `journalctl` | Windows Event Log, or the orchestrator's own rolling log file |
| `internal/update/apply.go` | tarball + systemd restart | Zip/MSI + SCM restart |

Everything else — `store`, `picker`, `tmdb`, `indexer`, `jellyfin`, `library`, `websource`,
`redact`, `config`, `api` — is portable Go. **`internal/library/writer.go` deserves a careful
read**: its `safeName`/`containedPath` path-safety logic was written for POSIX and must be
re-audited for Windows (reserved device names `CON`, `PRN`, `AUX`, `NUL`, `COM1..9`,
`LPT1..9`; trailing dots and spaces; `\` as a separator; ADS via `:`). The existing code
already handles `..`, NUL, zero-width and BiDi padding and was probed for traversal — but it
was probed on Linux.

## Off the shelf — all four have Windows builds

### Jellyfin (`:8096`)
The player UI on every device. This is why the whole design targets `.strm` files: Jellyfin
already speaks every client we care about, including Apple TV.
**Windows:** official installer or portable build. No change in role.

### TorrServer (`:8090`)
The streaming engine, driven over HTTP. Chosen for its **bounded RAM ring buffer** — this is
lesson 1 from [01](01-mission-and-constraints.md) and is not negotiable. Configure cache mode
and size; do not switch it to disk caching to "improve" anything.
**Windows:** official `TorrServer-windows-amd64.exe`. Runs fine as a service.

### Prowlarr (`:9696`)
Indexer aggregation. **Prowlarr only**, over its JSON API (`internal/indexer/client.go`) —
not Torznab, so Jackett is *not* a drop-in substitute.
**Windows:** official Windows installer, runs as a service.
**Important:** stock Prowlarr binds every interface with authentication disabled for local
addresses. The Linux installer now fixes this (`harden_prowlarr` in `release/install.sh`).
**Your Windows installer must do the same** — see [07](07-security.md).

### FlareSolverr (`:8191`)
Cloudflare bypass, driving a headless browser. x64 only upstream.
**Windows:** official Windows build exists. Note the Linux box needed its bundled Chrome
redirected at a system browser and had persistent trouble here — see
[09](09-gotchas.md#flaresolverr-is-the-flakiest-component).

### yt-dlp
Extractor behind paste-a-link web sources. The official self-contained build is used, not a
distro package, because sites change their players constantly and `yt-dlp -U` is the fix for
most breakage.
**Windows:** `yt-dlp.exe`, same self-update story.

### TMDB
Metadata and search. HTTP API, no local component.

## Paths

| Purpose | Linux | Windows (proposed) |
|---|---|---|
| Program files | `/opt/jellyfreedom` | `C:\Program Files\JellyFreedom` |
| Config | `/etc/jellyfreedom/config.yaml` | `C:\ProgramData\JellyFreedom\config.yaml` |
| Data (SQLite, VPN configs) | `/var/lib/jellyfreedom` | `C:\ProgramData\JellyFreedom\data` |
| Libraries (`.strm` trees) | `/srv/jellyfreedom/{movies,tv,...}` | `C:\JellyFreedom\Media\{Movies,TV,...}` |
| VPN plumbing | `/opt/vpntorrent` | `C:\Program Files\JellyFreedom\vpn` |
| Logs | journald | Event Log or `...\data\logs` |

Nothing may hardcode a user-specific path (constraint 4). Resolve these from a single place,
the way `internal/config` already does on Linux.

## Permissions — this is not optional detail

On Linux the data directory is `0700`, the database `0600`, and the service runs with
`UMask=0077`. That is there because the database stores **session tokens in plaintext** — a
copied row *is* a logged-in admin — plus `play.hmac_key` and every provider API key. It was
world-readable once, and that was the most serious finding of the security audit.

**On Windows you must re-implement this with ACLs.** A `chmod` equivalent does not exist; use
`icacls` at install time and verify at runtime. The Go-side belt-and-braces `os.Chmod(path,
0o600)` in `internal/store.Open` is a no-op on Windows — replace it with an ACL check, or the
protection silently does not exist.

## Service management

| Linux | Windows |
|---|---|
| `jellyfreedom.service` | Windows Service (`golang.org/x/sys/windows/svc`) |
| `torrserver-netns.service` | Service, network-scoped per [04](04-the-vpn-problem.md) |
| `flaresolverr.service`, `prowlarr.service` | Services |
| `vpntorrent-netns.service` | Tunnel setup — see [04](04-the-vpn-problem.md) |
| `vpntorrent-watchdog.timer` (60s) | Scheduled Task or an internal ticker |
| `jf-tmpreaper.timer` (hourly) | Scheduled Task; see [09](09-gotchas.md#temp-directories-fill-up) |
| `sudo` + allow-listed `systemctl` verbs | An elevated helper service with a closed verb set |

Prefer implementing the service wrapper **inside the orchestrator binary** (`svc.Run`) over
shipping NSSM: one binary is a stated constraint, and a second dependency is a second thing
to keep signed and updated.
