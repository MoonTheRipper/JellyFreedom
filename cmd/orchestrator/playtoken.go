package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sync"

	"jellyfreedom/internal/store"
)

// ── Play capability tokens ────────────────────────────────────────────────────
//
// /play/... and /proxy/stream MUST stay unauthenticated: Jellyfin fetches the URL
// inside a .strm file with no session cookie, so requiring a login would break every
// client. But an unauthenticated /play used to accept ANY identity, and /proxy/stream
// accepted ANY attacker-supplied info hash and would add that torrent to TorrServer —
// i.e. an unauthenticated stranger could make the box download arbitrary content over
// the owner's VPN.
//
// The fix is a capability URL: the identity is HMAC'd with a server-side key and the
// tag is part of the path's query. Possession of a valid .strm (which only the library
// owner's Jellyfin has) is the credential. The key never leaves the server and is
// generated on first run.

const playKeySetting = "play.hmac_key"

// playTokenRequiredSetting gates ENFORCEMENT. It is set to "true" only after the
// startup migration has successfully rewritten every existing .strm with a tokenised
// URL. Without that gate, deploying this change would instantly break playback of every
// item already in the library (their .strm files carry no token).
const playTokenRequiredSetting = "play.token_required"

var (
	playKeyMu sync.RWMutex
	playKey   []byte
)

// loadPlayKey reads the HMAC key from settings, generating and persisting one on first
// run. A failure here is fatal to the caller: silently continuing with no key would
// mean silently serving unauthenticated capability URLs.
func loadPlayKey(db *store.Store) error {
	v, err := db.GetSetting(playKeySetting)
	if err != nil {
		return fmt.Errorf("read %s: %w", playKeySetting, err)
	}
	if v == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("generate play key: %w", err)
		}
		v = hex.EncodeToString(b)
		if err := db.SetSetting(playKeySetting, v); err != nil {
			return fmt.Errorf("persist %s: %w", playKeySetting, err)
		}
	}
	key, err := hex.DecodeString(v)
	if err != nil || len(key) < 16 {
		return fmt.Errorf("stored play key is malformed; delete the %q setting to regenerate", playKeySetting)
	}
	playKeyMu.Lock()
	playKey = key
	playKeyMu.Unlock()
	return nil
}

// playIdentity is the canonical string a token authorises.
func playIdentity(mediaType string, tmdbID, season, episode int) string {
	if mediaType == "movie" {
		return fmt.Sprintf("movie:%d", tmdbID)
	}
	return fmt.Sprintf("tv:%d:%d:%d", tmdbID, season, episode)
}

// playToken returns the capability tag for an identity.
func playToken(mediaType string, tmdbID, season, episode int) string {
	playKeyMu.RLock()
	key := playKey
	playKeyMu.RUnlock()
	if len(key) == 0 {
		return ""
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(playIdentity(mediaType, tmdbID, season, episode)))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// validPlayToken checks a supplied tag in constant time.
func validPlayToken(got, mediaType string, tmdbID, season, episode int) bool {
	want := playToken(mediaType, tmdbID, season, episode)
	if want == "" || got == "" {
		return false
	}
	return hmac.Equal([]byte(got), []byte(want))
}
