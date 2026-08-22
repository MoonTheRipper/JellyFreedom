package main

import (
	"context"
	"log/slog"
	"sync/atomic"

	"jellyfreedom/internal/api"
)

// ── Restarting ourselves without root ─────────────────────────────────────────
//
// The dashboard's restart button for jellyfreedom used to be unusable: the hardened
// sudoers policy grants no `systemctl restart jellyfreedom` rule on purpose (a service
// that can bounce itself through root is a persistence primitive), so the request could
// only ever fail.
//
// The orchestrator does not need root for this. jellyfreedom.service carries
// Restart=on-failure, so a clean shutdown followed by a NON-ZERO exit makes systemd
// start it again. internal/api owns the endpoint, the single-flight guard and the
// Restart= policy check; this file supplies the shutdown itself.

// selfRestartRequested records that an admin asked for a restart, so main knows to exit
// non-zero once the normal shutdown has finished.
var selfRestartRequested atomic.Bool

// selfRestartExitCode is LOAD-BEARING — DO NOT "clean it up" to 0.
//
// It is not an error indicator. It is the mechanism: systemd's Restart=on-failure only
// fires for a non-zero exit, so exiting 0 here would shut JellyFreedom down and leave it
// down, turning the dashboard's restart button into a stop button. internal/api refuses
// the request up front unless the unit's Restart= would actually revive us from a
// non-zero exit.
const selfRestartExitCode = 1

// wireSelfRestart registers the shutdown trigger for POST /api/services/jellyfreedom/restart.
//
// stop is the cancel func from signal.NotifyContext, so calling it cancels the root
// context exactly as SIGTERM does: every background worker is cancelled, runServer
// drains in-flight requests through srv.Shutdown, and main closes the store (checkpointing
// the SQLite WAL) on the way out. A self-restart is therefore not a harder stop than a
// systemd stop — it is the same stop with a different exit code.
func wireSelfRestart(stop context.CancelFunc) {
	api.SetSelfRestart(func() {
		selfRestartRequested.Store(true)
		slog.Info("self-restart: cancelling the root context (the SIGTERM shutdown path)")
		stop()
	})
}
