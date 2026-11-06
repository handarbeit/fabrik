package engine

import (
	"github.com/handarbeit/fabrik/internal/appwebhook"
)

// The replayer itself lives in internal/appwebhook (#2475, ADR 2475; replay
// design #2144, ADR 2144). This file is the engine's factory for it.

// newDeliveryReplayer builds the replayer for transport tr (log tag tag: the
// transport's name, "direct" or "tailscale"), or nil when replay is off or
// cannot work (no App auth, no intake): in those cases nothing is reported on
// the health row either.
func (e *Engine) newDeliveryReplayer(tr appwebhook.ReplayStatusSink, tag string) *appwebhook.Replayer {
	var api appwebhook.HookDeliveryAPI = e.replayAPIOverride
	if api == nil && e.ghAppAuth != nil {
		api = e.ghAppAuth
	}
	if !e.cfg.ReplayFailedDeliveries || api == nil || e.intake == nil || e.faults.active(FaultNoReplay) {
		return nil
	}
	r := appwebhook.NewReplayer(api, ledgerView{e.intake}, tr,
		func(format string, args ...any) { e.logf(0, tag, format, args...) }, e.now)
	if e.replayWaitOverride != nil {
		r.SetWait(e.replayWaitOverride)
	}
	if e.replayBuilt != nil {
		e.replayBuilt(r)
	}
	return r
}
