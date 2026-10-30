package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyfreedom/internal/jellyfin"
	"jellyfreedom/internal/store"
	"jellyfreedom/internal/torrserver"
)

// The PlaybackStop webhook never dropped a torrent: the item lookup got a 400 from every
// Jellyfin since 10.11, read the URL inside the .strm instead of its path, and the
// "anyone else watching?" check compared ids in two spellings. These drive the handler
// end to end against a fake Jellyfin and a fake TorrServer, and judge it only by what
// TorrServer was told to drop.

const (
	stoppedID  = "3c588455-2da7-836c-3b55-3c51d6d89b60" // as the Webhook plugin sends it
	stoppedAPI = "3c5884552da7836c3b553c51d6d89b60"     // as /Sessions prints it
	packHash   = "1111111111111111111111111111111111111111"
)

type fakeJellyfin struct {
	paths    map[string]string // canonical item id → Path
	playing  []string          // ids /Sessions reports as playing
	sessions int               // status for /Sessions; 0 means 200
	items    int               // status for /Items; 0 means 200
}

func (f *fakeJellyfin) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Items":
			if f.items != 0 {
				w.WriteHeader(f.items)
				return
			}
			id := strings.ToLower(strings.ReplaceAll(r.URL.Query().Get("ids"), "-", ""))
			items := []map[string]string{}
			if p, ok := f.paths[id]; ok {
				items = append(items, map[string]string{"Path": p})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": items})
		case "/Sessions":
			if f.sessions != 0 {
				w.WriteHeader(f.sessions)
				return
			}
			out := []map[string]any{{"NowPlayingItem": nil}}
			for _, id := range f.playing {
				out = append(out, map[string]any{"NowPlayingItem": map[string]string{"Id": id}})
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeTorrServer records the hash of every drop it is asked for.
type fakeTorrServer struct {
	mu      sync.Mutex
	dropped []string
}

func (f *fakeTorrServer) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Action, Hash string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path == "/torrents" && body.Action == "rem" {
			f.mu.Lock()
			f.dropped = append(f.dropped, body.Hash)
			f.mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeTorrServer) drops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dropped...)
}

// libraryWith opens a store holding one ready episode per path, all from one season pack.
func libraryWith(t *testing.T, paths ...string) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for i, p := range paths {
		if err := db.Upsert(&store.Item{TMDBID: 1399, MediaType: "tv", Title: "Show", Season: 1, Episode: i + 1,
			InfoHash: packHash, StrmPath: p, LibraryName: "Shows", Status: "ready", Updated: time.Now()}); err != nil {
			t.Fatalf("upsert %s: %v", p, err)
		}
	}
	return db
}

func runStop(t *testing.T, db *store.Store, jf *fakeJellyfin) []string {
	t.Helper()
	ts := &fakeTorrServer{}
	dropAfterPlaybackStop(db, jellyfin.New(jf.serve(t).URL, "key"), torrserver.New(ts.serve(t).URL), stoppedID)
	return ts.drops()
}

const e1 = "/srv/jellyfreedom/tv/Show/Season 01/Show S01E01.strm"

func TestPlaybackStopDropsTheTorrentWhenTheLastViewerStops(t *testing.T) {
	got := runStop(t, libraryWith(t, e1), &fakeJellyfin{paths: map[string]string{stoppedAPI: e1}})
	if len(got) != 1 || got[0] != packHash {
		t.Errorf("drops = %v, want exactly [%s]", got, packHash)
	}
}

func TestPlaybackStopKeepsTheTorrentWhileSomeoneElseWatches(t *testing.T) {
	// The other viewer's session is listed in the API's spelling, the stop in the plugin's.
	got := runStop(t, libraryWith(t, e1), &fakeJellyfin{paths: map[string]string{stoppedAPI: e1}, playing: []string{stoppedAPI}})
	if len(got) != 0 {
		t.Errorf("dropped %v while another session was still playing the item", got)
	}
}

func TestPlaybackStopKeepsTheTorrentWhenJellyfinCannotSay(t *testing.T) {
	for name, jf := range map[string]*fakeJellyfin{
		"sessions rejected": {paths: map[string]string{stoppedAPI: e1}, sessions: http.StatusUnauthorized},
		"sessions broken":   {paths: map[string]string{stoppedAPI: e1}, sessions: http.StatusInternalServerError},
		"item lookup fails": {paths: map[string]string{stoppedAPI: e1}, items: http.StatusBadRequest},
	} {
		if got := runStop(t, libraryWith(t, e1), jf); len(got) != 0 {
			t.Errorf("%s: dropped %v; an unknown answer must keep the torrent", name, got)
		}
	}
}

func TestPlaybackStopKeepsASeasonPackAnotherEpisodeNeeds(t *testing.T) {
	e2 := "/srv/jellyfreedom/tv/Show/Season 01/Show S01E02.strm"
	got := runStop(t, libraryWith(t, e1, e2), &fakeJellyfin{paths: map[string]string{stoppedAPI: e1}})
	if len(got) != 0 {
		t.Errorf("dropped %v; episode 2 is still in the library on the same torrent", got)
	}
}

func TestPlaybackStopIgnoresItemsOutsideTheLibrary(t *testing.T) {
	got := runStop(t, libraryWith(t, e1), &fakeJellyfin{paths: map[string]string{stoppedAPI: "/media/home-videos/a.mkv"}})
	if len(got) != 0 {
		t.Errorf("dropped %v for an item that is not one of ours", got)
	}
}
