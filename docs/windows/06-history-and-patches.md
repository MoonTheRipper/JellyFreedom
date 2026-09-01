# 06 — History and patches

Every significant bug this system has had, what caused it, and what it should teach you.
`CHANGELOG.md` has the user-facing version; this is the engineering version.

Read it not as trivia but as a **map of where the sharp edges are**. Several of these will
have direct Windows analogues.

---

## The three founding failures

Predate the versioned history; the architecture exists because of them. Covered in
[01](01-mission-and-constraints.md): storage filled up, \*arr availability never matched
streaming, and a frozen info hash rots.

## Duplicate queue entries and an unreadable queue (pre-0.5)

The queue showed the same film several times "in progress", and a show's episodes appeared
as a flat wall of rows.

**Cause:** the two idempotency checks — "already in the library" and "already in flight" —
sat *inside* an `if req.Magnet == ""` branch, so any request carrying a magnet skipped both.

**Fix:** three layers, because one was clearly not enough — the checks moved out of the
branch, a `RequeueWithMagnet` path for the legitimate case, and a unique index
(`idx_queue_active_identity`) so the database refuses a duplicate even if the code forgets.
Cleanup took 26,187 rows to 1,591.

**Lesson:** when a guard is skipped by a code path, do not just move the guard — add a
constraint that does not depend on the code being right.

---

## 0.5.2 — Identity stops meaning "TMDB"

Five features, but one idea: an item's identity became `(provider, provider_id)`, with
`tmdb_id` kept as the TMDB *spelling* of it. This is what later made web sources possible.
Also added per-user library visibility (default-deny, enforced in SQL) and an external
ingest API.

**Watch for:** `reconcileIdentity` in `internal/store` rejects a row claiming both a provider
identity and a `tmdb_id`. That invariant is what keeps the two key spaces from meeting.

## 0.5.3 — Paste-a-link web sources

Paste a video page URL, yt-dlp extracts it, a `.strm` is written holding
`/play/p/web/movie/{id}` — never the direct media URL, because CDN links are signed and
expire within hours.

## 0.5.4 — The proxy could not read its own network interface

`jf-netnsproxy` exited on **every start on every machine**, so web sources never worked
outside development.

**Cause:** the unit's sandbox granted `AF_INET AF_INET6 AF_UNIX`. The service derives its own
listen address from the namespace's veth using `net.Interfaces()` — which is a
`NETLINK_ROUTE` socket. Without `AF_NETLINK` it failed with "address family not supported by
protocol".

**Why nobody noticed:** `systemctl restart` returns when the process has *forked*, not when
it has *survived*. The unit died ~200 ms later, so the installer printed a tick and reported
`websources ready` on a system where nothing could work.

**Lesson, and it recurs:** a success signal that does not observe the thing succeeding is not
a success signal. On Windows, `sc start` has exactly the same property.

## 0.5.5 — The fix could not start on the machines that needed it

0.5.4 shipped a correct unit that systemd then refused to run: a host upgrading from the
broken 0.5.3 had a unit that had failed five times, and "Start request repeated too quickly"
blocks every further start — including the one that would have run the fix.

**Fix:** `systemctl reset-failed` before restarting.

**Lesson:** a repair path must consider the state of the machine it is repairing. Windows
SCM has its own failure/restart policy; check what it does after repeated failures.

## 0.6.0 — Batch link paste

The Links box takes a list (whitespace/comma separated — **not** colons: every URL contains
`://`), reads two at a time in the background, and the library is chosen once from a dropdown
rather than typed.

## 0.6.1 — `repair` deleted the files it was meant to repair

`jellyfreedom repair` re-runs the installer *from* `/opt/jellyfreedom`, so source and
destination were the same directory. The web step is `rm -rf "$APP_DIR/web"` followed by a
copy *from* that path — it deleted the assets, then could not stat the directory it had just
emptied. It also reported a helper as missing that was installed and working.

It got away with it only because the orchestrator serves its assets from inside the binary.

**Lesson, and the big one:** **no test exercised the in-place path.** Under `JF_DESTDIR`
every harness scenario ran from the staged bundle and took the other branch. That blind spot
is why this bug and both `jf-netnsproxy` bugs shipped with green CI. When you build a Windows
installer, make sure the harness can run it *over an existing install*, not only into a clean
one.

## 0.6.2 — `/tmp` filled up, three times in four days

FlareSolverr drives a browser per request and each launch leaves a scratch directory. Under a
snap Chromium those land in a root-owned `0700` path that FlareSolverr's own cleanup cannot
even traverse. Measured: **3,894 directories, 7.7 GB, 741k inodes**, filling a 7.8 GB tmpfs.

The consequences arrive disguised — apt stages archives there, the updater stages its
download there, `go build` fails to link — and none of them mention `/tmp`.

**Fix:** an hourly reaper, narrow on purpose (inside a snap's tmp, an hour old, *and* named
like a browser). Later extended to yt-dlp's `_MEI*` scratch on the data partition, which
leaks for a different reason: the bundle is SIGKILLed on timeout or client disconnect, and a
killed PyInstaller bundle cleans up nothing.

**Windows analogue:** `%TEMP%` and per-service temp dirs. Same failure, same disguise.

## 0.6.3 — Every non-TMDB `.strm` was rewritten to `/play/movie/0`

The user's pasted links stopped playing. All eight `.strm` files held the **same URL and the
same token**.

**Cause:** `migrateStrmTokens` rewrites every `.strm` at startup and built the URL from
`it.TMDBID` alone. A web source has no TMDB id, so that field is `0` for all of them — eight
distinct links collapsed onto one identity. The rows carried their real identity in
`provider`/`provider_id` the whole time; the rewriter was never taught to read it. Two rows
of different identity therefore shared a capability token.

**Why it survived:** it fires on **restart**, days after the links were added and were
playing. Adding a link writes its own correct URL and never touches that path, so the
feature's end-to-end test passed while the bug waited for the next reboot.

**Lesson:** test the *startup* paths, not only the request paths. A migration that runs at
boot is code that nothing in a normal test session executes.

## 0.7.0 — 0.7.2 — The security audit

Five parallel reviewers went over auth and tokens, the privilege boundary, web sources and
SSRF, the browser surface, and secrets at rest. Thirteen findings fixed across three
releases. Full detail in [07](07-security.md); the four that mattered:

1. **The database was readable by every local account** — with session tokens in plaintext.
2. **`/play` enforcement failed open on restart** — it was re-derived each boot.
3. **`/proxy/stream` was an unauthenticated existence oracle** over the whole library.
4. **Web-source thumbnails were fetched by the browser from the source site, outside the
   VPN** — leaking the home IP, in the feature whose entire purpose is not doing that.

---

## Patterns worth internalising

- **A green tick that does not observe the outcome is not evidence.** Cost three releases.
- **Untested paths ship broken.** In-place repair and startup migrations were both invisible
  to CI, and both shipped bugs.
- **Fix the code AND add the constraint.** Duplicate queue rows needed a unique index, not
  just a corrected branch.
- **A test that cannot fail is worse than no test.** A regression check for `AF_NETLINK`
  passed with the fix reverted, because the explanatory comment containing the word was
  written into the same file being grepped. Always confirm a new test fails without the fix.
- **Verify on the real machine before tagging.** Every same-day follow-up release in this
  project's history passed all eight CI jobs first.
