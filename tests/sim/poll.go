package sim

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// RunPoll drives exactly one engine poll cycle via the Engine.PollOnce test
// seam (ADR-1449) and fails the test on error.
func RunPoll(t *testing.T, env *Env) {
	t.Helper()
	if err := env.Engine.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

// RunPolls drives n poll cycles in sequence.
func RunPolls(t *testing.T, env *Env, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		RunPoll(t, env)
	}
}

// AdvanceUntil drives poll cycles, calling cond after each, until cond
// returns true or maxPolls is reached — whichever comes first. Returns
// normally when cond becomes true (including immediately, before any poll,
// if cond already holds). On exhaustion it fails the test with a diagnostic
// containing the board's current mutation log — R4/AC7: a failing assertion
// must print enough to diagnose without a re-run.
//
// cond is called with env so it can inspect any part of the harness's
// state (labels, PR state, board status) it needs.
func AdvanceUntil(t *testing.T, env *Env, cond func(*Env) bool, maxPolls int) {
	t.Helper()
	if cond(env) {
		return
	}
	for i := 0; i < maxPolls; i++ {
		RunPoll(t, env)
		if cond(env) {
			return
		}
	}
	t.Fatalf("AdvanceUntil: condition not met after %d polls\n\n%s", maxPolls, diagnostics(env))
}

// diagnostics renders the harness's current state for a failing assertion —
// the board state (every item's status and labels) and the mutation log's
// most recent entries (via Instrumented.Log().Dump) — so a test failure is
// diagnosable from CI output alone, without a re-run (R4/AC7).
func diagnostics(env *Env) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== board state ===\n%s\n", boardSummary(env))
	fmt.Fprintf(&b, "=== mutation log (most recent) ===\n%s\n", env.Sim.Log().Dump(50))
	return b.String()
}

// boardSummary renders every item's number, status column, and labels.
func boardSummary(env *Env) string {
	board, err := env.Sim.FetchProjectBoard(env.ProjectOwner, env.Repo, env.ProjectNum, "User")
	if err != nil {
		return fmt.Sprintf("(could not fetch board: %v)", err)
	}
	var b strings.Builder
	for _, item := range board.Items {
		fmt.Fprintf(&b, "#%d %-20s labels=%v closed=%v\n", item.Number, item.Status, item.Labels, item.IsClosed)
	}
	if len(board.Items) == 0 {
		b.WriteString("(no items)\n")
	}
	return b.String()
}
