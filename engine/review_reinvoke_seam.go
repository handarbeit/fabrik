package engine

import "sync/atomic"

// reviewReinvokeDisabledForTest suppresses the review-feedback reinvoke dispatch
// in handleReviewGate (#2170). It reproduces the observable shape of engine
// defect #2160 (a feedback gate holding an item's advance while nothing is
// dispatched to address the feedback) independently of that defect, so the
// item-wedged sim test keeps reproducing the hold whether or not #2160 is fixed.
// Production never sets it.
var reviewReinvokeDisabledForTest atomic.Bool

// SetReviewReinvokeDisabledForTest turns the review-feedback reinvoke dispatch
// off, returning the function that turns it back on.
func SetReviewReinvokeDisabledForTest() (restore func()) {
	reviewReinvokeDisabledForTest.Store(true)
	return func() { reviewReinvokeDisabledForTest.Store(false) }
}
