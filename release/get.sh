#!/usr/bin/env bash
# get.sh — one-line bootstrap. Fetches the JellyFreedom release bundle for THIS machine's
# architecture, verifies its checksum, and runs the installer:
#
#     curl -fsSL https://github.com/MoonTheRipper/JellyFreedom/releases/latest/download/get.sh | sudo bash
#
# Overrides (testing / forks):
#     JELLYFREEDOM_URL=...        exact bundle URL; skips arch selection
#     JELLYFREEDOM_BASE=...       release base URL (default: this repo's latest release)
#     JELLYFREEDOM_SKIP_VERIFY=1  proceed even if checksums are unavailable
set -euo pipefail

BASE="${JELLYFREEDOM_BASE:-https://github.com/MoonTheRipper/JellyFreedom/releases/latest/download}"

die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }
say() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }

[ "$(id -u)" -eq 0 ] || die "run with sudo:  curl -fsSL <url>/get.sh | sudo bash"
command -v curl >/dev/null || die "curl is required"
command -v tar  >/dev/null || die "tar is required"

# Pick the bundle for this architecture. Shipping an x86-64 binary to an arm64 box produces
# a service that fails to exec with no explanation — refuse up front instead.
arch="$(uname -m)"
case "$arch" in
    x86_64|amd64)  goarch=amd64 ;;
    aarch64|arm64) goarch=arm64 ;;
    *) die "unsupported architecture '$arch'. JellyFreedom publishes amd64 and arm64 builds.
       To run on this machine you would need to build from source: https://github.com/MoonTheRipper/JellyFreedom" ;;
esac

ASSET="jellyfreedom-linux-${goarch}.tar.gz"
URL="${JELLYFREEDOM_URL:-$BASE/$ASSET}"

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

say "downloading $URL"
curl -4 -fsSL --retry 3 --retry-delay 2 "$URL" -o "$tmp/jf.tar.gz" \
    || die "download failed. Check network access to github.com, or pass JELLYFREEDOM_URL=<url>."

# Verify against the release's SHA256SUMS. This is a pipe-to-root-shell install; the user
# deserves to know the bytes are the published ones.
if [ -n "${JELLYFREEDOM_URL:-}" ]; then
    say "custom URL supplied — skipping checksum verification"
elif curl -4 -fsSL --retry 2 "$BASE/SHA256SUMS" -o "$tmp/SHA256SUMS" 2>/dev/null; then
    expected="$(awk -v a="$ASSET" '$2 == a || $2 == "*"a {print $1}' "$tmp/SHA256SUMS" | head -1)"
    if [ -z "$expected" ]; then
        die "SHA256SUMS has no entry for $ASSET — refusing to install an unverifiable bundle.
       Override with JELLYFREEDOM_SKIP_VERIFY=1 if you understand the risk."
    fi
    actual="$(sha256sum "$tmp/jf.tar.gz" | cut -d' ' -f1)"
    [ "$expected" = "$actual" ] || die "CHECKSUM MISMATCH for $ASSET
       expected: $expected
       actual:   $actual
       Do not install this. Re-run to retry, or report it."
    say "checksum verified"
elif [ "${JELLYFREEDOM_SKIP_VERIFY:-0}" = "1" ]; then
    say "SHA256SUMS unavailable — continuing because JELLYFREEDOM_SKIP_VERIFY=1"
else
    die "could not fetch $BASE/SHA256SUMS to verify the download.
       Re-run with JELLYFREEDOM_SKIP_VERIFY=1 to install without verification."
fi

say "extracting"
tar xzf "$tmp/jf.tar.gz" -C "$tmp" || die "the downloaded bundle is not a valid tar.gz"
dir="$(find "$tmp" -maxdepth 1 -type d -name 'jellyfreedom-*' | head -1)"
[ -n "$dir" ] && [ -x "$dir/install.sh" ] || die "unexpected bundle layout — no jellyfreedom-*/install.sh inside"

# This temp dir is deleted on exit, so nothing here survives. The installer is responsible
# for persisting uninstall.sh and the control CLI into /opt — otherwise a one-liner user is
# left with no way to remove what they just installed.
say "running installer from $(basename "$dir")"
bash "$dir/install.sh"
