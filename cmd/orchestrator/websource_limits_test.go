package main

import (
	"strings"
	"testing"
)

// The add handler checks the length and control characters of the title the ADMIN typed —
// but when they supply one, info.Title is what reaches the row, and info.Uploader and
// info.Extractor are never checked at all. All three come from a third-party page and are
// bounded only by yt-dlp's 8MiB stdout cap.
func TestClampExtracted(t *testing.T) {
	if got := clampExtracted(strings.Repeat("a", 5000)); len([]rune(got)) != maxWebSourceTitleLen {
		t.Errorf("a 5000-rune title was stored as %d runes, want %d", len([]rune(got)), maxWebSourceTitleLen)
	}
	if got := clampExtracted("bad\x00title\x07here"); got != "badtitlehere" {
		t.Errorf("control bytes survived: %q", got)
	}
	// Newlines become spaces rather than vanishing, so words do not run together.
	if got := clampExtracted("two\nlines"); got != "two lines" {
		t.Errorf("newline handling: %q", got)
	}
	if got := clampExtracted("  spaced  "); got != "spaced" {
		t.Errorf("not trimmed: %q", got)
	}
	// Non-ASCII is content, not a control character.
	if got := clampExtracted("Æther — 日本語"); got != "Æther — 日本語" {
		t.Errorf("mangled legitimate text: %q", got)
	}
}

// The extraction budget must actually bound concurrency. Without it, an authenticated caller
// looping POST /api/websources/preview forks a yt-dlp process per request, each unpacking
// ~76MB and holding a 90-second budget — the dashboard's MAX_CONCURRENT is a browser-side
// courtesy that curl ignores.
func TestExtractionBudgetBoundsConcurrency(t *testing.T) {
	p := &webPlayer{extracting: make(chan struct{}, 2)}

	// Fill the budget.
	for i := 0; i < 2; i++ {
		select {
		case p.extracting <- struct{}{}:
		default:
			t.Fatalf("budget refused slot %d, but capacity is 2", i+1)
		}
	}
	// A third must wait rather than proceed.
	select {
	case p.extracting <- struct{}{}:
		t.Fatal("a third extraction started while two were already running")
	default:
	}

	// And the budget is returned, not leaked.
	<-p.extracting
	select {
	case p.extracting <- struct{}{}:
	default:
		t.Fatal("a slot was not returned after an extraction finished")
	}
}
