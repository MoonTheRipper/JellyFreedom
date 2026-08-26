package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
func containsStr(h, n string) bool      { return strings.Contains(h, n) }

// vw builds a Viewer for a test that is not itself about library access.
//
// The user id is derived from the username so that two names are never the same viewer,
// and it is deliberately NOT a real users row: such a viewer holds no library grants at
// all. That is exactly right for the tests that use it, because every row they write
// carries an empty library_name — which the library gate exempts, for the reasons set
// out in the Library visibility block in store.go. If one of those rows ever grows a
// real library name, the test will start failing, and it should.
//
// An empty username is ANONYMOUS and gets the zero Viewer, the most restrictive one.
func vw(username string, isAdmin bool) Viewer {
	if username == "" {
		return Viewer{IsAdmin: isAdmin}
	}
	var id int64
	for _, c := range username {
		id = id*31 + int64(c)
	}
	return Viewer{UserID: id, Username: username, IsAdmin: isAdmin}
}

// mustUser creates a real user row and returns its Viewer, for the tests that DO
// exercise library access — a grant is keyed on a user id, so those tests need an id the
// users table actually issued (user_library_access has a foreign key onto it).
func mustUser(t *testing.T, s *Store, username string, isAdmin bool) Viewer {
	t.Helper()
	if err := s.CreateUser(&User{Username: username, AuthSource: "local", IsAdmin: isAdmin}); err != nil {
		t.Fatalf("CreateUser(%s): %v", username, err)
	}
	u, err := s.GetUserByUsername(username)
	if err != nil || u == nil {
		t.Fatalf("GetUserByUsername(%s): %v", username, err)
	}
	return ViewerOf(u)
}
