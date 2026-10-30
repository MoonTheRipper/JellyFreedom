package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"jellyfreedom/internal/store"
	"jellyfreedom/internal/torrserver"
	"jellyfreedom/internal/websource"
)

func askPlaybackActive(t *testing.T, jellyfin func() (int, error)) (active, known bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	playbackActiveHandler(jellyfin)(rec, httptest.NewRequest(http.MethodGet, "/api/playback/active", nil))
	var body struct{ Active, Known bool }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body.Active, body.Known
}

// A transcode Jellyfin has no session for is still reading a /play URL. It used to be
// reported as nobody watching, so the watchdog could restart TorrServer under it.
func TestPlaybackActiveCountsStreamsJellyfinCannotSee(t *testing.T) {
	asked := false
	noSessions := func() (int, error) { asked = true; return 0, nil }

	if active, known := askPlaybackActive(t, noSessions); active || !known {
		t.Fatalf("idle: active=%v known=%v, want false/true", active, known)
	}

	done := trackStream()
	asked = false
	active, known := askPlaybackActive(t, noSessions)
	if !active || !known {
		t.Errorf("open stream, no session: active=%v known=%v, want true/true", active, known)
	}
	if asked {
		t.Error("asked Jellyfin although an open stream already answers the question")
	}
	// And it holds when Jellyfin is down, as a known answer rather than a guess.
	if active, known := askPlaybackActive(t, func() (int, error) { return 0, errors.New("down") }); !active || !known {
		t.Errorf("open stream, Jellyfin down: active=%v known=%v, want true/true", active, known)
	}

	done()
	if active, _ := askPlaybackActive(t, noSessions); active {
		t.Error("still active after the stream closed")
	}
}

func TestPlaybackActiveFallsBackToJellyfin(t *testing.T) {
	if active, known := askPlaybackActive(t, func() (int, error) { return 1, nil }); !active || !known {
		t.Errorf("session, no stream (a paused player): active=%v known=%v, want true/true", active, known)
	}
	if active, known := askPlaybackActive(t, func() (int, error) { return 0, errors.New("down") }); !active || known {
		t.Errorf("Jellyfin down, no stream: active=%v known=%v, want true/false (fail closed)", active, known)
	}
}

// heldUpstream serves the first bytes of a body, then holds the response open until
// release is closed — a player partway through a film.
func heldUpstream(t *testing.T) (srv *httptest.Server, release chan struct{}) {
	t.Helper()
	release = make(chan struct{})
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("first bytes"))
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(srv.Close)
	return srv, release
}

// assertCountedWhileOpen runs serve, which must stream from a held upstream, and checks
// that the stream is counted while bytes flow and not after.
func assertCountedWhileOpen(t *testing.T, release chan struct{}, serve func()) {
	t.Helper()
	if n := openStreams.Load(); n != 0 {
		t.Fatalf("openStreams = %d before the test", n)
	}
	finished := make(chan struct{})
	go func() { serve(); close(finished) }()

	deadline := time.Now().Add(5 * time.Second)
	for openStreams.Load() != 1 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("an open stream was never counted (openStreams = %d)", openStreams.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if active, _ := askPlaybackActive(t, func() (int, error) { return 0, nil }); !active {
		t.Error("playback/active reported nobody watching while a stream was open")
	}
	close(release)
	<-finished
	if n := openStreams.Load(); n != 0 {
		t.Errorf("openStreams = %d after the stream ended, want 0", n)
	}
}

func TestTorrentStreamIsCountedWhileOpen(t *testing.T) {
	upstream, release := heldUpstream(t)
	ts := torrserver.New(upstream.URL)
	assertCountedWhileOpen(t, release, func() {
		proxyTorrentStream(ts, httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/play/movie/1", nil),
			"0123456789abcdef0123456789abcdef01234567", 0)
	})
}

func TestWebSourceStreamIsCountedWhileOpen(t *testing.T) {
	upstream, release := heldUpstream(t)
	e := newWebEnv(t, upstream.URL+"/media.mp4")
	ws := &store.WebSource{ID: "abc", PageURL: "https://example.com/v/1"}
	assertCountedWhileOpen(t, release, func() {
		e.player.streamWebSource(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/play/p/web/abc", nil),
			ws, websource.Stream{URL: upstream.URL + "/media.mp4"})
	})
}

// A request that never reaches a stream — a bad reference, an unreachable TorrServer —
// must not leave the count raised, or the watchdog would defer restarts forever.
func TestFailedStreamIsNotCounted(t *testing.T) {
	proxyTorrentStream(torrserver.New("http://127.0.0.1:1"), httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/play/movie/1", nil), "0123456789abcdef0123456789abcdef01234567", 0)
	proxyTorrentStream(torrserver.New("http://127.0.0.1:1"), httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/play/movie/1", nil), "not-a-hash", 0)
	if n := openStreams.Load(); n != 0 {
		t.Errorf("openStreams = %d after two failed requests, want 0", n)
	}
}
