package engine

// SetReviewReinvokeDisabledForTest suppresses the review-feedback reinvoke
// dispatch in handleReviewGate (#2170) for this engine only (#2210), returning
// the function that turns it back on. It reproduces the observable shape of
// engine defect #2160 (a feedback gate holding an item's advance while nothing
// is dispatched to address the feedback) independently of that defect, so the
// item-wedged sim test keeps reproducing the hold whether or not #2160 is
// fixed. Production never sets it.
func (e *Engine) SetReviewReinvokeDisabledForTest() (restore func()) {
	e.chanTest.reviewReinvokeOff.Store(true)
	return func() { e.chanTest.reviewReinvokeOff.Store(false) }
}
