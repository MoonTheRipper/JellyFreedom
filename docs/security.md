# Privacy and security, in practical terms

This describes what the system actually does with your traffic and your privileges, so you
can decide whether it matches what you assumed. For reporting a vulnerability, and for the
formal threat model, see [SECURITY.md](../SECURITY.md) in the repository root.

---

## 1. What goes through the VPN — and what does not

This is the part people get wrong. **Only torrent traffic is tunnelled.** Everything else
leaves over your normal connection.

| Traffic | Path | Through the VPN? |
|---|---|---|
| TorrServer ↔ the torrent swarm (trackers, DHT, peers) | inside the `vpntorrent` namespace → `wg0-vpntorrent` | **Yes**, with a fail-closed kill switch |
| DNS lookups made by TorrServer | resolvers reached **through** the tunnel | **Yes** |
| **Indexer searches** (Prowlarr, FlareSolverr) | host network | **No — your real IP** |
| **Metadata lookups** (TMDB posters, episode data, from both the orchestrator and Jellyfin) | host network | **No — your real IP** |
| **Web sources** — extracting a pasted link, and streaming it | orchestrator → `jf-netnsproxy` inside the namespace → `wg0-vpntorrent` | **Yes**, with the same kill switch |
| Jellyfin ↔ your Apple TV, phone, browser | your LAN | **No**, deliberately — tunnelling it would only add latency |
| Orchestrator ↔ TorrServer | the `10.42.0.0/30` link between host and namespace | Never leaves the machine |

**What that means in plain terms:** the swarm sees your VPN's address, never yours. But your
indexers see your real address along with your search terms, and TMDB sees which titles you
looked up. If that matters to you, route Prowlarr through a namespace of its own using the
same pattern — nothing in the design prevents it, it is simply not done for you.

No torrent hashes or magnet links are ever sent to TMDB.

### Web sources are tunnelled, and cannot be un-tunnelled by accident

A pasted link is fetched twice over: once to work out what the video is, and again to stream
it. Both identify you to the site, so both go through the tunnel — and the mechanism is not a
setting that could be flipped.

The orchestrator itself runs in the host namespace, because it has to serve your LAN. It
cannot enter the `vpntorrent` namespace without `CAP_SYS_ADMIN`, which is root by another
name. So instead of moving the orchestrator, one socket moves: `jf-netnsproxy.service` runs a
SOCKS proxy **inside** the namespace and dials on the orchestrator's behalf. Its connections
are subject to the same fail-closed rules as TorrServer's, because they are the same rules —
when the tunnel is down, the namespace's `iptables` policy drops the packets and playback
fails rather than falling back.

Three details are worth knowing:

- **The DNS lookup is tunnelled too.** The proxy is addressed as `socks5h://`, which sends
  the *hostname* to be resolved inside the namespace. Resolving it locally first would put a
  lookup for the site's domain on your home connection — the leak, minus the video.
- **The proxy is not reachable from your LAN.** It binds only the namespace's end of the
  `10.42.0.0/30` link and accepts connections only from the host's end of it, which is the
  one other address that exists on a /30.
- **It will only dial the public internet.** A `CONNECT` to a loopback, RFC 1918, link-local
  or carrier-NAT address is refused, so it cannot be used as a way back into your host or
  your LAN. The check runs on the *resolved* address, in the same process that then dials it.

The site still learns everything a visitor normally would — your VPN's address, and that
somebody there watched that video. Proxying hides where you are, not that it happened.

**Nothing about the link is written to disk except the page URL.** The media URL — the signed,
expiring CDN link — exists only in memory, and only for as long as it is valid.

---

## 2. The kill switch

Torrent traffic runs inside a Linux network namespace called `vpntorrent`. Your real network
interface is not in that namespace at all, so there is nothing for traffic to fall back to.
Two independent layers enforce it:

1. **Routing.** The only default route inside the namespace is the WireGuard interface. No
   tunnel means no default route, and connections fail immediately with "network
   unreachable".
2. **Filtering.** An `OUTPUT DROP` policy inside the namespace permits only loopback, the
   tunnel, the point-to-point link to the host, and the WireGuard handshake itself. Layer 1
   alone is only as good as the routing table — a stray `Table =` directive or a second
   `wg-quick` could put a default route on the host link and everything would quietly leave
   in the clear. Layer 2 makes failing closed a rule rather than a side effect.

This is why **a fresh install with no VPN config is not broken**: the namespace comes up
deny-all, TorrServer runs, and nothing can reach the internet until you activate a config.

**IPv6** is handled separately and just as firmly: IPv6 is disabled on the namespace's only
non-VPN interface, an `ip6tables OUTPUT DROP` policy backs that up, and router
advertisements from inside the namespace are not accepted by the host.

### Verify it yourself

```bash
# The namespace's exit address must differ from your real one
sudo /opt/vpntorrent/jf-netns-helper exit-ip
curl -s https://1.1.1.1/cdn-cgi/trace | sed -n 's/^ip=//p'

# Machine-readable kill-switch facts
sudo /opt/vpntorrent/jf-netns-helper leakcheck
```

Expect `ipv4_output_policy=DROP`, `ipv4_default_dev=wg0-vpntorrent`, `wg_interface=up`.

Prove it fails closed:

```bash
sudo /opt/vpntorrent/jf-netns-helper vpn-down
sudo ip netns exec vpntorrent curl --max-time 3 https://1.1.1.1 && echo "LEAK" || echo "BLOCKED"
sudo /opt/vpntorrent/jf-netns-helper vpn-up
```

`BLOCKED` is correct. `sudo jellyfreedom doctor vpn` runs the exit-IP comparison for you and
**fails hard** if the namespace's exit address equals your host's.

---

## 3. How much you seed

Two mechanisms, neither of which depends on you remembering anything:

- **The cache is a bounded ring buffer.** It keeps a window around the playhead and evicts the
  rest. You never hold a complete file, so there is never a complete file to share — only the
  small window currently in the cache.
- **Torrents are dropped when you stop.** The upload rate is capped low but nonzero (zero gets
  you choked by peers and stalls your own stream), and idle torrents time out on their own.
  With Jellyfin's webhook configured, the drop happens the moment playback ends.

---

## 4. The privilege model

The orchestrator runs as an unprivileged system user, `jellyfreedom`, with no shell and no
home directory. TorrServer, FlareSolverr and Prowlarr each run as their own system user.

Managing a VPN tunnel needs root, so there is exactly one path to it: a **root-owned helper**
at `/opt/vpntorrent/jf-netns-helper`, reachable through a passwordless sudo policy that names
a **closed set of verbs with no free-form arguments**:

```
systemctl restart jellyfin | torrserver-netns | vpntorrent-netns | prowlarr | flaresolverr
jf-netns-helper status | exit-ip | leakcheck | routes | vpn-up | vpn-down
```

**No wildcards.** That matters: an earlier policy granted the service user
`ip netns exec vpntorrent curl *` as root, which is root-equivalence — `curl -o` writes any
file as root and `curl file://` reads any file. Every dynamic value that used to cross the
sudo boundary is now computed inside the helper, on the trusted side.

The helper must be root-owned in a root-owned directory. If the service user could edit it,
or its directory, those sudo rules would be a root shell. `jellyfreedom doctor privs` checks
exactly this, along with the absence of wildcards and the sudoers file's validity.

### Uploaded VPN configs cannot become root code execution

`wg-quick` runs `PostUp`, `PostDown`, `PreUp` and `PreDown` through `bash` **as root**. Since
you can upload a config from a web dashboard, one such line would be a root shell.

Two independent layers prevent it:

1. On upload, those directives — plus `Table`, `SaveConfig` and `DNS` — are stripped, and the
   UI tells you what was removed.
2. At tunnel bring-up, the root-owned helper **rebuilds the config from scratch** into a
   root-owned file under `/run`, emitting only keys from an allow-list: `PrivateKey`,
   `ListenPort`, `MTU`, `FwMark`, `Address` in `[Interface]`, and `PublicKey`,
   `PresharedKey`, `Endpoint`, `PersistentKeepalive`, `AllowedIPs` in `[Peer]`. Anything else
   is dropped, and an allow-listed value containing shell metacharacters is refused outright.

Rebuilding rather than filtering removes any parser differential: what `wg-quick` sees is
exactly what was decided, not a line someone tried and failed to match.

`Table` is stripped for a specific reason — it can suppress route installation and silently
defeat a routing-based kill switch. `DNS` is stripped because it needs `resolvconf` and
because the namespace has its own resolver configuration that goes through the tunnel.

---

## 5. Where your secrets are

| What | Where | Protection |
|---|---|---|
| API keys set in the dashboard, password hashes, sessions, the play signing key, the webhook secret | `/var/lib/jellyfreedom/jellyfreedom.db` | directory owned by the service user |
| API keys set in the file | `/etc/jellyfreedom/config.yaml` | mode 640, owned by the service user |
| **Uploaded WireGuard configs, including private keys** | `/var/lib/jellyfreedom/vpnconfigs/` | mode 700, owned by the service user |

Nothing is encrypted at rest. Anyone with root on the host, or a copy of these paths, has
everything. Keys entered in the dashboard are never sent back to the browser, and secrets are
masked in logs by a redaction backstop.

`.gitignore` excludes `config.yaml`, `*.db`, `*.conf` and `vpnconfigs/`, so a fork cannot
commit them by accident. Check before publishing one anyway.

---

## 6. What to expose, and what never to

> **Do not put JellyFreedom on the public internet.** Not on port 1990, not behind a naive
> port forward.

It serves plain HTTP with no TLS anywhere, and several routes are deliberately
unauthenticated. For remote access, use a private overlay network — Tailscale, WireGuard,
ZeroTier — or an authenticating reverse proxy you control.

**Reasonable on a trusted LAN:** port 1990 (media UI and dashboard) and port 8096 (Jellyfin).

**Keep on localhost:** Prowlarr (9696) and FlareSolverr (8191). The installer binds
FlareSolverr to `127.0.0.1` for a concrete reason — it fetches arbitrary URLs with no
authentication, so binding it to every interface publishes an open request proxy on your
network.

**Not reachable from the network at all:** TorrServer, which lives inside the VPN namespace
and is only reachable across the host↔namespace link.

### Which routes need a login

- **Admin session:** the dashboard and everything under `/api/` that is not explicitly public
  — settings, VPN config upload and activation, users, logs, tasks, service restarts, VPN
  status and the leak check. Anything unrecognised under `/api/` is admin-only by default.
- **Any logged-in user:** requesting titles, cancelling queue items, subscriptions, removing
  library items.
- **No login at all:** the media UI and search, health and status endpoints, the TMDB
  read-through used to render pages, library and queue **reads**, and — necessarily — the
  streaming endpoints.

**The streaming endpoints have to be open**, because Jellyfin fetches the URL inside a `.strm`
file with no session cookie. They are protected differently: each `.strm` URL carries a
signature computed with a server-side key, so a stranger cannot invent a URL that makes your
box start downloading something. Possession of a valid `.strm` is the credential. The key is
generated on first run and never leaves the server.

The playback-stopped webhook is authenticated by a shared secret, checked in constant time,
and **fails closed** — with no secret configured it rejects every call rather than accepting
anonymous ones.

### The first-run race

`/dashboard/setup` is open until the first account exists, and that account is an admin.
**Whoever reaches the box first owns it.** Create your admin account immediately after
installing.

---

## 7. Things worth knowing about the third-party pieces

- **FlareSolverr runs a browser with `--no-sandbox`.** That is upstream's own default, and on
  Ubuntu 24.04 and later it is required, because unprivileged user namespaces are restricted
  by AppArmor and FlareSolverr's `chrome_sandbox` binary is not setuid. It is a real
  reduction in browser isolation. Do not point FlareSolverr at untrusted URLs.
- **Jellyfin, Prowlarr, TorrServer and FlareSolverr have their own security posture.** Report
  problems in those projects upstream.
- The installer's downloads are fetched over HTTPS from the projects' own release URLs.
  JellyFreedom's own bundle is checksum-verified before installation, and releases built by
  the automated workflow carry signed build provenance.

---

## 8. There is no telemetry

Nothing phones home. The only outbound connections are the ones you configured: TMDB, your
indexers, your VPN provider, the swarm, and the update check when **you** run
`jellyfreedom --update`.
