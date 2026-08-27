# Troubleshooting

Almost everything in this system fails **quietly**. A Prowlarr with no indexers, a
FlareSolverr whose browser cannot fetch, a VPN server that connects but moves no data — none
of them raise an error anywhere you would look. They all present as "it finds nothing" or "it
won't play". This page is organised by what you actually see.

---

## Start here: `jellyfreedom doctor`

```bash
sudo jellyfreedom doctor
```

It runs without the orchestrator (the point is diagnosing an instance that is down), works
unprivileged with reduced checks, and **every failure prints the command that fixes it**.
Exit status is 0 when nothing failed, 1 otherwise.

```bash
jellyfreedom doctor --quiet          # only problems
jellyfreedom doctor flaresolverr     # one section
```

Sections: `system`, `install`, `privs`, `services`, `ports`, `orchestrator`, `prowlarr`,
`flaresolverr`, `torrserver`, `jellyfin`, `vpn`, `library`.

Most problems are fixed by re-running the installer, which is idempotent and keeps your
config, database and VPN configs:

```bash
sudo jellyfreedom repair
```

### Where things live

| | |
|---|---|
| Orchestrator log | `journalctl -u jellyfreedom -f` |
| TorrServer log | `journalctl -u torrserver-netns -n 50` |
| VPN namespace log | `journalctl -u vpntorrent-netns -n 50` |
| Watchdog log | `journalctl -t vpntorrent-watchdog -n 50` |
| FlareSolverr log | `journalctl -u flaresolverr -n 80` |
| Prowlarr log | `journalctl -u prowlarr -n 50` |
| Jellyfin log | `journalctl -u jellyfin -n 50` |
| Install log | `/var/log/jellyfreedom-install.log` |
| Config | `/etc/jellyfreedom/config.yaml` |
| Database | `/var/lib/jellyfreedom/jellyfreedom.db` |
| `.strm` library | `/srv/jellyfreedom/movies`, `/srv/jellyfreedom/tv` |

### Ports

| Port | Service | Bound to |
|---|---|---|
| 1990 | Orchestrator (media UI + dashboard) | all interfaces |
| 8096 | Jellyfin | all interfaces |
| 9696 | Prowlarr | localhost |
| 8191 | FlareSolverr | localhost |
| 8090 | TorrServer | inside the VPN namespace; reachable from the host at `10.42.0.2:8090` |

---

## Searches return nothing

In order of how often it is the cause.

### 1. Prowlarr has zero indexers

This is the single most common cause, and nothing in any UI tells you.

```bash
curl -s "http://127.0.0.1:9696/api/v1/indexer?apikey=<PROWLARR_KEY>" | jq length
```

`0` means every search will return empty forever. Open `http://<host>:9696` → **Indexers →
Add Indexer**, add at least one, and **Test** it. `jellyfreedom doctor prowlarr` reports this
count for you when the orchestrator is running.

### 2. FlareSolverr is up but cannot fetch pages

FlareSolverr passing a `GET /` check proves only that the **browser launched**. Its startup
self-test reads the binary's version and user agent and **navigates nowhere**, so a browser
that dies on its first real page load passes it. Only a real fetch proves anything:

```bash
curl -s -XPOST http://127.0.0.1:8191/v1 \
  -H 'Content-Type: application/json' \
  -d '{"cmd":"request.get","url":"https://example.com","maxTimeout":60000}'
```

`"status": "ok"` is the only acceptable answer. Anything else, with FlareSolverr apparently
"running", **is** your empty-search problem. See the FlareSolverr section below.

### 3. FlareSolverr is not attached to your indexers

Installing FlareSolverr does nothing by itself. Prowlarr uses it only if you have added it as
an **indexer proxy** and put its **tag on each indexer**. See
[first-run.md §4](first-run.md). An untagged Cloudflare-protected indexer fails no matter how
healthy FlareSolverr is.

### 4. The first search after idle just times out

Waking FlareSolverr and cold indexers can take **90 seconds or more**, and the request may
fail with a deadline error. Warm searches return in well under a second. Retry; the
`indexer-warmup` background task (every 20 minutes) keeps things warm after that. You can run
it by hand from the dashboard's **Tasks** panel.

### 5. TMDB is not configured

No TMDB key means no search results at all, because the search box queries TMDB, not the
indexers.

```bash
curl -s http://127.0.0.1:1990/api/configured
```

Every one of `tmdb`, `prowlarr`, `jellyfin`, `torrserver` should be `true`. Fix in
**Dashboard → Settings → Connections**.

### 6. Nothing is seeded

Old or niche episodes genuinely may not have a well-seeded release right now. The picker
requires a minimum seeder count (default 5) and rejects CAM/telesync rips. Lower
`min_seeders` if you want to see marginal releases — see [configuration.md](configuration.md)
— and expect them to stream badly.

---

## FlareSolverr

### Check it, in the order that proves progressively more

```bash
systemctl status flaresolverr
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8191/health   # 1: is it up
curl -s http://127.0.0.1:8191/ | head -c 200                            # 2: browser launches
curl -s -XPOST http://127.0.0.1:8191/v1 -H 'Content-Type: application/json' \
  -d '{"cmd":"request.get","url":"https://example.com","maxTimeout":60000}'  # 3: can it fetch
journalctl -u flaresolverr -n 80 --no-pager
```

### It is not listening at all

The **first** start downloads a matching chromedriver and can take up to 90 seconds. After
that, check the log. If the service is restart-looping, the usual causes are a destroyed
bundle (below) or a stale chromedriver cache.

### "Bundled Chrome crashes on this kernel" — this is a myth, don't act on it

The bundled Chromium 142 runs fine on modern kernels. What actually happens without
`--no-sandbox` is:

```
FATAL ... No usable sandbox!
```

Ubuntu 24.04 and later restrict unprivileged user namespaces through AppArmor, and
FlareSolverr's `chrome_sandbox` ships mode 0755 rather than setuid, so there is no fallback
path. **Upstream FlareSolverr already passes `--no-sandbox` itself**, so the bundled browser
is the right thing to run and no wrapper is needed. If you find an older guide telling you to
replace the bundled Chrome with a system browser, ignore it — that advice is what caused the
next problem.

### Recovering a FlareSolverr an old installer destroyed

An earlier version of this installer overwrote FlareSolverr's bundled 462 MB Chrome binary
with a 61-byte shell wrapper pointing at `/usr/bin/chromium-browser`. On Ubuntu that path is a
transitional package containing only a shim that execs the chromium **snap** — and
`apt-get install chromium-browser` exits 0 while installing no browser at all, so the
fallback never ran. There was no backup, and re-running the installer saw the files present
and reported success.

Check whether you are affected:

```bash
ls -l /opt/flaresolverr/_internal/chrome/chrome*
file /opt/flaresolverr/_internal/chrome/chrome
```

A tiny text file rather than a large ELF binary means the bundle was replaced. The current
installer heals this automatically where a `chrome.real` backup exists, and never truncates
anything again — it renames aside and symlinks, which is reversible.

```bash
sudo jellyfreedom repair                      # heals it if recoverable
sudo /opt/jellyfreedom/install.sh --repair flaresolverr   # forces a clean ~233MB re-download
```

### After any browser change, purge the driver cache

The cached chromedriver is keyed to Chrome's major version; a mismatch fails at launch. The
installer purges it for you, but by hand:

```bash
sudo rm -rf /var/lib/flaresolverr/.local/share/undetected_chromedriver \
            /var/lib/flaresolverr/.cache/selenium
sudo systemctl restart flaresolverr
```

### On arm64 there is no FlareSolverr

Upstream publishes x64 binaries only, so the installer skips it and tells you. Non-Cloudflare
indexers work normally. If you need it, run it from source against your distribution's
`chromium`.

---

## Prowlarr

```bash
systemctl status prowlarr
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9696/ping     # expect 200
journalctl -u prowlarr -n 50 --no-pager
```

- **Not answering:** check the log. Prowlarr keeps its data in `/var/lib/prowlarr` and runs as
  the `prowlarr` user; a permissions problem there stops it dead.
- **Answering, but JellyFreedom says it is not configured:** the API key is wrong. Prowlarr →
  Settings → General → API Key, then paste it into **Dashboard → Settings → Connections** and
  press **Test**.
- **Answering, key valid, still no results:** you have no indexers, or they are all failing.
  Test each one in Prowlarr's UI.

---

## "It says ready but Jellyfin shows nothing"

Work down this list.

**1. Did a `.strm` file actually get written?**

```bash
find /srv/jellyfreedom -name '*.strm' | head
cat "/srv/jellyfreedom/movies/Some Movie (2024)/Some Movie (2024).strm"
```

The file should contain a URL like
`http://192.168.1.50:1990/play/movie/12345?t=<token>`.

**If it says `http://CHANGE-ME-LAN-IP:1990/...` you have found your problem** — `public_url`
was never set. Fix `/etc/jellyfreedom/config.yaml` and restart; existing files are rewritten
on startup. See [first-run.md §2](first-run.md).

**2. Are the folders added to Jellyfin as libraries, with the right content type?**
`/srv/jellyfreedom/movies` must be a **Movies** library, `/srv/jellyfreedom/tv` a **Shows**
library.

**3. Are metadata providers enabled on those libraries?** With internet providers disabled,
Jellyfin finds the file but cannot match it: no poster, no title, and it does not appear in
search. It is in the database, invisible.

**4. Did Jellyfin scan?** JellyFreedom triggers a scan after writing a `.strm`, using its
Jellyfin API key. If that key is wrong, files are written and never picked up. Test the
Jellyfin connection in **Settings → Connections**, then run the `jellyfin-scan` task from the
dashboard's **Tasks** panel, or trigger a scan in Jellyfin itself.

**5. Permissions.** The folders must be readable by Jellyfin and writable by the
`jellyfreedom` user:

```bash
sudo jellyfreedom doctor library
```

---

## Playback fails, or takes forever

### What is normal

- **First play of a title: roughly 5–15 seconds.** The release is chosen at play time from
  what is seeded right now, then connected to and validated. Replays and the next episode are
  fast because the choice is cached.
- **First 45 seconds after any TorrServer restart:** few or no peers while the DHT
  bootstraps. Do not judge connectivity in that window.
- **Throughput ramps.** A cold torrent starts slow and climbs.

### What the errors mean

| What you see | Meaning |
|---|---|
| `no playable release available right now` (502) | Nothing resolvable and connectable was found for that title. |
| `could not find a playable release within the time limit` (504) | Candidates were tried but none connected in time. Often all the top candidates are "ghosts" — a healthy scrape count with no connectable peers. Retry; it may pick differently. |
| Playback starts then stalls | Swarm speed, or the tunnel dropped mid-stream. |
| "media is not supported by this client" | Jellyfin's playback policy. Enable **remuxing** and **audio transcoding**, keep **video transcoding off**. |

### Check the path yourself

```bash
# 1. Is the VPN actually carrying data?
sudo /opt/vpntorrent/jf-netns-helper status
sudo /opt/vpntorrent/jf-netns-helper exit-ip

# 2. Is TorrServer alive? (from the host, across the veth)
curl -s http://10.42.0.2:8090/echo

# 3. What did the orchestrator do?
journalctl -u jellyfreedom -n 100 --no-pager
```

### TorrServer's BitTorrent client has crashed

Symptom: `/echo` answers normally, but every attempt to add a torrent returns HTTP 500 and the
log says `BT client not connected`. The watchdog detects this (two consecutive failures, and
never while a stream is playing) and restarts the service by itself. By hand:

```bash
sudo systemctl restart torrserver-netns
```

Then wait ~45 seconds for the DHT before testing.

### Do not diagnose with the "downloaded" counter

TorrServer is lazy: its `downloaded` counter stays at 0 until something actually reads the
file. A stream request returning **HTTP 206** is the real proof that data is moving.

---

## Torrents connect to seeders but download nothing

**This is a VPN server problem, not a bug.** You are almost certainly on a server that your
provider does not flag for P2P, or one behind strict NAT. Tracker announces succeed, peers
connect, and piece data never transfers. It looks exactly like a broken torrent client.

The fix is to pick a **P2P-flagged server** from your provider and activate that config
(**Dashboard → VPN**). Nothing else you change will help.

If your provider offers NAT-PMP port forwarding, the `vpntorrent-portforward` service picks it
up automatically and roughly doubles the number of connectable seeders. It is optional: on
providers without NAT-PMP it idles quietly and streaming still works, because leeching is
outbound and needs no forwarded port.

```bash
systemctl status vpntorrent-portforward
cat /run/vpntorrent-portforward
```

---

## The VPN

### "No tunnel is up" on a fresh install is correct

Until you upload and activate a config, the namespace comes up with a **deny-all kill switch
and no tunnel**. TorrServer runs but cannot reach the internet. That is the intended safe
state — it fails closed rather than leaking. It is not a fault, and `doctor` reports it as a
warning with that explanation.

### Activation fails

Activation is verified: the tunnel must complete a real handshake, or your previous config is
restored and you get an error. Common causes:

- **The endpoint is unreachable** — wrong port, provider outage, or the host's own firewall.
  The handshake leaves through the host's NAT, so host-level firewall rules can block it.
- **The config is not a WireGuard config** — it needs `[Interface]`, `PrivateKey`, `[Peer]`
  and `Endpoint`. OpenVPN files are rejected.
- **A value contains shell metacharacters** — refused deliberately, because that file is
  parsed by a root process.
- **An IPv6-only config on a host booted with `ipv6.disable=1`** — refused with a clear
  message rather than half-configuring a tunnel.
- **`PostUp`/`PostDown`/`Table`/`DNS` lines vanished** — that is intentional sanitisation, not
  a failure. DNS inside the namespace is configured separately.

```bash
journalctl -u vpntorrent-netns -n 50 --no-pager
sudo systemctl restart vpntorrent-netns
```

### Verifying the kill switch

```bash
# The namespace's exit address must differ from the host's real one
sudo /opt/vpntorrent/jf-netns-helper exit-ip
curl -s https://1.1.1.1/cdn-cgi/trace | sed -n 's/^ip=//p'

# Facts about the kill switch, machine-readable
sudo /opt/vpntorrent/jf-netns-helper leakcheck
```

Expect `ipv4_output_policy=DROP`, `ipv4_default_dev=wg0-vpntorrent`, `wg_interface=up`.

Prove it fails closed by taking the tunnel down:

```bash
sudo /opt/vpntorrent/jf-netns-helper vpn-down
sudo ip netns exec vpntorrent curl --max-time 3 https://1.1.1.1 && echo "LEAK" || echo "BLOCKED"
sudo /opt/vpntorrent/jf-netns-helper vpn-up
```

`BLOCKED` is the correct answer. Meanwhile Jellyfin on your LAN is unaffected — it never uses
the VPN.

> If the namespace's exit IP ever **equals** your real public IP, stop torrenting and
> investigate immediately. `doctor` fails hard on this specific condition.

### The tunnel keeps dropping

A watchdog runs every 60 seconds and escalates on its own: re-assert the host NAT →
`wg-quick` down/up → full namespace rebuild. Watch it work:

```bash
journalctl -t vpntorrent-watchdog -n 50
cat /run/vpntorrent-status
```

`down: needs new wireguard config` means it exhausted every recovery step — the server has
most likely rotated or been withdrawn. Get a fresh config from your provider and activate it.

---

## Install failures

### It hung with no output at all

The old installer piped Jellyfin's upstream installer into `bash` with all output discarded.
That script deliberately reads from `/dev/tty`, so piping it **cannot** skip its confirmation
prompt — it waited forever, invisibly. The current installer passes upstream's supported
`SKIP_CONFIRM=true` and logs everything to `/var/log/jellyfreedom-install.log`. If you hit
this, you are on an old installer; fetch a current release.

### Jellyfin failed to install

Upstream's installer requires **2 GB free on both `/var/lib` and `/tmp`** and only supports
specific distribution codenames. It can also exit 0 having failed, which is why JellyFreedom
checks for the unit afterwards rather than trusting the exit status.

```bash
grep -i jellyfin /var/log/jellyfreedom-install.log | tail -40
df -h /var/lib /tmp
```

Install Jellyfin manually from the official documentation, then `sudo jellyfreedom repair`.

### TorrServer would not download

The installer resolves the current TorrServer release from upstream's API, with a pinned
fallback, because a previously hardcoded tag was **removed upstream** and 404'd — leaving
fresh installs with no streaming engine while still printing "Installed". If the download
fails now it is network access to github.com. **Nothing will stream until this succeeds.**

```bash
sudo /opt/jellyfreedom/install.sh --only torrserver
```

### "waiting for another package manager to finish"

Ubuntu cloud images run `unattended-upgrades` on first boot and hold the dpkg lock for
minutes. The installer waits up to 5 minutes, then gives up and asks you to re-run. Not a
fault.

### Not enough disk

The installer refuses below 4 GB free on `/opt`. FlareSolverr alone extracts to about 705 MB.

### A port is already in use

Preflight warns when 1990, 8090, 8096, 8191, 8192 or 9696 is held by something that is not
ours. Stop the other service or change ours before continuing.

### Wrong architecture

`get.sh` refuses to install an amd64 bundle onto an arm64 box, because the result is a service
that will not exec with no explanation. `jellyfreedom doctor install` also verifies the
installed binary matches the CPU.

---

## Pasted links (web sources)

Start with `sudo jellyfreedom doctor websources` — it separates the three things that fail
independently here.

**The Links section says "web sources are switched off in the configuration."**
The feature is opt-in and an upgrade never rewrites your existing config. Add the block
`doctor` prints, then `sudo systemctl restart jellyfreedom`. See
[configuration.md](configuration.md#web_sources).

**"yt-dlp is not installed."**
`sudo jellyfreedom repair websources` fetches it to `/usr/local/bin/yt-dlp`. Nothing else
depends on it, which is why the installer treats a failed download as a warning.

**A link that used to work now fails to extract.**
Almost always a stale extractor — sites change their players constantly:

```bash
sudo yt-dlp -U
sudo systemctl restart jellyfreedom
```

The dashboard shows the running version, and each link records why it last failed and when it
last worked. If updating does not fix it, the site's extractor is genuinely broken upstream.

**"Failed to extract … decompression resulted in return code -1"**
This is not a video problem. The yt-dlp binary unpacks ~76 MB of itself into its scratch
directory on every run, and that directory has no room:

```bash
df -h /var/lib/jellyfreedom          # where temp_dir lives
ls -ld /var/lib/jellyfreedom/tmp     # must exist and be writable by the service user
grep -A4 '^web_sources:' /etc/jellyfreedom/config.yaml
```

If `temp_dir` is unset or points at `/tmp`, fix it — `/tmp` is a RAM-backed tmpfs on stock
Ubuntu, so it is both small and expensive to fill.

**Nothing plays, and the log says "could not reach the video (is the VPN up?)".**
Exactly what it says. Web sources fail closed with the tunnel, like everything else:

```bash
sudo jellyfreedom doctor vpn
systemctl status jf-netnsproxy
curl -s --socks5-hostname 10.42.0.2:1080 -o /dev/null -w '%{http_code}\n' https://api.github.com/
```

That last command is the honest end-to-end test of the proxy: a code means the namespace can
reach the internet, `000` means it cannot.

**"that video is only offered as an adaptive stream."**
The site publishes it as HLS or DASH only — a manifest of thousands of separately-signed
segments rather than one seekable file. There is nothing to configure; that video cannot be
proxied yet.

**A link plays in the browser but the entry vanished from Jellyfin.**
Jellyfin has not rescanned. Trigger a scan from the dashboard's Tasks section.

---

## Service-by-service quick reference

```bash
# everything at once
sudo jellyfreedom doctor services

systemctl status jellyfreedom
systemctl status vpntorrent-netns torrserver-netns vpntorrent-portforward jf-netnsproxy
systemctl status flaresolverr prowlarr jellyfin
systemctl status vpntorrent-watchdog.timer

curl -s http://127.0.0.1:1990/healthz          # orchestrator
curl -s http://127.0.0.1:1990/api/configured   # what is wired up
curl -s http://10.42.0.2:8090/echo             # TorrServer, inside the netns
curl -s --socks5-hostname 10.42.0.2:1080 -o /dev/null -w '%{http_code}\n' https://api.github.com/  # the VPN proxy
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8096/System/Info/Public  # Jellyfin
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9696/ping                # Prowlarr
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8191/health              # FlareSolverr
```

---

## Last resorts

```bash
sudo jellyfreedom repair     # re-run the installer over this instance; keeps config + data
sudo jellyfreedom --update   # pull the current release and run its installer
```

Neither touches your config, database or VPN configs. If you need to start completely clean,
see the uninstall levels in [install.md](install.md#6-uninstalling).

If you are reporting a problem, include the output of `sudo jellyfreedom doctor`, your
version (`jellyfreedom --version`), and the relevant journal lines. Please do not paste your
API keys or the contents of a WireGuard config — the latter contains a private key.
