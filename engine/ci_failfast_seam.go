package engine

// SetFailFastPromotionDisabledForTest turns off the promotion of a
// fail-fast-cancelled run with a failed step to a failure (#2614), restoring
// the ADR 2296 classification, and returns the restore function. It is how the
// sim and engine twins show they are not vacuous.
func (e *Engine) SetFailFastPromotionDisabledForTest() (restore func()) {
	e.failFastOff.Store(true)
	return func() { e.failFastOff.Store(false) }
}
