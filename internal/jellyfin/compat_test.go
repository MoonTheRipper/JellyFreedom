//go:build integration

package jellyfin

// These run against a real Jellyfin server and skip themselves unless JF_COMPAT_URL is
// set. tests/jellyfin/test_jellyfin_compat.sh starts throwaway servers and sets all of it;
// by hand:
//
//	JF_COMPAT_URL=http://127.0.0.1:18096 JF_COMPAT_KEY=… go test -tags integration ./internal/jellyfin/
//
//	JF_COMPAT_USER, JF_COMPAT_PASS   a user to log in as (AuthenticateUser, session test)
//	JF_COMPAT_ITEM, JF_COMPAT_STRM   a .strm item's id and its path on disk
//	JF_COMPAT_SCAN=1                 also start a library scan
//
// Everything is read-only except the scan and the session test, which reports a play
// start and stop as JF_COMPAT_USER — and so fires the server's PlaybackStop webhook.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// compatEnv returns the named variables, skipping the test if any is unset.
func compatEnv(t *testing.T, names ...string) []string {
	t.Helper()
	vals := make([]string, len(names))
	for i, n := range names {
		vals[i] = os.Getenv(n)
		if vals[i] == "" {
			t.Skipf("%s is not set", n)
		}
	}
	return vals
}

func compatClient(t *testing.T) *Client {
	t.Helper()
	env := compatEnv(t, "JF_COMPAT_URL", "JF_COMPAT_KEY")
	return New(env[0], env[1])
}

func TestCompatPing(t *testing.T) {
	if err := compatClient(t).Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// A check that passes with any key proves nothing, so make sure the server refuses a
// wrong one before believing the rest.
func TestCompatRejectsWrongKey(t *testing.T) {
	env := compatEnv(t, "JF_COMPAT_URL")
	c := New(env[0], "00000000000000000000000000000000")
	if err := c.Ping(context.Background()); err == nil {
		t.Error("Ping succeeded with a wrong key: this server is not enforcing auth, so no other check here means anything")
	}
	if _, err := c.ListUsers(); err == nil {
		t.Error("ListUsers succeeded with a wrong key")
	}
}

func TestCompatListUsers(t *testing.T) {
	users, err := compatClient(t).ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) == 0 {
		t.Fatal("ListUsers returned no users")
	}
	if want := os.Getenv("JF_COMPAT_USER"); want != "" {
		for _, u := range users {
			if u.Name == want && u.ID != "" {
				return
			}
		}
		t.Errorf("ListUsers = %+v, want a user named %q with an id", users, want)
	}
}

func TestCompatAuthenticateUser(t *testing.T) {
	env := compatEnv(t, "JF_COMPAT_USER", "JF_COMPAT_PASS")
	c := compatClient(t)
	id, err := c.AuthenticateUser(env[0], env[1])
	if err != nil {
		t.Fatalf("AuthenticateUser: %v", err)
	}
	if id == "" {
		t.Fatal("AuthenticateUser returned an empty user id")
	}
	if _, err := c.AuthenticateUser(env[0], env[1]+"-wrong"); err == nil {
		t.Error("AuthenticateUser accepted a wrong password")
	}
}

// The scan must find the task by name. If it cannot, TriggerLibraryScan quietly falls
// back to /Library/Refresh, which does not pick up new .strm files at all.
func TestCompatScanTask(t *testing.T) {
	c := compatClient(t)
	id, err := c.scanTaskID()
	if err != nil || id == "" {
		t.Fatalf("scanTaskID = %q, %v: the scan would fall back to /Library/Refresh", id, err)
	}
	if os.Getenv("JF_COMPAT_SCAN") == "1" {
		if err := c.TriggerLibraryScan(); err != nil {
			t.Fatalf("TriggerLibraryScan: %v", err)
		}
	}
}

func TestCompatActivePlaybackCount(t *testing.T) {
	if _, err := compatClient(t).ActivePlaybackCount(); err != nil {
		t.Fatalf("ActivePlaybackCount: %v", err)
	}
}

// The webhook looks the stopped item up by its .strm path, so GetItemPath has to return
// exactly that path, not the URL inside the file.
func TestCompatGetItemPath(t *testing.T) {
	env := compatEnv(t, "JF_COMPAT_ITEM", "JF_COMPAT_STRM")
	got, err := compatClient(t).GetItemPath(env[0])
	if err != nil {
		t.Fatalf("GetItemPath: %v", err)
	}
	if got != env[1] {
		t.Errorf("GetItemPath = %q, want the .strm path %q: a PlaybackStop for this item would never drop its torrent", got, env[1])
	}
}

// Reports a play start and stop as a real user and checks both counters follow, since
// the webhook decides whether to drop a torrent from ActiveSessionsForItem.
func TestCompatSessionLifecycle(t *testing.T) {
	env := compatEnv(t, "JF_COMPAT_URL", "JF_COMPAT_USER", "JF_COMPAT_PASS", "JF_COMPAT_ITEM")
	base, item := env[0], env[3]
	c := compatClient(t)
	token := compatLogin(t, base, env[1], env[2])

	progress := map[string]any{"ItemId": item, "PlaySessionId": "jfcompat", "PositionTicks": 0,
		"CanSeek": true, "IsPaused": false, "PlayMethod": "DirectPlay"}
	compatPost(t, base, token, "/Sessions/Playing", progress)
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			progress["PositionTicks"] = 10_000_000
			compatPost(t, base, token, "/Sessions/Playing/Stopped", progress)
		}
	}
	t.Cleanup(stop)

	waitFor(t, "ActivePlaybackCount to reach 1 after a play start", func() bool {
		n, err := c.ActivePlaybackCount()
		return err == nil && n == 1
	})
	// The webhook hands over the dashed spelling; /Items gave us the plain one.
	for _, id := range []string{item, compatDashed(item)} {
		if n, err := c.ActiveSessionsForItem(id); err != nil || n != 1 {
			t.Errorf("ActiveSessionsForItem(%q) = %d, %v while it is playing, want 1", id, n, err)
		}
	}
	stop()
	waitFor(t, "ActivePlaybackCount to return to 0 after a play stop", func() bool {
		n, err := c.ActivePlaybackCount()
		return err == nil && n == 0
	})
	if n, err := c.ActiveSessionsForItem(compatDashed(item)); err != nil || n != 0 {
		t.Errorf("ActiveSessionsForItem = %d, %v after it stopped, want 0", n, err)
	}
}

// compatDashed spells a 32-digit Jellyfin id the way the Webhook plugin does.
func compatDashed(id string) string {
	if len(id) != 32 {
		return id
	}
	return id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
}

// compatAuth is the MediaBrowser header for the test's own session. It is built here
// rather than borrowed from the client so this file compiles against any version of it.
func compatAuth(token string) string {
	h := `MediaBrowser Client="jfcompat", Device="compat-test", DeviceId="jfcompat-session", Version="1.0"`
	if token != "" {
		h += `, Token="` + token + `"`
	}
	return h
}

func compatLogin(t *testing.T, base, user, pass string) string {
	t.Helper()
	var resp struct {
		AccessToken string `json:"AccessToken"`
	}
	body := compatPost(t, base, "", "/Users/AuthenticateByName", map[string]string{"Username": user, "Pw": pass})
	if err := json.Unmarshal(body, &resp); err != nil || resp.AccessToken == "" {
		t.Fatalf("log in as %s: %v (%s)", user, err, body)
	}
	return resp.AccessToken
}

func compatPost(t *testing.T, base, token, path string, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", compatAuth(token))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("POST %s: status %d: %s", path, resp.StatusCode, out.Bytes())
	}
	return out.Bytes()
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
