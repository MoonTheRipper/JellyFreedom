#!/usr/bin/env bash
# get.sh — one-line bootstrap. Fetches the latest JellyFreedom release bundle and runs its
# installer:
#     curl -fsSL https://<host>/get.sh | sudo bash
#
# Override the bundle URL for testing/forks:  JELLYFREEDOM_URL=... curl ... | sudo bash
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "Run with sudo:  curl -fsSL <url>/get.sh | sudo bash"; exit 1; }

# TODO: set to the hosted release tarball once releases are published.
URL="${JELLYFREEDOM_URL:-https://github.com/MoonTheRipper/JellyFreedom/releases/latest/download/jellyfreedom.tar.gz}"

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
echo "==> downloading $URL"
curl -4 -fsSL "$URL" -o "$tmp/jf.tar.gz"
tar xzf "$tmp/jf.tar.gz" -C "$tmp"
dir="$(find "$tmp" -maxdepth 1 -type d -name 'jellyfreedom-*' | head -1)"
[ -n "$dir" ] && [ -x "$dir/install.sh" ] || { echo "unexpected bundle layout"; exit 1; }
echo "==> running installer from $dir"
bash "$dir/install.sh"
