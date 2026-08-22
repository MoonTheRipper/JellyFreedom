package update

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// ── HTTP surface ──────────────────────────────────────────────────────────────
//
// All three routes are registered on the protected mux and are therefore wrapped in
// api.RequireAdmin: an unauthenticated caller gets 401 and a non-admin 403 before any
// of this runs. Nothing here re-checks auth, and nothing here must be registered on the
// public mux — /api/update/apply runs a root helper.

// Service bundles the checker and the applier so main() wires one object.
type Service struct {
	Checker *Checker
	Applier *Applier
}

// New builds the update service for the running build's version.
func New(currentVersion string) *Service {
	return &Service{Checker: NewChecker(currentVersion), Applier: NewApplier()}
}

// Register installs the three routes on the PROTECTED mux.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/update/check", s.CheckHandler)
	mux.HandleFunc("POST /api/update/apply", s.ApplyHandler)
	mux.HandleFunc("GET /api/update/status", s.StatusHandler)
}

// CheckHandler — GET /api/update/check?refresh=1
//
// ALWAYS 200. A failed check returns available:false with a readable error, because the
// dashboard calls this on every load and must not break when GitHub is unreachable.
func (s *Service) CheckHandler(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	res := s.Checker.Check(r.Context(), refresh)
	if res.Error != "" {
		slog.Warn("update: check failed", "err", res.Error)
	}
	writeJSON(w, http.StatusOK, res)
}

// ApplyHandler — POST /api/update/apply
//
// Returns immediately; the service is about to be restarted by the updater.
func (s *Service) ApplyHandler(w http.ResponseWriter, r *http.Request) {
	// The request body is deliberately ignored. No version, URL or path from the caller
	// reaches the privileged command — see runUpdater.
	switch err := s.Applier.Apply(); {
	case err == nil:
		slog.Info("update: apply requested")
		writeJSON(w, http.StatusOK, map[string]bool{"started": true})
	case errors.Is(err, ErrAlreadyRunning):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrNotInstalled):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	default:
		slog.Error("update: apply failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start the updater"})
	}
}

// StatusHandler — GET /api/update/status
func (s *Service) StatusHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Applier.Status(r.Context()))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
