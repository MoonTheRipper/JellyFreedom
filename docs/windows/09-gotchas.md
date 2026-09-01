# 09 — Gotchas

Things that have already cost hours. Read before debugging, not after.

## A "started" signal is not a "running" signal

`systemctl restart` returns when the process has forked. A unit that dies 200 ms later still
produced a green tick, and the installer reported `websources ready` on a system where
nothing could work. **`sc start` on Windows behaves the same way.** Always re-check with
`is-active` / `sc query` after a beat.

## Test that your test can fail

A regression check for `AF_NETLINK` passed *with the fix reverted*, because the explanatory
comment containing that word was written into the same unit file being grepped. Every new
regression test in this repo is now confirmed red before it is confirmed green. Do the same.

## FlareSolverr is the flakiest component

It drives a headless browser to defeat Cloudflare, and it has broken repeatedly:

- the bundled Chrome would not run on the host kernel, so the installer redirects it at a
  system browser — and a later update **restored the bundled one**, re-breaking it;
- it failed with `Unable to receive message from renderer` for days; the eventual cause was
  space pressure in the temp directory, after two wrong hypotheses (the snap sandbox, then
  the browser binary itself);
- it ended up running as root with its code owned by an unprivileged account, so anything
  writing as that user could plant code root would execute.

**Expect to spend time here.** Diagnose it by asking it to fetch a page (`POST /v1` with
`request.get`), not by checking that the service is up — it starts happily and fails on the
first real fetch.

## Temp directories fill up

Covered in [06](06-history-and-patches.md). Two independent leaks: the browser leaves a
scratch dir per request, and yt-dlp's bundle unpacks ~76 MB per run and cleans up nothing
when it is killed. On Linux this filled a 7.8 GB tmpfs three times in four days, and the
resulting errors never once mentioned `/tmp`.

**On Windows:** watch `%TEMP%`, the service account's temp, and wherever you point
`web_sources.temp_dir`. Do not put the extractor's scratch on a RAM disk.

## `pkill -f` matches your own shell

Cost three separate interruptions in one session: a pattern that appears in the command line
you are typing also matches the shell running it. Kill by PID, or by the process listening on
a port. The Windows analogue is `taskkill /IM` with an over-broad image name.

## Stale processes make you test the wrong binary

A test server was left holding port 1991, so a rebuilt binary silently failed to bind and
every "verification" hit the *old* code. The result looked plausible. Check what is actually
listening before trusting a local test.

## The installer must be idempotent and non-destructive

It is re-run as the repair path, and users run it over working systems. It detects existing
components and leaves them alone. One version of `repair` deleted the web assets it was meant
to restore — see [06](06-history-and-patches.md).

## `.strm` files are contracts

Once written, they are in the user's Jellyfin library and Jellyfin has scanned them. Changing
the URL shape breaks playback for everything already there. The TMDB identity spelling is
**frozen** for this reason, and a startup migration re-signs files in place rather than
requiring a re-add. Any change you make to URL shape needs the same treatment.

## Windows-specific traps to expect

None of these have been hit yet — they are predictions, flagged so you look:

- **Path length.** `MAX_PATH` is 260 unless long paths are enabled. Library titles are
  user-supplied and already bounded at 200 runes for the *filename*, but the full path adds
  the library root and a season directory. Enable long paths, or bound the total.
- **Reserved names.** `CON`, `PRN`, `AUX`, `NUL`, `COM1`–`COM9`, `LPT1`–`LPT9`, plus trailing
  dots and spaces, are illegal or magic as filenames. `safeName` handles POSIX hazards; it
  has **not** been audited for these.
- **`:` in filenames** opens an alternate data stream rather than failing. Titles routinely
  contain colons.
- **Case-insensitive filesystem.** Two identities that differ only by case will collide on
  disk where they did not on Linux.
- **File locking.** Windows will refuse to replace a running executable; the self-update path
  must stop the service, or write beside and swap on restart.
- **Service account vs interactive user.** A service running as a non-interactive account has
  a different `%TEMP%`, no user profile by default, and different network drive visibility.
