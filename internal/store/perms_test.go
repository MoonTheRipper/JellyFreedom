package store

import (
	"os"
	"path/filepath"
	"testing"
)

// The database holds session tokens IN PLAINTEXT — a copied row is a logged-in admin — plus
// play.hmac_key and every provider API key. SQLite creates the file 0666 &^ umask, which is
// 0644 under systemd's default, and on a live install that meant every local account on the
// box could read all of it. The directory is 0700 as well; this pins the second lock.
func TestOpenRestrictsTheDatabaseFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "perms.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("database file mode is %04o, want 0600 — group and other must not read it", got)
	}
}
