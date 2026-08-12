package sim

import (
	"strings"
	"testing"
)

// This file is R4: inject a failure at each intermediate step of a
// multi-call GitHub mutation sequence and assert the resulting state is one
// the engine can recover from on the next poll — or, where it genuinely
// cannot, pin that as-found with a comment naming the follow-up (R4's own
// explicit allowance; fixing it is out of scope for this issue).
//
// Every multi-call sequence this package has found and characterized,
// across this file and its siblings:
//
//	sequence                                    | step under test              | outcome      | covered by
//	---------------------------------------------|-------------------------------|--------------|------------------------------------
//	addCompleteLabelAndRemoveCI (engine/ci.go)    | 1st call (AddLabelToIssue)   | recoverable  | restart_recovery_test.go: TestRestartRecovery_KillBeforeFirstLabelWrite
//	addCompleteLabelAndRemoveCI                   | 2nd call (RemoveLabelFromIssue) | recoverable | restart_recovery_test.go: TestRestartRecovery_KillBetweenLabelPair
//	ensureDraftPR -> markPRReady (engine/pr.go)   | MarkPRReady (post-PR-creation) | UNRECOVERABLE (pinned, #1582) | restart_recovery_test.go: TestRestartRecovery_KillAfterPRCreatedBeforeReady
//	spawnChildren (engine/spawn.go)               | AddProjectV2ItemById         | UNRECOVERABLE (pinned, #1583) | TestPartialMutation_SpawnChildren_ProjectAddFails (this file)
//	spawnChildren                                 | AddBlockedByIssue            | UNRECOVERABLE (pinned, #1583) | restart_recovery_test.go: TestRestartRecovery_KillDuringSpawnSequence
//	spawnChildren                                 | UpdateProjectItemStatus (placement) | recoverable | settle_scan_escalation_test.go: TestSettleScan_AwaitingPlacement
//	landSingleton (engine/merge_train.go)         | (not covered)                 | out of scope — merge-train landing is sibling issue #1452's territory (real git assembly), not this issue's (see this issue's own Scope section)
//
// Three of spawnChildren's five per-child steps (AddProjectV2ItemById,
// AddBlockedByIssue, UpdateProjectItemStatus) are now characterized. The
// remaining two (CreateIssue itself, and the best-effort
// fabrik:sub-issue/yolo/cruise label copies which are deliberately
// non-fatal — see spawnChildren's own doc comment) are not: CreateIssue
// failing is the "before any GitHub mutation for this child" case, already
// structurally equivalent to R1's/R3's "kill before first write" shape
// (nothing child-specific to add), and the label copies are intentionally
// fire-and-forget with no recovery contract to test.

// TestPartialMutation_SpawnChildren_ProjectAddFails faults
// AddProjectV2ItemById (engine/spawn.go) — the step immediately after
// CreateIssue in spawnChildren's per-child sequence, one step earlier than
// TestRestartRecovery_KillDuringSpawnSequence's AddBlockedByIssue fault.
// The child issue is created (a real, durable GitHub mutation) but never
// added to the project board and never linked as blockedBy. spawnChildren
// pauses the parent hard with the same "manually close orphaned children,
// remove fabrik:paused, then re-advance to retry" instruction it uses for
// every per-child failure — this scenario proves that instruction's
// "re-advance to retry" half reproduces the exact same unrecoverable-without-
// awareness shape TestRestartRecovery_KillDuringSpawnSequence already pinned
// for the next step in the sequence: retrying from scratch has no memory of
// the already-created (but board-orphaned) child, so it creates a second
// child issue rather than resuming the first one's placement.
func TestPartialMutation_SpawnChildren_ProjectAddFails(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: crossRepoSpawnStages()})

	parent := seedSpawnReadyParent(t, env, env.OwnerRepo, "sim partial-mutation spawn child", "Body for the project-add-fails scenario.")

	env.Sim.Faults().FailNTimes("AddProjectV2ItemById", 1, errInjectedSettleFault)

	WaitForIssueLabel(t, env, parent, "fabrik:paused", 40)
	if hasLabel(IssueLabels(t, env, parent), "fabrik:children-spawned") {
		t.Fatal("fabrik:children-spawned present despite the injected AddProjectV2ItemById failure — spawn should not have completed")
	}
	if len(projectItem(t, env, parent).BlockedBy) != 0 {
		t.Fatal("parent already has a blockedBy edge despite the injected fault — fault fired too late to test this step")
	}
	childrenBefore := countChildIssuesTitled(t, env, "sim partial-mutation spawn child")
	if childrenBefore != 1 {
		t.Fatalf("expected exactly 1 child issue created before the fault fired, got %d", childrenBefore)
	}
	t.Logf("parent #%d paused mid-spawn: child created, never added to the project board", parent)

	env.Sim.Faults().Clear("AddProjectV2ItemById")

	owner, repo, _ := strings.Cut(env.OwnerRepo, "/")
	if err := env.Sim.RemoveLabelFromIssue(owner, repo, parent, "fabrik:paused"); err != nil {
		t.Fatalf("simulated operator unpause: %v", err)
	}

	WaitForIssueLabel(t, env, parent, "fabrik:children-spawned", 40)
	if len(projectItem(t, env, parent).BlockedBy) == 0 {
		t.Fatal("parent still has no blockedBy edge after the retried spawn")
	}

	childrenAfter := countChildIssuesTitled(t, env, "sim partial-mutation spawn child")
	if childrenAfter == 1 {
		t.Log("NOTE: exactly 1 child issue exists after the retried spawn — the as-found duplicate-child gap this scenario pins may have been fixed; if so, update/close #1583.")
	} else {
		t.Logf("as-found confirmed: %d child issues exist after the retried spawn (expected exactly 1 in a fully-recovered world) — same root cause as TestRestartRecovery_KillDuringSpawnSequence, one step earlier in the sequence (pinned, see #1583)", childrenAfter)
	}

	// The first (board-orphaned) child is a genuinely leaked issue — never
	// placed on the board, never linked, and now permanently unreachable by
	// normal dispatch. Confirming its continued existence (rather than
	// somehow having been garbage-collected) is part of pinning the defect
	// accurately: this is not just "a duplicate," it's an orphan plus a
	// duplicate.
	if childrenAfter < 2 {
		t.Errorf("expected at least 2 child issues (the original board-orphaned one, plus the retried spawn's own) — got %d, meaning the orphan from before the fault no longer exists, which would itself be worth understanding", childrenAfter)
	}
}
