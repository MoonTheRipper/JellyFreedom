package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestApplier returns an Applier that never shells out: the helper "exists", the
// launch is a stub, and systemd is silent (the no-systemd fallback path).
func newTestApplier() (*Applier, *atomic.Int64) {
	var launches atomic.Int64
	a := NewApplier()
	a.path = "/nonexistent/jf-update"
	a.statFn = func(string) error { return nil }
	a.runner = func(context.Context, string) error { launches.Add(1); return nil }
	a.systemctl = func(context.Context, ...string) (string, error) { return "", errors.New("no systemd") }
	return a, &launches
}

// Two rapid clicks must not launch two updaters.
func TestApplyIsSingleFlight(t *testing.T) {
	a, launches := newTestApplier()
	release := make(chan struct{})
	a.runner = func(context.Context, string) error {
		launches.Add(1)
		<-release
		return nil
	}

	if err := a.Apply(); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// Wait for the goroutine to actually be inside the runner.
	waitFor(t, func() bool { return launches.Load() == 1 })

	if err := a.Apply(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second apply = %v, want ErrAlreadyRunning", err)
	}
	if got := a.Status(context.Background()).State; got != StateRunning {
		t.Errorf("state while running = %q, want %q", got, StateRunning)
	}

	close(release)
	a.Wait()
	if n := launches.Load(); n != 1 {
		t.Errorf("launched %d updaters, want exactly 1", n)
	}
	if got := a.Status(context.Background()).State; got != StateDone {
		t.Errorf("state after a clean launch = %q, want %q", got, StateDone)
	}

	// Once finished, another apply is allowed again.
	if err := a.Apply(); err != nil {
		t.Errorf("apply after completion = %v, want nil", err)
	}
	a.Wait()
}

// Many concurrent applies still launch exactly one updater (run under -race).
func TestApplyConcurrentSingleFlight(t *testing.T) {
	a, launches := newTestApplier()
	release := make(chan struct{})
	a.runner = func(context.Context, string) error { launches.Add(1); <-release; return nil }

	var ok, conflict atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.Apply(); err == nil {
				ok.Add(1)
			} else if errors.Is(err, ErrAlreadyRunning) {
				conflict.Add(1)
			}
		}()
	}
	wg.Wait()
	close(release)
	a.Wait()
	if ok.Load() != 1 || conflict.Load() != 24 {
		t.Errorf("started=%d conflicts=%d, want 1 and 24", ok.Load(), conflict.Load())
	}
	if launches.Load() != 1 {
		t.Errorf("launched %d updaters, want 1", launches.Load())
	}
}

// An install that predates the helper gets an actionable 503, not a sudo failure.
func TestApplyMissingHelper(t *testing.T) {
	a := NewApplier()
	a.path = filepath.Join(t.TempDir(), "jf-update")
	a.runner = func(context.Context, string) error {
		t.Fatal("the updater must not run when the helper is missing")
		return nil
	}
	a.systemctl = func(context.Context, ...string) (string, error) { return "", errors.New("no systemd") }

	err := a.Apply()
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("apply = %v, want ErrNotInstalled", err)
	}
	if got := a.Status(context.Background()).State; got != StateIdle {
		t.Errorf("state = %q, want idle after a refused apply", got)
	}

	// And it works once the helper appears.
	if err := os.WriteFile(a.path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.runner = func(context.Context, string) error { return nil }
	if err := a.Apply(); err != nil {
		t.Errorf("apply with the helper present = %v, want nil", err)
	}
	a.Wait()
}

// A helper that fails to launch surfaces as failed, with its message.
func TestApplyLaunchFailure(t *testing.T) {
	a, _ := newTestApplier()
	a.runner = func(context.Context, string) error { return errors.New("sudo: a password is required") }
	if err := a.Apply(); err != nil {
		t.Fatalf("apply = %v", err)
	}
	a.Wait()
	st := a.Status(context.Background())
	if st.State != StateFailed || st.Detail == "" {
		t.Errorf("status = %+v, want failed with a detail", st)
	}
}

func TestStatusIdle(t *testing.T) {
	a, _ := newTestApplier()
	if st := a.Status(context.Background()); st.State != StateIdle {
		t.Errorf("status = %+v, want idle", st)
	}
}

// systemd is the authority while the transient unit exists.
func TestStatusFromSystemd(t *testing.T) {
	cases := []struct {
		out  string
		err  error
		want string
	}{
		{"active\n", nil, StateRunning},
		{"activating\n", nil, StateRunning},
		{"failed\n", errors.New("exit 3"), StateFailed},
		{"inactive\n", errors.New("exit 3"), StateIdle}, // unit gone, nothing launched here
		{"unknown\n", errors.New("exit 4"), StateIdle},  // older/newer systemd wording
		{"", errors.New("no systemctl"), StateIdle},     // systemd not available at all
	}
	for _, c := range cases {
		a, _ := newTestApplier()
		a.systemctl = func(context.Context, ...string) (string, error) { return c.out, c.err }
		if got := a.Status(context.Background()).State; got != c.want {
			t.Errorf("is-active %q -> %q, want %q", c.out, got, c.want)
		}
	}
}

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
