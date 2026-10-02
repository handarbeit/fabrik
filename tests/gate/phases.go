package gate

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/handarbeit/fabrik/tests/e2e/registry"
)

// The two-phase leg (#1977, ADR-1977). One cell is still one bed restart
// (TestSwitchTrainMode), but the `go test` that follows is split by the
// registry's isolation classes into up to three invocations, in this order:
//
//  1. shared             at the cell's -parallel (E2E_PARALLEL / E2E_PARALLEL_ON);
//  2. default-base-train at -parallel 1: the tests that assert on the default-base
//     merge-train partition, which the shared yolo pipeline tests also enqueue
//     into under train "on", so they must not overlap anything shared;
//  3. exclusive          at -parallel 1: the tests that stop, restart or poison the
//     bed. Last, so a shared test never inherits what one leaves behind (the
//     runaway guard poisoning a repo's train state for an hour) and no bed
//     restart is needed between phases.
//
// Serial-by-omission (Go runs a non-t.Parallel test to completion before
// releasing the parallel ones) used to be the whole mechanism; the registry now
// makes it explicit and checkable (registry.CheckParallelConsistency).

// Phase is one `go test` invocation of a leg.
type Phase struct {
	// Name is the phase's log/report name: "shared", "default-base-train" or
	// "exclusive". "" is the single undivided invocation used when the gate has
	// no registry to classify with (the pre-#1977 shape).
	Name string
	// Tests are the top-level tests the phase runs ("" phase: nil, the cell's own
	// -run/-skip selects them).
	Tests []string
	// Args are the `go test` arguments after ./tests/e2e/....
	Args []string
	// Parallel is the `go test -parallel` cap, passed through verbatim.
	Parallel string
}

// phaseOrder is the run order, and the parallelism rule: only shared runs wide.
var phaseOrder = []registry.Isolation{
	registry.IsolationShared,
	registry.IsolationDefaultBaseTrain,
	registry.IsolationExclusive,
}

// switchTrainModeTest is the leg's own restart step. It is exclusive in the
// registry (it reconfigures the bed) but is never part of a phase: RunLeg already
// runs it, with E2E_TRAIN_SWITCH=1, before the first phase, and inside a suite
// invocation it only self-skips.
const switchTrainModeTest = "TestSwitchTrainMode"

// single is the one undivided phase: the cell's own selection at its own -parallel.
func single(cell Cell) []Phase {
	return []Phase{{Args: cell.Args, Parallel: cell.Parallel}}
}

// PlanPhases splits the tests a cell selects into its phases. live is the
// candidate set (the full live set, or — for a retry — just the tests being
// re-run); the cell's own -run/-skip (a caller's, a --resume rewrite, the sparse
// plan's narrowing) selects from it first, so the phase split sits AFTER every
// selection mechanism and never changes which (test, leg) pairs run. Each phase
// carries one anchored -run over its tests with the cell's other arguments and any
// caller subtest filter preserved (narrowArgs).
//
// A nil classes map (no registry) or an unparsable selection yields the single
// undivided phase, so go test itself reports a bad -run. A test the registry does
// not know is treated as exclusive: the completeness test makes that unreachable,
// and running serially is the safe failure. Empty phases are omitted.
func PlanPhases(cell Cell, live []string, classes map[string]registry.Isolation) []Phase {
	if classes == nil {
		return single(cell)
	}
	sel, err := SelectedTests(live, cell.Args)
	if err != nil {
		return single(cell)
	}
	groups := map[registry.Isolation][]string{}
	for _, n := range sel {
		if n == switchTrainModeTest {
			continue
		}
		c, ok := classes[n]
		if !ok {
			c = registry.IsolationExclusive
		}
		groups[c] = append(groups[c], n)
	}
	var out []Phase
	for _, c := range phaseOrder {
		names := groups[c]
		if len(names) == 0 {
			continue
		}
		p := Phase{Name: string(c), Tests: names, Args: narrowArgs(cell.Args, names), Parallel: "1"}
		if c == registry.IsolationShared {
			p.Parallel = cell.Parallel
		}
		out = append(out, p)
	}
	return out
}

// retryPhases is PlanPhases for a #1973 retry of names: classification is
// preserved, so a retried exclusive test is re-run exclusively and a retried shared
// test at the shared parallelism. Without a registry it is the pre-#1977 retry: one
// invocation over names at the cell's -parallel.
func retryPhases(cell Cell, names []string, classes map[string]registry.Isolation) ([]Phase, error) {
	if classes == nil {
		args, err := rewriteSelection(cell.Args, names)
		if err != nil {
			return nil, err
		}
		return []Phase{{Args: args, Parallel: cell.Parallel}}, nil
	}
	return PlanPhases(cell, names, classes), nil
}

// exclusiveRan reports whether phases include an exclusive phase.
func exclusiveRan(phases []Phase) bool {
	for _, p := range phases {
		if p.Name == string(registry.IsolationExclusive) {
			return true
		}
	}
	return false
}

// hasNonExclusive reports whether phases include a phase that is not exclusive
// (the undivided no-registry phase counts: it is never exclusive-only).
func hasNonExclusive(phases []Phase) bool {
	for _, p := range phases {
		if p.Name != string(registry.IsolationExclusive) {
			return true
		}
	}
	return false
}

// phaseLogPath names a phase's log next to base: base itself for the undivided
// phase, otherwise ".../go-test.json" becomes ".../go-test.shared.json".
func phaseLogPath(base string, p Phase) string {
	if p.Name == "" {
		return base
	}
	if strings.HasSuffix(base, ".json") {
		return strings.TrimSuffix(base, ".json") + "." + p.Name + ".json"
	}
	return filepath.Clean(base) + "." + p.Name
}

func (p Phase) label() string {
	if p.Name == "" {
		return "suite"
	}
	return p.Name
}

// describePhases is the one-line plan of a leg's phases.
func describePhases(phases []Phase) string {
	parts := make([]string, len(phases))
	for i, p := range phases {
		parts[i] = fmt.Sprintf("%s (%d test(s), -parallel=%s)", p.label(), len(p.Tests), p.Parallel)
	}
	return fmt.Sprintf("%d phase(s): %s", len(phases), strings.Join(parts, ", "))
}
