// Command stub stands in for the orchestrator while tests/jellyfin/test_jellyfin_compat.sh
// runs throwaway Jellyfin servers. It serves one clip for every /play/ URL the test .strm
// files point at, accepts the PlaybackStop webhook, and appends one JSON line per request
// to a log, so the harness can count exactly what Jellyfin fetched, when, and how much.
//
// It talks to nothing else. A request Jellyfin makes here is a request that, on a real
// install, would have resolved a torrent and pulled it through the VPN.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// entry is one logged request.
type entry struct {
	Time    string          `json:"time"`
	Kind    string          `json:"kind"` // play | webhook | other
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	UA      string          `json:"ua,omitempty"`
	Range   string          `json:"range,omitempty"`
	Status  int             `json:"status"`
	Bytes   int64           `json:"bytes"`
	TokenOK *bool           `json:"token_ok,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
	// Sessions still playing the webhook's item at the moment it arrived; -1 if Jellyfin
	// could not be asked. The orchestrator makes this same query to decide whether
	// anyone else is still watching before it drops the torrent.
	PlayingAtWebhook *int `json:"playing_at_webhook,omitempty"`
}

// canonicalID is a Jellyfin id in one spelling: Jellyfin's API prints them without
// dashes, its Webhook plugin with them.
func canonicalID(id string) string {
	return strings.ToLower(strings.ReplaceAll(id, "-", ""))
}

// playingNow counts the Jellyfin sessions whose NowPlayingItem is itemID.
func playingNow(base, keyFile, itemID string) int {
	key, err := os.ReadFile(keyFile)
	if err != nil || base == "" {
		return -1
	}
	req, err := http.NewRequest(http.MethodGet, base+"/Sessions", nil)
	if err != nil {
		return -1
	}
	req.Header.Set("Authorization", `MediaBrowser Client="jfcompat-stub", Device="stub", DeviceId="jfcompat-stub", Version="1.0", Token="`+
		strings.TrimSpace(string(key))+`"`)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	var sessions []struct {
		NowPlayingItem *struct {
			ID string `json:"Id"`
		} `json:"NowPlayingItem"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&sessions) != nil {
		return -1
	}
	n := 0
	for _, s := range sessions {
		if s.NowPlayingItem != nil && canonicalID(s.NowPlayingItem.ID) == canonicalID(itemID) {
			n++
		}
	}
	return n
}

// countingWriter records the status and how many body bytes actually reached the client,
// which for a probe that hangs up early is far less than the file size.
type countingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *countingWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func main() {
	addr := flag.String("addr", "127.0.0.1:19900", "listen address")
	clip := flag.String("clip", "", "video file served for every /play/ request")
	logPath := flag.String("log", "", "file that gets one JSON line per request")
	secret := flag.String("secret", "", "X-JellyFreedom-Token the webhook must carry")
	jellyfin := flag.String("jellyfin", "", "Jellyfin base URL, to ask who is still playing when a webhook arrives")
	keyFile := flag.String("keyfile", "", "file holding the Jellyfin API key, read on each webhook")
	flag.Parse()
	if *clip == "" || *logPath == "" {
		log.Fatal("-clip and -log are required")
	}
	if _, err := os.Stat(*clip); err != nil {
		log.Fatal(err)
	}
	out, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	var mu sync.Mutex
	record := func(e entry) {
		e.Time = time.Now().UTC().Format(time.RFC3339Nano)
		b, err := json.Marshal(e)
		if err != nil {
			log.Printf("log entry: %v", err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, err := out.Write(append(b, '\n')); err != nil {
			log.Printf("write log: %v", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/play/", func(w http.ResponseWriter, r *http.Request) {
		f, err := os.Open(*clip)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cw := &countingWriter{ResponseWriter: w}
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(cw, r, "clip.mp4", st.ModTime(), f)
		record(entry{Kind: "play", Method: r.Method, Path: r.URL.Path, UA: r.UserAgent(),
			Range: r.Header.Get("Range"), Status: cw.status, Bytes: cw.bytes})
	})
	mux.HandleFunc("POST /webhook/jellyfin", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 256<<10))
		got := r.Header.Get("X-JellyFreedom-Token")
		ok := *secret != "" && subtle.ConstantTimeCompare([]byte(got), []byte(*secret)) == 1
		e := entry{Kind: "webhook", Method: r.Method, Path: r.URL.Path, UA: r.UserAgent(),
			Status: http.StatusNoContent, TokenOK: &ok}
		if json.Valid(body) {
			e.Body = body
		} else {
			e.Body, _ = json.Marshal(string(body))
		}
		var p struct {
			ItemID string `json:"ItemId"`
		}
		if *jellyfin != "" && json.Unmarshal(body, &p) == nil && p.ItemID != "" {
			n := playingNow(*jellyfin, *keyFile, p.ItemID)
			e.PlayingAtWebhook = &n
		}
		w.WriteHeader(http.StatusNoContent)
		record(e)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
		record(entry{Kind: "other", Method: r.Method, Path: r.URL.Path, UA: r.UserAgent(),
			Status: http.StatusNotFound})
	})

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("stub listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}
