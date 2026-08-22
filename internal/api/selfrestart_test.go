package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withSelfRestart installs a fake trigger and Restart= policy for one test and restores
// the package state afterwards. The pending flag is intentionally never cleared in
// production (the process is going away), so tests must clear it themselves.
func withSelfRestart(t *testing.T, policy string, policyErr error) (fired chan struct{}) {
	t.Helper()
	fired = make(chan struct{}, 4)

	selfRestartMu.Lock()
	oldTrigger, oldPolicy, oldDelay := selfRestartTrigger, selfRestartPolicy, selfRestartDelay
	selfRestartTrigger = func() { fired <- struct{}{} }
	selfRestartPolicy = func() (string, error) { return policy, policyErr }
	selfRestartDelay = time.Millisecond
	selfRestartPending = false
	selfRestartMu.Unlock()

	t.Cleanup(func() {
		selfRestartMu.Lock()
		selfRestartTrigger, selfRestartPolicy, selfRestartDelay = oldTrigger, oldPolicy, oldDelay
		selfRestartPending = false
		selfRestartMu.Unlock()
	})
	return fired
}

// TestSelfRestartIsSingleFlight: two rapid clicks must not stack shutdowns. The unit has
// a systemd start limit, and a spammed self-restart could trip it and leave the service
// down.
func TestSelfRestartIsSingleFlight(t *testing.T) {
	fired := withSelfRestart(t, "on-failure", nil)

	if err := RequestSelfRestart(); err != nil {
		t.Fatalf("first request: %v", err)
	}
	for i := 0; i < 3; i++ {
		err := RequestSelfRestart()
		if !errors.Is(err, ErrSelfRestartPending) {
			t.Fatalf("repeat request %d: got %v, want ErrSelfRestartPending", i+2, err)
		}
	}

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the shutdown trigger never ran")
	}
	// Exactly one shutdown, not four.
	select {
	case <-fired:
		t.Fatal("the shutdown trigger ran more than once")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestSelfRestartRefusesUnitThatWouldNotComeBack: exiting under a policy that does not
// revive us from a non-zero exit would stop the service permanently.
func TestSelfRestartRefusesUnitThatWouldNotComeBack(t *testing.T) {
	// on-success/on-abnormal/on-abort/on-watchdog do NOT fire for exit 1, so they are
	// just as fatal here as Restart=no.
	for _, policy := range []string{"no", "", "on-success", "on-abnormal", "on-abort", "on-watchdog", "weird"} {
		fired := withSelfRestart(t, policy, nil)
		err := RequestSelfRestart()
		if !errors.Is(err, ErrSelfRestartNoPolicy) {
			t.Errorf("Restart=%q: got %v, want ErrSelfRestartNoPolicy", policy, err)
		}
		select {
		case <-fired:
			t.Errorf("Restart=%q: the service was shut down anyway", policy)
		case <-time.After(20 * time.Millisecond):
		}
		selfRestartMu.Lock()
		pending := selfRestartPending
		selfRestartMu.Unlock()
		if pending {
			t.Errorf("Restart=%q: a refused request must not latch the pending flag", policy)
		}
	}
}

func TestSelfRestartAcceptsRevivingPolicies(t *testing.T) {
	for _, policy := range []string{"always", "on-failure", " on-failure "} {
		fired := withSelfRestart(t, policy, nil)
		if err := RequestSelfRestart(); err != nil {
			t.Errorf("Restart=%q: got %v, want it accepted", policy, err)
			continue
		}
		select {
		case <-fired:
		case <-time.After(2 * time.Second):
			t.Errorf("Restart=%q: the shutdown trigger never ran", policy)
		}
	}
}

// TestSelfRestartUnsupportedWithoutTrigger: a build that never registered a shutdown
// path must refuse rather than pretend.
func TestSelfRestartUnsupportedWithoutTrigger(t *testing.T) {
	selfRestartMu.Lock()
	oldTrigger, oldPolicy := selfRestartTrigger, selfRestartPolicy
	selfRestartTrigger = nil
	selfRestartPolicy = func() (string, error) { return "always", nil }
	selfRestartPending = false
	selfRestartMu.Unlock()
	t.Cleanup(func() {
		selfRestartMu.Lock()
		selfRestartTrigger, selfRestartPolicy = oldTrigger, oldPolicy
		selfRestartPending = false
		selfRestartMu.Unlock()
	})

	if err := RequestSelfRestart(); !errors.Is(err, ErrSelfRestartUnsupported) {
		t.Fatalf("got %v, want ErrSelfRestartUnsupported", err)
	}
}

// TestSelfRestartPolicyErrorPropagates: when the policy cannot be read at all, the
// caller must see the refusal rather than a silent shutdown.
func TestSelfRestartPolicyErrorPropagates(t *testing.T) {
	fired := withSelfRestart(t, "", errors.New("systemctl exploded"))
	err := RequestSelfRestart()
	if err == nil || !strings.Contains(err.Error(), "systemctl exploded") {
		t.Fatalf("got %v, want the policy error", err)
	}
	select {
	case <-fired:
		t.Fatal("the service was shut down despite an unreadable restart policy")
	case <-time.After(20 * time.Millisecond):
	}
}

// ── HTTP surface ──────────────────────────────────────────────────────────────

func postRestart(t *testing.T, svc string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/services/"+svc+"/restart", nil)
	rec := httptest.NewRecorder()
	ServiceRestartHandler(rec, req)
	return rec
}

// TestServiceRestartHandlerSelfPath: 202 first, 409 on a repeat.
func TestServiceRestartHandlerSelfPath(t *testing.T) {
	fired := withSelfRestart(t, "on-failure", nil)

	rec := postRestart(t, selfServiceName)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first POST: status %d, body %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("first POST: body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if got["status"] != "restarting" || got["service"] != selfServiceName || got["self"] != true {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
	if cl := rec.Header().Get("Content-Length"); cl == "" {
		t.Error("Content-Length must be set so the client can read the body before the listener goes away")
	}
	if !rec.Flushed {
		t.Error("the response must be flushed before the shutdown starts")
	}

	rec2 := postRestart(t, selfServiceName)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second POST: status %d, want 409; body %s", rec2.Code, rec2.Body.String())
	}
	var errBody map[string]string
	if err := json.Unmarshal(rec2.Body.Bytes(), &errBody); err != nil || errBody["error"] == "" {
		t.Errorf("second POST: want an {\"error\": ...} body, got %s", rec2.Body.String())
	}

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the shutdown trigger never ran")
	}
	select {
	case <-fired:
		t.Fatal("two POSTs produced two shutdowns")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestServiceRestartHandlerRejectsUnknownService: the generic path still validates the
// name against the hardcoded allowlist BEFORE any privileged command is built, so an
// unknown or injected name never reaches sudo.
func TestServiceRestartHandlerRejectsUnknownService(t *testing.T) {
	// No trigger registered: if the self-restart branch were reachable by any of these
	// names, the test would see 501/202 rather than 400.
	selfRestartMu.Lock()
	oldTrigger := selfRestartTrigger
	selfRestartTrigger = nil
	selfRestartMu.Unlock()
	t.Cleanup(func() {
		selfRestartMu.Lock()
		selfRestartTrigger = oldTrigger
		selfRestartMu.Unlock()
	})

	for _, svc := range []string{
		"sshd",
		"unknown-service",
		"jellyfreedom.service",
		"JELLYFREEDOM",
		"jellyfin%20",
		"..",
	} {
		rec := postRestart(t, svc)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST restart %q: status %d, want 400 (body %s)", svc, rec.Code, rec.Body.String())
		}
	}
}

func TestServiceRestartHandlerRejectsNonPOST(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/services/"+selfServiceName+"/restart", nil)
	rec := httptest.NewRecorder()
	ServiceRestartHandler(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}
