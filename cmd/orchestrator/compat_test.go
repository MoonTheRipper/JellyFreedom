//go:build integration

package main

// Runs the dashboard's Jellyfin connection test against a real server. Skips unless
// JF_COMPAT_URL and JF_COMPAT_KEY are set; see internal/jellyfin/compat_test.go.

import (
	"context"
	"os"
	"testing"
)

func TestCompatConnectionTest(t *testing.T) {
	url, key := os.Getenv("JF_COMPAT_URL"), os.Getenv("JF_COMPAT_KEY")
	if url == "" || key == "" {
		t.Skip("JF_COMPAT_URL and JF_COMPAT_KEY are not set")
	}
	ctx := context.Background()
	if ok, msg := testConnection(ctx, "jellyfin", url, key); !ok {
		t.Errorf("connection test with the right key: %q", msg)
	}
	// A wrong key must be reported as rejected, or a pass above proves nothing.
	const rejected = "reachable, but the API key was rejected"
	if ok, msg := testConnection(ctx, "jellyfin", url, "00000000000000000000000000000000"); ok || msg != rejected {
		t.Errorf("connection test with a wrong key = %v, %q; want false, %q", ok, msg, rejected)
	}
}
