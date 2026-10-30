package update

import (
	"strings"
)

// ── Install verdicts ──────────────────────────────────────────────────────────
//
// `jellyfreedom --update` used to compare the installed and downloaded versions for
// EQUALITY only, so anything different was installed — including an older release. That
// is how a box running an unreleased 0.8.0 would have been "updated" to the published
// 0.7.6, which does not work with Jellyfin 12. The CLI now asks the orchestrator binary
// for a verdict, so the shell and the dashboard share this one comparison.
//
// Nightlies need one rule semver does not have. They are versioned
// <released version>+nightly.<YYYYMMDD>.<sha>, and semver ignores build metadata, so by
// precedence every nightly equals the release it was built after. They are not equal: a
// nightly carries what landed on main since, so it ranks above that release, and nightlies
// of the same release rank by their date.

// Verdict is what installing a candidate version over the current one would be.
type Verdict string

const (
	VerdictUpgrade   Verdict = "upgrade"
	VerdictSame      Verdict = "same"
	VerdictDowngrade Verdict = "downgrade"
	// VerdictUnknown means the two cannot be ordered. Callers must treat it like a
	// downgrade: installing as root on a guess is the failure this exists to prevent.
	VerdictUnknown Verdict = "unknown"
)

// InstallVerdict decides whether installing candidate over current moves forward.
func InstallVerdict(current, candidate string) Verdict {
	cur, cand := normalize(current), normalize(candidate)
	if cur == cand && cur != "" {
		return VerdictSame
	}
	c, err := CompareVersions(cand, cur)
	if err != nil {
		return VerdictUnknown
	}
	switch {
	case c > 0:
		return VerdictUpgrade
	case c < 0:
		return VerdictDowngrade
	}

	// Equal precedence, different strings: only the build metadata differs.
	curDay, curNightly := nightlyDay(cur)
	candDay, candNightly := nightlyDay(cand)
	switch {
	case candNightly && !curNightly && buildMeta(cur) == "":
		return VerdictUpgrade // a nightly built after the installed release
	case curNightly && !candNightly && buildMeta(cand) == "":
		return VerdictDowngrade // back to the release this nightly was built after
	case curNightly && candNightly:
		switch {
		case candDay > curDay:
			return VerdictUpgrade
		case candDay < curDay:
			return VerdictDowngrade
		}
		// Same day, different commit. The CLI only ever fetches the newest nightly, so
		// a different one from the same day is the later build.
		return VerdictUpgrade
	}
	return VerdictUnknown
}

// buildMeta returns what follows "+", or "".
func buildMeta(v string) string {
	if i := strings.IndexByte(v, '+'); i >= 0 {
		return v[i+1:]
	}
	return ""
}

// nightlyDay extracts the YYYYMMDD of a nightly.<YYYYMMDD>.<sha> build. Eight digits
// compare correctly as strings, so the day is returned as one.
func nightlyDay(v string) (string, bool) {
	parts := strings.Split(buildMeta(v), ".")
	if len(parts) < 2 || parts[0] != "nightly" || len(parts[1]) != 8 {
		return "", false
	}
	for _, r := range parts[1] {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return parts[1], true
}
