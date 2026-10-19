package engine

// SetObservedOrderingDisabledForTest turns off the observed-at ordering rule on
// the engine's store (#2348, ADR 2348): no observation record is kept or
// consulted and Reconcile's removal guard is lifted, which restores the
// pre-#2348 behaviour (engine-write protection only). It is how the sim twins
// show they are not vacuous. Test seam only (tests/sim); production never calls
// this.
func (e *Engine) SetObservedOrderingDisabledForTest(off bool) {
	e.store.SetObservedOrderingDisabledForTest(off)
}
