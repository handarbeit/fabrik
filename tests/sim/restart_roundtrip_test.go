package sim

import (
	"os"
	"path/filepath"
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
// Non-vacuity (AC8): the assertion that matters most is NOT the pushed head
// SHA — a pushed commit already exists on simgh's own backing remote, so
// even a broken RestartEnv that discarded env.WM and rebuilt a fresh
// WorktreeManager pointed at a brand-new re-clone would still observe it,
// making a head-SHA comparison alone pass by accident. What only a genuine
// env.WM reuse can preserve is *local, unpushed* worktree content — exactly
// the "partial work" CLAUDE.md's Worktrees section says must never be
// destroyed. This test plants an uncommitted marker file directly in the
// pre-restart worktree (bypassing git — standing in for in-progress work a
// real kill mid-stage could leave behind) and asserts it is still present,
// byte-for-byte, at the identical path after RestartEnv. A RestartEnv that
// re-clones (even into the same rootDir by coincidence) would produce a
// clean checkout with no such file — this is the one assertion in this file
// that a fresh-clone implementation cannot pass by accident. Confirmed
// during review (PR #1584) that no prior version of this test, nor any of
// restart_recovery_test.go's four kill points (which all fault a pure
// GitHub-API call, after any relevant local commit was already pushed),
// actually exercised this property despite the ADR calling WM reuse "the
// single highest-risk mistake RestartEnv exists to prevent" — see
// adrs/1451-sim-bed-restart-harness.md.
func TestRestartEnv_RoundTrip(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})

	num := FileIssue(t, env, "restart round-trip", "Prove RestartEnv preserves state.", "Specify")

	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 80)
	preRestart := projectItem(t, env, num)

	// Plant local, unpushed worktree content that only a genuine env.WM
	// reuse (not a fresh re-clone) can carry across the restart — see the
	// doc comment above for why this, not the pushed head SHA, is the load-
	// bearing check.
	worktreeDir := env.WM.WorktreeDir(num)
	markerPath := filepath.Join(worktreeDir, "uncommitted-work-in-progress.txt")
	const markerContent = "partial work that was never committed or pushed\n"
	if err := os.WriteFile(markerPath, []byte(markerContent), 0o644); err != nil {
		t.Fatalf("planting uncommitted marker file: %v", err)
	}

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

	// The load-bearing worktree-fidelity check: same path, same uncommitted
	// content, still there post-restart.
	if got := restarted.WM.WorktreeDir(num); got != worktreeDir {
		t.Fatalf("WorktreeDir(%d) after restart = %q, want %q (env.WM reuse must resolve to the identical path)", num, got, worktreeDir)
	}
	gotContent, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("uncommitted marker file lost across restart (a fresh re-clone would not have it): %v", err)
	}
	if string(gotContent) != markerContent {
		t.Fatalf("uncommitted marker file content = %q, want %q", gotContent, markerContent)
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
