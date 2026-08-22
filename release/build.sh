#!/usr/bin/env bash
# build.sh — assemble a release bundle for JellyFreedom (the parts we own: the Go
# orchestrator, its web assets, the VPN-netns plumbing and the privileged helper).
# The off-the-shelf stack (Jellyfin, Prowlarr, TorrServer, FlareSolverr) is NOT bundled —
# install.sh provisions those.
#
# Usage:
#   ./release/build.sh [version]              build for the host architecture
#   JF_VERSION=0.3.0 ./release/build.sh       version via env (takes precedence over $1)
#   GOARCH=arm64 ./release/build.sh 0.3.0     cross-compile
#
# Output (exactly one tarball per invocation — CI depends on this):
#   dist/jellyfreedom-<version>-linux-<goarch>/
#   dist/jellyfreedom-<version>-linux-<goarch>.tar.gz
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"

# Version precedence: JF_VERSION env -> $1 -> the VERSION file. NEVER a date: a
# date-stamped bundle whose binary reports a different version is how the version
# drifted before.
VERSION="${JF_VERSION:-${1:-}}"
if [ -z "$VERSION" ]; then
  [ -f VERSION ] || { echo "no version given and no VERSION file" >&2; exit 1; }
  VERSION="$(tr -d '[:space:]' < VERSION)"
fi
[ -n "$VERSION" ] || { echo "empty version" >&2; exit 1; }

GOOS="${GOOS:-linux}"
GOARCH="${GOARCH:-$(go env GOARCH)}"
BUNDLE="jellyfreedom-$VERSION-$GOOS-$GOARCH"
OUT="$REPO/dist/$BUNDLE"

# Clean only THIS invocation's outputs. Never wipe dist/ wholesale: it may hold previously
# published artifacts. CI builds one arch per job on a clean runner, so its "exactly one
# tarball" rule still holds.
rm -rf "$OUT" "$OUT.tar.gz" "$OUT.tar.gz.sha256"
mkdir -p "$OUT"

echo "==> building orchestrator $VERSION ($GOOS/$GOARCH, static)"
CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
  go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
  -o "$OUT/bin/orchestrator" ./cmd/orchestrator

# Fail loudly rather than shipping a bundle for the wrong architecture: an arm64 user
# receiving an x86-64 binary gets a service that will not exec, with no explanation.
if command -v file >/dev/null; then
  case "$GOARCH:$(file -b "$OUT/bin/orchestrator")" in
    amd64:*x86-64*|arm64:*aarch64*|arm:*ARM*) ;;
    *) echo "built binary does not match GOARCH=$GOARCH:" >&2
       file -b "$OUT/bin/orchestrator" >&2; exit 1 ;;
  esac
fi

echo "==> staging assets"
mkdir -p "$OUT/web" "$OUT/vpntorrent"
# Copy the whole web tree: it is split into shared/ + per-page assets, so an explicit
# file list would silently drop new files.
cp -r web/. "$OUT/web/"
cp vpntorrent/setup-netns.sh vpntorrent/watchdog.sh vpntorrent/portforward.sh "$OUT/vpntorrent/"
chmod +x "$OUT/vpntorrent/"*.sh
# NOTE on `set -e`: bash exempts a non-final command in an `&&` list, so `[ -f x ] && cp`
# does NOT abort mid-script when the test fails — but as the LAST line of a script it makes
# the script exit non-zero. Use if-blocks for optional files: the intent is clearer and the
# exit status is never accidental.
if [ -f vpntorrent/jf-netns-helper ]; then
  install -m 755 vpntorrent/jf-netns-helper "$OUT/vpntorrent/jf-netns-helper"
else
  echo "FATAL: vpntorrent/jf-netns-helper is missing — the privileged helper is required." >&2
  exit 1
fi
if [ -d vpntorrent/apparmor ]; then
  cp -r vpntorrent/apparmor "$OUT/vpntorrent/"
fi

echo "==> staging installer + sample config + control CLI"
cp release/install.sh release/uninstall.sh release/jellyfreedom release/doctor.sh release/jf-update "$OUT/"
chmod +x "$OUT/install.sh" "$OUT/uninstall.sh" "$OUT/jellyfreedom" "$OUT/doctor.sh" "$OUT/jf-update"
cp release/config.sample.yaml "$OUT/config.sample.yaml"
printf '%s\n' "$VERSION" > "$OUT/VERSION"

echo "==> tarball"
( cd "$REPO/dist" && tar czf "$BUNDLE.tar.gz" "$BUNDLE" )
( cd "$REPO/dist" && sha256sum "$BUNDLE.tar.gz" > "$BUNDLE.tar.gz.sha256" )

echo "==> done: dist/$BUNDLE.tar.gz"
echo "    sha256: $(cut -d' ' -f1 < "$REPO/dist/$BUNDLE.tar.gz.sha256")"
echo "    install on a target with:  sudo ./$BUNDLE/install.sh"
