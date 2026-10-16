package engine

// SetLayer1AsEngineWriteForTest makes the Layer 1 status refresh apply its read
// as an engine write again (UpdateItemStatus → LocalStatusUpdated), the pre-#2347
// behaviour, so non-vacuity tests can show the old path reproduces the bug. It is
// per-Engine; the default (off) is the read-sourced apply.
func (e *Engine) SetLayer1AsEngineWriteForTest(on bool) {
	e.layer1AsEngineWrite.Store(on)
}
