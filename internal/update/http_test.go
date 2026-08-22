package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testService(t *testing.T, current string, h http.HandlerFunc) (*Service, *http.ServeMux) {
	t.Helper()
	c, _ := newTestChecker(t, current, h)
	a, _ := newTestApplier()
	s := &Service{Checker: c, Applier: a}
	mux := http.NewServeMux()
	s.Register(mux)
	return s, mux
}

func do(t *testing.T, mux *http.ServeMux, method, target string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(method, target, nil))
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s %s: body is not JSON (%v): %s", method, target, err, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	return rr, body
}

// The contract's exact key set, so a rename breaks the test and not the dashboard.
func TestCheckHandlerShape(t *testing.T) {
	_, mux := testService(t, "0.3.0", okHandler("v0.4.0"))
	rr, body := do(t, mux, http.MethodGet, "/api/update/check")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	for _, k := range []string{"current", "latest", "available", "notes", "url", "published_at", "checked_at", "error"} {
		if _, ok := body[k]; !ok {
			t.Errorf("missing key %q in %v", k, body)
		}
	}
	if body["available"] != true {
		t.Errorf("available = %v, want true", body["available"])
	}
	if _, ok := body["notes"].([]any); !ok {
		t.Errorf("notes must be a JSON array, got %T", body["notes"])
	}
}

// A failing check is still HTTP 200: it must never break the dashboard.
func TestCheckHandlerSoftFailure(t *testing.T) {
	_, mux := testService(t, "0.3.0", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	rr, body := do(t, mux, http.MethodGet, "/api/update/check")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even on failure", rr.Code)
	}
	if body["available"] != false || body["error"] == "" {
		t.Errorf("body = %v, want available:false with an error", body)
	}
	if notes, ok := body["notes"].([]any); !ok || len(notes) != 0 {
		t.Errorf("notes = %v, want []", body["notes"])
	}
}

func TestCheckHandlerRefreshBypassesCache(t *testing.T) {
	var hits int
	_, mux := testService(t, "0.3.0", func(w http.ResponseWriter, r *http.Request) {
		hits++
		okHandler("v0.4.0")(w, r)
	})
	do(t, mux, http.MethodGet, "/api/update/check")
	do(t, mux, http.MethodGet, "/api/update/check")
	if hits != 1 {
		t.Errorf("hits = %d, want 1 (cached)", hits)
	}
	do(t, mux, http.MethodGet, "/api/update/check?refresh=1")
	if hits != 2 {
		t.Errorf("hits after refresh=1 = %d, want 2", hits)
	}
}

func TestApplyHandlerCodes(t *testing.T) {
	svc, mux := testService(t, "0.3.0", okHandler("v0.4.0"))
	release := make(chan struct{})
	svc.Applier.runner = func(context.Context, string) error { <-release; return nil }

	rr, body := do(t, mux, http.MethodPost, "/api/update/apply")
	if rr.Code != http.StatusOK || body["started"] != true {
		t.Fatalf("first apply: %d %v, want 200 {started:true}", rr.Code, body)
	}

	rr, body = do(t, mux, http.MethodPost, "/api/update/apply")
	if rr.Code != http.StatusConflict || body["error"] != ErrAlreadyRunning.Error() {
		t.Errorf("second apply: %d %v, want 409 %q", rr.Code, body, ErrAlreadyRunning)
	}
	close(release)
	svc.Applier.Wait()
}

func TestApplyHandlerNotInstalled(t *testing.T) {
	svc, mux := testService(t, "0.3.0", okHandler("v0.4.0"))
	svc.Applier.statFn = func(string) error { return errors.New("no such file") }
	rr, body := do(t, mux, http.MethodPost, "/api/update/apply")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	want := "the updater is not installed on this instance — run: sudo jellyfreedom repair"
	if body["error"] != want {
		t.Errorf("error = %q, want %q", body["error"], want)
	}
}

func TestStatusHandlerShape(t *testing.T) {
	_, mux := testService(t, "0.3.0", okHandler("v0.4.0"))
	rr, body := do(t, mux, http.MethodGet, "/api/update/status")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if body["state"] != StateIdle {
		t.Errorf("state = %v, want %q", body["state"], StateIdle)
	}
	if _, ok := body["detail"]; !ok {
		t.Errorf("missing detail in %v", body)
	}
}

// Method routing: the contract fixes the verbs.
func TestMethodRouting(t *testing.T) {
	_, mux := testService(t, "0.3.0", okHandler("v0.4.0"))
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/update/check"},
		{http.MethodGet, "/api/update/apply"},
		{http.MethodPost, "/api/update/status"},
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(c.method, c.path, nil))
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", c.method, c.path, rr.Code)
		}
	}
}
