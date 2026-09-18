package engine

import "errors"

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
