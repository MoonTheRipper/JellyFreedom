# Operations

Running the system: what each moving part does at runtime, how it is tuned, and how to verify
it. For installing, see [../install.md](../install.md); for symptom-driven fixes, see
[../troubleshooting.md](../troubleshooting.md).

Everything here is native — systemd units and binaries, no containers.

---

## 1. What runs, and where

| Service | Unit | Port | Runs as |
|---|---|---|---|
| Orchestrator | `jellyfreedom` | 1990, all interfaces | `jellyfreedom` |
| Jellyfin | `jellyfin` | 8096, all interfaces | `jellyfin` |
| Prowlarr | `prowlarr` | 9696, localhost | `prowlarr` |
| FlareSolverr | `flaresolverr` | 8191, localhost | `flaresolverr` |
| TorrServer | `torrserver-netns` | 8090, **inside the `vpntorrent` namespace** | `torrserver` |
| VPN namespace setup | `vpntorrent-netns` (oneshot) | — | root |
| Port-forward keeper | `vpntorrent-portforward` | — | root |
| Watchdog | `vpntorrent-watchdog.timer` (60s) | — | root |

TorrServer is reachable from the host at **`10.42.0.2:8090`** across the veth link. That
address is derived from `VPNTORRENT_VETH_SUBNET` (default `10.42.0.0/30`) and published to
every consumer in `/run/vpntorrent/netns.env`, so nothing re-derives or hardcodes it. If you
change the subnet, change `torrserver.base_url` to match.

Unit dependencies worth knowing:

- `torrserver-netns` has **`BindsTo=vpntorrent-netns`**. Without it, restarting the namespace
  deletes and recreates it while TorrServer keeps a handle on the orphaned original.
- The namespace unit is written to **succeed whenever the namespace itself is healthy**. A
  missing or broken VPN config is a loud warning, not a unit failure — the namespace fails
  closed either way, and failing the unit would stop TorrServer and take the dashboard's own
  repair path down with it.

---

## 2. TorrServer: stream, don't hoard

The bounded cache is the entire reason TorrServer was chosen. The profile is seeded from
`torrserver.cache.*` in the config and applied over TorrServer's `/settings` API at startup;
the dashboard can change it live.

| Setting | Shipped value | Why |
|---|---|---|
| Cache size | 2048 MB | RAM ring buffer. Hard cap — it cannot be exceeded. |
| Mode | `ram` | Nothing lingers on disk, nothing to seed, no flash wear. |
| Disconnect timeout | 90 s | Idle torrents drop themselves — a belt for the webhook-driven drop. |
| Connections limit | 80 | Bounded, but enough to find seeders. |
| Retrackers mode | 1 | Injects extra public trackers, which speeds up cold starts noticeably. |
| Upload limit | 50 KB/s | Low but **nonzero** — zero gets you choked under tit-for-tat and stalls your own stream. |

**Gotcha: TorrServer's `set` action replaces the entire settings object.** Always GET, modify,
then POST the whole thing back. Sending a partial object silently reverts every field you did
not include — this wiped the cache profile once.

Do **not** disable DHT or PEX. Both must stay on; disabling them cripples peer discovery.

**Stream URL shape:** `/stream?index=<file_id>&link=<hash>&play`. The `&play` parameter is
required — without it TorrServer answers HTTP 200 with `Content-Length: 0`, so the connection
succeeds and no bytes arrive. `index` is the file's `id` from `file_stats`, which is 1-based.

### Two reliability facts that will otherwise waste your afternoon

- **DHT warm-up.** For roughly 45 seconds after any TorrServer restart, torrents connect to few
  or no peers. Do not judge connectivity in that window. A healthy release on a warm TorrServer
  connects to dozens.
- **A scrape is not a connection.** The seeder count from an indexer is a tracker scrape and can
  be a stale ghost: metadata resolves, peers are discovered, and nothing ever connects.
  Resolve-at-Play therefore requires a proven *connected* peer before committing a release, and
  re-resolves if a cached choice has since become a ghost.

### The BT client can crash

Under rapid add/drop churn the BitTorrent engine can die: `/echo` still answers, the log says
`BT client not connected`, and every add returns HTTP 500. The watchdog detects and recovers
this automatically (see §4). By hand: `systemctl restart torrserver-netns`.

Diagnose liveness with a stream read reaching **HTTP 206**, never with the `downloaded`
counter — TorrServer is lazy and that counter stays at 0 until something reads the file.

### Network collapse: the root cause, in case it recurs

The symptom was "playing media kills the whole LAN". It was **not** connection count — host
conntrack was fine, since the traffic is tunnelled. It was bandwidth saturation, plus a Jellyfin
metadata probe running `ffprobe -probesize 1G` against the streaming URLs (pulling up to a
gigabyte per scan), plus video transcoding, plus a too-long disconnect timeout.

The fixes are all in place and should stay: a download rate cap that leaves headroom on the
line, the 90-second disconnect timeout, the disabled Jellyfin scheduled tasks in §5, and the
transcoding policy in §5.

---

## 3. The VPN namespace and the kill switch

`vpntorrent-netns.service` runs `/opt/vpntorrent/setup-netns.sh` at boot. It is idempotent:
running it twice is a no-op, not an error.

What it does:

1. Creates the `vpntorrent` namespace and brings up loopback.
2. Creates the veth pair — `veth-host` (`10.42.0.1`) on the host, `veth-vpn` (`10.42.0.2`)
   inside. This link is how the orchestrator and Jellyfin reach TorrServer's HTTP API.
3. Enables IPv4 forwarding and MASQUERADEs the link subnet on the host, so the WireGuard
   handshake can reach the provider through the real interface.
4. Applies **IPv6 leak prevention**: `accept_ra=0` on the host veth, IPv6 disabled on the
   namespace veth, and an `ip6tables OUTPUT DROP` policy permitting only loopback and the
   tunnel. All of it is guarded, so a host booted with `ipv6.disable=1` does not fail the unit.
5. Writes `/etc/netns/vpntorrent/resolv.conf` with public resolvers (`1.1.1.1`, `9.9.9.9` by
   default; override with `VPNTORRENT_DNS`). These are reached **through the tunnel**, so they
   work with any provider and leak nothing.
6. Applies the **v4 filter layer** of the kill switch *before* the tunnel exists: `OUTPUT DROP`,
   permitting loopback, the tunnel interface, the host link, and established replies. ACCEPT
   rules go in first and the DROP policy last, so a re-run never opens a window in which a live
   stream's packets are dropped.
7. Calls the privileged helper to sanitise the active config and bring the tunnel up.

### DNS inside the namespace

`torrserver-netns.service` bind-mounts that per-namespace `resolv.conf` over `/etc/resolv.conf`,
because systemd's `NetworkNamespacePath=` does **not** pick up `/etc/netns/...` the way
`ip netns exec` does. Without it, the namespace inherits the host's `127.0.0.53` stub, which
nothing answers inside the namespace, so tracker and DHT hostnames fail to resolve and you get
zero peers.

### Two layers, not one

1. **Routing.** The only default route in the namespace is `dev wg0-vpntorrent`. No tunnel, no
   route, no traffic — and there is no fallback, because the real interface is not in the
   namespace at all.
2. **Filtering.** The `OUTPUT DROP` policy above. Layer 1 alone is only as good as the routing
   table: a stray `Table =` directive, a second `wg-quick`, or a plain bug could put a default
   route on the veth and everything would quietly leave in the clear through the host's NAT.
   Layer 2 makes failing closed a rule rather than an emergent property of a missing route.

### The privileged helper

`/opt/vpntorrent/jf-netns-helper`, root-owned, mode 0755, in a root-owned directory. It is the
single implementation of config sanitisation, tunnel bring-up and the endpoint firewall rule —
`setup-netns.sh` and the watchdog both call it rather than duplicating any of that.

| Verb | Does |
|---|---|
| `status` | `wg show` for the tunnel, with key material filtered out |
| `exit-ip` | the namespace's public IPv4, one line |
| `leakcheck` | stable `key=value` kill-switch facts, parsed by the dashboard |
| `routes` | routes, rules and links inside the namespace |
| `vpn-up` | sanitise the active config, pin the endpoint route, bring the tunnel up |
| `vpn-down` | take the tunnel down |

Exit code 3 means "no config activated yet" — a warning, not a failure. The namespace is up and
still fails closed.

`exit-ip` deliberately queries Cloudflare's trace endpoint by IP literal, so no DNS is involved
and no dynamic argument crosses any boundary. Its fallback resolves a hostname **on the host
side** and pins it with `--resolve`.

### Config storage and activation

Configs live in `/var/lib/jellyfreedom/vpnconfigs` (mode 700, service-user owned), **not**
`/etc/wireguard`. The active one is `wg0-vpntorrent.conf` in that directory. `wg-quick` is
granted read access by an AppArmor override at `/etc/apparmor.d/local/wg-quick`.

Activation from the dashboard writes the chosen config, restarts `vpntorrent-netns` and
`torrserver-netns`, and then **waits for a real handshake**. If none arrives, the previous
config is restored and the user gets an error — a restart that "succeeded" proves nothing.

### Verifying

```bash
sudo /opt/vpntorrent/jf-netns-helper status
sudo /opt/vpntorrent/jf-netns-helper leakcheck
sudo /opt/vpntorrent/jf-netns-helper exit-ip
curl -s https://1.1.1.1/cdn-cgi/trace | sed -n 's/^ip=//p'   # must differ

# fail-closed test
sudo /opt/vpntorrent/jf-netns-helper vpn-down
sudo ip netns exec vpntorrent curl --max-time 3 https://1.1.1.1 && echo LEAK || echo BLOCKED
curl -s http://127.0.0.1:8096/System/Info/Public >/dev/null && echo "Jellyfin unaffected"
sudo /opt/vpntorrent/jf-netns-helper vpn-up
```

---

## 4. The watchdog and the port-forward keeper

**Watchdog** — `/opt/vpntorrent/watchdog.sh`, fired every 60 seconds with a 120-second hang
guard. Each run:

1. **Re-asserts** `net.ipv4.ip_forward=1` and the MASQUERADE rule, healing an external iptables
   flush (a `ufw` reload, a firewall reset) that would otherwise silently break the tunnel.
2. **Checks the BT client**: if `/echo` is alive but a synthetic add returns 500, that is a BT
   client crash. It restarts `torrserver-netns` only after **two consecutive** failures, and
   **skips the probe entirely while a Jellyfin session is playing** — a live stream already
   proves the client works, so there is no reason to churn.
3. **Probes the data path** against two raw-IP, DNS-independent targets. Two failures five
   seconds apart escalate: bounce the tunnel via the helper (TorrServer stays up) → rebuild the
   whole namespace (and restart the port-forward keeper, which the rebuild orphaned) →
   persistent failure writes `down: needs new wireguard config` to `/run/vpntorrent-status`.
4. **Asserts anonymity, not just reachability.** A run is only allowed to write `ok` once the
   namespace's default route still leaves via `wg0-*` **and** the address it actually came out
   on is not this machine's own public address. On positive evidence of a leak it stops
   `torrserver-netns` and latches the reason in `/run/vpntorrent-status`; when the tunnel is
   genuinely anonymous again a later run restarts it by itself.

Step 4 exists because steps 1–3 cannot tell a tunnelled path from a leaking one — both answer
"did bytes come back" identically. The probe already fetches `/cdn-cgi/trace`, whose body
reports the caller's egress address; the earlier version discarded it with `-o /dev/null`. The
host's own address is cached in `/run/vpntorrent-host-ip` for an hour, since it changes rarely
and this runs every minute.

An address that cannot be determined is never treated as a leak. Stopping the torrent stack
because a lookup timed out would be an outage of its own, so the check acts only on positive
evidence that the two addresses match.

Bouncing the tunnel re-sanitises the active config, so it also picks up a config activated in
the dashboard, including one pointing at a different server.

```bash
journalctl -t vpntorrent-watchdog -n 50
cat /run/vpntorrent-status
```

**Port-forward keeper** — `/opt/vpntorrent/portforward.sh`, a simple loop. Where the provider
offers NAT-PMP (default gateway `10.2.0.1`, override with `VPNTORRENT_PF_GATEWAY`) it renews the
mapping and keeps TorrServer's `PeersListenPort` in sync. A port change needs a TorrServer
restart, so it **defers that restart while a stream is playing**: streaming is outbound and
unaffected by the listen port, and only inbound connectivity is briefly reduced. On providers
without NAT-PMP it simply never gets a mapping, backs off, and idles.

---

## 5. Jellyfin

- **Libraries:** `/srv/jellyfreedom/movies` as Movies, `/srv/jellyfreedom/tv` as Shows.
- **Metadata providers must be enabled** on both. With them off, Jellyfin finds the `.strm`
  file but cannot match it: no poster, no title match, and it never appears in search. It is in
  the database, invisible.
- **Scans** are triggered by running Jellyfin's "Scan Media Library" scheduled task by id — not
  `POST /Library/Refresh`, which only refreshes metadata for existing items and does not pick up
  new files. The task id is looked up at runtime.
- **Disable these scheduled tasks** (clear their triggers): *Extract Chapter Images*, *Scan
  Media Library*, *Media Segment Scan*. Each seeks and probes whole files through the streaming
  proxy — see the network-collapse note in §2. The orchestrator triggers scans on demand.
- **Playback policy** on the user: video transcoding **off**, audio transcoding **on**,
  remuxing **on**. See [decisions.md](decisions.md) D11.
- **The playback-stopped webhook** is optional. Point Jellyfin's Webhook plugin at
  `POST http://127.0.0.1:1990/webhook/jellyfin` for PlaybackStop events, with the shared secret
  in an `X-JellyFreedom-Token` header (or `?token=` in the URL). Without the secret the endpoint
  refuses every call. The secret is generated on first run and currently readable only from the
  database:
  ```bash
  sudo apt-get install -y sqlite3      # not installed by default
  sudo sqlite3 /var/lib/jellyfreedom/jellyfreedom.db \
    "select value from settings where key='webhook.secret';"
  ```

### What the webhook does — and does not do

On PlaybackStop the orchestrator resolves the item's `.strm` path, checks that no other Jellyfin
session is still playing it, counts other library rows sharing the same info hash (a season pack
may back several episodes), and drops the torrent only if nothing else needs it. If the
reference count cannot be determined it **keeps** the torrent, because dropping one another
episode is streaming would kill someone else's playback.

**The item stays `ready`.** Finishing a video is not expiry. Staleness is decided solely by
resolvability, by the library health check, and never by playback ending.

---

## 6. Apple TV

Use the native Jellyfin client or Swiftfin. The rule is: keep it direct-playing. The picker
prefers H.264/H.265 with AAC/AC3/E-AC3 in MP4 or MKV, video transcoding is off server-side, and
container or audio mismatches are handled by the cheap remux and audio transcode.

If playback stutters, suspect in this order: swarm speed on a cold first play; an audio or
container remux; the port-forward keeper being down, which degrades throughput on providers that
offer NAT-PMP.

---

## 7. Remote access

Use a private overlay network — Tailscale, WireGuard, ZeroTier — or an authenticating reverse
proxy you control. **Never expose the orchestrator, Jellyfin, Prowlarr or TorrServer on a public
port.** There is no TLS in the orchestrator and several routes are deliberately
unauthenticated; see [../security.md](../security.md).

If a TLS-terminating proxy does sit in front, set `server.secure_cookies: true`. Do not set it
otherwise: over plain HTTP the browser stops sending the session cookie at all and every user is
logged out instantly.

---

## 8. Routine operations

```bash
sudo jellyfreedom doctor        # full diagnostic, with a fix for every failure
sudo jellyfreedom repair        # re-run the installer over this instance
sudo jellyfreedom --update      # fetch the current release and run its installer
jellyfreedom logs               # follow the orchestrator
sudo jellyfreedom restart
```

Backups: `/etc/jellyfreedom` (config) and `/var/lib/jellyfreedom` (database **and WireGuard
private keys** — treat as secret). Stop the service first for a clean database copy. The `.strm`
files under `/srv/jellyfreedom` are regenerated on demand.

Installation, updating and uninstalling are covered in [../install.md](../install.md). There is
no manual bring-up procedure any more: the installer is the definition of a correct deployment,
and re-running it is the supported repair path.
