package redact

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestRegisterAndScrub(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	const key = "abcdef0123456789abcdef0123456789"
	Register(key)

	in := "Get \"http://prowlarr:9696/api/v1/search?apikey=" + key + "&query=x\": dial tcp: refused"
	out := String(in)
	if contains(out, key) {
		t.Fatalf("the secret survived redaction: %s", out)
	}
	if !contains(out, "[redacted]") {
		t.Fatalf("no mask in output: %s", out)
	}
	if !contains(out, "dial tcp: refused") {
		t.Fatalf("redaction destroyed the useful part of the message: %s", out)
	}
}

// TestRedactsRealURLError is the exact shape the audit found: *url.Error embeds the
// whole request URL, and that error string was being returned to unauthenticated callers.
func TestRedactsRealURLError(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	const key = "PROWLARRKEY0123456789"
	Register(key)

	err := &url.Error{
		Op:  "Get",
		URL: "http://127.0.0.1:9696/api/v1/search?apikey=" + key + "&query=inception",
		Err: fmt.Errorf("connection refused"),
	}
	if !contains(err.Error(), key) {
		t.Fatal("precondition: url.Error should embed the key")
	}
	if got := Error(err); contains(got, key) {
		t.Fatalf("redact.Error leaked the key: %s", got)
	}
}

func TestShortSecretsAreIgnored(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	// Registering a short or empty value would mangle unrelated output.
	Register("")
	Register("abc")
	if got := String("abc is a common substring"); got != "abc is a common substring" {
		t.Fatalf("a too-short secret was applied: %s", got)
	}
}

func TestMultipleSecretsAndIdempotentRegistration(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	Register("tmdbkey-aaaaaaaaaa")
	Register("prowlarrkey-bbbbbbbb")
	Register("tmdbkey-aaaaaaaaaa") // duplicate

	out := String("tmdbkey-aaaaaaaaaa and prowlarrkey-bbbbbbbb")
	if contains(out, "aaaaaaaaaa") || contains(out, "bbbbbbbb") {
		t.Fatalf("a secret survived: %s", out)
	}
}

func TestNilErrorAndEmptyString(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	if got := Error(nil); got != "" {
		t.Errorf("Error(nil) = %q, want empty", got)
	}
	if got := String(""); got != "" {
		t.Errorf("String(\"\") = %q, want empty", got)
	}
}

func TestConcurrentUse(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	// Settings are edited live, so Register races with String in production.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			Register(fmt.Sprintf("secret-key-%08d", i))
		}
		close(done)
	}()
	for i := 0; i < 500; i++ {
		_ = String("some message with secret-key-00000001 in it")
	}
	<-done
}

func contains(h, n string) bool { return strings.Contains(h, n) }
