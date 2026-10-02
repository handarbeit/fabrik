package gate

import (
	"context"
	"fmt"
	"strings"
)

// Coverage is `gate coverage`: a read-only acceptance check of the per-SHA
// ledger (#1972, R5). It never starts a bed, never runs a test and never
// writes — it answers "is live coverage complete for this SHA?" and prints the
// per-leg summary.
//
//	gate coverage [--sha <full-sha>] [--format notes]
//
// The SHA defaults to what E2E_BED_REF (default origin/main) resolves to in this
// checkout — what a gate run would resolve it to at bed preflight, since the
// release flow keeps local main equal to origin/main. The required set is the
// plan the gate itself would build for the current E2E_AUTH_MODE/E2E_TRAIN_MODE,
// so it follows PlanCells rather than a hard-coded matrix.
//
// Exit 0: complete. ExitCoverageIncomplete (8): not. Anything else is an error
// in the check itself. With --format notes, stdout carries only the release-notes
// line (and only when complete); the summary moves to stderr.
func (g *Gate) Coverage(ctx context.Context, argv []string) int {
	sha, format := "", "text"
	for i := 0; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "--sha" && i+1 < len(argv):
			sha = argv[i+1]
			i++
		case strings.HasPrefix(a, "--sha="):
			sha = strings.TrimPrefix(a, "--sha=")
		case a == "--format" && i+1 < len(argv):
			format = argv[i+1]
			i++
		case strings.HasPrefix(a, "--format="):
			format = strings.TrimPrefix(a, "--format=")
		default:
			g.errf("gate coverage: unknown argument %q (usage: coverage [--sha <sha>] [--format notes])\n", a)
			return ExitUsage
		}
	}
	if format != "text" && format != "notes" {
		g.errf("gate coverage: unknown --format %q (want text or notes)\n", format)
		return ExitUsage
	}
	if g.Cfg.CoverageDir == "" {
		g.errln("gate coverage: the coverage ledger is disabled (E2E_COVERAGE_DIR resolved to nothing)")
		return ExitUsage
	}
	if sha == "" {
		ref := orDefault(g.Getenv("E2E_BED_REF"), "origin/main")
		so, se, res := output(ctx, g.Exec, Cmd{Name: "git", Args: []string{"rev-parse", "--verify", ref + "^{commit}"}, Dir: g.Cfg.RepoRoot, Env: g.Env})
		if res.ExitCode != 0 {
			g.errf("gate coverage: cannot resolve %s: %s\n", ref, strings.TrimSpace(se))
			return ExitPreflightFailed
		}
		sha = strings.TrimSpace(so)
	}
	modes, err := ResolveAuthModes(g.Getenv("E2E_AUTH_MODE"))
	if err != nil {
		g.errln(err.Error())
		return ExitPreconditionFailed
	}
	in, err := g.loadCoverageInputs()
	if err != nil {
		g.errf("gate coverage: %v\n", err)
		return ExitPreflightFailed
	}
	legs, required, err := RequiredTests(in.live, PlanCells(PlanInput{
		AuthModes: modes, TrainMode: g.Getenv("E2E_TRAIN_MODE"), Parallel: g.Cfg.Parallel, ParallelOn: g.Cfg.ParallelOn,
	}))
	if err != nil {
		g.errf("gate coverage: %v\n", err)
		return ExitPreflightFailed
	}

	// Read-only: no directory is created for a SHA with no ledger.
	l := &Ledger{Root: g.Cfg.CoverageDir, SHA: sha, now: g.Now}
	rep := g.coverageReport(ctx, l, in, legs, required, true)
	rep.Pregate = g.pregateLine(ctx)

	summary := g.Out
	if format == "notes" {
		summary = g.Err
	}
	fmt.Fprintln(summary, rep.Format())
	if !rep.Complete() {
		return ExitCoverageIncomplete
	}
	if format == "notes" {
		g.outln(rep.NotesLine())
	}
	return 0
}
