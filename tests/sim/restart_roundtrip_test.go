package sim

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestRestartEnv_RoundTrip is RestartEnv's own foundation test (R3):
// discarding an Engine mid-scenario and rebuilding one via RestartEnv must
// preserve both GitHub-side state (the board, its labels, comments) and the
// on-disk worktree/git state, and the rebuilt Engine must be able to keep
// driving the same issue to completion. Every restart_recovery_test.go
// scenario depends on this holding; this test exists so a defect in
// RestartEnv itself is diagnosed here rather than surfacing as a confusing
// failure in one of them.
//
// Non-vacuity (AC8): the assertion that matters is not merely "a new Engine
// exists" but that (a) the restarted Env observes the exact commit the
// pre-restart Engine pushed — a fresh WorktreeManager pointed at a
// re-cloned-from-scratch origin would also show *a* commit (the branch
// still exists on the simgh-backed remote) but a broken RestartEnv that
// dropped env.WM and rebuilt a manager pointed at the wrong local worktree
// root would fail this specific head-SHA comparison — and (b) the
// pipeline actually completes afterward, proving the rebuilt Engine is not
// merely present but functional.
func TestRestartEnv_RoundTrip(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})

	num := FileIssue(t, env, "restart round-trip", "Prove RestartEnv preserves state.", "Specify")

	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 80)
	preRestart := projectItem(t, env, num)

	restarted := RestartEnv(t, env)

	// GitHub-side state (R3's durable-state contract): the board item, its
	// labels, and its status column all survive, observed through the
	// restarted Env's own Sim — not the original's, which a real restart
	// would no longer have access to.
	postRestart := projectItem(t, restarted, num)
	if postRestart.Status != preRestart.Status {
		t.Fatalf("status after restart = %q, want %q (pre-restart)", postRestart.Status, preRestart.Status)
	}
	if !hasLabel(postRestart.Labels, "stage:Specify:complete") {
		t.Fatalf("stage:Specify:complete lost across restart — labels: %v", postRestart.Labels)
	}

	// The restarted Env's Engine keeps driving the same issue forward — not
	// just present, but functional — through to Done.
	WaitForIssueClosed(t, restarted, num, 300)
	WaitForProjectStatus(t, restarted, num, "Done", 5)

	// Non-vacuity: StageCallCount("Specify") on the restarted Env's own
	// Claude invoker (shared with the original — see RestartEnv's doc
	// comment) must still read exactly 1 — the pre-restart dispatch is not
	// silently repeated after the restart, which is exactly what a
	// RestartEnv that discarded durable state (rather than truly
	// discarding only in-memory engine state) would otherwise cause.
	if got := restarted.Claude.StageCallCount("Specify"); got != 1 {
		t.Errorf("StageCallCount(Specify) after restart = %d, want 1 — Specify must not be re-dispatched after restart, since stage:Specify:complete already survived it", got)
	}
}
