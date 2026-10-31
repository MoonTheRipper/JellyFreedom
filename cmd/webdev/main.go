// Command webdev serves the media UI from a working tree while sending every API call
// to a running orchestrator, so UI changes can be watched against real data without
// rebuilding or restarting the service.
//
//	go run ./cmd/webdev                       # web/public on :1991, backend on 127.0.0.1:1990
//	go run ./cmd/webdev -listen :1991 -backend http://127.0.0.1:1990 -dir web/public
//
// A path is served from -dir when a file exists there (directories resolve to their
// index.html); anything else — /search, /api/..., /request, /dashboard/... — is
// proxied to -backend unchanged. Files are sent with Cache-Control: no-store, so a
// reload always shows the current edit.
//
// It is a development tool only: it is not part of a release and adds no
// authentication of its own. The backend still decides who is signed in, and its
// session cookie works here because browsers scope cookies by host, not by port.
package main

import (
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func main() {
	listen := flag.String("listen", ":1991", "address to serve on")
	backend := flag.String("backend", "http://127.0.0.1:1990", "orchestrator to send API calls to")
	dir := flag.String("dir", "web/public", "directory holding the media UI")
	flag.Parse()

	target, err := url.Parse(*backend)
	if err != nil || target.Scheme == "" || target.Host == "" {
		log.Fatalf("webdev: -backend %q is not an absolute URL", *backend)
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatalf("webdev: %v", err)
	}
	if st, err := os.Stat(filepath.Join(root, "index.html")); err != nil || st.IsDir() {
		log.Fatalf("webdev: %s has no index.html — run from the repo root or pass -dir", root)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	files := os.DirFS(root)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if name, ok := localFile(files, r.URL.Path); ok && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFileFS(w, r, files, name)
			return
		}
		proxy.ServeHTTP(w, r)
	})

	log.Printf("webdev: UI from %s on %s, API → %s", root, *listen, target)
	log.Fatal(http.ListenAndServe(*listen, nil))
}

// localFile maps a URL path to a regular file under the UI directory.
func localFile(fsys fs.FS, urlPath string) (string, bool) {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" {
		name = "index.html"
	}
	st, err := fs.Stat(fsys, name)
	if err == nil && st.IsDir() {
		name = path.Join(name, "index.html")
		st, err = fs.Stat(fsys, name)
	}
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("webdev: %s: %v", name, err)
		}
		return "", false
	}
	return name, st.Mode().IsRegular()
}
