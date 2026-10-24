package engine

import (
	"strings"
	"time"
)

// noIntakeDrainApplyDelay (a variable only so a test can shorten it) is how long the no-intake-drain fault holds each apply
// before running it. Applies normally take milliseconds, so the intake queue is
// usually empty when a SIGHUP arrives and skipping the drain would lose nothing;
// the delay guarantees a delivery is still queued at the exec, so the fault
// really does lose it (#2427).
var noIntakeDrainApplyDelay = 10 * time.Second

// TestFaultsEnv names the hidden, TEST-ONLY environment variable that disables
// one ingestion behaviour in a bed daemon so a live e2e test can be proven
// non-vacuous (#2427, ADR 2427-b). Like FABRIK_TEST_POLL_CONTROL (ADR 1978) it
// has no flag, no YAML key and no documentation in the user guide; it is read
// only by cmd/root.go and is honoured only when the poll-control seam is also
// enabled (Config.PollControlFile), which only a bed launch sets. With it unset
// every code path is byte-identical to production.
const TestFaultsEnv = "FABRIK_TEST_FAULTS"

// Fault names. A value is a comma-separated list of these (and
// "drop-event:<type>" entries).
const (
	// FaultNoDedupe applies a delivery whose ID the ledger already holds.
	FaultNoDedupe = "no-dedupe"
	// FaultNoEcho makes the echo registry never recognise an engine write's echo.
	FaultNoEcho = "no-echo"
	// FaultNoReplay turns delivery replay off.
	FaultNoReplay = "no-replay"
	// FaultNoIntakeDrain makes the SIGHUP restart and the self-upgrade exec skip
	// the intake drain/quiesce, and delays every apply by noIntakeDrainApplyDelay
	// so queued deliveries are lost to the exec.
	FaultNoIntakeDrain = "no-intake-drain"
	// FaultObservedOrdering turns the observed-at ordering rule off (ADR 2348).
	FaultObservedOrdering = "observed-ordering"
	// FaultNoHookConfigCheck skips the App webhook-config check (R7, #2143).
	FaultNoHookConfigCheck = "no-hook-config-check"
	// faultDropEventPrefix: "drop-event:<type>" skips the apply of one GitHub
	// event type after dedupe.
	faultDropEventPrefix = "drop-event:"
)

// testFaults is the parsed FABRIK_TEST_FAULTS value. The zero value (and nil)
// is inert.
type testFaults struct {
	flags map[string]bool
	drops map[string]bool
}

// parseTestFaults parses the comma-separated fault list. Unknown names are
// returned so the caller can refuse a typo loudly rather than silently run an
// un-neutralised "neutralised" leg.
func parseTestFaults(raw string) (*testFaults, []string) {
	known := map[string]bool{
		FaultNoDedupe: true, FaultNoEcho: true, FaultNoReplay: true,
		FaultNoIntakeDrain: true, FaultObservedOrdering: true, FaultNoHookConfigCheck: true,
	}
	f := &testFaults{flags: map[string]bool{}, drops: map[string]bool{}}
	var unknown []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
		case strings.HasPrefix(part, faultDropEventPrefix) && len(part) > len(faultDropEventPrefix):
			f.drops[strings.TrimPrefix(part, faultDropEventPrefix)] = true
		case known[part]:
			f.flags[part] = true
		default:
			unknown = append(unknown, part)
		}
	}
	if len(f.flags) == 0 && len(f.drops) == 0 {
		return nil, unknown
	}
	return f, unknown
}

// active reports whether the named fault is set; nil-safe.
func (f *testFaults) active(name string) bool {
	return f != nil && f.flags[name]
}

// dropsEvent reports whether applies of eventType are skipped; nil-safe.
func (f *testFaults) dropsEvent(eventType string) bool {
	return f != nil && f.drops[eventType]
}

// describe lists the active faults for the startup notice.
func (f *testFaults) describe() string {
	if f == nil {
		return ""
	}
	var out []string
	for k := range f.flags {
		out = append(out, k)
	}
	for k := range f.drops {
		out = append(out, faultDropEventPrefix+k)
	}
	return strings.Join(out, ",")
}

// installTestFaults parses cfg.TestFaults, refusing it without the poll-control
// seam, and logs one loud startup notice when any fault is active. Called from
// New().
func (e *Engine) installTestFaults() {
	if e.cfg.TestFaults == "" {
		return
	}
	if e.cfg.PollControlFile == "" {
		e.logf(0, "startup", "%s is set but ignored: test faults are honoured only on a bed (FABRIK_TEST_POLL_CONTROL unset)\n", TestFaultsEnv)
		return
	}
	f, unknown := parseTestFaults(e.cfg.TestFaults)
	if len(unknown) > 0 {
		e.logf(0, "startup", "%s: ignoring unknown fault(s) %s\n", TestFaultsEnv, strings.Join(unknown, ", "))
	}
	if f == nil {
		return
	}
	e.faults = f
	e.logf(0, "startup", "TEST FAULTS ACTIVE (%s): %s — this daemon is deliberately degraded for a non-vacuity run; never run it in production\n", TestFaultsEnv, f.describe())
	if f.active(FaultObservedOrdering) {
		e.SetObservedOrderingDisabledForTest(true)
	}
}
