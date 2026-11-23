package torrserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeSettings serves TorrServer's /settings get/set and records every set.
type fakeSettings struct {
	mu   sync.Mutex
	cur  map[string]any
	sets int
}

func (f *fakeSettings) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Action string         `json:"action"`
			Sets   map[string]any `json:"sets"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch body.Action {
		case "get":
			_ = json.NewEncoder(w).Encode(f.cur)
		case "set":
			f.sets++
			f.cur = body.Sets
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The profile the live box runs, as TorrServer reports it back (numbers decode as float64).
func liveSettings() map[string]any {
	return map[string]any{
		"CacheSize": float64(2147483648), "TorrentDisconnectTimeout": float64(90),
		"ConnectionsLimit": float64(40), "RetrackersMode": float64(1), "UseDisk": false,
		"RemoveCacheOnDrop": false, "UploadRateLimit": float64(0), "EnableDLNA": false,
	}
}

func liveProfile() CacheSettings {
	one := 1
	return CacheSettings{Mode: "ram", SizeMB: 2048, DisconnectTimeoutS: 90, ConnectionsLimit: 40, RetrackersMode: &one}
}

// A settings write makes TorrServer drop every torrent and reconnect its client, which cut
// off a stream on the live box. Re-applying the profile it already has must write nothing.
func TestApplyCacheSettingsLeavesAnUnchangedProfileAlone(t *testing.T) {
	f := &fakeSettings{cur: liveSettings()}
	changed, err := New(f.serve(t).URL).ApplyCacheSettings(liveProfile())
	if err != nil {
		t.Fatal(err)
	}
	if changed || f.sets != 0 {
		t.Fatalf("changed=%v sets=%d; an unchanged profile must not be written", changed, f.sets)
	}
}

func TestApplyCacheSettingsWritesAChange(t *testing.T) {
	f := &fakeSettings{cur: liveSettings()}
	p := liveProfile()
	p.ConnectionsLimit = 100
	changed, err := New(f.serve(t).URL).ApplyCacheSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || f.sets != 1 {
		t.Fatalf("changed=%v sets=%d; want exactly one write", changed, f.sets)
	}
	if got := f.cur["ConnectionsLimit"]; got != float64(100) {
		t.Errorf("ConnectionsLimit = %v, want 100", got)
	}
	if got := f.cur["EnableDLNA"]; got != false {
		t.Errorf("an unmanaged field was not preserved: EnableDLNA = %v", got)
	}
}
