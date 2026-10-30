package jellyfin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const wantAuth = `MediaBrowser Client="JellyFreedom", Device="Orchestrator", ` +
	`DeviceId="jellyfreedom-orchestrator", Version="1.0", Token="abc123"`

// wantClientID is the same header with no token.
var wantClientID = "MediaBrowser " + strings.Join(clientID, ", ")

// TestAuthHeaderFormat checks the header Jellyfin 10.11 and 12 both read. Jellyfin
// 12 ignores X-Emby-Token and X-Emby-Authorization, so a token sent that way looks
// like an anonymous request and every call comes back 401.
func TestAuthHeaderFormat(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "abc123")
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if auth := got.Get("Authorization"); auth != wantAuth {
		t.Errorf("Authorization = %q, want %q", auth, wantAuth)
	}
	for _, dead := range []string{"X-Emby-Token", "X-Emby-Authorization"} {
		if v := got.Get(dead); v != "" {
			t.Errorf("%s = %q, want it unset", dead, v)
		}
	}
}

// TestAuthenticateUserHeader checks the unauthenticated login call identifies
// the client without carrying a token — there is no key yet at that point.
func TestAuthenticateUserHeader(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"User":{"Id":"u1"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "abc123")
	id, err := c.AuthenticateUser("someone", "pw")
	if err != nil {
		t.Fatalf("AuthenticateUser: %v", err)
	}
	if id != "u1" {
		t.Errorf("user id = %q, want %q", id, "u1")
	}

	auth := got.Get("Authorization")
	if auth != wantClientID {
		t.Errorf("Authorization = %q, want %q", auth, wantClientID)
	}
}

// TestAuthHeaderNoKey checks an unconfigured client does not send an empty
// Token parameter, which some servers read as a credential and reject.
func TestAuthHeaderNoKey(t *testing.T) {
	if got := AuthHeader(""); got != wantClientID {
		t.Errorf("AuthHeader(\"\") = %q, want %q", got, wantClientID)
	}
}

// fixedServer answers every request with one status and body.
func fixedServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// GetItemPath is how the webhook finds the library row for a stopped item. It must ask
// the item list — with an API key and no user, Jellyfin 10.11 and 12 answer /Items/{id}
// with 400 — and read the item's own Path, not MediaSources[0].Path, which for a .strm
// is the URL inside it.
func TestGetItemPath(t *testing.T) {
	var asked *url.URL
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL
		_, _ = w.Write([]byte(`{"Items":[{"Path":"/srv/jellyfreedom/movies/A (2020)/A (2020).strm",` +
			`"MediaSources":[{"Path":"http://192.168.1.10:1990/play/movie/1?t=x"}]}]}`))
	}))
	defer srv.Close()

	const id = "3c588455-2da7-836c-3b55-3c51d6d89b60"
	got, err := New(srv.URL, "abc123").GetItemPath(id)
	if err != nil {
		t.Fatalf("GetItemPath: %v", err)
	}
	if want := "/srv/jellyfreedom/movies/A (2020)/A (2020).strm"; got != want {
		t.Errorf("GetItemPath = %q, want the .strm path %q", got, want)
	}
	if q := asked.Query(); asked.Path != "/Items" || q.Get("ids") != id || q.Get("Fields") != "Path" {
		t.Errorf("asked %s, want /Items?ids=<id>&Fields=Path", asked)
	}
}

// The id comes from a webhook body. It is one value, never a way to add parameters.
func TestGetItemPathEscapesTheID(t *testing.T) {
	var q url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		_, _ = w.Write([]byte(`{"Items":[]}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "k").GetItemPath("x&Recursive=true"); err != nil {
		t.Fatalf("GetItemPath: %v", err)
	}
	if ids := q["ids"]; len(ids) != 1 || ids[0] != "x&Recursive=true" || q.Has("Recursive") {
		t.Errorf("query = %v, want the id as a single ids value", q)
	}
}

func TestGetItemPathNotFoundAndErrors(t *testing.T) {
	if got, err := New(fixedServer(t, http.StatusOK, `{"Items":[]}`).URL, "k").GetItemPath("x"); err != nil || got != "" {
		t.Errorf("unknown item: GetItemPath = %q, %v; want \"\", nil", got, err)
	}
	if _, err := New(fixedServer(t, http.StatusUnauthorized, ``).URL, "k").GetItemPath("x"); err == nil {
		t.Error("rejected key: GetItemPath returned no error")
	}
	if _, err := New("", "").GetItemPath("x"); err == nil {
		t.Error("unconfigured client: GetItemPath returned no error")
	}
}

// The webhook's ItemId is a dashed GUID and /Sessions prints ids without dashes. Compared
// as plain strings they never matched, so "is anyone else watching?" was always no.
func TestActiveSessionsForItemMatchesEitherSpelling(t *testing.T) {
	srv := fixedServer(t, http.StatusOK, `[
		{"NowPlayingItem":{"Id":"3c5884552da7836c3b553c51d6d89b60"}},
		{"NowPlayingItem":null},
		{"NowPlayingItem":{"Id":"ffffffffffffffffffffffffffffffff"}}]`)
	c := New(srv.URL, "k")
	for _, id := range []string{
		"3c588455-2da7-836c-3b55-3c51d6d89b60", // as the Webhook plugin sends it
		"3c5884552da7836c3b553c51d6d89b60",     // as the API prints it
		"3C588455-2DA7-836C-3B55-3C51D6D89B60",
	} {
		if n, err := c.ActiveSessionsForItem(id); err != nil || n != 1 {
			t.Errorf("ActiveSessionsForItem(%q) = %d, %v; want 1, nil", id, n, err)
		}
	}
	if n, err := c.ActivePlaybackCount(); err != nil || n != 2 {
		t.Errorf("ActivePlaybackCount = %d, %v; want 2, nil", n, err)
	}
}

// Zero is the answer that lets the webhook drop a torrent, so a count that could not be
// established has to be an error rather than a 0.
func TestActiveSessionsForItemFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"rejected key", http.StatusUnauthorized, ``},
		{"server error", http.StatusInternalServerError, `oops`},
		{"not JSON", http.StatusOK, `<html></html>`},
	} {
		if n, err := New(fixedServer(t, tc.status, tc.body).URL, "k").ActiveSessionsForItem("x"); err == nil {
			t.Errorf("%s: ActiveSessionsForItem = %d, nil; want an error", tc.name, n)
		}
	}
	if _, err := New("", "").ActiveSessionsForItem("x"); err == nil {
		t.Error("unconfigured client: ActiveSessionsForItem returned no error")
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if _, err := New(gone.URL, "k").ActiveSessionsForItem("x"); err == nil {
		t.Error("unreachable Jellyfin: ActiveSessionsForItem returned no error")
	}
}
