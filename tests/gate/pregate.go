package gate

import (
	"context"
	"fmt"
	"regexp"
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

	g.outln("== pre-gate: sim suite + github wire-contract tests (R1, #1454) ==")
	res := g.Exec.Run(ctx, Cmd{
		Name: g.Cfg.RepoRoot + "/scripts/sim/run.sh", Args: []string{"--all"},
		Dir: g.Cfg.RepoRoot, Env: g.Env, Stdout: g.Out, Stderr: g.Err,
	})
	if res.ExitCode != 0 {
		return &ExitError{Code: ExitPregateFailed, Msg: "pre-gate: sim suite failed — aborting before touching the live bed or making any live call."}
	}
	res = g.Exec.Run(ctx, Cmd{
		Name: "go", Args: []string{"test", "-race", "-count=1", "./github/..."},
		Dir: g.Cfg.RepoRoot, Env: g.Env, Stdout: g.Out, Stderr: g.Err, Session: true, Grace: g.Cfg.KillGrace,
	})
	if res.ExitCode != 0 {
		return &ExitError{Code: ExitPregateFailed, Msg: "pre-gate: github wire-contract tests failed — aborting before touching the live bed or making any live call."}
	}
	g.outln("== pre-gate passed ==")
	return nil
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
