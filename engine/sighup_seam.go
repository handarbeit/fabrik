package engine

// Test seam for the SIGHUP restart tail (#2450). tests/sim is an external
// package and sighupExecFn is unexported, so this is how a scenario drives the
// real performSighupRestart ordering (transports, intake close, local API,
// channel events) with a stub in place of the exec. Production never calls it.

// SetSighupExecForTest replaces the exec performSighupRestart ends with.
func (e *Engine) SetSighupExecForTest(fn func(argv0 string, argv []string, envv []string) error) {
	e.sighupExecFn = fn
}

// PerformSighupRestartForTest runs the restart tail a SIGHUP drain ends in, minus
// the instance locks (a sim engine holds none).
func (e *Engine) PerformSighupRestartForTest() { performSighupRestart(e, nil) }
