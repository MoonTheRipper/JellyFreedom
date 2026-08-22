# First run: from a finished install to playing something

Follow this in order. Several steps are invisible in every UI involved — skipping them
produces a system that looks healthy and finds nothing.

Check the install first:

```bash
sudo jellyfreedom doctor
```

On a brand-new install you should expect exactly two complaints: the connection settings are
not configured yet, and **no WireGuard tunnel is up**. Both are fixed below. The VPN warning
is not a fault — until you activate a config, torrent traffic is blocked rather than leaked.

---

## 1. Create the admin account — do this immediately

Open `http://<your-server>:1990/dashboard/`.

The setup page is public until the first account exists, and that first account is an admin.
**Whoever reaches the box first owns it.** Create yours before anyone else can reach the host.

---

## 2. Set `public_url` — the step nothing warns you about

The installer writes a starter config containing a placeholder:

```yaml
server:
  public_url: "http://CHANGE-ME-LAN-IP:1990"
```

This URL is written **inside every `.strm` file**. It is how Jellyfin — and in some playback
paths the client device itself — reaches the stream. If it still says `CHANGE-ME-LAN-IP`,
items will appear in your library and playback will fail with nothing useful in any log.

```bash
ip -4 addr show scope global | grep inet          # find this host's LAN address
sudo nano /etc/jellyfreedom/config.yaml           # set public_url to http://<that-ip>:1990
sudo systemctl restart jellyfreedom
```

Use the host's LAN IP or a hostname that resolves on your network — not `127.0.0.1`, unless
you are certain nothing but the Jellyfin server on this same box will ever fetch the stream.

Existing `.strm` files are rewritten on startup when the URL changes, so fixing this later
works; fixing it now avoids the confusion.

---

## 3. Add indexers to Prowlarr — the required step that reports nothing

Open Prowlarr at `http://<your-server>:9696`.

JellyFreedom ships **no indexers**. A Prowlarr with a perfectly valid API key and **zero
indexers returns empty results forever**, with no error anywhere explaining why. It is the
most common "it's broken" report there is.

1. **Settings → General** — the **API Key**. The installer normally detects this from
   Prowlarr's own config and pre-fills it for you, so step 5 may already be done — check
   there before copying it by hand.
2. **Indexers → Add Indexer** — add the indexers you intend to use.
3. Use **Test** on each one. Green is the only acceptable result.

Then confirm it end to end from a shell:

```bash
curl -s "http://127.0.0.1:9696/api/v1/indexer?apikey=<PROWLARR_KEY>" | jq length
```

Anything other than a number greater than zero means step 3 is not finished.

---

## 4. Add the FlareSolverr proxy in Prowlarr — and tag it

Installing FlareSolverr does **nothing on its own**. Prowlarr will not use it until it is
configured as an indexer proxy and that proxy's tag is attached to the indexers that need it.

**The installer does the first half for you** — if FlareSolverr passed its post-install probe,
a `FlareSolverr` indexer proxy and a `flaresolverr` tag already exist in Prowlarr. (It is
deliberately skipped when the probe failed: pointing Prowlarr at a proxy that cannot fetch
makes every search slow or failing, which is worse than not having one.) You still have to
**attach the tag to the indexers that need it** — that part is a per-indexer choice.

To check whether the proxy already exists, open **Settings → Indexers → Indexer Proxies** in
Prowlarr. If a `FlareSolverr` entry is there, skip to step 3 below and just tag your indexers.

To add it by hand:

1. **Settings → Indexers → Indexer Proxies → `+` → FlareSolverr**
2. **Host:** `http://127.0.0.1:8191`
3. **Tags:** give it a tag, e.g. `flaresolverr`. **Save.**
4. Open each Cloudflare-protected indexer and add that **same tag** to it.

An untagged indexer never touches FlareSolverr, no matter how healthy FlareSolverr is.

Confirm FlareSolverr can actually fetch a page — not merely that it started:

```bash
curl -s -XPOST http://127.0.0.1:8191/v1 \
  -H 'Content-Type: application/json' \
  -d '{"cmd":"request.get","url":"https://example.com","maxTimeout":60000}' | head -c 200
```

You want `"status": "ok"`. A `GET /` proves only that the browser launched — its startup
self-test navigates nowhere, so a browser that dies on its first real fetch passes it. That
failure presents to you as "searches return nothing".

> On arm64 FlareSolverr is not installed at all (upstream ships x64 binaries only). Indexers
> that are not behind Cloudflare still work.

---

## 5. Connect the orchestrator: TMDB, Prowlarr, Jellyfin

Get the two remaining keys:

- **TMDB**: create a free account at themoviedb.org → Settings → API → API Key.
- **Jellyfin**: `http://<your-server>:8096` → finish Jellyfin's own setup wizard if you have
  not → **Dashboard → API Keys → `+`**.

Then in the JellyFreedom dashboard: **Settings → Connections**

| Field | Value |
|---|---|
| TMDB API key | your key |
| Prowlarr URL | `http://127.0.0.1:9696` |
| Prowlarr API key | from step 3 |
| Jellyfin URL | `http://127.0.0.1:8096` |
| Jellyfin API key | from above |
| TorrServer URL | `http://10.42.0.2:8090` (leave as installed) |

Keys entered here are stored server-side and are never sent back to the browser. Use **Test**
on each. You can put them in `config.yaml` instead, but the dashboard is preferred — see
[configuration.md](configuration.md) for how the two interact.

---

## 6. Upload and activate a WireGuard config

**Dashboard → VPN → Configurations → Upload**, pick your `.conf`, then **Activate**.

- Any provider works, or self-hosted. Nothing is Proton-specific.
- **Choose a server your provider flags as P2P.** This matters more than anything else in
  this document. On a non-P2P or strict-NAT server, torrents connect to dozens of seeders and
  then transfer **zero bytes** — it looks precisely like a bug and is not one.
- Upload never activates by itself; activation is an explicit click.
- Activation is verified: the tunnel must complete a real handshake or the previous config is
  restored and you get an error. "Activated" means the tunnel is genuinely up.
- Directives that `wg-quick` would execute as root (`PostUp`, `PostDown`, `PreUp`,
  `PreDown`) plus `Table`, `SaveConfig` and `DNS` are stripped from the file. This is
  deliberate; the UI tells you what was removed. DNS inside the namespace is handled
  separately, so a stripped `DNS =` line costs you nothing.

Verify:

```bash
sudo /opt/vpntorrent/jf-netns-helper status     # the tunnel, key material filtered out
sudo /opt/vpntorrent/jf-netns-helper exit-ip    # must NOT equal your real public IP
curl -s https://1.1.1.1/cdn-cgi/trace | sed -n 's/^ip=//p'   # your real public IP
```

If those two addresses match, torrent traffic is not going through the VPN. Stop and fix it
before doing anything else — `jellyfreedom doctor` fails loudly on exactly this.

---

## 7. Add the library folders to Jellyfin

The installer created `/srv/jellyfreedom/movies` and `/srv/jellyfreedom/tv` and owns them.

In Jellyfin: **Dashboard → Libraries → Add Media Library**

| | |
|---|---|
| Content type **Movies** | folder `/srv/jellyfreedom/movies` |
| Content type **Shows** | folder `/srv/jellyfreedom/tv` |

For each one, leave the metadata downloaders **enabled**. With internet providers off,
Jellyfin finds the `.strm` file but cannot match it to any metadata: the item exists in its
database with no poster, no title match, and does not appear in search.

### Two Jellyfin settings that matter a great deal

**Turn off these scheduled tasks** (Dashboard → Scheduled Tasks — remove their triggers):
*Extract Chapter Images*, *Scan Media Library*, *Media Segment Scan*. Each of them seeks and
probes whole files through the streaming URLs, which means pulling large amounts of data from
the swarm in the background. JellyFreedom triggers a scan itself after writing a `.strm`, so
you lose nothing.

**Set the playback policy** on your Jellyfin user (Dashboard → Users → *user* → Playback):

| Setting | Value | Why |
|---|---|---|
| Video transcoding | **off** | A full re-encode while pulling from a swarm is what saturates a network and stutters. |
| Audio transcoding | **on** | Cheap, and fixes AC3/DTS on clients that cannot handle it. |
| Playback remuxing | **on** | Repackages a container (e.g. h264-in-MKV in a browser) with no re-encode. |

With all three off you get "media is not supported by this client" on perfectly good
releases. With video transcoding on, you get stutter. The combination above is the one that
works.

---

## 8. Optional: the playback-stopped webhook

Without it, everything still works — TorrServer drops idle torrents on its own timeout. With
it, the cache is freed the moment you stop watching.

Install Jellyfin's official **Webhook** plugin and add a destination:

- URL: `http://127.0.0.1:1990/webhook/jellyfin`
- Notification type: **Playback Stop** only
- Add a request header `X-JellyFreedom-Token` with the shared secret

The endpoint **fails closed**: without the correct secret it rejects every call, so that a
stranger who guesses an item ID cannot drop a torrent mid-playback. The secret is generated on
first run and is currently only readable from the database (the `sqlite3` CLI is not
installed by default):

```bash
sudo apt-get install -y sqlite3
sudo sqlite3 /var/lib/jellyfreedom/jellyfreedom.db \
  "select value from settings where key='webhook.secret';"
```

If the plugin build you have cannot add a header, append `?token=<secret>` to the URL instead
— the endpoint accepts either.

---

## 9. First search, first play

Open the media UI at `http://<your-server>:1990/`, search for something, and request it.

What you should see: the request is queued, resolves within a few seconds, and appears as a
`.strm` in Jellyfin. Then play it from any Jellyfin client.

**What "normal" looks like on a cold start:**

- **The very first search after an idle period can take 90 seconds or more** and may time out.
  Prowlarr is waking FlareSolverr and the indexers. Warm searches come back in well under a
  second. A background task re-warms them every 20 minutes, so this mostly affects the first
  request after an install. Retry it.
- **The first play of a title buffers for roughly 5–15 seconds.** The release is chosen at
  play time from what is actually seeded right now, then connected to. Replays and the next
  episode are much faster because the choice is cached.
- **For about 45 seconds after any TorrServer restart**, torrents connect to few or no peers
  while the DHT bootstraps. Do not judge connectivity in that window.
- Throughput ramps: a cold torrent starts slowly and climbs as it finds seeders. Popular
  releases connect to dozens quickly; old or niche episodes may never find many.

If nothing plays, or searches come back empty, go to [troubleshooting.md](troubleshooting.md)
— it is organised by symptom.

---

## 10. A final check

```bash
sudo jellyfreedom doctor
```

Every section green, and the VPN section reporting an exit IP different from your real one,
means the system is properly set up.
