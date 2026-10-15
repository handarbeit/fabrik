package engine

// SetCommentBlockedOnInputDisabledForTest turns off the comment path's handling
// of FABRIK_BLOCKED_ON_INPUT (#2280) so a test can show its assertions bite
// (the non-vacuity seam). Per-Engine, like SetOrphanAwaitingInputClearDisabledForTest.
func (e *Engine) SetCommentBlockedOnInputDisabledForTest(disabled bool) {
	e.commentBlockedOnInputOff.Store(disabled)
}
