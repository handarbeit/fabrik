package sim

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// workerYield is a small, fixed real-wall-clock pause after each poll cycle,
// giving the goroutines PollOnce just dispatched a chance to actually run.
//
// This is a deliberate, narrow exception to R4's "a scenario must never
// depend on real sleeping" — that requirement targets the live-bed anti-
// pattern of sleeping past a GitHub-side async event (a review arriving, CI
// finishing) that the scenario cannot otherwise observe. It says nothing
// about the engine's own dispatch concurrency: poll() hands each item's work
// to a background goroutine and returns immediately, and part of that
// goroutine's real, production behavior is genuinely real-time-bound —
// acquireLockAndVerify's multi-instance lock-verify step
// (engine/item.go's lockVerifyDelay, 2s) sleeps on the real clock, not the
// injected one, because it exists to let a *different Fabrik process*
// observe a competing lock, which the Clock seam has no way to represent.
// Without some real pause here, a tight PollOnce loop can out-run that
// goroutine's progress indefinitely, exactly as the very first version of
// this harness did in testing (30 polls all executing before the dispatched
// worker's 2-second lock-verify sleep even elapsed).
//
// This is bounded and documented rather than open-ended: total added real
// time for any AdvanceUntil call is at most maxPolls*workerYield, and it is
// the honest cost R8 asks this package to measure and record, not a silent
// flakiness source.
const workerYield = 100 * time.Millisecond

// RunPoll advances Clock by env.PollInterval, drives exactly one engine poll
// cycle via the Engine.PollOnce test seam (ADR-1449), fails the test on
// error, and then pauses for workerYield (see its doc comment).
//
// Advancing the clock here — not just before the poll loop starts — is what
// keeps itemstate.CooldownAt-based dispatch suppression (R3's engine-local
// group) from becoming permanently stuck: a cooldown is stamped as
// e.now()+duration, so without this a scenario's Clock would sit frozen at
// its start time forever and no cooldown would ever expire, deadlocking
// dispatch exactly as it would if a real poll loop's wall-clock cadence
// simply stopped.
func RunPoll(t *testing.T, env *Env) {
	t.Helper()
	if env.PollInterval > 0 {
		env.Clock.Advance(env.PollInterval)
	}
	if err := env.Engine.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	time.Sleep(workerYield)
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
	board, err := env.Sim.FetchProjectBoard(env.Owner, env.Repo, env.ProjectNum, "User")
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
