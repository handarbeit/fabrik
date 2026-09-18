//go:build windows

package engine

// probeSentinelLive is unsupported on Windows: there is no portable,
// no-shell way to list every process's full argv the way `ps` does on Unix.
// Every call reports errSentinelProbeUnsupported, routing Windows entirely
// through R4's bounded-unverifiable-cycles path — the same posture
// isProcessAlive's Windows stub already takes for signal-0 liveness
// (procattr_windows.go: always reports alive, never confirms dead).
func probeSentinelLive(sentinel string) sentinelProbeResult {
	return sentinelProbeResult{Err: errSentinelProbeUnsupported}
}

// listProcessArgv mirrors probeSentinelLive's unsupported posture: there is
// no portable, no-shell way to list every process's full argv on Windows, so
// every call reports errSentinelProbeUnsupported, routing dispatchCandidates'
// batched R5 check through the same bounded-unverifiable path as the scan
// (#1779).
func listProcessArgv() ([]procArgvEntry, error) {
	return nil, errSentinelProbeUnsupported
}
