package engine

import (
	"fmt"

	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/warnings"
)

// localAPIWarningType and localAPIWarningSubject make the panel key
// "local_api:unavailable" (#2223, ADR 2135).
const (
	localAPIWarningType    = "local_api"
	localAPIWarningSubject = "unavailable"
)

// localAPIUnavailableCondition is the warnings-panel entry for a local API that
// failed to start. There is no FixAction: the cause is environmental (the socket
// path's directory, its permissions, a file in the way), so no single command
// applies. The fingerprint is pinned to the socket path so a varying OS error
// string cannot un-dismiss a dismissed entry.
func localAPIUnavailableCondition(path string, err error) warnings.Condition {
	return warnings.Condition{
		Type:    localAPIWarningType,
		Subject: localAPIWarningSubject,
		Title:   "Local API unavailable",
		Detail: fmt.Sprintf("The daemon could not open its local API socket at %s: %v. "+
			"The daemon keeps running, but `fabrik mcp` and the PM plugin tools will report the daemon as not running. "+
			"Check the socket path's directory and its permissions; a non-socket file at that path is never replaced, so remove it by hand if it is in the way.",
			path, err),
		Fingerprint: path,
	}
}

// startLocalAPI binds the daemon's local read API socket (#1967, ADR-1966-a).
//
// It must run only once Run() holds the instance lock: that is what makes a
// socket file already at the path stale (left by a previous exec or crash)
// rather than owned by a live daemon, and so safe to replace. It must also run
// after e.transports is assigned, because the health view reads that field
// from the socket goroutines.
//
// Failure to bind is non-fatal, like the webhook manager: the API is a
// convenience, never a reason for the daemon not to run.
func (e *Engine) startLocalAPI() {
	path := localapi.SocketPath(e.fabrikDir)
	srv := localapi.NewServer(path, e.LocalAPIBackend(), func(format string, args ...any) {
		e.logf(0, "localapi", format, args...)
	})
	srv.Version = e.cfg.Version
	// Streaming (protocol v2, #1968) is advertised only when the channel hub
	// is running; a v1-style server is the fallback and read tools still work.
	if st := e.channelStreamer(); st != nil {
		srv.Streamer = st
	}
	// The mutating action methods (#1969, ADR-1966-c) are a separate optional
	// interface, dispatched apart from the read switch.
	srv.Actor = e.overseerActor()
	// The situation view (#2171) is read-only and in-memory like Backend; it is
	// a separate optional interface so the three-method contract is unchanged.
	srv.Situation = localAPIBackend{e: e}
	if err := srv.Start(); err != nil {
		e.logf(0, "localapi", "local API unavailable (continuing without it): %v\n", err)
		// nil writer: the line above is the one log line per failure.
		if rerr := warnings.Report(nil, localAPIUnavailableCondition(path, err)); rerr != nil {
			e.logf(0, "localapi", "recording local API warning: %v\n", rerr)
		}
		return
	}
	e.localAPIMu.Lock()
	e.localAPI = srv
	e.localAPIMu.Unlock()
	e.logf(0, "localapi", "serving local API on %s\n", path)
	// The panel file outlives the process, so clear on every successful start,
	// not only after a failure in this process (SIGHUP restart, self-upgrade).
	if rerr := warnings.Resolve(localAPIWarningType + ":" + localAPIWarningSubject); rerr != nil {
		e.logf(0, "localapi", "clearing local API warning: %v\n", rerr)
	}
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
