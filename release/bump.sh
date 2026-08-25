#!/usr/bin/env bash
# bump.sh — prepare a release: move the version, close off the changelog, open the PR.
#
# The release workflow refuses to build if the tag does not match ./VERSION, and refuses
# again if CHANGELOG.md has no section for that version. Both of those are easy to get
# wrong by hand at the exact moment you are least interested in being careful, so this
# does them together or not at all.
#
#   ./release/bump.sh patch     0.5.1 -> 0.5.2    a fix
#   ./release/bump.sh minor     0.5.1 -> 0.6.0    a feature
#   ./release/bump.sh major     0.5.1 -> 1.0.0    a break
#   ./release/bump.sh 1.2.3                       an exact version
#
# It stops before tagging. Tagging is what publishes, and that should follow a green CI
# run on the merged commit, not a local guess — see the closing instructions it prints.
set -euo pipefail

die() { printf 'bump: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "${BASH_SOURCE[0]}")/.."
[ -f VERSION ] || die "no ./VERSION here — run this from inside the repository"
[ -f CHANGELOG.md ] || die "no ./CHANGELOG.md here"

[ $# -eq 1 ] || die "usage: release/bump.sh <patch|minor|major|X.Y.Z>"

# A dirty tree means the commit below would sweep up work you did not mean to release.
[ -z "$(git status --porcelain)" ] || die "working tree is dirty — commit or stash first"

current="$(tr -d '[:space:]' < VERSION)"
[[ "$current" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]] || die "./VERSION is '$current', which is not X.Y.Z"
maj="${BASH_REMATCH[1]}"; min="${BASH_REMATCH[2]}"; pat="${BASH_REMATCH[3]}"

case "$1" in
  patch) next="$maj.$min.$((pat + 1))" ;;
  minor) next="$maj.$((min + 1)).0" ;;
  major) next="$((maj + 1)).0.0" ;;
  *)
    [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "'$1' is not patch, minor, major, or an X.Y.Z version"
    next="$1"
    ;;
esac

[ "$next" != "$current" ] || die "already at $current"
git rev-parse -q --verify "refs/tags/v$next" >/dev/null && die "tag v$next already exists"

# Everything under ## [Unreleased] becomes the new section. Refuse when it is empty:
# a release whose notes say nothing is worse than no release, and the workflow would
# publish those empty notes to everyone who watches the repository.
notes="$(awk '
  /^## \[Unreleased\]/ { grab = 1; next }
  grab && /^## \[/     { exit }
  grab                  { print }
' CHANGELOG.md)"
[ -n "$(printf '%s' "$notes" | tr -d '[:space:]')" ] \
  || die "## [Unreleased] in CHANGELOG.md is empty — write the notes before bumping"

printf '%s\n' "$next" > VERSION

today="$(date +%Y-%m-%d)"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

# Rename [Unreleased] to the new version and open a fresh empty [Unreleased] above it.
awk -v v="$next" -v d="$today" '
  /^## \[Unreleased\]/ && !done {
    print "## [Unreleased]"; print ""; print "## [" v "] - " d; done = 1; next
  }
  { print }
' CHANGELOG.md > "$tmp" && mv "$tmp" CHANGELOG.md
tmp="$(mktemp)"

# Repoint the Unreleased compare link and add one for the version being cut.
awk -v v="$next" -v p="$current" -v repo="MoonTheRipper/JellyFreedom" '
  /^\[Unreleased\]:/ {
    print "[Unreleased]: https://github.com/" repo "/compare/v" v "...HEAD"
    print "[" v "]: https://github.com/" repo "/compare/v" p "...v" v
    next
  }
  { print }
' CHANGELOG.md > "$tmp" && mv "$tmp" CHANGELOG.md

# Exactly the extraction the release workflow performs. If it comes back empty here it
# would fail the build there, after the tag is already pushed and awkward to take back.
check="$(awk -v v="$next" '
  $0 ~ "^## \\[" v "\\]" { grab = 1; next }
  grab && /^## \[/       { exit }
  grab && /^\[/          { exit }
  grab                    { print }
' CHANGELOG.md)"
[ -n "$(printf '%s' "$check" | tr -d '[:space:]')" ] \
  || die "the workflow's own note extraction found nothing for $next — CHANGELOG.md layout changed?"

branch="release/$next"
git checkout -q -b "$branch"
git add VERSION CHANGELOG.md
git commit -q -m "Release $next"

cat <<EOF

  $current -> $next   on branch $branch

  Release notes the workflow will publish:
$(printf '%s\n' "$check" | sed 's/^/    /')

  Next:
    git push -u origin $branch
    gh pr create --fill
    # let CI finish, then:
    gh pr merge --squash --delete-branch
    git checkout main && git pull
    git tag -a v$next -m "JellyFreedom $next"
    git push origin v$next        # this is what builds and drafts the release

EOF
