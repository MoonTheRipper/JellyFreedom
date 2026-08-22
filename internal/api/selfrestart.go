package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── Self-restart ──────────────────────────────────────────────────────────────
//
// The dashboard offers a restart button for every managed service, jellyfreedom
// included. jellyfreedom is deliberately NOT in restartableUnits (see privileged.go):
// a service that can bounce itself through root is a persistence primitive, and the
// hardened sudoers policy grants no `restart jellyfreedom` rule, so that button could
// only ever fail with "unknown or non-restartable service".
//
// It does not need root. The unit carries Restart=on-failure, so a graceful shutdown
// followed by a non-zero exit makes systemd bring the process straight back. That is
// what this file implements: no new sudo rule, no privilege at all.
//
// The sequence is:
//
//	POST /api/services/jellyfreedom/restart
//	  → check systemd would actually revive us (below)
//	  → mark a restart pending (single-flight)
//	  → answer 202 and FLUSH it, so the client holds the confirmation
//	  → after selfRestartDelay, run the registered trigger, which cancels the root
//	    context exactly as SIGTERM does: drain HTTP, close the store, exit non-zero.

// selfServiceName is the dashboard's name for this process's own unit, and selfUnitName
// the systemd unit behind it. Only this name takes the self-restart path; every other
// service still goes through the hardcoded sudo allowlist.
const (
	selfServiceName = "jellyfreedom"
	selfUnitName    = "jellyfreedom.service"
)

// selfRestartDelay is the gap between answering the request and starting the shutdown.
// It exists so the 202 is on the wire (and through any reverse proxy) before the
// listener goes away; otherwise the dashboard sees a connection error instead of a
// confirmation. A var, not a const, so tests need not sleep for it.
var selfRestartDelay = 750 * time.Millisecond

var (
	// ErrSelfRestartPending — a restart is already in flight. Two rapid clicks must not
	// stack shutdowns: the unit has a systemd start limit, and a spammed self-restart
	// could trip it and leave the service down.
	ErrSelfRestartPending = errors.New("a restart of this service is already in progress")
	// ErrSelfRestartUnsupported — nothing registered a shutdown trigger, so this build
	// cannot restart itself (it is not the long-running server process).
	ErrSelfRestartUnsupported = errors.New("this process cannot restart itself")
	// ErrSelfRestartNoPolicy — systemd would NOT bring the unit back, so shutting down
	// would stop the service permanently. Refuse rather than kill it.
	ErrSelfRestartNoPolicy = errors.New("systemd would not restart this service automatically")
)

var (
	selfRestartMu      sync.Mutex
	selfRestartPending bool
	selfRestartTrigger func()
	// selfRestartPolicy reads the unit's Restart= setting. A var so tests can substitute
	// it without a systemd on the machine running them.
	selfRestartPolicy = systemdRestartPolicy
)

// SetSelfRestart registers the shutdown trigger used by the self-restart endpoint.
//
// The trigger must perform the NORMAL shutdown — the same path SIGTERM takes — and must
// end in a non-zero exit. cmd/orchestrator registers one that cancels the root context
// created by signal.NotifyContext, so HTTP drains and the SQLite store is closed
// (checkpointing the WAL) exactly as on a systemd stop. A self-restart must never be a
// harder stop than SIGTERM.
func SetSelfRestart(trigger func()) {
	selfRestartMu.Lock()
	defer selfRestartMu.Unlock()
	selfRestartTrigger = trigger
}

// RequestSelfRestart validates and schedules a self-restart. It returns immediately;
// the shutdown happens selfRestartDelay later, on its own goroutine.
//
// Errors are the sentinels above, so the HTTP layer can map them to status codes.
func RequestSelfRestart() error {
	selfRestartMu.Lock()
	defer selfRestartMu.Unlock()

	if selfRestartTrigger == nil {
		return ErrSelfRestartUnsupported
	}
	// Single-flight. The flag is never cleared: the process is on its way out, and a
	// second shutdown would only race the first. If the shutdown somehow fails to end
	// the process, the operator still has `systemctl restart` on the host.
	if selfRestartPending {
		return ErrSelfRestartPending
	}
	policy, err := selfRestartPolicy()
	if err != nil {
		return err
	}
	if !restartPolicyRevives(policy) {
		return fmt.Errorf("%w: the unit has Restart=%s, so exiting would stop it for good "+
			"— set Restart=on-failure (or always) in %s and reload systemd",
			ErrSelfRestartNoPolicy, policy, selfUnitName)
	}

	selfRestartPending = true
	trigger := selfRestartTrigger
	delay := selfRestartDelay
	go func() {
		time.Sleep(delay)
		slog.Warn("self-restart requested: shutting down gracefully; systemd will bring the service back",
			"unit", selfUnitName, "restart_policy", policy)
		trigger()
	}()
	return nil
}

// restartPolicyRevives reports whether systemd would restart the unit after a NON-ZERO
// exit, which is the only exit a self-restart can produce.
//
// Be exact about this rather than accepting anything that is not "no": on-abnormal fires
// only for signals/timeouts, on-abort only for an uncaught signal, on-watchdog only for
// a watchdog timeout, and on-success only for exit 0. None of them would bring us back
// from exit 1, so treating them as good enough would silently stop the service.
func restartPolicyRevives(policy string) bool {
	switch strings.TrimSpace(policy) {
	case "always", "on-failure":
		return true
	default:
		return false
	}
}

// systemdRestartPolicy reads Restart= for this unit. `systemctl show` is a cheap,
// unprivileged read — the dashboard already uses it for every service's status.
//
// Failure handling is deliberately asymmetric:
//   - An unknown unit reports "no", which fails the check. That is the safe answer.
//   - If the query itself fails we cannot tell. When systemd is supervising us
//     (INVOCATION_ID is set in a unit's environment) we refuse, because a wrong guess
//     there means a production service that never comes back. Otherwise we allow it:
//     nothing is supervising the process, so exiting is no worse than the developer
//     pressing Ctrl-C on their own foreground run.
func systemdRestartPolicy() (string, error) {
	out, err := exec.Command("systemctl", "show", "-p", "Restart", "--value", selfUnitName).Output()
	if err != nil {
		if os.Getenv("INVOCATION_ID") != "" {
			return "", fmt.Errorf("%w: could not read the unit's Restart= setting (%v), and this "+
				"process is running under systemd — refusing rather than risk stopping it for good",
				ErrSelfRestartNoPolicy, err)
		}
		slog.Warn("self-restart: could not read the unit's Restart= setting; this process does not "+
			"appear to be running under systemd, so allowing the shutdown", "err", err)
		return "not-supervised", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// selfRestartStatus maps a RequestSelfRestart error to its HTTP status.
func selfRestartStatus(err error) int {
	switch {
	case errors.Is(err, ErrSelfRestartPending):
		return http.StatusConflict
	case errors.Is(err, ErrSelfRestartUnsupported):
		return http.StatusNotImplemented
	case errors.Is(err, ErrSelfRestartNoPolicy):
		return http.StatusPreconditionFailed
	default:
		return http.StatusInternalServerError
	}
}

// handleSelfRestart answers POST /api/services/jellyfreedom/restart.
func handleSelfRestart(w http.ResponseWriter) {
	if err := RequestSelfRestart(); err != nil {
		code := selfRestartStatus(err)
		if code == http.StatusConflict {
			slog.Info("self-restart already pending; ignoring the repeat request")
		} else {
			slog.Error("self-restart refused", "err", err)
		}
		jsonErr(w, err.Error(), code)
		return
	}
	// 202, not 200: the restart has been accepted, not completed. Flushed before we
	// return so the client has it in hand before the listener disappears.
	writeFlushed(w, http.StatusAccepted, map[string]any{
		"status":  "restarting",
		"service": selfServiceName,
		"self":    true,
		"detail": "JellyFreedom is shutting down and systemd will start it again; " +
			"the dashboard will be unreachable for a few seconds",
	})
}

// writeFlushed writes a JSON response with an explicit Content-Length and pushes it out
// of the server's buffers immediately.
//
// The default path buffers, sniffs and may chunk the body, and none of that is
// guaranteed to reach the socket before the process starts tearing the listener down.
func writeFlushed(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		jsonErr(w, "internal error", http.StatusInternalServerError)
		return
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	_, _ = w.Write(body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
