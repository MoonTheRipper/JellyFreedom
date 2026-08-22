package update

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ── Applying an update ────────────────────────────────────────────────────────
//
// The orchestrator runs unprivileged and cannot install anything. Root work goes
// through ONE root-owned, argument-free helper — the same pattern as the netns helper
// in internal/api/privileged.go — with a single sudoers line and no wildcard:
//
//	jellyfreedom ALL=(root) NOPASSWD: /opt/jellyfreedom/jf-update
//
// The helper re-launches the real update in a SEPARATE transient systemd unit
// (systemd-run --unit=jellyfreedom-update --collect), because the update restarts
// jellyfreedom.service and systemd kills the whole control group on restart — a child
// of this process would be killed mid-install.

const (
	// UpdaterPath is the root-owned helper. Root-owned directory, root:root 0755.
	UpdaterPath = "/opt/jellyfreedom/jf-update"
	// UpdateUnit is the transient unit the helper starts.
	UpdateUnit = "jellyfreedom-update"
	// systemctlPath is deliberately absolute and deliberately /usr/bin — see the same
	// note in internal/api/privileged.go.
	systemctlPath = "/usr/bin/systemctl"

	// launchTimeout bounds the handoff to the helper. The helper only has to hand the
	// job to systemd-run and exit, so this is generous.
	launchTimeout = 30 * time.Second
	// statusTimeout bounds the systemctl probe behind GET /api/update/status.
	statusTimeout = 5 * time.Second
)

// ErrAlreadyRunning is a second apply while one is in flight (HTTP 409).
var ErrAlreadyRunning = errors.New("an update is already running")

// ErrNotInstalled means this install predates the updater helper (HTTP 503). The
// message names the fix rather than leaking a sudo failure.
var ErrNotInstalled = errors.New("the updater is not installed on this instance — run: sudo jellyfreedom repair")

// State values for GET /api/update/status.
const (
	StateIdle    = "idle"
	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed"
)

// Status is the exact JSON body of GET /api/update/status.
type Status struct {
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// Applier launches the updater, at most one at a time.
type Applier struct {
	path string

	// mu guards every field below. running is the single-flight gate: two rapid clicks
	// must not launch two updaters.
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	lastState string
	lastErr   string

	// wg lets tests wait for the background reaper.
	wg sync.WaitGroup

	// runner executes the helper. Swapped in tests so nothing ever shells out to sudo
	// from a unit test.
	runner func(ctx context.Context, path string) error
	// statFn reports whether the helper exists. Swapped in tests.
	statFn func(path string) error
	// systemctl probes the transient unit. Swapped in tests.
	systemctl func(ctx context.Context, args ...string) (string, error)
}

// NewApplier builds an Applier against the real root-owned helper.
func NewApplier() *Applier {
	return &Applier{
		path:      UpdaterPath,
		lastState: StateIdle,
		runner:    runUpdater,
		statFn:    statExecutable,
		systemctl: runSystemctl,
	}
}

// Apply starts the updater and returns immediately.
//
// It deliberately does NOT wait: the helper hands the job to systemd-run and this
// service is about to be restarted underneath us, so there is no completion to wait for
// on this side.
func (a *Applier) Apply() error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return ErrAlreadyRunning
	}
	if err := a.statFn(a.path); err != nil {
		a.mu.Unlock()
		return ErrNotInstalled
	}
	a.running = true
	a.startedAt = time.Now()
	a.lastState = StateRunning
	a.lastErr = ""
	path := a.path
	a.mu.Unlock()

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		// The background context is intentional: the HTTP request that triggered this is
		// already finished, and cancelling the launch when the client disconnects would
		// abort the update.
		ctx, cancel := context.WithTimeout(context.Background(), launchTimeout)
		defer cancel()

		err := a.runner(ctx, path)

		a.mu.Lock()
		a.running = false
		if err != nil {
			a.lastState = StateFailed
			a.lastErr = err.Error()
			slog.Error("update: the updater helper failed to launch", "err", err)
		} else {
			a.lastState = StateDone
			slog.Info("update: updater launched; this service will restart")
		}
		a.mu.Unlock()
	}()
	return nil
}

// Wait blocks until any in-flight launch has been reaped (tests only).
func (a *Applier) Wait() { a.wg.Wait() }

// runUpdater is the ONLY privileged call in this feature.
//
// INTENT, stated at the call site as required: no version, URL, path or any other value
// from the HTTP request is passed here. The command is a compile-time constant with NO
// arguments at all. The updater itself decides what to install (always the current
// published release), so a hostile request body cannot choose what runs as root, and
// the sudoers rule needs no wildcard.
func runUpdater(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, "sudo", "-n", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return err
		}
		return errors.New(msg)
	}
	return nil
}

// statExecutable reports whether the helper exists and is a regular file.
func statExecutable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return errors.New("not a file")
	}
	return nil
}

// Status reports what the update is doing, best effort.
//
// systemd is the authority while the transient unit exists. It is only "best effort"
// because the unit runs with --collect, so it disappears once it finishes either way,
// and because this process is itself restarted by the update — in-process state does
// not survive. The frontend treats an unreachable server during this window as
// "restarting", not as failure.
func (a *Applier) Status(ctx context.Context) Status {
	a.mu.Lock()
	running, state, lastErr := a.running, a.lastState, a.lastErr
	a.mu.Unlock()

	sctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()

	switch a.unitState(sctx) {
	case "active", "activating", "reloading", "deactivating":
		return Status{State: StateRunning, Detail: "the updater is running"}
	case "failed":
		detail := "the updater failed — check: journalctl -u " + UpdateUnit
		if lastErr != "" {
			detail = lastErr
		}
		return Status{State: StateFailed, Detail: detail}
	}

	// No unit (finished and collected, or never started): fall back to what this
	// process knows.
	if running {
		return Status{State: StateRunning, Detail: "starting the updater"}
	}
	switch state {
	case StateDone:
		return Status{State: StateDone, Detail: "the updater finished; the service is restarting"}
	case StateFailed:
		detail := lastErr
		if detail == "" {
			detail = "the updater failed to start"
		}
		return Status{State: StateFailed, Detail: detail}
	}
	return Status{State: StateIdle, Detail: "no update is running"}
}

// unitState returns systemd's word for the transient unit, or "" when systemd cannot
// answer (not installed, not systemd, permission denied).
//
// The exit code is deliberately ignored: `systemctl is-active` exits NON-ZERO for the
// perfectly informative answers "inactive" and "failed", so the printed word — checked
// against a closed set — is the signal, not the status code.
func (a *Applier) unitState(ctx context.Context) string {
	out, _ := a.systemctl(ctx, "is-active", UpdateUnit)
	switch s := strings.TrimSpace(out); s {
	case "active", "activating", "reloading", "deactivating", "inactive", "failed", "unknown":
		return s
	default:
		return ""
	}
}

// runSystemctl runs an unprivileged, fixed-argument systemctl query. No sudo: reading
// unit state does not need root, and no value here comes from a request.
func runSystemctl(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, systemctlPath, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
