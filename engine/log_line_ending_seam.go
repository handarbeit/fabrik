package engine

// SetLogLineEndingNormalisationDisabledForTest makes logEvent write plain-text
// stdout and log-file lines exactly as formatted, without the line-ending
// normalisation of #2583. It exists so the regression tests can show they fail
// when the fix is neutralised. Per-Engine; the zero value is production.
func (e *Engine) SetLogLineEndingNormalisationDisabledForTest(off bool) {
	e.logLineEndingOff.Store(off)
}
