// Package jellyfreedom exists solely to embed the web assets into the binary.
//
// "Single static binary" was previously conditional on a sibling web/ directory being
// present AND matching the binary's expectations: --assets defaulted to a relative
// "web", so an upgrade that moved, removed, or half-updated that directory produced a
// running service serving a blank or stale UI. Embedding removes that whole class of
// install failure.
//
// The embed directive must live in a package at or above the asset directory, which is
// why this file sits at the repo root rather than in cmd/orchestrator.
//
// --assets remains supported as a development override: point it at web/ and edits are
// picked up without rebuilding.
package jellyfreedom

import (
	"embed"
	"io/fs"
)

// The whole tree is embedded rather than a hardcoded file list, so restructuring web/
// (adding pages, splitting into subdirectories) needs no change here.
//
//go:embed web
var webFS embed.FS

// WebFS returns the embedded web assets rooted at the web/ directory itself, so
// "public/index.html" resolves identically whether it is served from the embedded FS
// or from a --assets directory on disk.
func WebFS() fs.FS {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		// Only reachable if the embed directive above stops matching, which is a
		// build-time fact, not a runtime condition.
		panic("jellyfreedom: embedded web assets missing: " + err.Error())
	}
	return sub
}
