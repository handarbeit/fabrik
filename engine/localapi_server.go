package engine

import (
	"github.com/handarbeit/fabrik/internal/localapi"
)

// startLocalAPI binds the daemon's local read API socket (#1967, ADR-1966-a).
//
// It must run only once Run() holds the instance lock: that is what makes a
// socket file already at the path stale (left by a previous exec or crash)
// rather than owned by a live daemon, and so safe to replace. It must also run
// after e.webhookMgr is assigned, because the health view reads that field
// from the socket goroutines.
//
// Failure to bind is non-fatal, like the webhook manager: the API is a
// convenience, never a reason for the daemon not to run.
func (e *Engine) startLocalAPI() {
	path := localapi.SocketPath(e.fabrikDir)
	srv := localapi.NewServer(path, e.LocalAPIBackend(), func(format string, args ...any) {
		e.logf(0, "localapi", format, args...)
	})
	// Streaming (protocol v2, #1968) is advertised only when the channel hub
	// is running; a v1-style server is the fallback and read tools still work.
	if st := e.channelStreamer(); st != nil {
		srv.Streamer = st
	}
	if err := srv.Start(); err != nil {
		e.logf(0, "localapi", "local API unavailable (continuing without it): %v\n", err)
		return
	}
	e.localAPIMu.Lock()
	e.localAPI = srv
	e.localAPIMu.Unlock()
	e.logf(0, "localapi", "serving read API on %s\n", path)
}

// closeLocalAPI stops the local API and removes its socket. Idempotent. It is
// called on Run()'s return and explicitly before a SIGHUP re-exec — an exec
// never runs deferred cleanup, so a socket left behind would only be replaced
// by the next process's stale-socket handling, which remains the backstop for
// the self-upgrade exec and for crashes.
func (e *Engine) closeLocalAPI() {
	e.localAPIMu.Lock()
	srv := e.localAPI
	e.localAPI = nil
	e.localAPIMu.Unlock()
	if srv != nil {
		srv.Close()
	}
}
