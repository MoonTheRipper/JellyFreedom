#!/usr/bin/env bash
# build.sh — assemble a release bundle for JellyFreedom (the parts we own: the Go orchestrator,
# its web assets, and the VPN-netns scripts). The off-the-shelf media stack (Jellyfin, Prowlarr,
# TorrServer, FlareSolverr) is NOT bundled — install.sh treats those as prerequisites.
#
# Output: dist/jellyfreedom-<version>/  and  dist/jellyfreedom-<version>.tar.gz
# Usage:  ./release/build.sh [version]
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="${1:-$(date +%Y.%m.%d)}"
OUT="$REPO/dist/jellyfreedom-$VERSION"

echo "==> building orchestrator (static binary)"
cd "$REPO"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$OUT/bin/orchestrator" ./cmd/orchestrator

echo "==> staging assets"
mkdir -p "$OUT/web" "$OUT/vpntorrent"
cp -r web/public web/dashboard "$OUT/web/"
cp vpntorrent/setup-netns.sh vpntorrent/watchdog.sh vpntorrent/portforward.sh "$OUT/vpntorrent/"
cp -r vpntorrent/apparmor "$OUT/vpntorrent/" 2>/dev/null || true
chmod +x "$OUT/vpntorrent/"*.sh

echo "==> staging installer + sample config + control CLI"
cp release/install.sh release/uninstall.sh "$OUT/"
chmod +x "$OUT/install.sh" "$OUT/uninstall.sh"
cp release/jellyfreedom "$OUT/jellyfreedom"
chmod +x "$OUT/jellyfreedom"
cp release/config.sample.yaml "$OUT/config.sample.yaml"
echo "$VERSION" > "$OUT/VERSION"

echo "==> tarball"
( cd "$REPO/dist" && tar czf "jellyfreedom-$VERSION.tar.gz" "jellyfreedom-$VERSION" )

echo "==> done: dist/jellyfreedom-$VERSION.tar.gz"
echo "    install on a target with:  sudo ./jellyfreedom-$VERSION/install.sh"
