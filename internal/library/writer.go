package library

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// WriteMovieStrm writes a .strm for a movie into dir and returns its path.
// Layout: <dir>/<Title> (<Year>)/<Title> (<Year>).strm
func WriteMovieStrm(dir, title, year, streamURL string) (string, error) {
	safe := safeName(fmt.Sprintf("%s (%s)", title, year))
	d := filepath.Join(dir, safe)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", fmt.Errorf("create movie dir: %w", err)
	}
	path := filepath.Join(d, safe+".strm")
	return path, os.WriteFile(path, []byte(streamURL), 0o644)
}

// WriteTVStrm writes a .strm for a TV episode into dir and returns its path.
// Layout: <dir>/<Show> (<Year>)/Season <NN>/<Show> (<Year>) S<NN>E<NN>.strm
func WriteTVStrm(dir, show, year string, season, episode int, streamURL string) (string, error) {
	path := TVStrmPath(dir, show, year, season, episode)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create tv dir: %w", err)
	}
	return path, os.WriteFile(path, []byte(streamURL), 0o644)
}

// TVStrmPath returns the path that WriteTVStrm would write, without creating anything.
func TVStrmPath(dir, show, year string, season, episode int) string {
	showSafe := safeName(fmt.Sprintf("%s (%s)", show, year))
	episodeName := fmt.Sprintf("%s S%02dE%02d", showSafe, season, episode)
	return filepath.Join(dir, showSafe, fmt.Sprintf("Season %02d", season), episodeName+".strm")
}

// RemoveStrm deletes a .strm file and its parent directory if empty.
func RemoveStrm(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	dir := filepath.Dir(path)
	if entries, _ := os.ReadDir(dir); len(entries) == 0 {
		os.Remove(dir)
	}
	return nil
}

var unsafeChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

func safeName(s string) string {
	return strings.TrimSpace(unsafeChars.ReplaceAllString(s, ""))
}
