package engine

// SetReviewStatusDisabledForTest turns the fabrik/review status off for this
// Engine (#2471): the scan writes nothing and the CI timers see no review wait.
// The non-vacuity seam for "the status is published" assertions.
func (e *Engine) SetReviewStatusDisabledForTest(disabled bool) {
	e.reviewStatus.off.Store(disabled)
}

// SetReviewStatusHeadBlindForTest makes the status verdict treat every review as
// a review of the current head (#2471, FR-004). The non-vacuity seam for the
// new-push reset and the "verdict on an earlier commit is not success" tests.
func (e *Engine) SetReviewStatusHeadBlindForTest(blind bool) {
	e.reviewStatus.headBlind.Store(blind)
}

// SetReviewWaitAttributionDisabledForTest turns the review-wait attribution off
// (#2471, R3): the CI wait timers and the backstop comment behave as before the
// feature, whatever the status says. The non-vacuity seam for FR-005/FR-006.
func (e *Engine) SetReviewWaitAttributionDisabledForTest(disabled bool) {
	e.reviewStatus.attributionOff.Store(disabled)
}
