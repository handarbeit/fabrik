package engine

// SetSpawnMarkerFirstDisabledForTest restores the pre-#2612 ordering in
// spawnChildren for this Engine: the servability guard runs over every block's
// repo before the fabrik:spawned-child markers are read, so a hand-created
// cross-repo child can never resume the parent. It is the non-vacuity seam — a
// test shows its marker-resume assertion bites without the fix.
func (e *Engine) SetSpawnMarkerFirstDisabledForTest(disabled bool) {
	e.spawnMarkerFirstOff.Store(disabled)
}
