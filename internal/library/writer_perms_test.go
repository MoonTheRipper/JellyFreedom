package library

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A library exists to be read by ANOTHER process. The orchestrator's unit once set
// UMask=0077 to keep its database private, which also made every .strm and every folder
// 0700/0600 — and Jellyfin, running as a different user, silently could not read them. New
// entries were simply absent from the library, with no error anywhere and the files present
// and correct on disk.
//
// os.MkdirAll and os.WriteFile take a mode the umask then masks off; chmod does not. This
// runs under a hostile umask, because a test for this under the developer's umask proves
// nothing.
func TestStrmFilesAreReadableRegardlessOfUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })

	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}

	moviePath, err := WriteMovieStrm(root, "A Film", "2024", "http://box:1990/play/movie/1?t=x")
	if err != nil {
		t.Fatalf("WriteMovieStrm: %v", err)
	}
	tvPath, err := WriteTVStrm(root, "A Show", "2024", 1, 2, "http://box:1990/play/tv/1/1/2?t=x")
	if err != nil {
		t.Fatalf("WriteTVStrm: %v", err)
	}

	check := func(p string, want os.FileMode) {
		t.Helper()
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s is %04o, want %04o — another user cannot read it", p, got, want)
		}
	}

	check(moviePath, 0o644)
	check(tvPath, 0o644)

	// Every directory we created, not only the leaf: a 0700 show folder hides its seasons
	// just as effectively as a 0700 season folder does its episodes.
	for _, d := range []string{
		filepath.Dir(moviePath),
		filepath.Dir(tvPath),               // Season 01
		filepath.Dir(filepath.Dir(tvPath)), // the show
	} {
		check(d, 0o755)
	}

	// And the walk must never touch the library root it was handed, let alone climb above it.
	if fi, err := os.Stat(root); err == nil && fi.Mode().Perm() != 0o755 {
		t.Errorf("the library root was modified: %04o", fi.Mode().Perm())
	}
}
