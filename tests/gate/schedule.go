package gate

import (
	"context"
	"fmt"
	"strings"

	"github.com/handarbeit/fabrik/tests/e2e/registry"
)

// Cell is one leg of the gate: one bed restart (TestSwitchTrainMode) followed
// by one `go test` invocation over ./tests/e2e/.... Today the gate is a serial
// auth × train matrix; #1975 (sparse matrix) and #1976 (multi-bed) drive Cells
// from a smarter Scheduler, and #1972's ledger keys on them.
type Cell struct {
	Auth  string // "pat" | "app"
	Train string // the E2E_TRAIN_MODE for the leg: "off" | "on" (passed through verbatim)
	// Parallel is the `go test -parallel` cap, a string passed through verbatim.
	Parallel string
	// Args are the `go test` arguments after ./tests/e2e/... — the isolation
	// flags (-skip/-run) and/or the caller's own passthrough arguments.
	Args []string
	// Isolated marks the leg that runs only the TrainIsolatedRE scenarios.
	Isolated bool
}

// Label is the report label: auth mode / train mode, e.g. "app/on" (#1861).
func (c Cell) Label() string { return c.Auth + "/" + c.Train }

// PlanInput is what the cell plan depends on.
type PlanInput struct {
	AuthModes []string
	// TrainMode is E2E_TRAIN_MODE: when set the caller forces a single mode.
	TrainMode string
	// CallerHasRun: the caller supplied -run/--run, so they are targeting
	// specific scenarios; honour that exactly rather than forcing an isolated
	// leg they did not ask for.
	CallerHasRun bool
	Parallel     string // E2E_PARALLEL
	ParallelOn   string // E2E_PARALLEL_ON: the default gate's "on" leg cap only
	// Args are the caller's passthrough arguments (--clean already consumed).
	Args []string
	// Sparse, when non-nil, plans the sparse auth × train matrix (#1975,
	// ADR-1975) instead of the full one. nil keeps today's full matrix.
	Sparse *SparseInput
}

// SparseInput is what the sparse plan needs to know about the live suite: the
// live set (the registry completeness test keeps it equal to the registry) and
// each test's registry entry.
type SparseInput struct {
	Live    []string
	Entries map[string]registry.Entry
}

func isolatedRunArg() string { return "^(" + TrainIsolatedRE + ")$" }

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// PlanCells is the auth × train loop at the bottom of run.sh, as data.
//
// The train-mode legs run once per auth mode, pat first — the long-proven path,
// so a regression there surfaces before the App legs spend anything. Within an
// auth mode:
//
//   - E2E_TRAIN_MODE set: a single forced mode, always at E2E_PARALLEL. "on"
//     without a caller -run is split into a main leg (-skip the isolated
//     scenarios) and the isolated leg, both at E2E_PARALLEL.
//   - unset (the default gate): "off" first (the path nearly all real usage
//     takes, so a regression there surfaces before the less-common train-on
//     run), then "on" at the tighter E2E_PARALLEL_ON cap, split the same way
//     unless the caller supplied -run.
//
// The isolated leg never receives the caller's passthrough arguments. The bed
// restarts between legs, which also clears the runaway guard's in-memory state,
// so the isolation is explicit, not merely dependent on ordering.
func PlanCells(p PlanInput) []Cell {
	if p.Sparse != nil && (p.TrainMode == "" || p.TrainMode == "on" || p.TrainMode == "off") {
		return planSparse(p)
	}
	var cells []Cell
	for _, auth := range p.AuthModes {
		mk := func(train, parallel string, args []string, isolated bool) Cell {
			return Cell{Auth: auth, Train: train, Parallel: parallel, Args: args, Isolated: isolated}
		}
		switch {
		case p.TrainMode != "" && p.TrainMode == "on" && !p.CallerHasRun:
			cells = append(cells,
				mk("on", p.Parallel, cat([]string{"-skip", TrainIsolatedRE}, p.Args), false),
				mk("on", p.Parallel, []string{"-run", isolatedRunArg()}, true))
		case p.TrainMode != "":
			cells = append(cells, mk(p.TrainMode, p.Parallel, p.Args, false))
		default:
			cells = append(cells, mk("off", p.Parallel, p.Args, false))
			if !p.CallerHasRun {
				cells = append(cells,
					mk("on", p.ParallelOn, cat([]string{"-skip", TrainIsolatedRE}, p.Args), false),
					mk("on", p.ParallelOn, []string{"-run", isolatedRunArg()}, true))
			} else {
				cells = append(cells, mk("on", p.ParallelOn, p.Args, false))
			}
		}
	}
	return cells
}

// Scheduler runs a plan's cells. SerialScheduler is today's behaviour; #1975
// (sparse matrix) and #1976 (multi-bed) replace it without touching Gate.Run.
type Scheduler interface {
	Run(ctx context.Context, g *Gate, cells []Cell) error
}

// SerialScheduler runs the cells one after another on the one bed. The first
// failing leg ends the run with that leg's exit code, exactly as `set -e` did:
// the remaining legs never run, and the shared bed is left in whatever train
// mode that leg left it (NOT restored to its prior mode).
type SerialScheduler struct{}

func (SerialScheduler) Run(ctx context.Context, g *Gate, cells []Cell) error {
	lastAuth := ""
	for _, c := range cells {
		if c.Auth != lastAuth {
			g.outf("== auth leg: %s ==\n", c.Auth)
			lastAuth = c.Auth
		}
		if err := g.runLeg(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

func (g *Gate) runLeg(ctx context.Context, c Cell) error {
	if g.runLegFn != nil {
		return g.runLegFn(ctx, c)
	}
	return g.RunLeg(ctx, c)
}

// The sparse plan's cells in run order: the baseline first (so a regression in
// the configuration the community runs surfaces before anything else spends),
// grouped by auth so SerialScheduler's "auth leg" banner stays accurate.
var sparseOrder = []struct{ auth, train string }{
	{"app", "on"},  // baseline: every live test
	{"app", "off"}, // other train mode, same auth: train-sensitive tests
	{"pat", "on"},  // other auth mode, same train: auth-sensitive tests
	{"pat", "off"}, // diagonal: tests sensitive on both axes
}

const (
	baselineAuth  = "app"
	baselineTrain = "on"
)

// sparseSelection returns the live tests the (auth, train) cell must run:
// every test in the baseline; otherwise the tests sensitive to every axis the
// cell changes relative to the baseline. A test with no registry entry is
// treated as sensitive on both axes (it runs everywhere) — the completeness
// test makes that unreachable, and running too much is the safe failure. A
// non-baseline pair the registry marks structurally skipped (skip_ok_legs) is
// dropped: the bed would be restarted only to watch the test skip itself.
func sparseSelection(in *SparseInput, auth, train string) []string {
	label := auth + "/" + train
	baseline := auth == baselineAuth && train == baselineTrain
	var out []string
	for _, name := range in.Live {
		if baseline {
			out = append(out, name)
			continue
		}
		e, known := in.Entries[name]
		if known && e.SkipOK(label) {
			continue
		}
		if auth != baselineAuth && known && e.Auth != registry.Sensitive {
			continue
		}
		if train != baselineTrain && known && e.Train != registry.Sensitive {
			continue
		}
		out = append(out, name)
	}
	return out
}

// narrowArgs replaces the caller's top-level -run/-skip selection with one
// anchored -run over names, keeping every other argument. A caller's subtest
// filter survives: -run "X/sub" becomes "^(…)$/sub" and a subtest-level -skip
// is kept as given, so narrowing a cell can never widen what the caller asked
// for.
func narrowArgs(args, names []string) []string {
	runPat, rest, _ := extractFlag(args, "run")
	skipPat, rest, hasSkip := extractFlag(rest, "skip")
	re := anchoredRun(names)
	if parts := splitTestPattern(runPat); len(parts) > 1 {
		re += "/" + strings.Join(parts[1:], "/")
	}
	out := []string{"-run", re}
	if hasSkip && len(splitTestPattern(skipPat)) > 1 {
		out = append(out, "-skip", skipPat)
	}
	return cat(out, rest)
}

// planSparse builds the sparse plan (#1975, R3). The plan is built in full
// first and E2E_AUTH_MODE / E2E_TRAIN_MODE then filter it, so a narrowing
// filter selects a subset of the sparse cells — and a pat-only or off-only run
// therefore omits the baseline and is a partial run.
//
// The baseline keeps today's shape: the main cell skips the isolated scenario,
// which gets a cell of its own. A narrowed cell carries one anchored -run over
// its selection; the isolated scenario is split into its own cell only when it
// is in that cell's selection. An empty selection drops the cell, and with it
// the bed restart. A caller -run is intersected with each cell's selection.
func planSparse(p PlanInput) []Cell {
	var cells []Cell
	for _, sc := range sparseOrder {
		if !authModesInclude(p.AuthModes, sc.auth) || (p.TrainMode != "" && p.TrainMode != sc.train) {
			continue
		}
		parallel := p.Parallel
		if p.TrainMode == "" && sc.train == "on" {
			parallel = p.ParallelOn
		}
		names := sparseSelection(p.Sparse, sc.auth, sc.train)
		mk := func(args []string, isolated bool) Cell {
			return Cell{Auth: sc.auth, Train: sc.train, Parallel: parallel, Args: args, Isolated: isolated}
		}
		baseline := sc.auth == baselineAuth && sc.train == baselineTrain
		if p.CallerHasRun {
			sel, err := SelectedTests(names, p.Args)
			switch {
			case err != nil:
				// An unparsable caller regexp must fail where go test is run, not
				// vanish from the plan.
				cells = append(cells, mk(p.Args, false))
			case len(sel) > 0:
				cells = append(cells, mk(narrowArgs(p.Args, sel), false))
			}
			continue
		}
		var main, isolated []string
		for _, n := range names {
			if n == TrainIsolatedRE {
				isolated = append(isolated, n)
			} else {
				main = append(main, n)
			}
		}
		switch {
		case baseline:
			cells = append(cells, mk(cat([]string{"-skip", TrainIsolatedRE}, p.Args), false))
		case len(main) > 0:
			cells = append(cells, mk(narrowArgs(p.Args, main), false))
		}
		if len(isolated) > 0 {
			cells = append(cells, mk([]string{"-run", isolatedRunArg()}, true))
		}
	}
	return cells
}

// describePlan is the one-line summary printed before the first leg: the cells in
// order, with their labels (an isolated cell is marked).
func describePlan(cells []Cell) string {
	if len(cells) == 0 {
		return "no cells to run"
	}
	parts := make([]string, len(cells))
	for i, c := range cells {
		parts[i] = c.Label()
		if c.Isolated {
			parts[i] += " (isolated)"
		}
	}
	return fmt.Sprintf("%d cell(s): %s", len(cells), strings.Join(parts, ", "))
}
