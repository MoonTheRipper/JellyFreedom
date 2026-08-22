// Package redact scrubs registered secrets out of strings before they are logged or
// returned to a caller.
//
// It exists because Go's net/url and net/http embed the FULL request URL in every
// error they produce (*url.Error wraps it), and this codebase used to pass API keys
// as query parameters. A single `return err.Error()` on a public route was therefore
// enough to hand an unauthenticated caller the Prowlarr API key — which was verified
// against the live box.
//
// Keys now travel in headers where the upstream supports it, so this package is a
// BACKSTOP, not the primary control: it catches any future path that reintroduces a
// secret into an error, a log line, or a response body.
package redact

import (
	"strings"
	"sync"
)

const mask = "[redacted]"

// minSecretLen guards against registering a short or empty value that would mangle
// unrelated output (e.g. an empty key matching everywhere).
const minSecretLen = 8

var (
	mu      sync.RWMutex
	secrets []string
)

// Register adds a secret to be scrubbed. Safe to call repeatedly with the same value
// and safe to call concurrently — connection settings are edited live from the
// Settings UI. Values shorter than minSecretLen are ignored.
func Register(secret string) {
	secret = strings.TrimSpace(secret)
	if len(secret) < minSecretLen {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	for _, s := range secrets {
		if s == secret {
			return
		}
	}
	secrets = append(secrets, secret)
}

// String returns s with every registered secret replaced by a fixed mask.
func String(s string) string {
	if s == "" {
		return s
	}
	mu.RLock()
	defer mu.RUnlock()
	for _, sec := range secrets {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, mask)
		}
	}
	return s
}

// Error returns err.Error() with registered secrets masked. Returns "" for a nil error.
func Error(err error) string {
	if err == nil {
		return ""
	}
	return String(err.Error())
}

// Reset drops all registered secrets. Tests only.
func Reset() {
	mu.Lock()
	secrets = nil
	mu.Unlock()
}
