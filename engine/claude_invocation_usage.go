package engine

import "bytes"

// invocationOwnUsageDisabledForTest neutralises invocationGuardUsage so a test
// can prove the resumed-session fixtures fail without the fix (#1949). Tests
// that set it must not run in parallel and must restore it.
var invocationOwnUsageDisabledForTest bool

// invocationGuardUsage returns the usage the "real work happened" guard of
// classifyUsageLimitExit and classifyAPIErrorExit should judge (#1949).
//
// On a fresh invocation the result's turns and cost are this invocation's own,
// so usage is returned unchanged. On a --resume invocation the CLI reports the
// resumed session's cumulative figures, which would make a limit rejected
// before any work look like an invocation that did real work. Whether this
// invocation did any work is read from its own stream instead, using the same
// definition of a turn as turnCountingWriter (a type:"user" line, i.e. a tool
// result): a did-not-run limit exit produces none. With none, the cumulative
// turns and cost are zeroed so the guard cannot fire; with one or more, usage
// is returned unchanged and the guard declines exactly as before, so a limit hit
// mid-work stays unclassified (ADR-1811's retained gate, #1183).
//
// Only line types are read, never message content (ADR-1183), and no
// pre-invocation baseline is needed, so a restart cannot make it wrong.
func invocationGuardUsage(usage TokenUsage, rawOutput []byte, resumed bool) TokenUsage {
	if !resumed || invocationOwnUsageDisabledForTest {
		return usage
	}
	for _, line := range bytes.Split(rawOutput, []byte("\n")) {
		if isUserTurnLine(line) {
			return usage
		}
	}
	usage.TurnsUsed = 0
	usage.CostUSD = 0
	return usage
}
