package main

import (
	"testing"

	"jellyfreedom/internal/config"
)

func TestWebhookURLFor(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Listen: "0.0.0.0:1990", PublicURL: "http://192.168.1.10:1990/"}}
	cases := map[string]string{
		// Jellyfin on this machine: loopback, whatever public_url says.
		"http://127.0.0.1:8096": "http://127.0.0.1:1990/webhook/jellyfin",
		"http://localhost:8096": "http://127.0.0.1:1990/webhook/jellyfin",
		"http://[::1]:8096":     "http://127.0.0.1:1990/webhook/jellyfin",
		// Jellyfin elsewhere: the address the LAN knows.
		"http://192.168.1.20:8096": "http://192.168.1.10:1990/webhook/jellyfin",
		"http://jellyfin.lan":      "http://192.168.1.10:1990/webhook/jellyfin",
	}
	for jf, want := range cases {
		if got := webhookURLFor(cfg, jf); got != want {
			t.Errorf("webhookURLFor(%q) = %q, want %q", jf, got, want)
		}
	}
}
