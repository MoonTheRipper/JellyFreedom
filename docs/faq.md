# FAQ

### Can I use it without a VPN?

Not as installed, no. TorrServer runs inside a network namespace whose only way out is the
WireGuard tunnel, and with no tunnel that namespace is deny-all: torrents reach nothing. That
is the design, not a licence check.

If you genuinely want to run without one, you would have to run TorrServer yourself on the
host network and point `torrserver.base_url` at it instead of `10.42.0.2:8090`. Nothing in
the orchestrator stops you. You would be giving up the kill switch entirely, and the
namespace, watchdog and port-forward machinery would be doing nothing for you. Your traffic,
your call — but be clear that it is a different setup, not a config toggle.

Self-hosting the WireGuard endpoint (a VPS you control) works fine and is treated exactly like
any commercial provider.

### Does it work on a Raspberry Pi?

Partly. There is an arm64 build and the installer runs, but:

- **FlareSolverr is x64-only upstream**, so it is skipped on arm64. Cloudflare-protected
  indexers will not work unless you run FlareSolverr yourself from source against the
  distribution's `chromium`. Indexers without Cloudflare are fine.
- **Memory.** The default 2 GB RAM cache does not fit on a 2 GB or 4 GB Pi alongside Jellyfin.
  Drop `cache.size_mb` to 512–1024.
- **Do not use `cache.mode: disk` on SD or eMMC flash.** It is a constant write load on media
  that wears out. A small RAM cache, or a USB SSD, instead.
- Jellyfin on a Pi cannot transcode video meaningfully — which suits this project, since video
  transcoding is meant to be off anyway.

32-bit ARM (`armv7l`) has no published bundle. Build from source.

### Is anything written to disk?

Almost nothing, and nothing that grows.

- The streaming cache is **in RAM by default** and is a bounded ring buffer. It physically
  cannot exceed `cache.size_mb`.
- `.strm` files are a few hundred **bytes** each. A library of thousands of items is a rounding
  error.
- The database is small — text records, not media.
- Jellyfin writes its own metadata and images as usual, which is the one thing that does grow
  slowly. That is Jellyfin's storage, not ours.

Disk usage stays essentially flat no matter how much you watch. That is the entire point of
the design.

### What does the database hold?

`/var/lib/jellyfreedom/jellyfreedom.db`, SQLite:

- **Library items** — TMDB id, title, year, season/episode, the `.strm` path, the last chosen
  info hash and magnet, seeder count, status, poster URL, who requested it.
- **The request queue** — pending, processing and finished requests with their progress.
- **Subscriptions** — airing shows being followed for new episodes.
- **Users and sessions** — usernames, bcrypt password hashes, session tokens.
- **Settings** — API keys entered in the dashboard, quality and cache overrides, the play
  signing key, the webhook secret.

It holds **no media** and no viewing history beyond what is in the library and queue.

### Can I point it at my existing Jellyfin?

Yes, and that is the normal case. Jellyfin only needs to be reachable over HTTP from the
orchestrator.

1. Set the Jellyfin URL and API key in **Settings → Connections** — `http://127.0.0.1:8096`
   for a Jellyfin on the same box, or its LAN address for one elsewhere.
2. Add `/srv/jellyfreedom/movies` and `/srv/jellyfreedom/tv` as libraries. If Jellyfin is on a
   different machine, those paths must be reachable from it — an NFS or SMB share works, since
   the files are tiny text pointers.
3. Make sure `server.public_url` is an address your Jellyfin server can actually reach.

The installer leaves an existing Jellyfin installation completely alone: config, libraries and
users are untouched.

Keep the streaming library **separate** from any library backed by real downloaded files. Do
not mix the two in one Jellyfin library — one is "a file exists", the other is "a magnet is
resolvable right now", and mixing them produces exactly the confused state this project exists
to avoid.

### What happens on reboot?

Everything comes back on its own. All units are enabled at install time: the namespace is
rebuilt, the tunnel comes back up from whichever config you activated, TorrServer starts inside
the namespace, and the orchestrator and Jellyfin start normally. A watchdog begins probing 90
seconds after boot and every 60 seconds after that.

No torrents are resumed — there is nothing to resume. Everything is resolved fresh when you
press play.

### How do I back it up?

Stop the service first so the database is copied cleanly:

```bash
sudo systemctl stop jellyfreedom
sudo tar czf jellyfreedom-backup.tar.gz \
  /etc/jellyfreedom /var/lib/jellyfreedom /srv/jellyfreedom
sudo systemctl start jellyfreedom
```

- `/etc/jellyfreedom` — your config.
- `/var/lib/jellyfreedom` — the database **and your uploaded WireGuard configs, which contain
  private keys**. Treat this archive as a secret.
- `/srv/jellyfreedom` — the `.strm` files. Optional: they are regenerated from the database
  the next time an item is played or re-requested.

To restore, reinstall and put the directories back before starting the service.

### Is there any telemetry?

None. Nothing reports usage, errors or installs anywhere. The only outbound connections are the
ones you configured — TMDB, your indexers, your VPN provider, the swarm — plus the GitHub
release download when **you** run `jellyfreedom --update`.

### What ports does it open on my LAN?

| Port | What | Reachable from |
|---|---|---|
| 1990 | Media UI and admin dashboard | your LAN |
| 8096 | Jellyfin | your LAN |
| 9696 | Prowlarr | localhost only |
| 8191 | FlareSolverr | localhost only |
| 8090 | TorrServer | inside the VPN namespace only |

Nothing needs to be forwarded on your router, and nothing here should be exposed to the
internet. If your provider offers NAT-PMP port forwarding, that mapping is made **inside the
VPN tunnel** — it does not open anything on your own router.

### Can I add a video by pasting its address instead of searching?

Yes — dashboard → **Links**. Paste the page you would normally watch on, press Preview, and
it becomes a Jellyfin entry. It uses no indexer at all, which makes it the way in for
anything indexers cover badly.

Three things to know:

- **The link is looked up again every time you press play.** Sites sign their video
  addresses with a short expiry, so anything saved once would break within hours. Only the
  *page* is stored, never the video address.
- **The lookup and the stream both go through your VPN**, with the same kill switch as
  torrent traffic. There is no setting to turn that off.
- **It can still die.** If the uploader deletes the video, nothing brings it back. If a link
  that used to work stops extracting, that is nearly always a stale extractor:
  `sudo yt-dlp -U && sudo systemctl restart jellyfreedom`.

Videos a site offers only as an adaptive stream (HLS or DASH) are refused with a message
saying so, and so are live streams.

### Does it replace Radarr and Sonarr?

For a streaming library, yes — and it deliberately does not integrate with them. Radarr and
Sonarr are built around "a downloaded file exists", which a streaming setup never satisfies,
so pointing them at this produces perpetual "missing" states and faked import events.

If you also want a **kept** collection of real files, run that as a second, entirely separate
Jellyfin library managed by Radarr/Sonarr. Two libraries, one backing model each.

### Can I use Jackett instead of Prowlarr?

No. The orchestrator talks to Prowlarr's JSON API specifically, not a generic Torznab feed.
Prowlarr can aggregate the same indexers Jackett does.

### Why is the first play slow, and later plays fast?

Because the release is chosen **at play time**, from what is actually seeded right now, rather
than being frozen when you requested the title. That costs roughly 5–15 seconds on a cold
start, and it means a title that was fine last month still plays this month even though the
release you originally picked has died. The choice is cached, so replays and the next episode
start quickly.

### Do I seed? Can I turn that off completely?

You seed only the small window of pieces currently in the cache, at a capped rate, and only
while a torrent is loaded. Setting the upload limit to **zero is a bad idea**: peers choke you
under tit-for-tat and your own stream stalls. Low but nonzero is the working answer, and it is
the default.

### Can I keep something I watched?

No. There is no complete file to keep — the cache holds a moving window and evicts the rest.
This is a streaming system, not a download manager. If you want to keep files, that is what a
conventional torrent client plus a separate Jellyfin library is for.

### Does it run on macOS or Windows?

The orchestrator builds and runs on macOS for development. The full system does not: it
depends on Linux network namespaces, `wg-quick`, iptables and systemd. Windows is not
supported at all.

### Will there be a Docker image?

No.

### Something is broken and this page did not cover it

```bash
sudo jellyfreedom doctor
```

Then [troubleshooting.md](troubleshooting.md), which is organised by symptom.
