package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	"jellyfreedom/internal/store"
)

// runRotatePlayKey replaces play.hmac_key, invalidating every capability token ever issued.
//
// # WHY THIS EXISTS
//
// A play token is a permanent, unbound, unrevokable bearer credential. It never expires, is
// tied to no user, and deleting the library item does not revoke it — /play resolves the
// identity out of the URL and streams. So a .strm that leaks (a backup, a screenshot, a
// shared Jellyfin library path) keeps working forever, and the ONLY revocation is rotating
// this key. There was no way to do that short of hand-editing SQLite and restarting.
//
// Rotation is safe because the orchestrator re-signs every .strm at startup: the new key is
// generated on the next boot, migrateStrmTokens rewrites each file with a token over the
// same identity, and playback continues. What stops working is every URL somebody copied.
func runRotatePlayKey(args []string) int {
	fs := flag.NewFlagSet("rotate-play-key", flag.ExitOnError)
	dbPath := fs.String("db", "/var/lib/jellyfreedom/jellyfreedom.db", "path to the SQLite database")
	yes := fs.Bool("yes", false, "do not prompt")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if !*yes {
		fmt.Println("This replaces the play signing key.")
		fmt.Println()
		fmt.Println("  * Every .strm in your library is re-signed on the next start, so playback")
		fmt.Println("    keeps working — Jellyfin does not even need a rescan.")
		fmt.Println("  * Every play URL that has ALREADY been copied out stops working. That is")
		fmt.Println("    the point: those URLs cannot otherwise be revoked.")
		fmt.Println()
		fmt.Print("Type ROTATE to confirm: ")
		var reply string
		_, _ = fmt.Scanln(&reply)
		if reply != "ROTATE" {
			fmt.Fprintln(os.Stderr, "aborted.")
			return 1
		}
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rotate-play-key: could not open %s: %v\n", *dbPath, err)
		return 1
	}
	defer db.Close()

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		fmt.Fprintf(os.Stderr, "rotate-play-key: could not generate a key: %v\n", err)
		return 1
	}
	if err := db.SetSetting(playKeySetting, hex.EncodeToString(b)); err != nil {
		fmt.Fprintf(os.Stderr, "rotate-play-key: could not store the new key: %v\n", err)
		return 1
	}

	fmt.Println("play signing key rotated.")
	fmt.Println("restart the service to re-sign the library:  sudo systemctl restart jellyfreedom")
	return 0
}
