package torrserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// seederScript serves connected_seeders from a script, one value per Stat call, and
// repeats the last one once the script runs out.
func seederScript(t *testing.T, script ...int) (*Client, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1)) - 1
		if n >= len(script) {
			n = len(script) - 1
		}
		_, _ = fmt.Fprintf(w, `{"hash":"aa","connected_seeders":%d,"active_peers":%d}`, script[n], script[n])
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL), &calls
}

func TestLiveSeedersStopsAsSoonAsTheSwarmIsStrongEnough(t *testing.T) {
	c, calls := seederScript(t, 3, 7, 12, 30)
	start := time.Now()
	got := c.LiveSeeders(context.Background(), "aa", 10, 5*time.Second, 10*time.Millisecond)
	if got != 12 {
		t.Fatalf("LiveSeeders = %d, want 12 (the first reading at or above 10)", got)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("polled %d times, want 3: it must stop at the first strong reading", n)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("a strong swarm waited out the window")
	}
}

// A weak swarm is judged by the best it managed, not its last reading: peers come and go.
func TestLiveSeedersReportsTheBestReadingOfAWeakSwarm(t *testing.T) {
	c, _ := seederScript(t, 1, 6, 2, 3)
	got := c.LiveSeeders(context.Background(), "aa", 10, 200*time.Millisecond, 10*time.Millisecond)
	if got != 6 {
		t.Fatalf("LiveSeeders = %d, want the best reading 6", got)
	}
}

func TestLiveSeedersHonoursContext(t *testing.T) {
	c, _ := seederScript(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got := c.LiveSeeders(ctx, "aa", 10, time.Minute, 10*time.Millisecond); got != 0 {
		t.Fatalf("LiveSeeders = %d, want 0", got)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("LiveSeeders ignored the context deadline")
	}
}
