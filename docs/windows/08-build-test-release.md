# 08 — Build, test, release

## Build

```bash
JF_VERSION=0.7.2 GOOS=linux GOARCH=amd64 CGO_ENABLED=0 ./release/build.sh 0.7.2
```

`release/build.sh` is the single source of truth for what a bundle contains. Version
precedence is `JF_VERSION` → `$1` → the `VERSION` file, **never a date**. Web assets are
embedded (`assets.go`, `go:embed`), so the binary is self-contained; `--assets <dir>` serves
from disk for UI development and is the fastest way to iterate on the front end.

For Windows you will add `GOOS=windows` targets and a packaging step. `CGO_ENABLED=0` already
holds because SQLite is pure Go.

## Tests

| Suite | What it covers |
|---|---|
| `go test ./...` | Everything Go. Includes token encoding, store migrations, the netns proxy's destination checks |
| `tests/install/test_install.sh` | Hermetic installer harness — drives `release/install.sh` against a fake root via `JF_DESTDIR`, no root, no network. ~129 assertions |
| `tests/install/test_prowlarr_hardening.sh` | Extracts `harden_prowlarr` and drives it against fixture configs |
| `tests/doctor/` | The diagnostic script |

CI runs eight jobs: gofmt, build/vet/test, shellcheck over every shipped script,
cross-compile for amd64 and arm64, the hermetic harness, and **installer smoke tests that run
the real installer on throwaway runner VMs** for 22.04, 24.04 and 24.04-arm.

The smoke tests are the important ones. The harness is fast but hermetic; the smoke test is
the only thing that has ever caught a real deployment failure.

**For Windows you need an equivalent smoke test** — a runner VM where the installer actually
runs and the stack actually comes up. Without it you are in exactly the position that let the
`jf-netnsproxy` bugs ship green three times.

### Two harness lessons

1. **It must be able to run over an existing install**, not only into a clean root. The
   `repair` bug in 0.6.1 existed because `JF_DESTDIR` made every scenario take the
   fresh-install branch.
2. **Confirm a new test fails without its fix.** A regression check for `AF_NETLINK` once
   passed with the fix reverted, because the explanatory comment containing that word was
   written into the file being grepped.

## Release channels

**Stable** is the set of versions someone explicitly promoted by tagging `v*`.
**Nightly** is whatever is on `main`, built and published on every merge that touches code.

The separation rests on one property, and it is GitHub's rather than ours: **a prerelease is
never "the latest release"**. Nightlies are prereleases; `get.sh` and the CLI resolve stable
through `releases/latest/download/…`, which excludes them. So no number of nightlies can move
a stable install onto one.

The channel lives in `/opt/jellyfreedom/CHANNEL` and **survives updates** — an upgrade that
says nothing keeps the channel it is on. Nightly tags are immutable `nightly-<date>-<sha>`,
pruned to ten; a moving pointer would invalidate the provenance attestation of whatever it
pointed at, which is why release tags are never moved either.

## When to cut one

Written down in `docs/RELEASING.md` after a run of seven releases in two working days, two of
them twenty-two minutes apart.

- Release when it carries a feature, or a fix somebody is actually hitting.
- Immediately for exactly three things: **breaks a fresh install, loses data, security.**
- Everything else rides along with the next one.

And the rule that matters most:

> **Install merged `main` on a real machine and exercise the change BEFORE tagging.**

Green CI is not that step. Every same-day follow-up release in this project's history passed
all eight jobs first. `0.5.4`'s flaw was found by deploying `0.5.4`.

## Release mechanics

`release/bump.sh patch|minor|major|X.Y.Z` moves `VERSION`, renames `## [Unreleased]` in the
changelog, repoints the compare links, and commits on a `release/<version>` branch. It refuses
a dirty tree, an existing tag, and an empty `[Unreleased]` — and re-runs the release
workflow's own note extraction against the file it just wrote, so a layout change is caught
before the tag exists.

Then: PR → green CI → squash-merge → tag the merged commit → the workflow builds both
architectures, generates `SHA256SUMS`, attests provenance and opens a **draft** release. The
draft is deliberate; publishing is a human step (`gh release edit vX --draft=false --latest`).

Never move a published tag. It invalidates the provenance attestation and the published
checksums of whatever it pointed at.
