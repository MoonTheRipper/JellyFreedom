package main

import (
	"net/url"
	"strings"
	"testing"
)

// The startup sweep decided "already signed" with strings.Contains(cur, "?t="). The legacy
// route spells its token "&t=", because the URL already has a query (?link=…&index=…). So a
// legacy .strm never looked signed, was re-signed on EVERY boot, and the token was APPENDED
// each time — one "&t=<43 chars>" per restart, for as long as the box kept running.
//
// Observed on a live box: one file with the same token repeated nine times.
func TestPlayTokenIsDetectedAndSetNotAppended(t *testing.T) {
	const legacy = "http://box:1990/proxy/stream?link=2ef50cc10f6eb563314c2db47d6a4e04734312e1&index=0"
	const tok = "UNhCvh-RuisST-JSN50Lq1M6aJwH_KA1o5H6l5iMjCM"

	if hasPlayToken(legacy) {
		t.Error("an unsigned legacy URL was reported as already carrying a token")
	}

	signed := withPlayToken(legacy, tok)
	if !hasPlayToken(signed) {
		t.Fatalf("a freshly signed URL was not detected as signed: %s", signed)
	}
	if got := strings.Count(signed, "t="); got != 1 {
		t.Errorf("expected exactly one t= parameter, got %d: %s", got, signed)
	}

	// The actual regression: re-signing must be idempotent, not additive.
	again := withPlayToken(signed, tok)
	if again != signed {
		t.Errorf("re-signing changed the URL — it will grow on every boot:\n  %s\n  %s", signed, again)
	}

	// And it must REPAIR a file that has already grown.
	grown := legacy
	for i := 0; i < 9; i++ {
		grown += "&t=" + tok
	}
	repaired := withPlayToken(grown, tok)
	if got := strings.Count(repaired, "t="); got != 1 {
		t.Errorf("a grown URL was not collapsed to one token, got %d: %s", got, repaired)
	}

	// Repaired or not, it must still address the same torrent and file.
	u, err := url.Parse(repaired)
	if err != nil {
		t.Fatalf("repaired URL does not parse: %v", err)
	}
	if u.Query().Get("link") != "2ef50cc10f6eb563314c2db47d6a4e04734312e1" ||
		u.Query().Get("index") != "0" || u.Query().Get("t") != tok {
		t.Errorf("repair lost or altered a parameter: %s", repaired)
	}

	// A modern identity URL spells it "?t=" and must still be recognised.
	if !hasPlayToken("http://box:1990/play/movie/550?t=" + tok) {
		t.Error("a normal /play URL was not detected as signed")
	}
	if hasPlayToken("http://box:1990/play/movie/550") {
		t.Error("an unsigned /play URL was reported as signed")
	}
}

// Fixing the append was not enough on its own: once a file carries a token, the sweep skips
// it, so the ones already grown to nine tokens would have stayed that way forever. Verified
// on the live box — after the append fix the file stopped growing and stayed at nine.
func TestDuplicateTokensCollapseToTheFirst(t *testing.T) {
	const base = "http://box:1990/proxy/stream?link=2ef50cc10f6eb563314c2db47d6a4e04734312e1&index=0"
	const tok = "UNhCvh-RuisST-JSN50Lq1M6aJwH_KA1o5H6l5iMjCM"

	grown := base
	for i := 0; i < 9; i++ {
		grown += "&t=" + tok
	}
	if got := countQueryValues(grown, "t"); got != 9 {
		t.Fatalf("fixture is wrong: %d tokens", got)
	}

	fixed := collapseDuplicateTokens(grown)
	if got := countQueryValues(fixed, "t"); got != 1 {
		t.Errorf("collapsed to %d tokens, want 1: %s", got, fixed)
	}
	// The FIRST value is what every handler reads, so keeping it means the file keeps
	// working exactly as before rather than silently changing which token is in force.
	if u, err := url.Parse(fixed); err != nil || u.Query().Get("t") != tok {
		t.Errorf("collapse did not keep the original token: %s", fixed)
	}
	if u, err := url.Parse(fixed); err != nil ||
		u.Query().Get("link") != "2ef50cc10f6eb563314c2db47d6a4e04734312e1" ||
		u.Query().Get("index") != "0" {
		t.Errorf("collapse lost a parameter: %s", fixed)
	}
	// Idempotent, and a file with one token is left exactly alone.
	if again := collapseDuplicateTokens(fixed); again != fixed {
		t.Errorf("not idempotent:\n  %s\n  %s", fixed, again)
	}
	single := base + "&t=" + tok
	if collapseDuplicateTokens(single) != single {
		t.Errorf("an already-correct URL was rewritten: %s", collapseDuplicateTokens(single))
	}
}
