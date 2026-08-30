package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"

	"jellyfreedom/internal/library"
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

// ── Identity encoding ─────────────────────────────────────────────────────────
//
// A token authorises exactly one identity, so the identity string IS the security
// boundary: two different titles that encode to the same string share a token, and a
// capability handed out for one of them silently authorises the other.
//
// The encoding is a ':'-joined tuple. That is only safe because NO FIELD CAN CONTAIN THE
// DELIMITER, which is enforced, not assumed:
//
//   - provider is [a-z0-9]{1,16}          (library.ValidProvider)
//   - provider id is [A-Za-z0-9_-]{1,64}  (library.ValidProviderID)
//   - media type is exactly "movie" or "tv"
//   - season and episode are ints rendered with %d, which is ':'-free by construction
//
// Given that, splitting the string on ':' recovers exactly the tuple that produced it —
// each field is a maximal run of non-':' bytes, so no two distinct tuples can flatten to
// the same string. Without that guarantee the encoding would be forgeable in the most
// direct way possible: a provider named "a" with id "b:c" and a provider named "a:b" with
// id "c" would produce identical bytes, hence an identical HMAC, hence one capability
// that opens two doors. This is why the validation lives in front of the encoder rather
// than at the HTTP edge only — the encoder is reachable from the .strm writer too.
//
// The two shapes are then separated by their first field:
//
//	TMDB (frozen):  movie:<id>              tv:<id>:<season>:<episode>
//	any provider:   p:<provider>:movie:<id>  p:<provider>:tv:<id>:<season>:<episode>
//
// "p" is not a media type, and the first field of a legacy identity is always a media
// type, so no namespaced identity can ever equal a legacy one. Note this holds even for a
// provider literally named "p" or "tv": the namespace tag occupies field 0 in its own
// right, so p:tv:movie:1 is still unambiguously the tuple (p, tv, movie, 1).
//
// The TMDB shape is frozen because ~1,000 .strm files on the live install contain tokens
// that are HMACs over these exact bytes. Changing them 403s the entire library at once.

// legacyPlayIdentity is the pre-provider encoding, reproduced here unchanged and called by
// everything that needs it. It is a separate function from the provider-aware encoder so
// that the frozen bytes live in exactly one place and a future edit has to touch a
// function whose name says it cannot change.
func legacyPlayIdentity(mediaType string, tmdbID, season, episode int) string {
	if mediaType == "movie" {
		return fmt.Sprintf("movie:%d", tmdbID)
	}
	return fmt.Sprintf("tv:%d:%d:%d", tmdbID, season, episode)
}

// playIdentity is the canonical string a token authorises, for callers that still speak
// TMDB integers. Unchanged in behaviour for every possible int input.
func playIdentity(mediaType string, tmdbID, season, episode int) string {
	return legacyPlayIdentity(mediaType, tmdbID, season, episode)
}

// playIdentityFor is the provider-aware encoder. For ProviderTMDB it returns the legacy
// string byte for byte; for anything else it returns the namespaced form.
//
// The TMDB branch does not concatenate providerID into the string. It parses the id to an
// int and re-renders it through legacyPlayIdentity, then only accepts the id if
// strconv.Itoa round-trips it unchanged. That is a proof rather than a promise: a
// providerID of "01622" or "+1622" or "1622 " is a DIFFERENT string from what the legacy
// encoder would have produced for the same title, so it must be rejected outright instead
// of minting a token nobody's .strm carries — or worse, a second valid token for a title
// that already has one.
func playIdentityFor(provider, mediaType, providerID string, season, episode int) (string, error) {
	if err := library.ValidPlayRef(provider, mediaType, providerID, season, episode); err != nil {
		return "", err
	}
	if provider == library.ProviderTMDB {
		n, err := strconv.Atoi(providerID)
		if err != nil || strconv.Itoa(n) != providerID {
			return "", fmt.Errorf("tmdb id %q is not a canonical decimal integer", providerID)
		}
		return legacyPlayIdentity(mediaType, n, season, episode), nil
	}
	if mediaType == "movie" {
		return "p:" + provider + ":movie:" + providerID, nil
	}
	return fmt.Sprintf("p:%s:tv:%s:%d:%d", provider, providerID, season, episode), nil
}

// ── playRef: one identity, carried whole ──────────────────────────────────────
//
// The handler used to pass (mediaType, tmdbID, season, episode) down as four positional
// arguments. Adding a provider to that would have made it five, in an order nothing
// enforces, threaded through a dozen call sites. Carrying the identity as one value means
// the /play routes and the .strm writer cannot disagree about what they are naming.

type playRef struct {
	provider   string // "tmdb" or another registered provider
	mediaType  string // "movie" | "tv"
	providerID string // TMDB's integer as text, or e.g. a UUID
	season     int
	episode    int
}

// tmdbRef builds the identity of a TMDB title from the integer the rest of the system
// still speaks in. It is the single place that int→string spelling happens on this side,
// mirroring store.TMDBIdentity, so the two cannot drift.
func tmdbRef(mediaType string, tmdbID, season, episode int) playRef {
	return playRef{
		provider:   library.ProviderTMDB,
		mediaType:  mediaType,
		providerID: strconv.Itoa(tmdbID),
		season:     season,
		episode:    episode,
	}
}

// itemRef is the identity a library row's .strm must be tokenised with.
//
// It has to come from the row's PROVIDER identity, never from TMDBID alone. A web source
// has no TMDB id, so reading TMDBID gives 0 for every one of them — which collapsed every
// non-TMDB entry in the library onto the single URL /play/movie/0 carrying one shared
// token, and /play answered "bad tmdb id". Worse, the rewrite runs at startup, so the
// damage landed on an ordinary restart long after the entries were added and working.
//
// An empty Provider means a row written before providers existed; those are TMDB.
//
// Library rows and queue rows carry the same identity in the same three fields but are
// different types, so the decision lives in refFor and both wrappers defer to it. Two
// copies of this rule is how one of them gets fixed and the other does not.
func refFor(provider, providerID, mediaType string, tmdbID, season, episode int) playRef {
	if provider == "" || provider == library.ProviderTMDB {
		return tmdbRef(mediaType, tmdbID, season, episode)
	}
	return playRef{
		provider:   provider,
		mediaType:  mediaType,
		providerID: providerID,
		season:     season,
		episode:    episode,
	}
}

func itemRef(it *store.Item) playRef {
	return refFor(it.Provider, it.ProviderID, it.MediaType, it.TMDBID, it.Season, it.Episode)
}

func queueRef(q *store.QueueItem) playRef {
	return refFor(q.Provider, q.ProviderID, q.MediaType, q.TMDBID, q.Season, q.Episode)
}

// isTMDB reports whether this identity is one the TMDB metadata client can resolve. Only
// TMDB has a metadata and search pipeline behind it today; a second provider can be
// routed, tokenised and served from cache before that exists.
func (ref playRef) isTMDB() bool { return ref.provider == library.ProviderTMDB }

// tmdbInt returns the TMDB integer id, or 0 for a non-TMDB identity. Callers that pass
// this into the TMDB-shaped resolve pipeline MUST check isTMDB first: a non-TMDB
// identity would arrive there as id 0, which is not "no title" but "TMDB title 0", and
// every non-TMDB identity would collapse onto it.
func (ref playRef) tmdbInt() int {
	if !ref.isTMDB() {
		return 0
	}
	n, err := strconv.Atoi(ref.providerID)
	if err != nil {
		return 0
	}
	return n
}

// storeIdentity converts to the store's identity tuple for a provider-keyed lookup.
func (ref playRef) storeIdentity() store.Identity {
	return store.Identity{
		Provider:   ref.provider,
		ProviderID: ref.providerID,
		MediaType:  ref.mediaType,
		Season:     ref.season,
		Episode:    ref.episode,
	}
}

// identity returns the string a token for this ref authorises.
func (ref playRef) identity() (string, error) {
	return playIdentityFor(ref.provider, ref.mediaType, ref.providerID, ref.season, ref.episode)
}

// ── Tokens ────────────────────────────────────────────────────────────────────

// signIdentity is the one place an identity becomes a tag. Every token in the system —
// legacy and provider-aware — goes through this function, so the two shapes cannot end up
// signed with different constructions.
func signIdentity(identity string) string {
	playKeyMu.RLock()
	key := playKey
	playKeyMu.RUnlock()
	if len(key) == 0 {
		return ""
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(identity))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// playToken returns the capability tag for a TMDB identity.
func playToken(mediaType string, tmdbID, season, episode int) string {
	return signIdentity(legacyPlayIdentity(mediaType, tmdbID, season, episode))
}

// validPlayToken checks a supplied tag for a TMDB identity in constant time.
func validPlayToken(got, mediaType string, tmdbID, season, episode int) bool {
	return constantTimeTokenMatch(got, playToken(mediaType, tmdbID, season, episode))
}

// playTokenFor returns the capability tag for any provider's identity. It returns an
// error for an identity that cannot be encoded, and "" (with no error) only when no key
// is loaded — the two are different failures and the caller usually cares which.
func playTokenFor(provider, mediaType, providerID string, season, episode int) (string, error) {
	id, err := playIdentityFor(provider, mediaType, providerID, season, episode)
	if err != nil {
		return "", err
	}
	return signIdentity(id), nil
}

// validPlayTokenFor checks a supplied tag for any provider's identity in constant time.
//
// An identity that fails validation is refused here rather than being encoded "as best we
// can" and compared. Comparing a token against a best-effort encoding of a malformed
// identity is how a validation bug turns into an authorisation bug.
func validPlayTokenFor(got, provider, mediaType, providerID string, season, episode int) bool {
	want, err := playTokenFor(provider, mediaType, providerID, season, episode)
	if err != nil {
		return false
	}
	return constantTimeTokenMatch(got, want)
}

// token returns this ref's capability tag, or "" if it cannot be minted.
func (ref playRef) token() string {
	t, err := playTokenFor(ref.provider, ref.mediaType, ref.providerID, ref.season, ref.episode)
	if err != nil {
		return ""
	}
	return t
}

// validToken checks a supplied tag against this ref.
func (ref playRef) validToken(got string) bool {
	return validPlayTokenFor(got, ref.provider, ref.mediaType, ref.providerID, ref.season, ref.episode)
}

// constantTimeTokenMatch compares two tags without leaking, through timing, how many
// leading bytes an attacker guessed correctly. The empty cases are rejected before the
// comparison because an empty want means "no key loaded" — accepting an empty got against
// it would turn a missing key into an open door.
func constantTimeTokenMatch(got, want string) bool {
	if want == "" || got == "" {
		return false
	}
	return hmac.Equal([]byte(got), []byte(want))
}
