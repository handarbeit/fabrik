package sim

import (
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// This file is R3: kill the engine at four distinct points relative to a
// durable-state write, using RestartEnv (restart.go) to prove recovery
// across a genuine process-state boundary — not merely a subsequent poll of
// the same process, which every other scenario in this package already
// exercises. Each scenario asserts (a) no duplicated work (no second PR, no
// re-run of an already-complete stage, no double dispatch) and (b) no lost
// durable state (the outstanding mutation is not silently dropped — it is
// either already-recovered or genuinely retried on the very next poll after
// restart).
//
// The four points, chosen to span the shapes R3's own issue text calls out:
//
//  1. TestRestartRecovery_KillBeforeFirstLabelWrite — after markers were
//     parsed but before ANY label was written (addCompleteLabelAndRemoveCI's
//     first call, engine/ci.go — an early return on failure, so genuinely
//     zero mutations land).
//  2. TestRestartRecovery_KillBetweenLabelPair — between the two label
//     mutations of a pair (addCompleteLabelAndRemoveCI's second call —
//     engine/ci_settle.go's own documented example of this exact shape).
//  3. TestRestartRecovery_KillAfterPRCreatedBeforeReady — a PR was created
//     (genuinely exists in simgh's model) but the engine's own follow-up
//     bookkeeping call about it failed before anything durable recorded
//     that fact.
//  4. TestRestartRecovery_KillDuringSpawnSequence — spawnChildren's
//     AddBlockedByIssue call fails after the child issue itself already
//     exists (shared with R4/partial_mutation_test.go's own coverage of the
//     same sequence — see that file for the enumeration this scenario is
//     one instance of).
//
// See adrs/1451-sim-bed-restart-harness.md for RestartEnv's own design.

// --- kill point 1: before any label write --------------------------------

// TestRestartRecovery_KillBeforeFirstLabelWrite drives Validate to a
// genuinely-clearable CI gate, then faults addCompleteLabelAndRemoveCI's
// very first call (AddLabelToIssue stage:Validate:complete — engine/ci.go)
// exactly once. That function returns immediately on this failure (see its
// own doc comment: "If adding the completion label fails, fabrik:awaiting-ci
// is preserved so the next poll cycle retries"), so nothing else in the
// sequence runs — indistinguishable, from GitHub's own perspective, from a
// process kill that landed before any write at all. Restarting and clearing
// the fault must let the very next settle-scan pass complete the sequence
// exactly once — not skip it (state lost) and not run it twice (duplicated
// work).
func TestRestartRecovery_KillBeforeFirstLabelWrite(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{
		Stages:    ciFixStages(),
		StartTime: time.Now(), // see TestCIFixReinvoke's identical comment
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxCiFixCycles = 5
		},
	})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	num := FileIssue(t, env, "restart kill before first label write", "Prove recovery when the engine dies before any completion label lands.", "Implement")

	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	sha1 := pr.HeadSHA
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, sha1, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: "success"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}

	// Narrow to the completion label specifically so nothing else in the
	// same poll (e.g. an unrelated AddLabelToIssue call) is affected.
	env.Sim.Faults().FailWhen("AddLabelToIssue",
		func(a simgh.Args) bool { return a.Number == num && a.Label == "stage:Validate:complete" },
		1, errInjectedSettleFault)

	// Drive exactly one poll where the CI gate clears and the completion
	// attempt fires and fails.
	RunPoll(t, env)
	if hasLabel(IssueLabels(t, env, num), "stage:Validate:complete") {
		t.Fatal("stage:Validate:complete present despite the injected fault on its own AddLabelToIssue call — fault did not fire as expected")
	}
	if !hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-ci") {
		t.Fatal("fabrik:awaiting-ci already cleared despite the completion label add failing — addCompleteLabelAndRemoveCI must preserve it on failure (see its own doc comment)")
	}
	t.Logf("#%d: completion attempt failed with zero mutations landed — kill-before-first-write state confirmed", num)

	restarted := RestartEnv(t, env)
	restarted.Sim.Faults().Clear("AddLabelToIssue")

	// Recovery must be driven by settleAwaitingCIScan re-running the full
	// sequence from scratch — exactly once.
	WaitForIssueLabel(t, restarted, num, "stage:Validate:complete", 80)
	WaitForLabelAbsent(t, restarted, num, "fabrik:awaiting-ci", 20)
	WaitForIssueClosed(t, restarted, num, 300)
	WaitForProjectStatus(t, restarted, num, "Done", 5)

	// Non-vacuity: Validate must not have been dispatched a second time —
	// the recovery is the settle scan retrying its own bookkeeping, not the
	// engine re-running the whole stage from scratch.
	if got := restarted.Claude.StageCallCount("Validate"); got != 1 {
		t.Errorf("StageCallCount(Validate) after restart = %d, want 1 — the recovered completion must not re-dispatch the stage", got)
	}
}

// --- kill point 2: between the two label mutations of a pair -------------

// TestRestartRecovery_KillBetweenLabelPair faults
// addCompleteLabelAndRemoveCI's SECOND call (RemoveLabelFromIssue
// fabrik:awaiting-ci) so the first call (AddLabelToIssue
// stage:Validate:complete) has already landed durably on GitHub — the
// engine dies with the pair half-applied. engine/ci_settle.go's own doc
// comment names this exact shape ("applied via two separate GitHub API
// calls") as the canonical "between the two label mutations of a pair" kill
// point.
func TestRestartRecovery_KillBetweenLabelPair(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{
		Stages:    ciFixStages(),
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxCiFixCycles = 5
		},
	})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	num := FileIssue(t, env, "restart kill between label pair", "Prove recovery when the engine dies between the two halves of addCompleteLabelAndRemoveCI's pair.", "Implement")

	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	sha1 := pr.HeadSHA
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, sha1, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: "success"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}

	env.Sim.Faults().FailWhen("RemoveLabelFromIssue",
		func(a simgh.Args) bool { return a.Number == num && a.Label == "fabrik:awaiting-ci" },
		1, errInjectedSettleFault)

	RunPoll(t, env)
	if !hasLabel(IssueLabels(t, env, num), "stage:Validate:complete") {
		t.Fatal("stage:Validate:complete missing — the first half of the pair should have landed before the fault fired on the second call")
	}
	if !hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-ci") {
		t.Fatal("fabrik:awaiting-ci already cleared despite the injected fault on its own removal call — fault did not fire as expected")
	}
	t.Logf("#%d: half-applied pair confirmed — stage:Validate:complete present, fabrik:awaiting-ci still present", num)

	restarted := RestartEnv(t, env)
	restarted.Sim.Faults().Clear("RemoveLabelFromIssue")

	WaitForLabelAbsent(t, restarted, num, "fabrik:awaiting-ci", 20)
	WaitForIssueClosed(t, restarted, num, 300)
	WaitForProjectStatus(t, restarted, num, "Done", 5)

	// The completion label must still be present exactly once — a
	// RestartEnv that re-ran the whole sequence from scratch (rather than
	// resuming the still-outstanding second half) could plausibly add it
	// twice or clear/re-add it; AddLabelToIssue's own idempotency in simgh
	// would mask a double-*call*, so what actually distinguishes "resumed"
	// from "duplicated" here is that Validate is never dispatched again.
	if got := restarted.Claude.StageCallCount("Validate"); got != 1 {
		t.Errorf("StageCallCount(Validate) after restart = %d, want 1 — recovering the second half of the pair must not re-dispatch the stage", got)
	}
}

// --- kill point 3: PR created, follow-up bookkeeping lost -----------------

// TestRestartRecovery_KillAfterPRCreatedBeforeReady drives Implement's
// default script to a genuine CreateDraftPR success, then faults the
// engine's own immediately-following bookkeeping call about that PR
// (MarkPRReady, per stage.MarkPRReadyOnComplete — engine/pr.go) so the PR
// exists in simgh's model precisely as a real GitHub would retain it, but
// the engine's own follow-up action about it is lost. Recovery must go
// through re-discovery (ensureDraftPR's own "an open PR already exists —
// use it" branch, keyed on FetchLinkedPR) on any subsequent Implement-stage
// touch, never a second CreateDraftPR call — the two would show up as two
// distinct PR numbers in simgh's model, which is the non-vacuity check
// below.
//
// markPRReady (engine/pr.go) does not itself leave any durable
// fabrik:awaiting-* marker on failure — it logs a warning and lets
// handleStageComplete proceed regardless (stage:Implement:complete is
// still granted even though the PR never became ready). No settle scan
// exists to retry a failed MarkPRReady call once that has happened. This
// scenario pins that as-found: the PR is confirmed to remain in draft state
// after restart, matching R4's own "if a step is found genuinely
// unrecoverable, pin the behavior as-found with a comment naming the
// follow-up" allowance. Filed as follow-up: #1582.
func TestRestartRecovery_KillAfterPRCreatedBeforeReady(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	num := FileIssue(t, env, "restart kill after PR created before ready", "Prove no duplicate PR is created after a lost MarkPRReady call.", "Implement", "stage:Plan:complete")

	env.Sim.Faults().FailWhen("MarkPRReady",
		func(a simgh.Args) bool { return true },
		1, errInjectedSettleFault)

	// Implement's own dispatch: CreateDraftPR succeeds (real PR, real
	// mutation), then markPRReady's MarkPRReady call fails.
	AdvanceUntil(t, env, func(env *Env) bool {
		pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
		return err == nil && pr != nil && pr.Number != 0
	}, 40)

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	firstPRNumber := pr.Number
	if !pr.Draft {
		t.Fatal("PR unexpectedly not draft — MarkPRReady fault did not fire as expected, this scenario proves nothing")
	}
	WaitForIssueLabel(t, env, num, "stage:Implement:complete", 40)
	t.Logf("#%d: PR #%d created, still draft after the injected MarkPRReady failure — Implement nonetheless completed", num, firstPRNumber)

	restarted := RestartEnv(t, env)
	restarted.Sim.Faults().Clear("MarkPRReady")

	// Drive further polls (bounded — see below for why this scenario does
	// NOT expect eventual closure) and confirm no duplicate PR is ever
	// created — the recovery-doesn't-duplicate-work half of the assertion,
	// provable independent of whether the stuck-draft gap below is ever
	// resolved.
	RunPolls(t, restarted, 20)
	finalPR, err := restarted.Sim.FetchLinkedPR(restarted.Owner, restarted.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR (final): %v", err)
	}
	if finalPR.Number != firstPRNumber {
		t.Fatalf("final linked PR is #%d, want #%d — a second CreateDraftPR was issued instead of re-discovering the existing PR", finalPR.Number, firstPRNumber)
	}
	if got := restarted.Claude.StageCallCount("Implement"); got != 1 {
		t.Errorf("StageCallCount(Implement) after restart = %d, want 1 — Implement must not be re-dispatched merely to retry MarkPRReady", got)
	}

	// Pin the as-found gap: nothing in the pipeline ever retries a failed
	// MarkPRReady call, and a draft PR's mergeable_state is unconditionally
	// "draft" (tests/sim/simgh/prs.go's deriveMergeableState — checked before
	// any check-run/dirty logic), which attemptMergeOnValidate's direct-merge
	// fallback treats as permanently not-CI-clean. The issue is therefore
	// genuinely stuck at Validate after this restart — not a slow
	// convergence, a structurally unrecoverable one — which is exactly the
	// class of defect R4 asks to be pinned with a comment and a linked
	// follow-up rather than fixed here — filed as #1582.
	if !finalPR.Draft {
		t.Log("NOTE: final PR unexpectedly not draft — the as-found gap this scenario pins (no settle scan retries a failed MarkPRReady) may have been fixed; if so, update/close #1582.")
	} else {
		t.Logf("as-found confirmed: PR #%d remains draft after restart+recovery polls with no mechanism ever retrying the lost MarkPRReady call, permanently blocking the direct-merge fallback (pinned, see #1582)", firstPRNumber)
	}
	if item := projectItem(t, restarted, num); item.IsClosed {
		t.Error("issue unexpectedly closed — the stuck-draft-PR gap this scenario pins appears to have been resolved; update this scenario's doc comment and the tracking follow-up accordingly")
	}
}

// --- kill point 4: mid-spawn-sequence -------------------------------------

// TestRestartRecovery_KillDuringSpawnSequence faults spawnChildren's
// AddBlockedByIssue call (engine/spawn.go) after the child issue itself has
// already been created — the child exists on GitHub, unlinked. spawnChildren
// has no settle-scan-owned recovery marker for this step (unlike the six
// R1-property scans): it pauses the parent hard, with an explicit
// human-recovery instruction ("remove fabrik:paused ... re-advance to
// retry"). This scenario proves that instruction is followed faithfully —
// once an operator (simulated here via a direct label removal, mirroring
// what clicking "remove label" on GitHub does) clears fabrik:paused, the
// engine reattempts the spawn — and pins the genuinely-unrecoverable-without-
// awareness shape this reveals: since fabrik:children-spawned was never
// applied, the retry re-runs spawnChildren from scratch and creates a
// SECOND child issue, per spawnChildren's own doc comment ("v1 does not
// skip already-created children on retry"). This is exactly the kind of
// as-found defect R4 asks to be pinned with a comment and a linked
// follow-up rather than fixed here — filed as #1583. Shared vehicle with
// partial_mutation_test.go's own coverage of this same sequence (see that
// file's enumeration).
func TestRestartRecovery_KillDuringSpawnSequence(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: crossRepoSpawnStages()})

	parent := seedSpawnReadyParent(t, env, env.OwnerRepo, "sim restart spawn child", "Body for the spawn-during-restart scenario.")
	parentItem := projectItem(t, env, parent)

	env.Sim.Faults().FailWhen("AddBlockedByIssue",
		func(a simgh.Args) bool { return a.ID == parentItem.ID },
		1, errInjectedSettleFault)

	WaitForIssueLabel(t, env, parent, "fabrik:paused", 40)
	if hasLabel(IssueLabels(t, env, parent), "fabrik:children-spawned") {
		t.Fatal("fabrik:children-spawned present despite the injected AddBlockedByIssue failure — spawn should not have completed")
	}
	if len(projectItem(t, env, parent).BlockedBy) != 0 {
		t.Fatal("parent already has a blockedBy edge despite the injected fault — fault did not fire as expected")
	}
	// The child issue itself must still have been created — spawnChildren's
	// CreateIssue call landed before the faulted AddBlockedByIssue call.
	childrenBefore := countChildIssuesTitled(t, env, "sim restart spawn child")
	if childrenBefore != 1 {
		t.Fatalf("expected exactly 1 child issue created before the fault fired, got %d", childrenBefore)
	}
	t.Logf("parent #%d paused mid-spawn: child created, blockedBy edge missing", parent)

	restarted := RestartEnv(t, env)
	restarted.Sim.Faults().Clear("AddBlockedByIssue")

	// Simulate the operator following pauseIssue's own printed instructions:
	// remove fabrik:paused, then let the engine re-advance on its own.
	owner, repo, _ := strings.Cut(restarted.OwnerRepo, "/")
	if err := restarted.Sim.RemoveLabelFromIssue(owner, repo, parent, "fabrik:paused"); err != nil {
		t.Fatalf("simulated operator unpause: %v", err)
	}

	WaitForIssueLabel(t, restarted, parent, "fabrik:children-spawned", 40)
	if len(projectItem(t, restarted, parent).BlockedBy) == 0 {
		t.Fatal("parent still has no blockedBy edge after the retried spawn")
	}

	// Pin the as-found duplicate-child defect: the retried spawn has no
	// memory of the child already created before the restart, so it creates
	// a second one with the same title.
	childrenAfter := countChildIssuesTitled(t, restarted, "sim restart spawn child")
	if childrenAfter == 1 {
		t.Log("NOTE: exactly 1 child issue exists after the retried spawn — the as-found duplicate-child gap this scenario pins may have been fixed; if so, update/close #1583.")
	} else {
		t.Logf("as-found confirmed: %d child issues exist after the retried spawn (expected exactly 1 in a fully-recovered world) — spawnChildren's retry has no memory of a prior partial attempt (pinned, see #1583)", childrenAfter)
	}
}

// countChildIssuesTitled counts successful CreateIssue calls in env's
// mutation log whose title (Args.Values[0]) equals title — the sim-side way
// to observe "how many issues did spawnChildren's CreateIssue call actually
// create". Deliberately NOT board-membership-based (e.g. FetchProjectBoard):
// several of this file's own scenarios fault the very step that would add
// the child to the board (AddProjectV2ItemById) or link it
// (AddBlockedByIssue), producing a genuinely board-orphaned child that a
// board scan would silently miss — undercounting exactly the leaked issue
// these scenarios exist to detect. The mutation log records the call
// regardless of what happened to the child afterward.
func countChildIssuesTitled(t *testing.T, env *Env, title string) int {
	t.Helper()
	n := 0
	for _, e := range env.Sim.Log().ByMethod("CreateIssue") {
		if e.Err == nil && len(e.Args.Values) > 0 && e.Args.Values[0] == title {
			n++
		}
	}
	return n
}
