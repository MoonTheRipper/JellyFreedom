package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"jellyfreedom/internal/config"
	"jellyfreedom/internal/jellyfin"
	"jellyfreedom/internal/store"
)

// runConfigureWebhook sets up Jellyfin's Webhook plugin for playback-stop cleanup, which
// used to be a page of manual steps where any miss failed silently. `jellyfreedom webhook`
// runs it as the service user, so the database is only ever opened by the user that owns
// it: root opening it while the service is stopped can leave journal files the service
// cannot write.
//
// It reads the Jellyfin connection and the webhook secret the running service uses, and
// writes nothing locally. The secret is never printed.
func runConfigureWebhook(args []string) int {
	fs := flag.NewFlagSet("configure-webhook", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/jellyfreedom/config.yaml", "path to config file")
	dbPath := fs.String("db", "/var/lib/jellyfreedom/jellyfreedom.db", "path to the SQLite database")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "configure-webhook: "+format+"\n", a...)
		return 1
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fail("%v", err)
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return fail("could not open %s: %v", *dbPath, err)
	}
	defer db.Close()

	// The service seeds these settings from config.yaml on every start; fall back to the
	// config the same way in case it has never run.
	setting := func(key, fallback string) (string, error) {
		v, err := db.GetSetting(key)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", key, err)
		}
		if v == "" {
			v = fallback
		}
		return v, nil
	}
	jfURL, err := setting("conn.jellyfin.url", cfg.Jellyfin.BaseURL)
	if err != nil {
		return fail("%v", err)
	}
	jfKey, err := setting("conn.jellyfin.key", cfg.Jellyfin.APIKey)
	if err != nil {
		return fail("%v", err)
	}
	secret, err := setting(webhookSecretSetting, "")
	if err != nil {
		return fail("%v", err)
	}
	if jfURL == "" || jfKey == "" {
		return fail("%v", jellyfin.ErrNotConfigured)
	}
	if secret == "" {
		return fail("there is no webhook secret yet — start the service once (sudo systemctl start jellyfreedom), it generates one")
	}

	hookURL := webhookURLFor(cfg, jfURL)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	version, err := jellyfin.New(jfURL, jfKey).ConfigureWebhook(ctx, hookURL, WebhookHeader, secret)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("Webhook %s now sends Playback Stop for movies and episodes to %s, with the shared secret.\n", version, hookURL)
	return 0
}

// webhookURLFor is the URL Jellyfin should call. A Jellyfin on this machine reaches the
// orchestrator over loopback, which keeps the call off the network and works whatever
// public_url says. One elsewhere has to use public_url, the address the LAN knows.
func webhookURLFor(cfg *config.Config, jellyfinURL string) string {
	if u, err := url.Parse(jellyfinURL); err == nil && isLoopbackHost(u.Hostname()) {
		if _, port, err := net.SplitHostPort(cfg.Server.Listen); err == nil && port != "" {
			return "http://127.0.0.1:" + port + "/webhook/jellyfin"
		}
	}
	return strings.TrimRight(cfg.Server.PublicURL, "/") + "/webhook/jellyfin"
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
