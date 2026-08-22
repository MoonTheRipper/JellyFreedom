package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"jellyfreedom/internal/store"
)

func setupAPI(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	SetStore(s)
	limiter = newLoginLimiter() // isolate rate-limit state per test
	t.Cleanup(func() { SetStore(nil); limiter = newLoginLimiter() })
	return s
}

func mkUser(t *testing.T, s *store.Store, name, pw string, admin bool) *store.User {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(&store.User{Username: name, PasswordHash: string(h), AuthSource: "local", IsAdmin: admin}); err != nil {
		t.Fatal(err)
	}
	u, err := s.GetUserByUsername(name)
	if err != nil || u == nil {
		t.Fatalf("read back user: %v", err)
	}
	return u
}

var sessionSeq int

func login(t *testing.T, s *store.Store, u *store.User) *http.Cookie {
	t.Helper()
	sessionSeq++
	tok := fmt.Sprintf("sess-%s-%d", u.Username, sessionSeq)
	if err := s.CreateSession(tok, u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: tok}
}

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
})

// TestAuthzMatrix pins the middleware behaviour: who reaches a handler, and what a
// rejected caller gets.
func TestAuthzMatrix(t *testing.T) {
	s := setupAPI(t)
	admin := mkUser(t, s, "root", "password123", true)
	user := mkUser(t, s, "bob", "password123", false)

	cases := []struct {
		name     string
		mw       func(http.Handler) http.Handler
		cookie   *http.Cookie
		path     string
		wantCode int
	}{
		{"RequireAuth rejects anonymous on an API path", RequireAuth, nil, "/api/x", http.StatusUnauthorized},
		{"RequireAuth allows a normal user", RequireAuth, login(t, s, user), "/api/x", http.StatusOK},
		{"RequireAuth allows an admin", RequireAuth, login(t, s, admin), "/api/x", http.StatusOK},
		{"RequireAdmin rejects anonymous", RequireAdmin, nil, "/api/x", http.StatusUnauthorized},
		{"RequireAdmin FORBIDS a normal user", RequireAdmin, login(t, s, user), "/api/x", http.StatusForbidden},
		{"RequireAdmin allows an admin", RequireAdmin, login(t, s, admin), "/api/x", http.StatusOK},
		{"OptionalAuth lets anonymous through", OptionalAuth, nil, "/api/x", http.StatusOK},
		{"OptionalAuth lets a user through", OptionalAuth, login(t, s, user), "/api/x", http.StatusOK},
		// Dashboard (browser) paths redirect instead of returning JSON.
		{"RequireAuth redirects an anonymous browser", RequireAuth, nil, "/dashboard/", http.StatusFound},
		{"RequireAdmin redirects a non-admin browser", RequireAdmin, login(t, s, user), "/dashboard/", http.StatusFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.cookie != nil {
				r.AddCookie(tc.cookie)
			}
			w := httptest.NewRecorder()
			tc.mw(okHandler).ServeHTTP(w, r)
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tc.wantCode, w.Body.String())
			}
		})
	}
}

// TestOptionalAuthLeavesAnonymousUnidentified is the middleware half of the visibility
// bug: an anonymous request must yield a nil user, never a stand-in that some caller
// then treats as privileged.
func TestOptionalAuthLeavesAnonymousUnidentified(t *testing.T) {
	setupAPI(t)
	var seen *store.User
	h := OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = UserFromContext(r)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/library", nil))
	if seen != nil {
		t.Fatalf("an anonymous request produced a user: %+v", seen)
	}
}

func TestExpiredSessionIsNotAuthenticated(t *testing.T) {
	s := setupAPI(t)
	u := mkUser(t, s, "bob", "password123", false)
	if err := s.CreateSession("expired", u.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "expired"})
	w := httptest.NewRecorder()
	RequireAuth(okHandler).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("an expired session authenticated (status %d)", w.Code)
	}
}

// TestLoginFailsWhenSessionCannotBeCreated is the regression test for the discarded
// CreateSession error: login returned 200 and set a cookie for a session row that did
// not exist, bouncing the user straight back to the login screen with no error anywhere.
func TestLoginFailsWhenSessionCannotBeCreated(t *testing.T) {
	s := setupAPI(t)
	mkUser(t, s, "bob", "password123", false)

	// Close the store so every write fails, simulating a DB failure at login time.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"username":"bob","password":"password123"}`))
	w := httptest.NewRecorder()
	APILoginHandler(w, r)

	if w.Code == http.StatusOK {
		t.Fatalf("login reported success despite being unable to create a session (body %q)", w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("a session cookie was set for a session that was never stored")
		}
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	s := setupAPI(t)
	mkUser(t, s, "bob", "password123", false)

	for _, body := range []string{
		`{"username":"bob","password":"wrong"}`,
		`{"username":"nosuchuser","password":"password123"}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
		w := httptest.NewRecorder()
		APILoginHandler(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("body %s: status = %d, want 401", body, w.Code)
		}
		// The message must not reveal WHICH half was wrong.
		if strings.Contains(strings.ToLower(w.Body.String()), "no such user") {
			t.Fatalf("the response distinguishes an unknown user: %s", w.Body.String())
		}
	}
}

func TestLoginSucceedsAndSetsAWorkingSession(t *testing.T) {
	s := setupAPI(t)
	mkUser(t, s, "bob", "password123", false)

	r := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"username":"bob","password":"password123"}`))
	w := httptest.NewRecorder()
	APILoginHandler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "$2a$") {
		t.Fatalf("the login response leaked a password hash: %s", w.Body.String())
	}

	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatal("no session cookie was set")
	}
	if !cookie.HttpOnly {
		t.Error("the session cookie must be HttpOnly")
	}
	// The session must actually authenticate a subsequent request.
	r2 := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	r2.AddCookie(cookie)
	w2 := httptest.NewRecorder()
	RequireAuth(okHandler).ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("the freshly issued session did not authenticate (status %d)", w2.Code)
	}
}

func TestSecureCookieFlagIsConfigDriven(t *testing.T) {
	s := setupAPI(t)
	mkUser(t, s, "bob", "password123", false)

	for _, secure := range []bool{false, true} {
		SetSecureCookies(secure)
		r := httptest.NewRequest(http.MethodPost, "/api/auth/login",
			strings.NewReader(`{"username":"bob","password":"password123"}`))
		w := httptest.NewRecorder()
		APILoginHandler(w, r)
		for _, c := range w.Result().Cookies() {
			if c.Name == sessionCookie && c.Secure != secure {
				t.Errorf("SetSecureCookies(%v): cookie.Secure = %v", secure, c.Secure)
			}
		}
	}
	SetSecureCookies(false)
}

// TestChangePasswordWorksForNonAdmin is the regression test for the 403: the handler
// was registered on a mux wrapped in RequireAdmin, even though it changes the CALLING
// user's password.
func TestChangePasswordWorksForNonAdmin(t *testing.T) {
	s := setupAPI(t)
	u := mkUser(t, s, "bob", "password123", false)
	cookie := login(t, s, u)

	// The route is now registered under RequireAuth, which a non-admin satisfies.
	h := RequireAuth(http.HandlerFunc(ChangePasswordHandler))
	r := httptest.NewRequest(http.MethodPost, "/api/auth/change-password",
		strings.NewReader(`{"current":"password123","new":"newpassword456"}`))
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("a non-admin could not change their own password: status %d, body %q", w.Code, w.Body.String())
	}
	updated, _ := s.GetUserByUsername("bob")
	if bcrypt.CompareHashAndPassword([]byte(updated.PasswordHash), []byte("newpassword456")) != nil {
		t.Fatal("the password was not actually changed")
	}
}

func TestChangePasswordRejectsWrongCurrent(t *testing.T) {
	s := setupAPI(t)
	u := mkUser(t, s, "bob", "password123", false)
	h := RequireAuth(http.HandlerFunc(ChangePasswordHandler))
	r := httptest.NewRequest(http.MethodPost, "/api/auth/change-password",
		strings.NewReader(`{"current":"WRONG","new":"newpassword456"}`))
	r.AddCookie(login(t, s, u))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// TestChangePasswordInvalidatesOtherSessions: after a password change, a cookie stolen
// earlier must stop working, while the caller's own tab keeps working.
func TestChangePasswordInvalidatesOtherSessions(t *testing.T) {
	s := setupAPI(t)
	u := mkUser(t, s, "bob", "password123", false)

	mine := login(t, s, u)
	if err := s.CreateSession("stolen", u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	h := RequireAuth(http.HandlerFunc(ChangePasswordHandler))
	r := httptest.NewRequest(http.MethodPost, "/api/auth/change-password",
		strings.NewReader(`{"current":"password123","new":"newpassword456"}`))
	r.AddCookie(mine)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("change-password failed: %d %s", w.Code, w.Body.String())
	}

	if _, ok := s.GetSessionUser("stolen"); ok {
		t.Error("a pre-existing session survived a password change")
	}
	if _, ok := s.GetSessionUser(mine.Value); !ok {
		t.Error("the caller's own session was invalidated")
	}
}

func TestChangePasswordEnforcesMinimumLength(t *testing.T) {
	s := setupAPI(t)
	u := mkUser(t, s, "bob", "password123", false)
	h := RequireAuth(http.HandlerFunc(ChangePasswordHandler))
	r := httptest.NewRequest(http.MethodPost, "/api/auth/change-password",
		strings.NewReader(`{"current":"password123","new":"short"}`))
	r.AddCookie(login(t, s, u))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestNeedsSetupFailsClosedOnDBError(t *testing.T) {
	s := setupAPI(t)
	mkUser(t, s, "root", "password123", true)
	if NeedsSetup() {
		t.Fatal("setup should not be needed when a user exists")
	}
	// A dead database must NOT be reported as "no users yet" — that would offer the
	// unauthenticated setup page, and with it a fresh admin account.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if NeedsSetup() {
		t.Fatal("NeedsSetup must fail CLOSED when the database cannot be read")
	}
}

func TestMeHandler(t *testing.T) {
	s := setupAPI(t)
	u := mkUser(t, s, "bob", "password123", false)

	w := httptest.NewRecorder()
	MeHandler(w, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /api/me = %d, want 401", w.Code)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.AddCookie(login(t, s, u))
	w = httptest.NewRecorder()
	MeHandler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/me = %d, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), "$2a$") || strings.Contains(w.Body.String(), "password") {
		t.Fatalf("/api/me leaked credential material: %s", w.Body.String())
	}
}
