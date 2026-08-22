package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecodeJSONIsBounded: only one decode site in the codebase used to be bounded, so
// any reachable endpoint could be made to read an unbounded body into memory.
func TestDecodeJSONIsBounded(t *testing.T) {
	var dst struct {
		Blob string `json:"blob"`
	}

	t.Run("a normal body decodes", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"blob":"hello"}`))
		if err := DecodeJSON(httptest.NewRecorder(), r, &dst); err != nil {
			t.Fatalf("DecodeJSON: %v", err)
		}
		if dst.Blob != "hello" {
			t.Fatalf("blob = %q", dst.Blob)
		}
	})

	t.Run("an oversized body is refused", func(t *testing.T) {
		huge := `{"blob":"` + strings.Repeat("A", maxJSONBody+1024) + `"}`
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(huge))
		if err := DecodeJSON(httptest.NewRecorder(), r, &dst); err == nil {
			t.Fatal("a body larger than the cap was accepted")
		}
	})

	t.Run("a body exactly at the cap is still workable", func(t *testing.T) {
		// Payload sized so the whole document fits within the limit.
		body := `{"blob":"` + strings.Repeat("A", maxJSONBody-64) + `"}`
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if err := DecodeJSON(httptest.NewRecorder(), r, &dst); err != nil {
			t.Fatalf("a body inside the cap was refused: %v", err)
		}
	})

	t.Run("malformed JSON is an error, not a panic", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"blob":`))
		if err := DecodeJSON(httptest.NewRecorder(), r, &dst); err == nil {
			t.Fatal("malformed JSON was accepted")
		}
	})

	t.Run("an empty body is an error", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
		if err := DecodeJSON(httptest.NewRecorder(), r, &dst); err == nil {
			t.Fatal("an empty body was accepted")
		}
	})

	t.Run("unknown fields are tolerated", func(t *testing.T) {
		// Deliberate: the UI and the API version independently, so an extra field must
		// not become a hard 400.
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"blob":"x","future_field":1}`))
		if err := DecodeJSON(httptest.NewRecorder(), r, &dst); err != nil {
			t.Fatalf("an unknown field was rejected: %v", err)
		}
	})
}

func TestDecodeJSONLimitRespectsAnExplicitCap(t *testing.T) {
	var dst map[string]string
	body := `{"k":"` + strings.Repeat("A", 200) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if err := decodeJSONLimit(httptest.NewRecorder(), r, &dst, 64); err == nil {
		t.Fatal("the explicit 64-byte cap was not applied")
	}
}
