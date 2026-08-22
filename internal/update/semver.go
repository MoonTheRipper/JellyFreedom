package update

import (
	"errors"
	"strconv"
	"strings"
)

// ── Semantic version comparison ───────────────────────────────────────────────
//
// String equality is not enough: "0.10.0" != "0.9.0" tells you they differ, not which
// is newer, and a lexical compare gets it BACKWARDS ("0.10.0" < "0.9.0" as strings).
// The update check must only ever offer a strictly newer release, so it needs a real
// numeric comparison with semver's pre-release rule (0.4.0-rc1 is OLDER than 0.4.0).

// ErrBadVersion means the string is not a version we are willing to reason about.
// Callers treat that as "no update available" — never as "an update exists".
var ErrBadVersion = errors.New("not a parseable version")

// Semver is a parsed version. Build metadata (+sha) is discarded: semver says it takes
// no part in precedence.
type Semver struct {
	Major, Minor, Patch int
	Pre                 []string // dot-separated pre-release identifiers, empty for a final release
}

// devVersion is what an un-stamped source build reports (see cmd/orchestrator/version.go).
const devVersion = "dev"

// IsDev reports whether a version string is a local source build.
//
// A developer running `go build` without -ldflags must NEVER be offered a release that
// would overwrite their own binary, so this case short-circuits the whole comparison.
// The prefix form also covers stamped dev builds like "dev-a1b2c3".
func IsDev(v string) bool {
	return strings.HasPrefix(strings.ToLower(normalize(v)), devVersion)
}

// normalize strips the noise GitHub tags carry: surrounding whitespace and a leading
// "v" ("v0.4.0" and "0.4.0" are the same release).
func normalize(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	return strings.TrimSpace(v)
}

// ParseVersion parses "v1.2.3-rc1+build" into a Semver.
//
// One or two numeric components are accepted and zero-filled ("1.2" == "1.2.0"),
// because release tags in the wild are not disciplined. Anything else — empty, "dev",
// letters in a numeric slot, negatives — is ErrBadVersion rather than a guess.
func ParseVersion(v string) (Semver, error) {
	s := normalize(v)
	if s == "" {
		return Semver{}, ErrBadVersion
	}
	// Build metadata is ignored for precedence.
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre, s = s[i+1:], s[:i]
		if strings.TrimSpace(pre) == "" {
			// "1.2.3-" is a trailing hyphen with no identifier: malformed, not "final".
			return Semver{}, ErrBadVersion
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return Semver{}, ErrBadVersion
	}
	nums := make([]int, 3)
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return Semver{}, ErrBadVersion
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Semver{}, ErrBadVersion
		}
		nums[i] = n
	}
	out := Semver{Major: nums[0], Minor: nums[1], Patch: nums[2]}
	if pre != "" {
		for _, id := range strings.Split(pre, ".") {
			if strings.TrimSpace(id) == "" {
				return Semver{}, ErrBadVersion
			}
			out.Pre = append(out.Pre, id)
		}
	}
	return out, nil
}

// CompareVersions returns -1 if a < b, 0 if equal, +1 if a > b.
// Unparseable input is reported through the error; the int is then meaningless.
func CompareVersions(a, b string) (int, error) {
	va, err := ParseVersion(a)
	if err != nil {
		return 0, err
	}
	vb, err := ParseVersion(b)
	if err != nil {
		return 0, err
	}
	return va.Compare(vb), nil
}

// Compare implements semver precedence between two parsed versions.
func (a Semver) Compare(b Semver) int {
	if c := cmpInt(a.Major, b.Major); c != 0 {
		return c
	}
	if c := cmpInt(a.Minor, b.Minor); c != 0 {
		return c
	}
	if c := cmpInt(a.Patch, b.Patch); c != 0 {
		return c
	}
	return cmpPre(a.Pre, b.Pre)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// cmpPre implements the pre-release rule. A version WITH a pre-release is lower than
// the same version without one, so 0.4.0-rc1 < 0.4.0 and an rc is never offered as an
// upgrade over the final release.
func cmpPre(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1 // a is final, b is a pre-release
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := cmpPreID(a[i], b[i]); c != 0 {
			return c
		}
	}
	// A longer set of identifiers wins when all the preceding ones are equal.
	return cmpInt(len(a), len(b))
}

// cmpPreID compares one pre-release identifier: numeric ones compare numerically and
// always rank below alphanumeric ones.
func cmpPreID(a, b string) int {
	na, errA := strconv.Atoi(a)
	nb, errB := strconv.Atoi(b)
	aNum, bNum := errA == nil, errB == nil
	switch {
	case aNum && bNum:
		return cmpInt(na, nb)
	case aNum:
		return -1
	case bNum:
		return 1
	}
	return strings.Compare(a, b)
}

// IsNewer reports whether latest is strictly newer than current — the single question
// the whole update check turns on.
//
// It FAILS CLOSED in every ambiguous case: a dev build, an empty string, a malformed
// tag, or an equal version all return false. Offering a bogus update is worse than
// missing a real one, because accepting it re-installs as root.
func IsNewer(current, latest string) bool {
	if IsDev(current) {
		return false
	}
	c, err := CompareVersions(latest, current)
	if err != nil {
		return false
	}
	return c > 0
}
