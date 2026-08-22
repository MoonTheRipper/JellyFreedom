package update

import (
	"regexp"
	"strings"
)

// ── Release-notes teaser ──────────────────────────────────────────────────────
//
// The banner shows a handful of one-line bullets, not the changelog. A GitHub release
// body is arbitrary remote markdown — headings, badges, image links, code fences, an
// "installation" section, a diff URL — so it is filtered down to the lines a human
// would actually read out loud, and hard-capped in both count and length.
//
// This is a display filter, NOT a security control: the frontend still escapes every
// value, because the text comes from a remote server.

const (
	maxNotes    = 6   // entries
	maxNoteRune = 120 // characters per entry
)

var (
	// ![alt](url) — images and badges carry no information in a text bullet.
	reImage = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	// [text](url) -> text
	reLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	// <https://…> autolinks and stray HTML tags.
	reAutolink = regexp.MustCompile(`<https?://[^>\s]+>`)
	reHTMLTag  = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)
	// Leading bullet or ordered-list marker.
	reBullet = regexp.MustCompile(`^\s{0,8}([-*+]|\d{1,3}[.)])\s+`)
	// A markdown heading line.
	reHeading = regexp.MustCompile(`^\s{0,3}#{1,6}\s*`)
	// Runs of whitespace, collapsed so a wrapped bullet reads as one line.
	reSpace = regexp.MustCompile(`\s+`)
	// Emphasis and code markers, stripped in place.
	reEmphasis = regexp.MustCompile("(\\*\\*|__|\\*|_|`)")
)

// Notes turns a GitHub release body into at most maxNotes short lines.
//
// Bullet lines win: if the body has any, only those are used. A body with no bullets at
// all (a plain paragraph release note) falls back to its prose lines rather than
// rendering an empty banner.
func Notes(body string) []string {
	bullets, prose := scanNotes(body)
	lines := bullets
	if len(lines) == 0 {
		lines = prose
	}
	if lines == nil {
		lines = []string{}
	}
	return lines
}

func scanNotes(body string) (bullets, prose []string) {
	bullets, prose = []string{}, []string{}
	inFence := false
	for _, raw := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)

		// Code fences: skip the fence markers and everything between them.
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "<!--") || isRule(trimmed) {
			continue
		}
		if reHeading.MatchString(trimmed) {
			// A heading is a section title, not a change. Dropped entirely.
			continue
		}

		isBullet := reBullet.MatchString(line)
		text := clean(reBullet.ReplaceAllString(line, ""))
		if text == "" {
			continue
		}
		if isBullet {
			if len(bullets) < maxNotes {
				bullets = append(bullets, text)
			}
			continue
		}
		if len(prose) < maxNotes {
			prose = append(prose, text)
		}
	}
	return bullets, prose
}

// isRule reports whether a line is a markdown horizontal rule ("---", "***", "___").
// RE2 has no backreferences, so this is a loop rather than a regexp.
func isRule(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	return strings.Count(s, string(c)) == len(s)
}

// clean strips markdown decoration from one line and caps its length.
func clean(s string) string {
	s = reImage.ReplaceAllString(s, "")
	s = reLink.ReplaceAllString(s, "$1")
	s = reAutolink.ReplaceAllStringFunc(s, func(m string) string { return strings.Trim(m, "<>") })
	s = reHTMLTag.ReplaceAllString(s, "")
	s = reEmphasis.ReplaceAllString(s, "")
	s = reSpace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	return truncate(s, maxNoteRune)
}

// truncate cuts to n characters (runes, so a multi-byte character is never split),
// ending with an ellipsis when it had to cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}
