package engine

// SetRebaseLabelClearOnPendingDisabledForTest restores the merge gate leaving
// fabrik:rebase-needed in place while CI is pending on a head GitHub already
// reports as conflict-free (#2613) and returns the restore function. It is how
// the unit and sim twins show they are not vacuous.
func (e *Engine) SetRebaseLabelClearOnPendingDisabledForTest() (restore func()) {
	e.rebaseLabelPendingClearOff.Store(true)
	return func() { e.rebaseLabelPendingClearOff.Store(false) }
}
