# 05 — Porting plan

Phases in dependency order. Each has a definition of done that is a *test*, not a feeling.
One phase at a time, one branch each, PR + green CI before the next.

## Phase 0 — Prove the Go code builds and its tests pass on Windows

Before designing anything, find out how much is actually broken.

```powershell
$env:GOOS="windows"; go build ./...
go test ./...
```

Expect failures confined to: `internal/netnsproxy`, `cmd/orchestrator/netnsproxy.go`,
`selfrestart.go`, `internal/api/privileged.go`, `internal/update/apply.go`, and the
`journalctl` call in `internal/api/dashboard.go`.

Isolate them behind `//go:build linux` / `//go:build windows` files rather than
`runtime.GOOS` branches — the build tag makes it impossible to accidentally ship a Linux
path in a Windows binary.

**Done when:** `GOOS=windows go build ./...` succeeds and every test that is not
network-namespace-specific passes on Windows.

## Phase 1 — The VPN and the kill switch

[04](04-the-vpn-problem.md), all of it. Do this second because everything else is pointless
without it, and first-after-Phase-0 because it may change the architecture.

**Done when:** all four conditions in
[04 § definition of done](04-the-vpn-problem.md#definition-of-done-for-this-component) hold.

## Phase 2 — Services, paths and permissions

- Orchestrator runs as a Windows Service (`golang.org/x/sys/windows/svc`), not a console app.
- Paths per [03 § paths](03-components.md#paths), resolved in one place.
- **ACLs**: data directory and database restricted to the service account and Administrators.
  The Linux `0700`/`0600`/`UMask=0077` protections are load-bearing (see
  [07](07-security.md)) and `os.Chmod` does nothing useful on Windows.
- A privileged helper with a **closed verb set** for anything needing elevation. Do not give
  the service account a general "run this command elevated" capability; that was explicitly
  designed against on Linux.

**Done when:** the service survives a reboot, an unprivileged local user cannot read the
database, and the helper refuses a verb that is not on its list.

## Phase 3 — Install, update, uninstall

- Installer provisions Jellyfin, TorrServer, Prowlarr, FlareSolverr, yt-dlp — or detects
  existing installs and leaves them alone. The Linux installer is aggressively
  non-destructive and re-runnable; keep that.
- **It must harden Prowlarr** (see [07](07-security.md)) rather than document that the user
  should.
- Self-update, matching `jellyfreedom --update`: download, verify SHA256, replace, restart.
- Uninstall must remove what it created, *including* third-party state holding secrets.

**Done when:** install → update → uninstall on a clean VM leaves no secrets behind, and a
re-run of the installer over a working system changes nothing it should not.

## Phase 4 — Feature parity

Everything else already works cross-platform in principle. Verify each end to end:
library writing, Resolve-at-Play, capability tokens, paste-a-link web sources, per-user
library visibility, the dashboard, the update channel split.

**Done when:** a `.strm` written on Windows plays in Jellyfin on an Apple TV, and the
security checks in [07](07-security.md) pass on Windows.

## Phase 5 — Release plumbing

Windows artefacts in the release workflow, the stable/nightly channel split, and an
equivalent of the hermetic installer harness. See [08](08-build-test-release.md).

## What to do first, concretely

Phase 0. It is an afternoon, it is pure information, and it tells you whether the port is a
week or a month before you have committed to any design.
