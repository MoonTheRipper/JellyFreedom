package update

import (
	"strings"
	"testing"
)

// A realistic multi-section GitHub release body: badge, headings, links, emphasis,
// inline code, a code fence with install instructions, a horizontal rule and a footer.
const realisticBody = "" +
	"![build](https://img.shields.io/badge/build-passing-green)\n" +
	"\n" +
	"## What's Changed\n" +
	"\n" +
	"- **Resolve-at-Play**: `.strm` files now point at a stable `/play/...` URL\n" +
	"- Fixed a data race in the [health cache](https://github.com/MoonTheRipper/JellyFreedom/pull/12)\n" +
	"* Added an in-dashboard *self-update* banner\n" +
	"\n" +
	"### Fixes\n" +
	"1. Fixed the VPN watchdog restarting TorrServer mid-stream\n" +
	"2. Fixed `sudo` rules losing their absolute paths\n" +
	"- Dropped the unreachable duplicate drop route\n" +
	"- This one is beyond the cap and must not appear\n" +
	"\n" +
	"---\n" +
	"\n" +
	"## Install\n" +
	"```bash\n" +
	"curl -fsSL https://example.invalid/install.sh | sudo bash\n" +
	"- this bullet is inside a code fence and must be ignored\n" +
	"```\n" +
	"\n" +
	"<!-- a comment -->\n" +
	"**Full Changelog**: <https://github.com/MoonTheRipper/JellyFreedom/compare/v0.3.0...v0.4.0>\n"

func TestNotesRealisticBody(t *testing.T) {
	got := Notes(realisticBody)
	want := []string{
		"Resolve-at-Play: .strm files now point at a stable /play/... URL",
		"Fixed a data race in the health cache",
		"Added an in-dashboard self-update banner",
		"Fixed the VPN watchdog restarting TorrServer mid-stream",
		"Fixed sudo rules losing their absolute paths",
		"Dropped the unreachable duplicate drop route",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d notes %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("note %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNotesCaps(t *testing.T) {
	body := strings.Repeat("- a bullet line\n", 20)
	if got := Notes(body); len(got) != maxNotes {
		t.Errorf("got %d notes, want the cap of %d", len(got), maxNotes)
	}

	long := "- " + strings.Repeat("x", 400)
	got := Notes(long)
	if len(got) != 1 {
		t.Fatalf("got %d notes, want 1", len(got))
	}
	if n := len([]rune(got[0])); n != maxNoteRune {
		t.Errorf("note length = %d runes, want %d", n, maxNoteRune)
	}
	if !strings.HasSuffix(got[0], "…") {
		t.Errorf("a truncated note should end in an ellipsis, got %q", got[0])
	}

	// Multi-byte characters must not be split mid-rune.
	multi := "- " + strings.Repeat("é", 400)
	if got := Notes(multi); len([]rune(got[0])) != maxNoteRune || !strings.HasSuffix(got[0], "…") {
		t.Errorf("multibyte truncation wrong: %q", got[0])
	}
}

func TestNotesEdgeCases(t *testing.T) {
	if got := Notes(""); len(got) != 0 || got == nil {
		t.Errorf("empty body should give an empty non-nil slice, got %#v", got)
	}
	if got := Notes("## Heading only\n\n### Another\n"); len(got) != 0 {
		t.Errorf("headings only should give no notes, got %q", got)
	}
	// A body with no bullets at all still produces something rather than an empty banner.
	got := Notes("## Notes\n\nThis release fixes the seeder decay problem.\nIt also adds a banner.\n")
	want := []string{"This release fixes the seeder decay problem.", "It also adds a banner."}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("prose fallback = %q, want %q", got, want)
	}
	// Windows line endings.
	if got := Notes("- one\r\n- two\r\n"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("CRLF body = %q", got)
	}
	// A bullet that is nothing but an image link disappears rather than becoming "".
	if got := Notes("- ![badge](https://x.invalid/b.svg)\n- real change\n"); len(got) != 1 || got[0] != "real change" {
		t.Errorf("image-only bullet not dropped: %q", got)
	}
}
