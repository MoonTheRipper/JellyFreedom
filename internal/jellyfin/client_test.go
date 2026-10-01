package jellyfin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const wantAuth = `MediaBrowser Client="JellyFreedom", Device="Orchestrator", ` +
	`DeviceId="jellyfreedom-orchestrator", Version="1.0", Token="abc123"`

// wantClientID is the same header with no token.
var wantClientID = "MediaBrowser " + strings.Join(clientID, ", ")

// TestAuthHeaderFormat checks the header current Jellyfin servers read. They
// ignore X-Emby-Token and X-Emby-Authorization, so a token sent that way looks
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
