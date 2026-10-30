package engine

// SetCompleteLabelSkipDisabledForTest restores the CI gate clear's unconditional
// add of an already-present completion label (#2530) and returns the restore
// function. It is how the sim and engine twins show they are not vacuous.
func (e *Engine) SetCompleteLabelSkipDisabledForTest() (restore func()) {
	e.completeLabelSkipOff.Store(true)
	return func() { e.completeLabelSkipOff.Store(false) }
}

// SetPeriodicActivityNarrowingDisabledForTest makes an admission caused only by an
// expired periodic re-evaluation count as poll activity again (#2530), and returns
// the restore function.
func (e *Engine) SetPeriodicActivityNarrowingDisabledForTest() (restore func()) {
	e.periodicActivityNarrowingOff.Store(true)
	return func() { e.periodicActivityNarrowingOff.Store(false) }
}
