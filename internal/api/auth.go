package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
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

func SetStore(s *store.Store)           { db = s }
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
func NeedsSetup() bool {
	if db == nil {
		return false
	}
	users, _ := db.ListUsers()
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
		created, _ := db.GetUserByUsername(username)
		if created != nil {
			startSession(w, created.ID)
		}
		http.Redirect(w, r, "/dashboard/", http.StatusFound)
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
		user, _ := db.GetUserByUsername(username)
		if user == nil || !authenticateUser(user, pw) {
			renderAuthPage(w, "login", "Incorrect username or password.", "")
			return
		}
		startSession(w, user.ID)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "bad request", http.StatusBadRequest)
		return
	}
	user, _ := db.GetUserByUsername(strings.TrimSpace(req.Username))
	if user == nil || !authenticateUser(user, req.Password) {
		jsonErr(w, "incorrect username or password", http.StatusUnauthorized)
		return
	}
	startSession(w, user.ID)
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
		db.DeleteSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, MaxAge: -1, Path: "/"})
	jsonOK(w, map[string]string{"status": "logged out"})
}

// LogoutHandler clears the session cookie and deletes the session from the store.
func LogoutHandler(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookie)
	if err == nil && db != nil {
		db.DeleteSession(cookie.Value)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
		jsonErr(w, "error updating password", http.StatusInternalServerError)
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

func startSession(w http.ResponseWriter, userID int64) {
	token := randomToken()
	expires := time.Now().Add(sessionTTL)
	if db != nil {
		db.CreateSession(token, userID, expires)
		db.PurgeSessions()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Expires:  expires,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func randomToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var authTmpl = template.Must(template.New("auth").Parse(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>JellyFreedom {{if eq .Mode "setup"}}Setup{{else}}Login{{end}}</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#0f1117;color:#e2e4f0;font:14px/1.5 system-ui,sans-serif;
  display:flex;align-items:center;justify-content:center;min-height:100vh}
.card{background:#1a1d27;border:1px solid #2d3148;border-radius:12px;padding:36px 40px;width:360px}
h1{font-size:20px;margin-bottom:6px}
.sub{color:#6b7094;font-size:13px;margin-bottom:28px}
label{display:block;font-size:13px;color:#6b7094;margin-bottom:6px}
input{width:100%;background:#0f1117;border:1px solid #2d3148;color:#e2e4f0;
  padding:10px 12px;border-radius:7px;font-size:14px;margin-bottom:16px;outline:none}
input:focus{border-color:#60a5fa}
button{width:100%;background:#4f46e5;border:none;color:#fff;padding:11px;border-radius:7px;
  font-size:14px;cursor:pointer;font-weight:600}
button:hover{background:#4338ca}
.err{background:#7f1d1d;color:#fca5a5;padding:10px 12px;border-radius:7px;
  font-size:13px;margin-bottom:16px}
</style>
</head>
<body>
<div class="card">
  <h1>🎬 JellyFreedom</h1>
  <p class="sub">{{if eq .Mode "setup"}}Create your admin account{{else}}Dashboard login{{end}}</p>
  {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
  <form method="POST">
    <label>Username</label>
    <input type="text" name="username" autocomplete="username" autofocus required
           {{if eq .Mode "setup"}}placeholder="Choose a username"{{end}}>
    <label>Password</label>
    <input type="password" name="password" autocomplete="{{if eq .Mode "setup"}}new-password{{else}}current-password{{end}}" required>
    {{if eq .Mode "setup"}}
    <label>Confirm password</label>
    <input type="password" name="password2" autocomplete="new-password" required>
    {{end}}
    <button>{{if eq .Mode "setup"}}Create Account{{else}}Login{{end}}</button>
  </form>
</div>
</body></html>`))

func renderAuthPage(w http.ResponseWriter, mode, errMsg, _ string) {
	w.Header().Set("Content-Type", "text/html")
	authTmpl.Execute(w, map[string]string{"Mode": mode, "Error": errMsg})
}
