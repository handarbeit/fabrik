package engine

// SetCleanStopResumeDisabledForTest turns the startup clean-stop resume off for
// this Engine (#2486, the non-vacuity seam: a test shows its resume assertion
// bites without it). Unlike Config.ResumeAfterCleanStop it can be flipped after
// construction.
func (e *Engine) SetCleanStopResumeDisabledForTest(disabled bool) {
	e.cleanStopResumeOff.Store(disabled)
}

// PauseInFlightForShutdownForTest performs a daemon clean stop's work without a
// signal, in the production order of beginShutdownPause: mark every in-flight
// per-issue context "daemon_shutdown", capture the in-flight snapshot, cancel
// those contexts (the stand-in for the root cancel, which tests/sim's PollOnce
// has no handle on), then run runShutdownPause synchronously so the pause labels
// and the daemon-stop marker comment are written before it returns. The workers
// wind down on their own goroutines, as they do in production.
func (e *Engine) PauseInFlightForShutdownForTest() {
	e.issueCtxs.Range(func(_, val any) bool {
		val.(issueCtxEntry).holder.val.Store("daemon_shutdown")
		return true
	})
	inFlight := e.inFlightSnapshot()
	e.issueCtxs.Range(func(_, val any) bool {
		val.(issueCtxEntry).cancel()
		return true
	})
	e.wg.Add(1)
	e.runShutdownPause(inFlight)
}
