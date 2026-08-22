package api

import (
	"encoding/json"
	"net/http"
)

// maxJSONBody caps every JSON request body the app accepts. Only one decode site was
// bounded before; every other one would happily read an unbounded body into memory,
// so any caller who could reach an endpoint could exhaust the server's RAM.
//
// 1 MiB is far above any legitimate request here (the largest is a WireGuard config,
// which has its own tighter 64 KiB limit at its own handler).
const maxJSONBody = 1 << 20

// decodeJSON reads a bounded JSON body into dst.
//
// Unknown fields are tolerated on purpose: the web UI and the API are versioned
// independently, and rejecting an extra field would turn a harmless client-side
// addition into a hard 400. The bound, not the strictness, is the security control.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	return decodeJSONLimit(w, r, dst, maxJSONBody)
}

// DecodeJSON is decodeJSON exported for cmd/orchestrator's handlers, so every JSON
// body in the program goes through the same bound.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	return decodeJSON(w, r, dst)
}

// decodeJSONLimit is decodeJSON with an explicit byte cap.
func decodeJSONLimit(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return json.NewDecoder(r.Body).Decode(dst)
}
