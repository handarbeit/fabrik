package engine

import "errors"

// procArgvEntry is one process's PID and full argument list, as returned by
// listProcessArgvFn. A single fetch is shared across every sentinel check in
// one dispatchCandidates pass (see matchSentinelInArgvList below) rather than
// invoking `ps` once per candidate — review finding on #1779: the R5 dispatch
// guard was spawning one `ps` subprocess per dispatch-eligible item per poll,
// serially, before any item in that poll could be dispatched.
type procArgvEntry struct {
	PID  int
	Argv []string
}

// listProcessArgvFn is a package-level function-var seam (mirroring
// sentinelProbeFn) so tests can substitute a canned process table without
// spawning `ps`. Production code must never reassign this outside tests.
var listProcessArgvFn = listProcessArgv

// matchSentinelInArgvList searches a pre-fetched process table for an exact
// argv-token match (R6/Acceptance 6: whole-token equality, never substring).
// Pure and allocation-free of any subprocess — the expensive part
// (listProcessArgvFn) is meant to be called once and its result matched
// against many sentinels, not re-fetched per check.
func matchSentinelInArgvList(sentinel string, procs []procArgvEntry) sentinelProbeResult {
	if sentinel == "" {
		return sentinelProbeResult{Err: errors.New("sentinel probe: empty sentinel")}
	}
	for _, p := range procs {
		for _, tok := range p.Argv {
			if tok == sentinel {
				return sentinelProbeResult{Live: true, PID: p.PID}
			}
		}
	}
	return sentinelProbeResult{Live: false}
}

// errSentinelProbeUnsupported is returned by probeSentinelLive when the probe
// itself cannot run on this platform (e.g. Windows, where there is no
// portable argv-listing mechanism). It is one of the possible causes of
// sentinelProbeResult.Err — R4 treats every non-nil Err identically
// ("unverifiable"), regardless of whether the cause was an unsupported
// platform or a transient `ps` failure.
var errSentinelProbeUnsupported = errors.New("sentinel probe unsupported on this platform")

// sentinelProbeUnverifiableCycleLimit bounds R4's "consecutive unverifiable
// probe cycles" grace period: after this many consecutive scan cycles in
// which the probe itself failed (as opposed to affirmatively finding no
// matching process), the worker is cleared anyway, logged as unverified.
// Matches this codebase's existing default for bounded-retry counters
// (MaxToolsDeniedRetries' default of 3) — enough to absorb a single
// transient `ps` hiccup without meaningfully widening the #1303 wedge
// window. Not configurable: see ADR-1779.
const sentinelProbeUnverifiableCycleLimit = 3

// sentinelProbeResult is the outcome of a single sentinel-liveness probe.
//   - Live && Err == nil: a process carrying the sentinel token was found.
//     PID is populated when the probe could also parse out the process's PID
//     (always true for the unix implementation); callers may adopt it via
//     itemstate.WorkerPIDSet.
//   - !Live && Err == nil: no process carrying the sentinel was found. This
//     is an affirmative "not found," not a failure — R3's plain clear applies.
//   - Err != nil: the probe itself could not run (unsupported platform, `ps`
//     invocation error, non-zero exit, or timeout). Live and PID are
//     meaningless in this case. R4's bounded-cycle-count applies.
type sentinelProbeResult struct {
	Live bool
	PID  int
	Err  error
}

// sentinelProbeFn is a package-level function-var seam (mirroring
// claudeNameFlagSupported's bool-var convention elsewhere in this package) so
// tests can substitute canned probe outcomes without spawning real
// processes. Production code must never reassign this outside tests.
var sentinelProbeFn = probeSentinelLive
