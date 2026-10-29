package engine

// SetCleanStopResumeDisabledForTest turns the startup clean-stop resume off for
// this Engine (#2486, the non-vacuity seam: a test shows its resume assertion
// bites without it). Unlike Config.ResumeAfterCleanStop it can be flipped after
// construction.
func (e *Engine) SetCleanStopResumeDisabledForTest(disabled bool) {
	e.cleanStopResumeOff.Store(disabled)
}

// PauseInFlightForShutdownForTest runs the clean-stop pause of every in-flight
// item synchronously, exactly as SIGINT/SIGTERM does after the workers are
// cancelled (inFlightSnapshot + runShutdownPause), so tests/sim can drive a real
// daemon-stop pause without sending a signal.
func (e *Engine) PauseInFlightForShutdownForTest() {
	inFlight := e.inFlightSnapshot()
	e.wg.Add(1)
	e.runShutdownPause(inFlight)
}
