## What this changes

<!-- One paragraph. What problem does it fix, and how? -->

Fixes #

## Type of change

- [ ] Bug fix
- [ ] New feature
- [ ] Installer / packaging
- [ ] VPN / networking / security
- [ ] Documentation only
- [ ] Refactor or chore

## How it was tested

<!-- Be specific. "It builds" is not a test. -->

- [ ] `go build ./...`, `go vet ./...`, `go test -race ./...` pass
- [ ] `gofmt -l ./cmd ./internal` prints nothing
- [ ] `shellcheck --severity=warning` is clean on any script I touched
- [ ] Both cross-compiles succeed (`GOARCH=amd64` and `GOARCH=arm64`)

**If this touches `release/*.sh` or `vpntorrent/*`, how did you test the installer?**

- [ ] Hermetic harness (`tests/install/`)
- [ ] A throwaway VM I destroyed afterwards
- [ ] CI's `installer-smoke` job
- [ ] Not applicable

<!-- Never test the installer on a live JellyFreedom box. -->

## Checklist

- [ ] I have read [CONTRIBUTING.md](../blob/main/CONTRIBUTING.md) and this change does not
      reverse a decision recorded in `docs/dev/decisions.md` (no Docker, no paid debrid, Go,
      Debian/Ubuntu, orchestrator-owned availability).
- [ ] Docs updated in this same change (`README.md`, `docs/install.md`,
      `docs/configuration.md`, `docs/troubleshooting.md`, `docs/dev/architecture.md`,
      `docs/dev/operations.md` — whichever describe the behaviour I changed).
- [ ] `CHANGELOG.md` has an entry under `## [Unreleased]`.
- [ ] If I changed routes or authentication, I re-checked `SECURITY.md` against reality.
- [ ] No secrets, API keys, WireGuard configs, or personal paths are in the diff.
- [ ] Commits follow the existing style (imperative subject, `-` bullet body, no
      tooling attribution).

## Notes for the reviewer

<!-- Anything surprising, deliberately deferred, or worth arguing about. -->
