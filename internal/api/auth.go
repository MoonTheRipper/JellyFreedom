package api

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"
	"jellyfreedom/internal/jellyfin"
	"jellyfreedom/internal/store"
)

const sessionCookie = "jf_session"
const sessionTTL = 24 * time.Hour

type ctxKey string

const userKey ctxKey = "user"

var db *store.Store
var jfClient *jellyfin.Client

func SetStore(s *store.Store)              { db = s }
func SetJellyfinClient(c *jellyfin.Client) { jfClient = c }

// UserFromContext returns the authenticated User from the request context,
// or nil if the request is unauthenticated.
func UserFromContext(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

// RequireAuth gates a handler behind a valid session and injects the User into context.
// Dashboard paths get a browser redirect; all other paths get a JSON 401.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := sessionUser(r)
		if user == nil {
			if strings.HasPrefix(r.URL.Path, "/dashboard") {
				http.Redirect(w, r, "/dashboard/login", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		ctx := context.WithValue(r.Context(), userKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireAdmin gates a handler behind a valid admin session.
// Implies RequireAuth — the user is also injected into context.
func RequireAdmin(next http.Handler) http.Handler {
	return RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r)
		if user == nil || !user.IsAdmin {
			if isAPIPath(r.URL.Path) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"forbidden"}`))
				return
			}
			http.Redirect(w, r, "/dashboard/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// OptionalAuth loads the authenticated user into context if a valid session is
// present, but does not block unauthenticated requests. Use on public routes
// where behavior differs based on whether the caller is logged in.
func OptionalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := sessionUser(r); user != nil {
			ctx := context.WithValue(r.Context(), userKey, user)
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// NeedsSetup returns true if no users have been created yet.
//
// FAILS CLOSED. A DB error used to yield an empty slice and therefore "setup needed",
// which would have offered the unauthenticated setup page — and the ability to create a
// fresh admin account — on an install that already has users.
func NeedsSetup() bool {
	if db == nil {
		return false
	}
	users, err := db.ListUsers()
	if err != nil {
		slog.Error("could not check whether setup is needed; assuming it is NOT", "err", err)
		return false
	}
	return len(users) == 0
}

func sessionUser(r *http.Request) *store.User {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	if db == nil {
		return nil
	}
	user, ok := db.GetSessionUser(cookie.Value)
	if !ok {
		return nil
	}
	return user
}

func isAPIPath(path string) bool {
	return strings.HasPrefix(path, "/api/")
}

// SetupHandler — GET shows setup form, POST creates the first (admin) user.
func SetupHandler(w http.ResponseWriter, r *http.Request) {
	if !NeedsSetup() {
		http.Redirect(w, r, "/dashboard/", http.StatusFound)
		return
	}
	if r.Method == http.MethodPost {
		username := strings.TrimSpace(r.FormValue("username"))
		pw := r.FormValue("password")
		pw2 := r.FormValue("password2")
		if username == "" {
			renderAuthPage(w, "setup", "Username is required.", "")
			return
		}
		if pw == "" || pw != pw2 {
			renderAuthPage(w, "setup", "Passwords do not match or are empty.", "")
			return
		}
		if len(pw) < 8 {
			renderAuthPage(w, "setup", "Password must be at least 8 characters.", "")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		if err != nil {
			renderAuthPage(w, "setup", "Error hashing password.", "")
			return
		}
		user := &store.User{
			Username:     username,
			PasswordHash: string(hash),
			AuthSource:   "local",
			IsAdmin:      true,
		}
		if err := db.CreateUser(user); err != nil {
			renderAuthPage(w, "setup", "Error creating user: "+err.Error(), "")
			return
		}
		created, err := db.GetUserByUsername(username)
		if err != nil || created == nil {
			renderAuthPage(w, "setup", "Account created but could not be read back. Check the server log.", "")
			return
		}
		if err := startSession(w, created.ID); err != nil {
			slog.Error("setup: could not create session", "err", err)
			renderAuthPage(w, "setup", "Account created, but starting a session failed. Try logging in.", "")
			return
		}
		// Land the brand-new admin on the setup checklist, not the Health page — on a
		// fresh install nothing is configured yet and the checklist is what tells them
		// what to do next.
		http.Redirect(w, r, "/dashboard/#setup", http.StatusFound)
		return
	}
	renderAuthPage(w, "setup", "", "")
}

// LoginHandler — GET shows login form, POST validates credentials and sets session.
func LoginHandler(w http.ResponseWriter, r *http.Request) {
	if NeedsSetup() {
		http.Redirect(w, r, "/dashboard/setup", http.StatusFound)
		return
	}
	if sessionUser(r) != nil {
		http.Redirect(w, r, "/dashboard/", http.StatusFound)
		return
	}
	if r.Method == http.MethodPost {
		username := strings.TrimSpace(r.FormValue("username"))
		pw := r.FormValue("password")
		ip := clientIP(r)
		if ok, wait := limiter.Allow(ip, username); !ok {
			renderAuthPage(w, "login",
				fmt.Sprintf("Too many failed attempts. Try again in %s.", wait), "")
			return
		}
		user, ok := checkCredentials(username, pw)
		if !ok {
			limiter.Fail(ip, username)
			renderAuthPage(w, "login", "Incorrect username or password.", "")
			return
		}
		limiter.Succeed(ip, username)
		if err := startSession(w, user.ID); err != nil {
			slog.Error("login: could not create session", "user", user.Username, "err", err)
			renderAuthPage(w, "login", "Could not start a session. Check the server log.", "")
			return
		}
		http.Redirect(w, r, "/dashboard/", http.StatusFound)
		return
	}
	renderAuthPage(w, "login", "", "")
}

// MeHandler returns the current user's info, or 401 if not logged in.
// Used by the media UI to check auth state on load.
func MeHandler(w http.ResponseWriter, r *http.Request) {
	user := sessionUser(r)
	if user == nil {
		jsonErr(w, "not logged in", http.StatusUnauthorized)
		return
	}
	jsonOK(w, map[string]any{
		"id":       user.ID,
		"username": user.Username,
		"is_admin": user.IsAdmin,
	})
}

// APILoginHandler accepts JSON credentials and sets a session cookie.
// Used by the media UI's inline login modal.
func APILoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if NeedsSetup() {
		jsonErr(w, "setup required — visit /dashboard/setup", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		jsonErr(w, "bad request", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(req.Username)
	ip := clientIP(r)
	if ok, wait := limiter.Allow(ip, username); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
		jsonErr(w, fmt.Sprintf("too many failed attempts — try again in %s", wait), http.StatusTooManyRequests)
		return
	}
	user, ok := checkCredentials(username, req.Password)
	if !ok {
		limiter.Fail(ip, username)
		jsonErr(w, "incorrect username or password", http.StatusUnauthorized)
		return
	}
	limiter.Succeed(ip, username)
	if err := startSession(w, user.ID); err != nil {
		slog.Error("login: could not create session", "user", user.Username, "err", err)
		jsonErr(w, "could not start a session", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{
		"id":       user.ID,
		"username": user.Username,
		"is_admin": user.IsAdmin,
	})
}

// APILogoutHandler clears the session via a JSON endpoint (no redirect).
func APILogoutHandler(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookie)
	if err == nil && db != nil {
		if err := db.DeleteSession(cookie.Value); err != nil {
			// The cookie is cleared regardless, so the browser is logged out; a
			// lingering row only wastes space and is swept by session-cleanup.
			slog.Warn("logout: could not delete session row", "err", err)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, MaxAge: -1, Path: "/"})
	jsonOK(w, map[string]string{"status": "logged out"})
}

// LogoutHandler clears the session cookie and deletes the session from the store.
func LogoutHandler(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookie)
	if err == nil && db != nil {
		if err := db.DeleteSession(cookie.Value); err != nil {
			slog.Warn("logout: could not delete session row", "err", err)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, MaxAge: -1, Path: "/"})
	http.Redirect(w, r, "/dashboard/login", http.StatusFound)
}

// ChangePasswordHandler — POST only. Changes the calling user's password.
func ChangePasswordHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		jsonErr(w, "bad request", http.StatusBadRequest)
		return
	}
	user := UserFromContext(r)
	if user == nil {
		jsonErr(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Current)) != nil {
		jsonErr(w, "current password incorrect", http.StatusUnauthorized)
		return
	}
	if len(req.New) < 8 {
		jsonErr(w, "new password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.New), bcrypt.DefaultCost)
	if err != nil {
		jsonErr(w, "error hashing password", http.StatusInternalServerError)
		return
	}
	user.PasswordHash = string(newHash)
	user.AuthSource = "local"
	if err := db.UpdateUser(user); err != nil {
		slog.Error("change password: update failed", "user", user.Username, "err", err)
		jsonErr(w, "error updating password", http.StatusInternalServerError)
		return
	}
	// A password change must invalidate every OTHER session for this user — that is the
	// whole point of changing it after a suspected compromise. The caller's own cookie is
	// preserved so they are not logged out of the tab they just used.
	keep := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		keep = c.Value
	}
	if err := db.DeleteSessionsForUser(user.ID, keep); err != nil {
		slog.Error("change password: could not invalidate other sessions", "user", user.Username, "err", err)
		jsonErr(w, "password changed, but other sessions could not be invalidated — log out everywhere manually",
			http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"status": "password changed"})
}

// authenticateUser checks credentials for a user. Local password hash takes
// precedence; if none is set and auth_source is "jellyfin", validates live
// against Jellyfin's auth API.
func authenticateUser(user *store.User, password string) bool {
	if user.PasswordHash != "" {
		return bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) == nil
	}
	if user.AuthSource == "jellyfin" && jfClient != nil {
		return jfClient.AuthenticateUser(user.Username, password) == nil
	}
	return false
}

// dummyHash is a real bcrypt hash of a value nobody knows, at the same cost as a live
// one. It is compared against when the username does not exist so that path burns the
// same ~60ms as a real check.
var dummyHash = mustHash("jellyfreedom-nonexistent-account-placeholder")

func mustHash(s string) []byte {
	h, err := bcrypt.GenerateFromPassword([]byte(s), bcrypt.DefaultCost)
	if err != nil {
		// Only possible on a bcrypt misconfiguration; a panic at init beats shipping
		// a timing oracle.
		panic("bcrypt: " + err.Error())
	}
	return h
}

// checkCredentials resolves a username and verifies the password, WITHOUT leaking via
// timing whether the username exists.
//
// The old code returned immediately for an unknown user, skipping bcrypt entirely — a
// ~60ms vs ~0ms difference that let anyone enumerate valid usernames. Now the unknown
// path performs an equivalent bcrypt compare against a dummy hash before failing.
func checkCredentials(username, password string) (*store.User, bool) {
	user, err := db.GetUserByUsername(username)
	if err != nil {
		slog.Error("lookup user for login", "err", err)
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, false
	}
	if user == nil {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, false
	}
	if !authenticateUser(user, password) {
		return nil, false
	}
	return user, true
}

// secureCookies controls the Secure flag on the session cookie. Off by default because
// the primary deployment is plain HTTP on a LAN, where Secure would make the cookie
// never be sent and log every user out instantly. Operators terminating TLS in front of
// the app turn it on. Set via SetSecureCookies from the config.
var secureCookies atomic.Bool

// SetSecureCookies enables/disables the Secure flag on the session cookie.
func SetSecureCookies(on bool) { secureCookies.Store(on) }

// startSession writes a session row and, ONLY IF that succeeded, sets the cookie.
//
// It used to discard the CreateSession error, so a failed write produced a 200 with a
// cookie pointing at a session row that did not exist: the browser was "logged in",
// every subsequent request was anonymous, and the user was bounced straight back to the
// login screen with no error anywhere. A login that cannot be recorded is a failed login.
func startSession(w http.ResponseWriter, userID int64) error {
	if db == nil {
		return errors.New("no store configured")
	}
	token, err := randomToken()
	if err != nil {
		return fmt.Errorf("generate session token: %w", err)
	}
	expires := time.Now().Add(sessionTTL)
	if err := db.CreateSession(token, userID, expires); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	// Best-effort housekeeping: failing to purge EXPIRED rows must not fail a
	// successful login. The session-cleanup task retries this on a schedule.
	if err := db.PurgeSessions(); err != nil {
		slog.Warn("purge expired sessions failed", "err", err)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Expires:  expires,
		Path:     "/",
		HttpOnly: true,
		Secure:   secureCookies.Load(),
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// randomToken returns a 192-bit hex token. crypto/rand errors are propagated rather
// than ignored — a token built from a short read would be guessable.
func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ── Auth pages ────────────────────────────────────────────────────────────────
//
// The setup/login pages are Go html/template documents living at web/auth/index.html,
// so they share the app's stylesheets instead of duplicating a slab of inline CSS that
// drifted away from the rest of the UI. They are loaded from the caller-supplied FS
// (the embedded web tree, or a --assets directory during development).
//
// Data contract (unchanged): .Mode is "setup" | "login"; .Error is a human-readable
// string, "" for none. html/template auto-escapes .Error.
//
// The form posts to its own URL as application/x-www-form-urlencoded with the fields
// the handlers above read: username, password, and password2 (setup only).

//go:embed auth_fallback.html
var authFallbackHTML string

var (
	authTmplMu sync.RWMutex
	authTmpl   = template.Must(template.New("auth").Parse(authFallbackHTML))
)

// SetAuthTemplateFS loads the auth page from the web asset tree. If the file is missing
// or will not parse we keep the built-in fallback and log loudly: an unusable login page
// would lock the operator out of their own dashboard, which is never an acceptable
// outcome for a cosmetic asset problem.
func SetAuthTemplateFS(fsys fs.FS, name string) {
	t, err := template.ParseFS(fsys, name)
	if err != nil {
		slog.Error("could not load the auth page template; using the built-in fallback",
			"path", name, "err", err)
		return
	}
	authTmplMu.Lock()
	authTmpl = t
	authTmplMu.Unlock()
}

func renderAuthPage(w http.ResponseWriter, mode, errMsg, _ string) {
	authTmplMu.RLock()
	t := authTmpl
	authTmplMu.RUnlock()

	// Render to a buffer first: writing directly means a mid-template failure emits a
	// half-page with a 200 already committed.
	var buf bytes.Buffer
	if err := t.Execute(&buf, map[string]string{"Mode": mode, "Error": errMsg}); err != nil {
		slog.Error("could not render the auth page", "mode", mode, "err", err)
		http.Error(w, "the login page could not be rendered — see the server log", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// These pages carry a credential form; never let a proxy or the browser cache them.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := buf.WriteTo(w); err != nil {
		slog.Debug("auth page write ended early", "err", err)
	}
}
