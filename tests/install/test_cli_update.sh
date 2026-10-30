#!/usr/bin/env bash
# test_cli_update.sh — `jellyfreedom --update` must never install an older release.
#
# It used to compare versions for equality only, so any bundle that differed was installed,
# including an older one. That would have taken a box running an unreleased 0.8.0 back to
# the published 0.7.6, which breaks on Jellyfin 12. The CLI now asks the installed
# orchestrator for a verdict; this checks that it acts on each verdict correctly. The
# ordering itself is tested in Go (internal/update/verdict_test.go).
#
# Runs the real release/jellyfreedom against a fake instance. No root, no network: id, curl
# and systemctl are stubbed, and the bundle's installer only records that it ran.
set -uo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); printf '  \033[1;32m✓\033[0m %s\n' "$1"; }
no(){ FAIL=$((FAIL+1)); printf '  \033[1;31m✗\033[0m %s\n' "$1"; [ -n "${2:-}" ] && printf '      %s\n' "$2"; }

TD="$(mktemp -d)"; trap 'rm -rf "$TD"' EXIT

# A bundle as build.sh lays it out; its installer leaves a marker instead of installing.
mkdir -p "$TD/build/jellyfreedom-NEW"
cat > "$TD/build/jellyfreedom-NEW/install.sh" <<EOF
#!/usr/bin/env bash
touch "$TD/installed"
EOF
chmod +x "$TD/build/jellyfreedom-NEW/install.sh"

mkdir -p "$TD/mock"
cat > "$TD/mock/id" <<'EOF'
#!/usr/bin/env bash
echo 0
EOF
cat > "$TD/mock/systemctl" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
# curl serves the bundle for any tarball URL and a SHA256SUMS that matches it.
cat > "$TD/mock/curl" <<EOF
#!/usr/bin/env bash
out=""; url=""; prev=""
for a in "\$@"; do [ "\$prev" = "-o" ] && out="\$a"; case "\$a" in http*) url="\$a";; esac; prev="\$a"; done
case "\$url" in
  */SHA256SUMS)
    sum="\$(sha256sum "$TD/bundle.tar.gz" | cut -d' ' -f1)"
    printf '%s  jellyfreedom-linux-amd64.tar.gz\n%s  jellyfreedom-linux-arm64.tar.gz\n' "\$sum" "\$sum" > "\$out" ;;
  *) cp "$TD/bundle.tar.gz" "\$out" ;;
esac
EOF
chmod +x "$TD/mock/"*

# update_case NAME CURRENT NEW VERDICT [ENV...] — run --update and capture the outcome.
# The stub orchestrator records its arguments and answers with VERDICT.
update_case() {
  local cur="$2" new="$3" verdict="$4"; shift 4
  rm -rf "$TD/app" "$TD/installed" "$TD/verdict-args"
  mkdir -p "$TD/app/bin"
  [ "$cur" != "-" ] && printf '%s\n' "$cur" > "$TD/app/VERSION"
  cat > "$TD/app/bin/orchestrator" <<EOF
#!/usr/bin/env bash
[ "\$1" = update-verdict ] || exit 2
printf '%s\n' "\$*" > "$TD/verdict-args"
echo "$verdict"
EOF
  chmod +x "$TD/app/bin/orchestrator"
  printf '%s\n' "$new" > "$TD/build/jellyfreedom-NEW/VERSION"
  tar czf "$TD/bundle.tar.gz" -C "$TD/build" jellyfreedom-NEW
  OUT="$(env PATH="$TD/mock:$PATH" JELLYFREEDOM_APP_DIR="$TD/app" JELLYFREEDOM_CHANNEL=stable \
         "$@" bash "$REPO/release/jellyfreedom" --update 2>&1)"
  RC=$?
}
installed(){ [ -f "$TD/installed" ]; }

printf '\033[1;33m▸ an upgrade installs\033[0m\n'
update_case upgrade 0.7.6 0.8.0 upgrade
[ "$RC" = 0 ] && installed && ok "0.7.6 -> 0.8.0 installed" || no "upgrade installs" "rc=$RC: $OUT"
[ "$(cat "$TD/verdict-args" 2>/dev/null)" = "update-verdict 0.7.6 0.8.0" ] \
  && ok "asks the orchestrator with the installed version first" \
  || no "verdict arguments" "got: $(cat "$TD/verdict-args" 2>/dev/null)"

printf '\033[1;33m▸ a downgrade is refused\033[0m\n'
update_case downgrade 0.8.0 0.7.6 downgrade
[ "$RC" != 0 ] && ! installed && ok "0.8.0 -> 0.7.6 refused, nothing installed" || no "downgrade refused" "rc=$RC: $OUT"
printf '%s' "$OUT" | grep -q 'OLDER than the installed 0.8.0' && ok "says why" || no "says why" "$OUT"
update_case force-downgrade 0.8.0 0.7.6 downgrade JELLYFREEDOM_FORCE=1
! installed && ok "JELLYFREEDOM_FORCE does not allow a downgrade (repair sets it)" || no "FORCE allowed a downgrade"
update_case allowed-downgrade 0.8.0 0.7.6 downgrade JELLYFREEDOM_ALLOW_DOWNGRADE=1
[ "$RC" = 0 ] && installed && ok "JELLYFREEDOM_ALLOW_DOWNGRADE=1 installs it" || no "explicit downgrade" "rc=$RC: $OUT"

printf '\033[1;33m▸ an unorderable version is refused\033[0m\n'
update_case unknown 0.8.0 garbage unknown
[ "$RC" != 0 ] && ! installed && ok "unknown verdict refused" || no "unknown refused" "rc=$RC: $OUT"
# A verdict that never arrives (a broken or foreign binary) must fail closed too.
update_case empty 0.8.0 0.9.0 ""
[ "$RC" != 0 ] && ! installed && ok "no verdict at all is refused" || no "empty verdict refused" "rc=$RC: $OUT"

printf '\033[1;33m▸ the same version\033[0m\n'
update_case same 0.8.0 0.8.0 same
[ "$RC" = 0 ] && ! installed && ok "same version is a no-op" || no "same is a no-op" "rc=$RC: $OUT"
update_case same-forced 0.8.0 0.8.0 same JELLYFREEDOM_FORCE=1
[ "$RC" = 0 ] && installed && ok "JELLYFREEDOM_FORCE=1 reinstalls it" || no "FORCE reinstalls" "rc=$RC: $OUT"

printf '\033[1;33m▸ no recorded version\033[0m\n'
update_case no-version - 0.8.0 downgrade
[ "$RC" = 0 ] && installed && ok "proceeds without asking, so repair can recover it" || no "missing VERSION proceeds" "rc=$RC: $OUT"

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
