package gate

import (
	"context"
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
