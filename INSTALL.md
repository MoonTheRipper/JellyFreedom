# JellyFreedom — Install & Release

> Status: **first-cut installer for the components we own.** It is **not yet tested on a clean
> machine** — treat it as release-candidate tooling. The live dev box still runs from
> `/home/ripper-moon/JellyFreedom`; migrating it to these FHS paths is a separate step.

## What the installer does (and doesn't)

**Installs (portable, FHS, dedicated service user — no user-specific paths):**

| Thing | Location |
|---|---|
| Orchestrator binary | `/opt/jellyfreedom/bin/orchestrator` |
| Web assets | `/opt/jellyfreedom/web/` |
| VPN-netns scripts | `/opt/vpntorrent/{setup-netns,watchdog,portforward}.sh` |
| Config | `/etc/jellyfreedom/config.yaml` |
| Data (DB + VPN configs) | `/var/lib/jellyfreedom/` (`vpnconfigs/` owned `700`) |
| systemd units | `jellyfreedom`, `vpntorrent-netns`, `torrserver-netns`, `vpntorrent-portforward`, `vpntorrent-watchdog.{service,timer}` |
| Scoped sudoers | `/etc/sudoers.d/jellyfreedom` (restart services + read VPN status only) |
| AppArmor override | `/etc/apparmor.d/local/wg-quick` (read uploaded configs) |
| Service users | `jellyfreedom` (orchestrator), `torrserver` (TorrServer) — system, nologin |

**Also installs:** the full supporting stack (non-clobbering — anything already present is left
alone): `TorrServer` (binary), `FlareSolverr` (+ Chromium + `xvfb` + the Chrome-142 crash fix),
`Jellyfin` (official apt repo), `Prowlarr` (self-contained tarball + service); plus apt deps
`wireguard-tools natpmpc iproute2 iptables jq curl xvfb`.

**Fresh-VM tested** (QEMU/KVM Ubuntu 24.04): clean install → all 8 services active, all
endpoints 200 (orchestrator/Jellyfin/Prowlarr/FlareSolverr). Idempotent re-runs leave existing
components and config untouched.

## Build a release bundle

```bash
./release/build.sh            # → dist/jellyfreedom-<version>/ + .tar.gz
```
Requires Go. Produces a static `orchestrator` binary + assets + scripts + `install.sh`.

## Install

```bash
tar xzf jellyfreedom-<version>.tar.gz
sudo ./jellyfreedom-<version>/install.sh
```
**One-liner** (once a release tarball is hosted — set the URL in `release/get.sh`):
```bash
curl -fsSL https://<host>/get.sh | sudo bash
```
`get.sh` downloads the bundle, extracts it, and runs `install.sh`. The download→extract→install
flow is VM-tested; only the public download URL is pending a release host.

## After install

1. Set TMDB / Prowlarr / Jellyfin URLs+keys in `/etc/jellyfreedom/config.yaml` **or** the
   dashboard → Settings (preferred — keys stay server-side).
2. Open `http://<host>:1990/dashboard/` → create the admin account.
3. **VPN → Configurations** → upload a WireGuard `.conf` from **any provider or self-hosted**
   (choose a torrent/P2P-friendly server) → **Activate**. The system needs only a valid
   WireGuard config — it is not tied to any provider. (NAT-PMP port forwarding is an optional
   optimization that activates only on providers that offer it, e.g. Proton.)
4. Add the `.strm` library folders (movies + tv) to Jellyfin.

## Security model (why it's safe-ish)

- Orchestrator runs **non-root** as `jellyfreedom`; it can only `systemctl restart` a fixed set
  of services and read VPN status — via a **scoped** sudoers rule (no arbitrary root).
- VPN configs live in an orchestrator-owned dir; activation writes there + restarts the netns
  (no privileged file-writing helper). See `DECISIONS.md` D14.
- Torrent traffic is confined to the `vpntorrent` netns behind a **fail-closed** WireGuard kill
  switch; Jellyfin↔LAN never uses the VPN. See `OPERATIONS.md` §3.

## Uninstall

```bash
sudo ./uninstall.sh           # removes app + units; KEEPS /var/lib/jellyfreedom + /etc/jellyfreedom
```

## Known gaps before this is truly turnkey

- Not yet validated on a fresh VM (do this next).
- `curl | sudo bash` download URL pending a release host.
- Off-the-shelf media stack must be installed separately (could be folded into the installer later).
- The live dev box is **not** migrated to these paths.
