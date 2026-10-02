package gate

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// RunPregate is run.sh's run_pregate (R1, #1454): refuse to spend live budget
// until the free, fast layers pass — scripts/sim/run.sh --all (the sim e2e
// scenarios plus simgh's own model tests), then the github/ wire-contract tests.
// Both are already unconditional inside `go test -race ./...` on every PR;
// re-running them here, scoped, is deliberate: the gate is regularly invoked
// standalone and must never assume unit tests were "just run".
//
// Ordering is load-bearing, exactly like the bed preflight: Run calls this
// strictly before PrepareBedAndReset, so a pre-gate failure is proven, by
// construction, to have made no bed preflight, no build and no live
// GitHub/Claude call (ExitPregateFailed).
func (g *Gate) RunPregate(ctx context.Context) error {
	if g.Getenv("E2E_SKIP_PREGATE") != "" {
		g.outln("== pre-gate skipped (E2E_SKIP_PREGATE set) — sim/wire-contract layers assumed already green ==")
		return nil
	}

	// R5, #1624: a tree-scoped dedup signal, not a blanket opt-out. HEAD is
	// re-resolved here rather than trusting the caller's claim — a stale or
	// mismatched value, or a dirty tree since the caller's own pre-gate ran,
	// always falls through to the full pre-gate, never a silent false skip.
	//
	// The SHA match alone is not sufficient: `git rev-parse HEAD` identifies only
	// the committed tree, and cut-release.sh's own step 4 can rewrite
	// plugin/known_embedded_versions.go on disk without committing it. So this
	// also requires a clean working tree. REQ7 (#1677): the caller can declare,
	// via FABRIK_PREGATE_ALLOWED_DIRTY_REGEX, the specific self-writes it knows
	// are benign; anything NOT matching still counts as dirty.
	if verified := g.Getenv("FABRIK_PREGATE_VERIFIED_SHA"); verified != "" {
		so, _, _ := output(ctx, g.Exec, Cmd{Name: "git", Args: []string{"rev-parse", "HEAD"}, Dir: g.Cfg.RepoRoot, Env: g.Env})
		current := strings.TrimSpace(so)
		dirty := g.dirtyLines(ctx)
		switch {
		case verified == current && len(dirty) == 0:
			g.outf("== pre-gate skipped (already verified for %s in this invocation, R5 #1624) ==\n", current)
			return nil
		case verified != current:
			g.outf("== pre-gate: FABRIK_PREGATE_VERIFIED_SHA (%s) does not match HEAD (%s) — running the full pre-gate ==\n", verified, current)
		default:
			g.outf("== pre-gate: FABRIK_PREGATE_VERIFIED_SHA matches HEAD (%s) but the working tree has uncommitted changes (beyond any declared allowlist) since it was verified — running the full pre-gate ==\n", current)
		}
	}

	// #1972 R4: --resume consults the per-SHA record of an earlier pass, so the
	// pre-gate runs once per HEAD across partial invocations. Same bar as the
	// signal above: the SAME HEAD and a clean tree, re-resolved here.
	if g.resume {
		if head, ok := g.pregateRecorded(ctx); ok {
			g.outf("== pre-gate skipped (already passed for clean HEAD %s — recorded in the coverage ledger, #1972) ==\n", head)
			return nil
		}
	}

	g.outln("== pre-gate: sim suite + github wire-contract tests (R1, #1454) ==")
	g.pregateRetry = nil
	if err := g.runPregateStep(ctx, "sim suite", func(out, errw io.Writer) Cmd {
		return Cmd{
			Name: g.Cfg.RepoRoot + "/scripts/sim/run.sh", Args: []string{"--all"},
			Dir: g.Cfg.RepoRoot, Env: g.Env, Stdout: out, Stderr: errw,
		}
	}, "pre-gate: sim suite failed — aborting before touching the live bed or making any live call."); err != nil {
		return err
	}
	if err := g.runPregateStep(ctx, "github wire-contract tests", func(out, errw io.Writer) Cmd {
		return Cmd{
			Name: "go", Args: []string{"test", "-race", "-count=1", "./github/..."},
			Dir: g.Cfg.RepoRoot, Env: g.Env, Stdout: out, Stderr: errw, Session: true, Grace: g.Cfg.KillGrace,
		}
	}, "pre-gate: github wire-contract tests failed — aborting before touching the live bed or making any live call."); err != nil {
		return err
	}
	g.outln("== pre-gate passed ==")
	g.recordPregatePass(ctx)
	return nil
}

// runPregateStep runs one pre-gate step. A non-zero exit is a hard stop
// (ExitPregateFailed) — except when the output carries ONLY the known TSan
// fork/exec crash signature (#1973 R5, IsTSanForkCrash): then the step is re-run
// once, the retry is recorded (with the host load average, which is the usual
// cause) and the second result is final. A second crash, or any other failure,
// is the same hard stop as ever. Output still streams to the terminal as it
// arrives; it is only scanned, never buffered.
func (g *Gate) runPregateStep(ctx context.Context, name string, mk func(out, errw io.Writer) Cmd, failMsg string) error {
	run := func() (Result, *crashScanner) {
		sc := &crashScanner{}
		res := g.Exec.Run(ctx, mk(io.MultiWriter(g.Out, sc), io.MultiWriter(g.Err, sc)))
		return res, sc
	}
	res, sc := run()
	if res.ExitCode == 0 {
		return nil
	}
	if ctx.Err() != nil || !sc.IsCrash() {
		return &ExitError{Code: ExitPregateFailed, Msg: failMsg}
	}
	load := g.loadAvgText()
	sig := sc.Signature()
	g.outf("== pre-gate: the %s hit the known TSan fork/exec crash (%s, #1624/#1677 — not an engine verdict); retrying once (host load average %s) ==\n", name, sig, load)
	res, sc2 := run()
	note := pregateRetryNote{Step: name, Signature: sig, LoadAvg: load, Outcome: "pass"}
	if res.ExitCode != 0 {
		note.Outcome = "fail"
		if sc2.IsCrash() {
			note.Outcome = "crashed again"
		}
	}
	g.pregateRetry = &note
	g.recordPregateRetry(ctx, note)
	if res.ExitCode == 0 {
		g.outf("== pre-gate: the %s passed on its one crash retry ==\n", name)
		return nil
	}
	return &ExitError{Code: ExitPregateFailed, Msg: failMsg + " (after its one TSan-crash retry)"}
}

// loadAvgText is the host's 1-minute load average for a note, "unavailable" when
// the probe cannot read it.
func (g *Gate) loadAvgText() string {
	if v, ok := g.loadAvg(); ok {
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	return "unavailable"
}

// dirtyLines is the working tree's `git status --porcelain` lines that are NOT
// covered by FABRIK_PREGATE_ALLOWED_DIRTY_REGEX (bash: grep -Ev). A regex that
// does not compile is treated as covering nothing — i.e. everything counts as
// dirty and the full pre-gate runs. (Bash's `grep -Ev "$bad" || true` failed
// OPEN there, vouching for any tree; failing closed is the deliberate delta,
// recorded in ADR-1994.) A failure to read the status at all also counts as dirty.
func (g *Gate) dirtyLines(ctx context.Context) []string {
	so, _, res := output(ctx, g.Exec, Cmd{Name: "git", Args: []string{"status", "--porcelain"}, Dir: g.Cfg.RepoRoot, Env: g.Env})
	if res.ExitCode != 0 {
		return []string{fmt.Sprintf("(git status failed, exit %d)", res.ExitCode)}
	}
	var allow *regexp.Regexp
	if pat := g.Getenv("FABRIK_PREGATE_ALLOWED_DIRTY_REGEX"); pat != "" {
		if re, err := regexp.Compile(pat); err == nil {
			allow = re
		} else {
			g.errf("warning: FABRIK_PREGATE_ALLOWED_DIRTY_REGEX does not compile (%v) — treating every change as dirty\n", err)
		}
	}
	var dirty []string
	for _, line := range strings.Split(so, "\n") {
		if line == "" {
			continue
		}
		if allow != nil && allow.MatchString(line) {
			continue
		}
		dirty = append(dirty, line)
	}
	return dirty
}
