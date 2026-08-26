package library

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSafeNameNeverEscapes is the traversal test, and it is written against safeName
// DIRECTLY rather than through the writers.
//
// That distinction is the whole point. Both writers happen to format the name as
// "%s (%s)" first, and the surviving parentheses mean no input can currently reach
// safeName as a bare "..". So a test that only drove the writers would pass against the
// old implementation, in which safeName("..") returned ".." — a directory traversal
// waiting for its first caller. The ingest API is that caller's neighbour, so the
// guarantee is asserted where it is named.
func TestSafeNameNeverEscapes(t *testing.T) {
	hostile := []string{
		"..",
		".",
		"...",
		"../..",
		"../../../../etc/passwd",
		"..\\..\\windows\\system32",
		"/etc/passwd",
		"/",
		"\\",
		"////",
		"a/../../b",
		"....//....//",
		".. ",
		" ..",
		"..\u00a0",       // trailing non-breaking space after a dot run
		"\u200b..\u200b", // zero-width space padding
		"..\x00/etc",
		"a\nb/../c",
		"C:\\Windows",
		"~/.ssh/authorized_keys",
		"",
		"   ",
		"\t\n\r",
		"...   ...",
		strings.Repeat("../", 200),
	}
	for _, in := range hostile {
		got := safeName(in)
		if got == "" {
			t.Errorf("safeName(%q) = %q: must never be empty — an empty component writes into the library root itself", in, got)
		}
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("safeName(%q) = %q: contains a path separator, so it is not one component", in, got)
		}
		if strings.Trim(got, ".") == "" {
			t.Errorf("safeName(%q) = %q: a pure dot run joins UPWARDS", in, got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("safeName(%q) = %q: not valid UTF-8", in, got)
		}
		if len(got) > MaxNameLen {
			t.Errorf("safeName(%q) is %d bytes, over the %d-byte cap", in, len(got), MaxNameLen)
		}
		// The property that actually matters: joining the result onto a directory can
		// only ever go downwards.
		joined := filepath.Join("/library", got)
		if !strings.HasPrefix(joined, "/library/") {
			t.Errorf("safeName(%q) = %q escaped: filepath.Join gives %q", in, got, joined)
		}
	}
}

// TestSafeNameLengthAndUTF8 covers the bound. An unbounded name is not merely untidy:
// over NAME_MAX the write fails outright, so an oversized title used to be a library
// entry that could never be created.
func TestSafeNameLengthAndUTF8(t *testing.T) {
	// Multi-byte runes, so a naive byte cut would land mid-sequence.
	long := strings.Repeat("あ", 500) // 1500 bytes
	got := safeName(long)
	if len(got) > MaxNameLen {
		t.Fatalf("length = %d, want <= %d", len(got), MaxNameLen)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a UTF-8 sequence")
	}
	// The cap must leave room for the longest suffix a caller appends: " S00E00.strm".
	if MaxNameLen+len(" S00E00.strm") > 255 {
		t.Fatalf("MaxNameLen %d leaves no room under NAME_MAX for a TV episode filename", MaxNameLen)
	}
	// A name at or under the cap is returned untouched — truncation must not perturb
	// the ~1,000 .strm paths already on disk.
	ok := strings.Repeat("a", MaxNameLen)
	if safeName(ok) != ok {
		t.Fatal("a name at the cap was modified")
	}
}

// TestSafeNamePreservesOrdinaryTitles pins the compatibility half of the contract. Every
// path in the live library was produced by the old safeName; a hardening that changed the
// output for an ordinary title would orphan every one of those files.
func TestSafeNamePreservesOrdinaryTitles(t *testing.T) {
	same := []string{
		"Inception (2010)",
		"Am\u00e9lie (2001)",
		"WALL\u00b7E (2008)",
		"Se7en (1995)",
		"Dr. Strangelove or: How I Learned... (1964)", // ':' stripped by BOTH old and new
		"\u5343\u3068\u5343\u5c0b\u306e\u795e\u96a0\u3057 (2001)",
		"9 (2009)",
		"[REC] (2007)",
		"Fast & Furious 6 (2013)",
	}
	for _, in := range same {
		want := strings.TrimSpace(unsafeChars.ReplaceAllString(in, ""))
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want the legacy result %q", in, got, want)
		}
	}
}

// TestSafeNameReservedAndTrailing covers the two filesystem quirks that make a written
// path differ from the recorded one.
func TestSafeNameReservedAndTrailing(t *testing.T) {
	if got := safeName("NUL"); got == "NUL" {
		t.Error("a DOS device name must be escaped — SMB refuses to create it")
	}
	if got := safeName("Movie."); got != "Movie" {
		t.Errorf("trailing dot = %q, want %q (Windows/SMB strip it on create)", got, "Movie")
	}
	if got := safeName("Movie   "); got != "Movie" {
		t.Errorf("trailing spaces = %q, want %q", got, "Movie")
	}
	// A reserved name only when it stands alone; the writers' "(year)" suffix means an
	// actual film called "Con" is untouched.
	if got := safeName("Con (2018)"); got != "Con (2018)" {
		t.Errorf("safeName(%q) = %q, want it unchanged", "Con (2018)", got)
	}
}

// TestContainedPathRefusesEscape exercises the second layer directly. It exists so that a
// future regression in safeName is caught by the writers rather than by a user noticing a
// file in the wrong media root.
func TestContainedPathRefusesEscape(t *testing.T) {
	for _, elems := range [][]string{
		{".."},
		{"..", "x.strm"},
		{"ok", "..", "..", "x.strm"},
		{"."},
	} {
		if p, err := containedPath("/library", elems...); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("containedPath(/library, %v) = %q, %v; want ErrUnsafePath", elems, p, err)
		}
	}
	// An "absolute" element is NOT an escape: filepath.Join treats every element after
	// the first as relative, so "/etc" joins to "<dir>/etc". Asserted rather than assumed,
	// because the opposite intuition is the common one and would send someone hunting for
	// a bug that is not there.
	if p, err := containedPath("/library", "/etc", "passwd"); err != nil || p != "/library/etc/passwd" {
		t.Fatalf("containedPath with an absolute element = %q, %v", p, err)
	}

	p, err := containedPath("/library", "Show (2020)", "Season 01", "ep.strm")
	if err != nil || p != "/library/Show (2020)/Season 01/ep.strm" {
		t.Fatalf("containedPath on a good name = %q, %v", p, err)
	}
}

// TestWriteStrmHostileTitle is the end-to-end version: a hostile title must produce a
// file INSIDE the library directory, and the directory must contain nothing else.
func TestWriteStrmHostileTitle(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	// A canary the traversal would overwrite if it worked.
	canary := filepath.Join(root, "secret.strm")
	if err := os.WriteFile(canary, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}

	titles := []string{
		"../secret",
		"../../etc/passwd",
		"..",
		"/absolute",
		strings.Repeat("A", 4000),
		"nul",
		"\x00\x01\x02",
	}
	for _, title := range titles {
		mp, err := WriteMovieStrm(lib, title, "2020", "http://x/play")
		if err != nil {
			t.Fatalf("WriteMovieStrm(%q): %v", title, err)
		}
		if !strings.HasPrefix(mp, lib+string(filepath.Separator)) {
			t.Fatalf("movie .strm for %q landed at %q, outside %q", title, mp, lib)
		}
		tp, err := WriteTVStrm(lib, title, "2020", 1, 2, "http://x/play")
		if err != nil {
			t.Fatalf("WriteTVStrm(%q): %v", title, err)
		}
		if !strings.HasPrefix(tp, lib+string(filepath.Separator)) {
			t.Fatalf("tv .strm for %q landed at %q, outside %q", title, tp, lib)
		}
	}

	if b, err := os.ReadFile(canary); err != nil || string(b) != "untouched" {
		t.Fatalf("the canary outside the library was modified: %q, %v", b, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 { // lib/ and secret.strm
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("files appeared outside the library directory: %v", names)
	}
}

// TestWriteStrmRoundTrip is the happy path: the layout Jellyfin expects, and the exact
// bytes in the file.
func TestWriteStrmRoundTrip(t *testing.T) {
	lib := t.TempDir()
	const url = "http://host:1990/play/p/anidb/movie/42?t=abc"

	mp, err := WriteMovieStrm(lib, "Inception", "2010", url)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(lib, "Inception (2010)", "Inception (2010).strm"); mp != want {
		t.Fatalf("movie path = %q, want %q", mp, want)
	}
	if b, _ := os.ReadFile(mp); string(b) != url {
		t.Fatalf("movie .strm contents = %q, want %q", b, url)
	}

	tp, err := WriteTVStrm(lib, "The Show", "2019", 2, 7, url)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(lib, "The Show (2019)", "Season 02", "The Show (2019) S02E07.strm")
	if tp != want {
		t.Fatalf("tv path = %q, want %q", tp, want)
	}
	if b, _ := os.ReadFile(tp); string(b) != url {
		t.Fatalf("tv .strm contents = %q, want %q", b, url)
	}

	// TVStrmPath must agree with what WriteTVStrm actually wrote, or the delete path
	// removes a file that is not the one on disk.
	pred, err := TVStrmPath(lib, "The Show", "2019", 2, 7)
	if err != nil || pred != tp {
		t.Fatalf("TVStrmPath = %q, %v; want %q", pred, err, tp)
	}

	if err := RemoveStrm(mp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mp); !os.IsNotExist(err) {
		t.Fatal("RemoveStrm left the file behind")
	}
}
