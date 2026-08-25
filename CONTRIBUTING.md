# Contributing to JellyFreedom

Thanks for looking. This is a small, opinionated, single-maintainer project. Contributions
are welcome, but please read the constraints below first — several of them are settled
decisions, and pull requests that relitigate them will be closed.

## Before you start

Open an issue describing the problem before writing a large change. A short discussion
saves a wasted afternoon. Small fixes (a bug, a typo, a broken link) can go straight to a
pull request.

## Settled constraints — please do not relitigate

These are recorded with their reasoning in [`docs/dev/decisions.md`](docs/dev/decisions.md).
Read it before proposing an alternative.

1. **No Docker.** Native binaries and systemd units only. The orchestrator must build to a
   single static Go binary (`CGO_ENABLED=0`).
2. **No paid debrid service.** Real-Debrid / AllDebrid / Premiumize are not the primary
   path and will not become it. Torrent streaming through a bounded cache *is* the design.
3. **Go, for the orchestrator.** Not Rust, not Node, not Python.
4. **Debian/Ubuntu is the target.** macOS is supported for development only. Keep the code
   portable — clone, build, run, with no host-specific paths baked in.
5. **No Radarr/Sonarr in the availability path.** Availability here means *"a magnet with
   enough seeders is resolvable right now"*, not *"a file exists on disk"*. The
   orchestrator owns that definition.
6. **Security and privacy are features, not extras.** Torrent traffic must remain confined
   to the VPN network namespace, and the kill switch must stay fail-closed.

## Development

```bash
git clone https://github.com/MoonTheRipper/JellyFreedom.git
cd JellyFreedom
go build ./...
```

### Build, test, lint

Everything CI runs, you can run locally:

```bash
gofmt -l ./cmd ./internal      # must print nothing
go vet ./...
go test -race ./...
go build ./...

# cross-compilation must keep working for both target architectures
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/orchestrator
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/orchestrator

# shell scripts (~900 lines of them run as root — lint them)
shellcheck --severity=warning release/*.sh release/jellyfreedom vpntorrent/*.sh
```

### Running locally

```bash
cp release/config.sample.yaml config.yaml   # then edit it; config.yaml is gitignored
go run ./cmd/orchestrator --config config.yaml --db ./jellyfreedom.db --assets ./web
```

The orchestrator starts without any API keys configured — set them at
`http://localhost:1990/dashboard/`. TorrServer, Prowlarr, and Jellyfin need to be running
separately for anything beyond the UI to work.

## Testing the installer — **safely**

`release/install.sh` runs as root, installs system packages, creates users, writes
systemd units and sudoers rules, and reconfigures networking.

> **Never run the installer on a machine you care about.** Never on your live
> JellyFreedom box. Never on a workstation.

Use one of these instead:

1. **The hermetic harness** (`tests/install/harness.sh`) — runs the installer against a
   fake filesystem root with every privileged and networked command mocked. No root, no
   network, no side effects. This is the fast loop; use it for anything you can.
2. **A throwaway VM or cloud instance** you are willing to destroy — a fresh
   Ubuntu 22.04 / 24.04 image, snapshot first, discard afterwards. This is the only
   honest way to test the third-party install steps end to end.
3. **CI** — pushing a branch runs `installer-smoke`, which does exactly (2) on a
   disposable GitHub runner and asserts the whole stack comes up. Let it do the work.

If your change touches `install.sh`, `get.sh`, `uninstall.sh`, or anything under
`vpntorrent/`, say in the pull request **which** of the above you used.

## Commit style

Match what is already in `git log`:

- Subject line: imperative mood, sentence case, no prefix or scope tag, roughly ≤ 72
  characters. *"Fix request-state logic and bump version to 0.2.1"*, not
  *"fix(api): request state"*.
- Body: `-` bullets, wrapped at about 80 columns, one bullet per meaningful change,
  explaining the *why* where it is not obvious.
- No `Co-Authored-By:` trailers and no tooling attribution. The commit author is the
  person submitting it.

## Pull requests

`main` is protected. It accepts pull requests only, all eight CI jobs must pass before a
merge is allowed, and it cannot be force-pushed or deleted — by anyone, including the
maintainer. So the loop for every change, however small, is:

```bash
git checkout main && git pull
git checkout -b feat/short-name        # or fix/short-name
# ... work, commit ...
git push -u origin feat/short-name
gh pr create --fill
gh pr checks --watch                   # eight jobs, a few minutes
gh pr merge --squash --delete-branch   # only unlocks once they are green
```

There is no way to skip the checks, which is the point: the merge button stays disabled
until CI agrees, so `main` is always a commit that built and passed its tests.

- One logical change per pull request.
- CI must be green: build, `go vet`, tests, `gofmt`, ShellCheck, both cross-compiles, and
  the installer smoke test.
- Update the docs in the same change. If you change behaviour that `docs/dev/architecture.md`,
  `docs/dev/operations.md`, `docs/install.md`, `docs/configuration.md`, or `README.md`
  describes, fix them too.
- Add an entry under `## [Unreleased]` in `CHANGELOG.md`. `release/bump.sh` turns that
  section into the published release notes later, and refuses to cut a release when it is
  empty — so an omission here is caught at release time, not discovered by readers.
- If you change routes or authentication, re-check `SECURITY.md` against reality.

## Reporting bugs

Use the issue templates — they ask for the things that actually make installer and
streaming problems debuggable (architecture, unit states, journal output, whether
FlareSolverr answers a `/v1` request). Answering them up front saves a round trip.

Please do not report security vulnerabilities in a public issue; see `SECURITY.md`.

## Code of conduct

By participating you agree to abide by the [Code of Conduct](CODE_OF_CONDUCT.md).

## Licence

Contributions are accepted under the [MIT Licence](LICENSE), the same terms as the project.
