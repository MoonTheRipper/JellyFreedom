package update

import "testing"

func TestInstallVerdict(t *testing.T) {
	cases := []struct {
		current, candidate string
		want               Verdict
	}{
		{"0.7.6", "0.8.0", VerdictUpgrade},
		{"0.9.0", "0.10.0", VerdictUpgrade}, // the case a string compare gets backwards
		{"0.8.0", "0.8.0", VerdictSame},
		{"0.8.0", "v0.8.0\n", VerdictSame},

		// The case that motivated this: an unreleased local 0.8.0 must not be "updated"
		// to the published 0.7.6.
		{"0.8.0", "0.7.6", VerdictDowngrade},
		{"0.10.0", "0.9.0", VerdictDowngrade},
		{"0.8.0", "0.8.0-rc1", VerdictDowngrade},
		{"0.8.0-rc1", "0.8.0", VerdictUpgrade},

		// Nightlies rank above the release they were built after, and by day.
		{"0.7.6", "0.7.6+nightly.20261031.abc1234", VerdictUpgrade},
		{"0.7.6+nightly.20261031.abc1234", "0.7.6", VerdictDowngrade},
		{"0.7.6+nightly.20261030.aaaaaaa", "0.7.6+nightly.20261031.bbbbbbb", VerdictUpgrade},
		{"0.7.6+nightly.20261031.bbbbbbb", "0.7.6+nightly.20261030.aaaaaaa", VerdictDowngrade},
		{"0.7.6+nightly.20261031.aaaaaaa", "0.7.6+nightly.20261031.bbbbbbb", VerdictUpgrade},
		{"0.7.6+nightly.20261031.aaaaaaa", "0.7.6+nightly.20261031.aaaaaaa", VerdictSame},
		// A release is newer than every nightly of the release before it.
		{"0.7.6+nightly.20261031.abc1234", "0.8.0", VerdictUpgrade},
		{"0.8.0", "0.7.6+nightly.20261031.abc1234", VerdictDowngrade},

		// Anything that cannot be ordered must not be installed on a guess.
		{"0.8.0", "", VerdictUnknown},
		{"", "0.8.0", VerdictUnknown},
		{"?", "0.8.0", VerdictUnknown},
		{"0.8.0", "garbage", VerdictUnknown},
		{"dev", "0.8.0", VerdictUnknown},
		{"0.8.0+local", "0.8.0", VerdictUnknown},
		{"0.8.0+nightly.2026103.abc", "0.8.0", VerdictUnknown}, // a 7-digit day is not a day
	}
	for _, c := range cases {
		if got := InstallVerdict(c.current, c.candidate); got != c.want {
			t.Errorf("InstallVerdict(%q, %q) = %s, want %s", c.current, c.candidate, got, c.want)
		}
	}
}
