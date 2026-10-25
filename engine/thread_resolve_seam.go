package engine

// SetThreadResolveByIdentityDisabledForTest restores the pre-#2444 handling of
// a resolved review thread on the engine's store: comments stored without a
// thread ID (those added by webhook) are not matched by the identities the
// delivery lists. It is how the sim twin shows it is not vacuous. Test seam
// only (tests/sim); production never calls this.
func (e *Engine) SetThreadResolveByIdentityDisabledForTest(off bool) {
	e.store.SetThreadResolveByIdentityDisabledForTest(off)
}
