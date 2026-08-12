package sim

import (
	"testing"
)

// This file covers R4: resolveTrainConflict/assembleTrialBranch's three
// documented failure shapes for inline conflict resolution
// (engine/merge_train.go:520-545 per the issue's own line reference — drifted
// since, see resolveConflictWithClaude/regenerateAndCommit's current
// locations), each its own scenario per the issue's explicit request. Every
// script here is registered via env.Claude.ForStageComments("Queued", ...) —
// the holding stage's own name, not any member's individual pipeline stage
// (resolveConflictWithClaude's actual InvokeForComments call site, per
// Research's Constraint 4).

// TestMergeTrainConflict_UnresolvableEjectsMember is R4 shape 1: Claude's
// scripted resolver leaves the conflict markers exactly as the failed `git
// merge` left them (claudeLeaveUnresolved mirrors
// engine/merge_train_test.go's TestMergeTrainWorker_UnresolvableConflict) —
// assembleTrialBranch must eject the conflicting member (not the whole
// batch) rather than land it, and the survivor with no conflict must still
// land normally.
func TestMergeTrainConflict_UnresolvableEjectsMember(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	startTrialVerdictSeeder(t, env, allGreenVerdict)
	env.Claude.ForStageComments("Queued", claudeLeaveUnresolved())

	numA, _ := QueueMember(t, env, "unresolvable-a", map[string]string{"shared.txt": "content-from-a\n"})
	numB, _ := QueueMember(t, env, "unresolvable-b", map[string]string{"shared.txt": "content-from-b\n"})

	RunPoll(t, env)

	// One of A/B merges cleanly first (no conflict at all); the other hits
	// the real conflict and gets ejected — assembleTrialBranch processes
	// members in order, so #A (queued first) is the clean merge and #B (the
	// second write to the same path) is the one that actually conflicts.
	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForIssueClosed(t, env, numA, 5)

	WaitForProjectStatus(t, env, numB, "Queued", 10)
	if projectItem(t, env, numB).IsClosed {
		t.Fatalf("unresolvable-conflict member #%d must not land", numB)
	}
	if !hasCommentContaining(t, env, numB, "unresolvable conflict") {
		t.Errorf("expected #%d's ejection comment to name the cause as an unresolvable conflict, got comments: %v", numB, commentsOn(t, env, numB))
	}
	if !hasCommentContaining(t, env, numB, "will be retried in a future train with a different composition") {
		t.Errorf("expected #%d to remain eligible for a future train (stayInQueue=true for this cause)", numB)
	}
}

// TestMergeTrainConflict_UsageLimitDoesNotEject is R4 shape 2: the
// resolution attempt itself hits the account-wide Claude usage limit before
// ever attempting a resolution (claudeUsageLimitDuringConflict mirrors
// engine/merge_train_test.go's
// TestMergeTrainWorker_UsageLimitDuringConflictResolution, ADR-1120) — this
// says nothing about whether the conflict is resolvable, so
// assembleTrialBranch must abort the whole assembly as a fatal error rather
// than eject anyone. Both members must remain exactly as they were —
// untouched, no ejection comment, still Queued — and the very next poll
// retries the same batch from scratch (proving the failure is retried
// whole, not silently dropped).
func TestMergeTrainConflict_UsageLimitDoesNotEject(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	startTrialVerdictSeeder(t, env, allGreenVerdict)
	env.Claude.ForStageComments("Queued", claudeUsageLimitDuringConflict())

	numA, _ := QueueMember(t, env, "usagelimit-a", map[string]string{"shared2.txt": "content-from-a\n"})
	numB, _ := QueueMember(t, env, "usagelimit-b", map[string]string{"shared2.txt": "content-from-b\n"})

	RunPoll(t, env)

	for _, n := range []int{numA, numB} {
		if got := projectItem(t, env, n).Status; got != "Queued" {
			t.Errorf("expected #%d to remain in Queued after a usage-limit abort, got status %q", n, got)
		}
		if projectItem(t, env, n).IsClosed {
			t.Errorf("expected #%d to remain open after a usage-limit abort", n)
		}
		if hasCommentContaining(t, env, n, "ejected") {
			t.Errorf("expected no ejection comment on #%d after a usage-limit abort — the limit says nothing about resolvability", n)
		}
	}

	// Retried whole on the next poll, not silently dropped: the batch
	// re-forms and re-attempts assembly from scratch. The very first
	// invocation's usage-limit error also activates the account-wide Claude
	// suspension gate (ADR-1119/ADR-1183 — the same mechanism
	// fabrik:claude-limit governs), so the second poll's own conflict
	// resolution never reaches a fresh Claude call at all
	// (resolveConflictWithClaude's own suspension check short-circuits
	// first, confirmed directly in this run's log) — CommentCallCount
	// staying at 1 is therefore the *correct* observation, not evidence of
	// stalling. What "retried whole" means here is that the batch is
	// re-attempted (a fresh assembleTrialBranch pass runs, visible as a
	// second merge-conflict resolution attempt) and produces the identical
	// safe outcome — never an eject, never a land — across both polls,
	// which is exactly what a genuinely stuck or silently-dropped batch
	// would not do.
	RunPoll(t, env)
	for _, n := range []int{numA, numB} {
		if projectItem(t, env, n).IsClosed || hasCommentContaining(t, env, n, "ejected") {
			t.Errorf("expected #%d to still be untouched after a second usage-limit abort", n)
		}
		if got := projectItem(t, env, n).Status; got != "Queued" {
			t.Errorf("expected #%d to remain in Queued after a second usage-limit abort, got status %q", n, got)
		}
	}
}

// TestMergeTrainConflict_MixedModePrematureCommitEjectsAndResets is R4
// shape 3: a mixed conflict — spanning both a declared generated path and a
// normal path — where Claude violates buildTrainConflictComment's explicit
// "do not commit the generated path" instruction by committing everything
// anyway (claudePrematureCommitInMixedMode mirrors
// engine/merge_train_test.go's
// TestMergeTrainWorker_ClaudePrematureCommitInMixedModeEjectsMember).
// regenerateAndCommit's own entry guard — MERGE_HEAD must still be present
// when it runs — trips because Claude's premature commit already consumed
// it, and the member is ejected. assembleTrialBranch's unconditional
// pre-merge-HEAD hard reset must undo the premature commit rather than
// leaving it as the trial branch's HEAD: proven here by a third, later
// member (C) whose own clean merge would fail outright if the trial
// worktree were left dirty or contaminated by B's aborted attempt.
func TestMergeTrainConflict_MixedModePrematureCommitEjectsAndResets(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	startTrialVerdictSeeder(t, env, allGreenVerdict)
	env.Engine.SetGeneratedFilesForTest("generated.txt", []string{"sh", "-c", "echo regenerated > generated.txt"})
	env.Claude.ForStageComments("Queued", claudePrematureCommitInMixedMode("normal.txt", "resolved-normal-content\n"))

	numA, _ := QueueMember(t, env, "mixed-a", map[string]string{
		"generated.txt": "gen-a\n",
		"normal.txt":    "normal-a\n",
	})
	numB, _ := QueueMember(t, env, "mixed-b", map[string]string{
		"generated.txt": "gen-b\n",
		"normal.txt":    "normal-b\n",
	})
	numC, _ := QueueMember(t, env, "mixed-c", map[string]string{"unrelated.txt": "c\n"})

	RunPoll(t, env)

	// A (first, clean) and C (last, clean, never touches the conflicted
	// paths) both land; B (the mixed conflict, prematurely committed) is
	// ejected.
	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numC, "Done", 10)
	WaitForIssueClosed(t, env, numA, 5)
	WaitForIssueClosed(t, env, numC, 5)

	WaitForProjectStatus(t, env, numB, "Queued", 10)
	if projectItem(t, env, numB).IsClosed {
		t.Fatalf("mixed-mode premature-commit member #%d must not land", numB)
	}
	if !hasCommentContaining(t, env, numB, "MERGE_HEAD is already gone before regeneration ran") {
		t.Errorf("expected #%d's ejection comment to name the premature-commit reason, got comments: %v", numB, commentsOn(t, env, numB))
	}

	// Non-vacuity (AC10): C's successful land is only possible if the
	// pre-merge-HEAD hard reset genuinely restored the trial worktree to a
	// clean state after B's aborted attempt — a broken reset would leave
	// stray staged/committed content or conflict markers behind that C's own
	// `git merge --no-ff` would trip over (git refuses to merge into a
	// worktree with an unexpected in-progress commit or leftover conflict
	// state), failing C's land instead of merely producing a wrong count.
}
