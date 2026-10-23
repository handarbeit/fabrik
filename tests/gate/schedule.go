package gate

import (
	"context"
	"fmt"
	"strings"

	"github.com/handarbeit/fabrik/tests/e2e/bedevents"
	"github.com/handarbeit/fabrik/tests/e2e/registry"
)

// The events dimension of a cell (#2426, ADR-2426): how the bed's engine ingests
// GitHub events on the leg.
const (
	// EventsPoll is a poll-only leg. It is the zero value of Cell.Events.
	EventsPoll = "poll"
	// EventsWebhook is a leg whose bed receives webhooks (the embedded Tailscale
	// Funnel receiver, #2408). It is App-auth only.
	EventsWebhook = "webhook"
)

// PlannedPollSeconds is the poll cadence the plan gives every cell, webhook cells
// included (#2426, FR-009): the poll legs' 60 s, so the existing suite's fixed
// waits hold. Webhook-path evidence comes from structure (the delivery preflight,
// observed deliveries, HoldPolls), not from a longer interval.
const PlannedPollSeconds = bedevents.PollOnlySeconds

// Cell is one leg of the gate: one bed restart (TestSwitchTrainMode) followed
// by one `go test` invocation over ./tests/e2e/.... Today the gate is a serial
// auth × train matrix; #1975 (sparse matrix) and #1976 (multi-bed) drive Cells
// from a smarter Scheduler, and #1972's ledger keys on them.
type Cell struct {
	Auth  string // "pat" | "app"
	Train string // the E2E_TRAIN_MODE for the leg: "off" | "on" (passed through verbatim)
	// Events is how the bed ingests events on the leg: EventsPoll (also the empty
	// value) or EventsWebhook (#2426). A webhook cell is App-auth only.
	Events string
	// PollSeconds is the bed's `-poll` cadence for the leg, set when the plan is
	// built, printed with it and archived with the leg (#2426, FR-008). Empty means
	// a hand-built cell; the plan always fills it.
	PollSeconds string
	// Parallel is the `go test -parallel` cap, a string passed through verbatim.
	Parallel string
	// Args are the `go test` arguments after ./tests/e2e/... — the cell's own
	// narrowing -run (sparse plan) and/or the caller's passthrough arguments.
	// RunLeg splits the selection into its isolation phases (#1977, phases.go).
	Args []string
}

// Label is the report label: auth mode / train mode, e.g. "app/on" (#1861). A
// webhook cell appends its events dimension ("app/on/webhook"); a poll cell's
// label stays two-part, so every ledger record written before the events axis
// keeps its key (#2426, FR-001).
func (c Cell) Label() string {
	if c.Webhook() {
		return c.Auth + "/" + c.Train + "/" + EventsWebhook
	}
	return c.Auth + "/" + c.Train
}

// Webhook reports whether the cell runs with a webhook transport.
func (c Cell) Webhook() bool { return c.Events == EventsWebhook }

// EventsKind is the cell's events dimension, with the zero value read as poll.
func (c Cell) EventsKind() string {
	if c.Webhook() {
		return EventsWebhook
	}
	return EventsPoll
}

// EventSource is the E2E_EVENT_SOURCE value the cell's leg environment carries.
func (c Cell) EventSource() string {
	if c.Webhook() {
		return bedevents.Tailscale
	}
	return bedevents.Poll
}

// isBaseline reports whether the cell is the sparse plan's baseline (app/on, poll).
// A webhook cell on the same auth and train is not: it runs a different selection.
func (c Cell) isBaseline() bool {
	return c.Auth == baselineAuth && c.Train == baselineTrain && !c.Webhook()
}

// PlanInput is what the cell plan depends on.
type PlanInput struct {
	AuthModes []string
	// TrainMode is E2E_TRAIN_MODE: when set the caller forces a single mode.
	TrainMode string
	// Events is E2E_EVENTS: "" plans both dimensions, EventsPoll only the poll
	// cells, EventsWebhook only the webhook cells. A filtered plan is partial
	// (#2426).
	Events string
	// CallerHasRun: the caller supplied -run/--run, so they are targeting
	// specific scenarios; honour that exactly.
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
//   - E2E_TRAIN_MODE set: a single forced mode, always at E2E_PARALLEL.
//   - unset (the default gate): "off" first (the path nearly all real usage
//     takes, so a regression there surfaces before the less-common train-on
//     run), then "on" at the tighter E2E_PARALLEL_ON cap.
//
// A cell is one bed restart; the exclusive scenarios that used to get a cell of
// their own (the runaway guard, TrainIsolatedRE) are now the last phase of the
// cell that selects them, chosen by the registry (#1977, phases.go).
func PlanCells(p PlanInput) []Cell {
	if p.Sparse != nil && (p.TrainMode == "" || p.TrainMode == "on" || p.TrainMode == "off") {
		return planSparse(p)
	}
	var cells []Cell
	for _, auth := range p.AuthModes {
		if p.Events == EventsWebhook {
			break
		}
		mk := func(train, parallel string) Cell {
			return Cell{Auth: auth, Train: train, Parallel: parallel, Args: p.Args, PollSeconds: PlannedPollSeconds}
		}
		if p.TrainMode != "" {
			cells = append(cells, mk(p.TrainMode, p.Parallel))
			continue
		}
		cells = append(cells, mk("off", p.Parallel), mk("on", p.ParallelOn))
	}
	// Webhook cells run the WHOLE suite under the full matrix (#2426, FR-003): a
	// release that changes ingestion must show every scenario still converges with
	// webhooks as the primary wake source. They come last, after every poll cell,
	// so a failed webhook preflight leaves the poll coverage already recorded.
	if p.Events != EventsPoll && authModesInclude(p.AuthModes, baselineAuth) {
		mk := func(train, parallel string) Cell {
			return Cell{Auth: baselineAuth, Train: train, Events: EventsWebhook, Parallel: parallel, Args: p.Args, PollSeconds: PlannedPollSeconds}
		}
		if p.TrainMode != "" {
			cells = append(cells, mk(p.TrainMode, p.Parallel))
		} else {
			cells = append(cells, mk("on", p.ParallelOn), mk("off", p.Parallel))
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
// The baseline passes the caller's arguments through (it runs every live test).
// A narrowed cell carries one anchored -run over its selection. An empty
// selection drops the cell, and with it the bed restart. A caller -run is
// intersected with each cell's selection.
func planSparse(p PlanInput) []Cell {
	var cells []Cell
	for _, sc := range sparseOrder {
		if p.Events == EventsWebhook {
			break
		}
		if !authModesInclude(p.AuthModes, sc.auth) || (p.TrainMode != "" && p.TrainMode != sc.train) {
			continue
		}
		parallel := p.Parallel
		if p.TrainMode == "" && sc.train == "on" {
			parallel = p.ParallelOn
		}
		names := sparseSelection(p.Sparse, sc.auth, sc.train)
		mk := func(args []string) Cell {
			return Cell{Auth: sc.auth, Train: sc.train, Parallel: parallel, Args: args, PollSeconds: PlannedPollSeconds}
		}
		baseline := Cell{Auth: sc.auth, Train: sc.train}.isBaseline()
		if p.CallerHasRun {
			sel, err := SelectedTests(names, p.Args)
			switch {
			case err != nil:
				// An unparsable caller regexp must fail where go test is run, not
				// vanish from the plan.
				cells = append(cells, mk(p.Args))
			case len(sel) > 0:
				cells = append(cells, mk(narrowArgs(p.Args, sel)))
			}
			continue
		}
		switch {
		case baseline:
			cells = append(cells, mk(p.Args))
		case len(names) > 0:
			cells = append(cells, mk(narrowArgs(p.Args, names)))
		}
	}
	return append(cells, planSparseWebhook(p)...)
}

// webhookCellTrain is the train mode of the sparse plan's webhook cell: the
// baseline's, so the cell changes only the events axis.
const webhookCellTrain = baselineTrain

// webhookSelection returns the live tests the sparse webhook cell must run: every
// test the registry marks events: sensitive, minus the pairs it marks structurally
// skipped on that cell (skip_ok_legs, e.g. a test that skips itself on a webhook
// leg). A test with no registry entry runs (the completeness test makes that
// unreachable, and running too much is the safe failure).
func webhookSelection(in *SparseInput) []string {
	label := Cell{Auth: baselineAuth, Train: webhookCellTrain, Events: EventsWebhook}.Label()
	var out []string
	for _, name := range in.Live {
		e, known := in.Entries[name]
		if known && (e.Events != registry.Sensitive || e.SkipOK(label)) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// planSparseWebhook is the one extra cell of the sparse plan (#2426, FR-003):
// app/on/webhook running every events: sensitive test. It is App-auth only and is
// planned last. A caller -run is intersected with the selection, as for any
// narrowed cell.
func planSparseWebhook(p PlanInput) []Cell {
	if p.Events == EventsPoll || !authModesInclude(p.AuthModes, baselineAuth) ||
		(p.TrainMode != "" && p.TrainMode != webhookCellTrain) {
		return nil
	}
	parallel := p.Parallel
	if p.TrainMode == "" {
		parallel = p.ParallelOn
	}
	names := webhookSelection(p.Sparse)
	mk := func(args []string) []Cell {
		return []Cell{{Auth: baselineAuth, Train: webhookCellTrain, Events: EventsWebhook, Parallel: parallel, Args: args, PollSeconds: PlannedPollSeconds}}
	}
	if p.CallerHasRun {
		sel, err := SelectedTests(names, p.Args)
		switch {
		case err != nil:
			return mk(p.Args)
		case len(sel) > 0:
			return mk(narrowArgs(p.Args, sel))
		}
		return nil
	}
	if len(names) == 0 {
		return nil
	}
	return mk(narrowArgs(p.Args, names))
}

// describePlan is the one-line summary printed before the first leg: the cells in
// order, with their labels.
func describePlan(cells []Cell) string {
	if len(cells) == 0 {
		return "no cells to run"
	}
	parts := make([]string, len(cells))
	for i, c := range cells {
		parts[i] = c.Label()
	}
	return fmt.Sprintf("%d cell(s): %s", len(cells), strings.Join(parts, ", "))
}
