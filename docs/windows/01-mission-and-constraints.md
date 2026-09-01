# 01 — Mission and constraints

## Mission, one line

A free, self-hosted system that **searches torrent indexers and streams the result straight
into Jellyfin**, so the user can watch on any device (especially Apple TV) through one clean
interface — without filling up disk and without leaking traffic.

## The three lessons the design is built around

These are not theory. Each is a thing that happened, and the architecture exists to prevent
it happening again. Do not "simplify" any of them away on Windows.

**1. Storage filled up.** A previous custom WebTorrent engine was, in effect, a downloader
that streamed. Fix: TorrServer's bounded RAM ring buffer. It physically cannot bloat, and you
cannot seed pieces you have already evicted.

**2. Availability tracking with Radarr/Sonarr/Jellyseerr was wrong.** The \*arr model is "a
downloaded file exists", which streaming never satisfies. Fix: the orchestrator owns
availability, defined as *resolvable*. Keep \*arr out of the streaming library's availability
path entirely.

**3. A frozen info hash rots.** Seeders die and the pointer breaks. Fix: **Resolve-at-Play** —
the `.strm` is keyed on identity (`/play/movie/{tmdb}`), and the release is chosen fresh at
play time. This is the single most important idea in the system.

## Hard constraints

Do not violate these without asking the user first.

1. **No Docker.** Native binaries, single-binary deploys. The orchestrator must build to one
   static binary. (On Windows this means one `.exe`; see [04](04-the-vpn-problem.md) for the
   one place this constraint is genuinely under pressure.)
2. **No paid debrid service.** This is *why* the design is torrent-streaming rather than
   something easier.
3. **Go** for the orchestrator. Settled.
4. **Nothing may hardcode a user-specific path.** "Clone, build, run" must hold on any
   machine. On Linux the tree is FHS (`/opt`, `/etc`, `/var/lib`); on Windows see
   [03](03-components.md#paths).
5. **Security is a first-class goal.** Torrent traffic goes through WireGuard with a
   fail-closed kill switch. Jellyfin↔client stays on the LAN.
6. **Provider-agnostic VPN.** Any WireGuard provider, or self-hosted. Nothing is
   Proton-specific; the only vendor-shaped feature is optional NAT-PMP port forwarding.

## What "fail closed" means here, precisely

If the tunnel is down, torrent traffic must not go anywhere. Not "should not" — *cannot*.
On Linux this is enforced twice over: the namespace has no default route except the tunnel,
**and** an `iptables OUTPUT DROP` policy allows only `lo`, the tunnel interface and the
host↔namespace link.

The test that matters, and the one you must be able to pass on Windows: **take the tunnel
down and confirm every outbound attempt from the torrent engine fails**, rather than quietly
falling back to the default route. On Linux this was verified by bringing `wg0` down and
watching every egress attempt return "Network is unreachable".

If your Windows design cannot pass that test, it is not done, however good it looks.

## Non-goals

- Multi-tenant use. One household, one trusted network.
- Running on the open internet. It talks plain HTTP and some pages need no password; it
  belongs on a LAN or a private tunnel like Tailscale.
- Replacing Radarr/Sonarr. It does not work alongside them either — see lesson 2.
