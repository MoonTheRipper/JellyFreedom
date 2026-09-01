# Windows port — start here

You are a fresh instance with no memory of how JellyFreedom got here. This folder is the
handover. Read it in order; it is written so you can start from nothing.

**Branch:** `feat/windows-port`, cut from `main` at v0.7.2.
**Nothing Windows-specific has been written yet.** This is a plan and a complete account of
the existing system, not a partial port. You are not finishing somebody's half-done work —
you are starting, with the Linux system fully documented so you do not rediscover it.

## Read in this order

| | |
|---|---|
| [01-mission-and-constraints.md](01-mission-and-constraints.md) | What this is for, and the rules you may not break |
| [02-architecture.md](02-architecture.md) | How the Linux system works, and why each piece exists |
| [03-components.md](03-components.md) | Every component, its Linux form, its Windows equivalent |
| [04-the-vpn-problem.md](04-the-vpn-problem.md) | **The one hard problem.** Read this before designing anything |
| [05-porting-plan.md](05-porting-plan.md) | Phases, in dependency order, with a definition of done |
| [06-history-and-patches.md](06-history-and-patches.md) | Every bug that has been fixed and why it happened |
| [07-security.md](07-security.md) | The audit, what was fixed, what is still open |
| [08-build-test-release.md](08-build-test-release.md) | How the build, tests, channels and releases work |
| [09-gotchas.md](09-gotchas.md) | Traps that have already cost time. Read before debugging |

## The short version

JellyFreedom searches torrent indexers and streams the result straight into Jellyfin,
without downloading to disk and without leaking the user's IP. One Go binary (the
orchestrator) plus four off-the-shelf services. It is live on a Linux box and has been
through a five-reviewer security audit.

The Go code is ~95% portable already. The port is not really about Go — **it is about
replacing a Linux network namespace with something on Windows that fails closed.** That is
[04](04-the-vpn-problem.md), and if you get it wrong the user's home IP leaks to a torrent
swarm. Everything else is plumbing by comparison.

## Who you are working for

Technically strong: runs home servers, set up WireGuard on a FritzBox, has run Jellyfin and
the *arr stack, has written a WebTorrent engine. Wants **decisive, opinionated senior
recommendations**, not menus of options. Lead with a call, justify it, name the one thing
that would change your mind. Do not moralise about what gets streamed; do give honest
security and privacy advice, because privacy is a stated goal of the project.

**Attribution: never sign commits as Claude.** moontheripper is the sole author.

## Working rules that apply to you

- **Never commit to the default branch.** Branch, PR, green CI, squash-merge. This was
  learned expensively: direct commits to `main` once forced a force-push that rewrote
  published tags and cost the repo every star it had.
- **Verify before reporting.** Run it, query the live system, read the output. Do not report
  a thing as working on the strength of the diff. Several claims in this project's history
  looked right from the code and were wrong against real data.
- **One feature at a time.** Land it before starting the next. Never two in one tree.
- **Do not cut a release per merge.** See [08](08-build-test-release.md#when-to-cut-one).
