package sim

import (
	"fmt"
	"testing"

	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// This file covers R7: killing the engine at three distinct points inside a
// train and proving reconstructTrainState (engine/merge_train.go) recovers
// correctly across a genuine process-state boundary (RestartEnv, not merely
// a subsequent poll of the same process — see restart.go's own doc
// comment). Each of reconstructTrainState's three routes gets its own
// scenario:
//
//   - Route 3 (orphan): a trial branch exists on origin with no relevant
//     train PR — the shape left behind by a crash between assembleTrialBranch
//     pushing the branch and CreateDraftPR ever being called. Cleaned up
//     silently; the batch re-forms fresh.
//   - Route 2 (resume): an open train PR with a live backing branch — the
//     shape left behind by a crash mid-CI-poll. resumeTrain re-resolves
//     members and continues polling from where it left off.
//   - Route 1 (deferred landing): a merged landing PR whose member is still
//     Queued — the shape left behind by a crash between MergePR succeeding
//     and the engine's own follow-up board/issue bookkeeping running.
//     completeDeferredLanding finds the already-merged PR and finishes the
//     bookkeeping without merging a second time.
//
// R7's own artifacts are directly seeded via simgh (never by racing a live
// goroutine against a real kill signal — see mergetrain_helpers.go's package
// doc comment and adrs/1452-mergetrain-sim-harness-seams.md): every return
// path inside runMergeTrainWorker's loop calls cleanupTrialArtifacts before
// returning, so no natural in-process failure in this harness ever leaves a
// trial branch orphaned the way a real SIGKILL would — the only way to
// construct these durable-state shapes is to place them directly, mirroring
// #1451's restart_recovery_test.go precedent (fault-inject or seed the exact
// state "the last thing that happened" would leave, never interrupt a live
// dispatch).

// --- Route 3: orphaned trial branch, no relevant PR -----------------------

// TestMergeTrainRestart_OrphanedTrialBranchCleanedUp seeds a merge-train
// trial branch directly on origin with no backing PR at all — the shape a
// crash between assembleTrialBranch's push and the subsequent CreateDraftPR
// call would leave. After a restart, the very next train dispatch's
// reconstructTrainState must find no relevant train PR (Route 3: "orphaned
// trial branch(es) on origin but no relevant train PR"), silently clean up
// the orphan, and proceed to form a fresh batch from the real Queued
// member — proving the orphan doesn't wedge the worker or leak a permanent
// stale branch.
func TestMergeTrainRestart_OrphanedTrialBranchCleanedUp(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	const orphanBranch = "fabrik/merge-train/merge-train-main-crashed-orphan"
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, orphanBranch, "main",
		map[string]string{"orphan.txt": "leftover from a simulated crash\n"},
		"orphan trial branch (simulated crash before CreateDraftPR)")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding orphan branch: %v", err)
	}

	num, _ := QueueMember(t, env, "restart-orphan", map[string]string{"real.txt": "real\n"})

	before, err := env.WM.ListTrainBranchesOnOrigin()
	if err != nil {
		t.Fatalf("ListTrainBranchesOnOrigin (before): %v", err)
	}
	if !containsBranchName(before, "fabrik/merge-train/merge-train-main-crashed-orphan") {
		t.Fatalf("orphan branch not present on origin before restart — seeding failed to land where the engine would look: %v", before)
	}

	restarted := restartMergeTrainEnv(t, env)
	startTrialVerdictSeeder(t, restarted, allGreenVerdict)

	RunPoll(t, restarted)

	// The orphan is gone — reconstructTrainState's Route 3 cleaned it up
	// silently on this poll, before (or instead of) forming today's batch.
	after, err := restarted.WM.ListTrainBranchesOnOrigin()
	if err != nil {
		t.Fatalf("ListTrainBranchesOnOrigin (after): %v", err)
	}
	if containsBranchName(after, "fabrik/merge-train/merge-train-main-crashed-orphan") {
		t.Errorf("orphan trial branch still present on origin after restart+poll — Route 3 cleanup did not run: %v", after)
	}

	// The real member still lands normally afterward — the orphan did not
	// wedge the worker or abort today's batch (reconstructTrainState
	// returns false on the orphan-only path specifically so the current
	// batch can still form on this same poll).
	WaitForProjectStatus(t, restarted, num, "Done", 20)
	WaitForIssueClosed(t, restarted, num, 5)

	// No leftover trial branches of any kind remain once the real member has
	// landed — proving nothing else got orphaned along the way either.
	final, err := restarted.WM.ListTrainBranchesOnOrigin()
	if err != nil {
		t.Fatalf("ListTrainBranchesOnOrigin (final): %v", err)
	}
	if len(final) != 0 {
		t.Errorf("expected zero merge-train branches on origin once the real member landed, got %v", final)
	}
}

// --- Route 2: open train PR with a live backing branch (resumeTrain) ------

// TestMergeTrainRestart_ResumesOpenTrialFromLiveBranch seeds a draft CI trial
// PR (batch marker, "Closes #N" for the member, still open) backed by a real
// trial branch on origin — the shape a crash mid-CI-poll would leave: the
// trial was assembled and its draft PR opened, but the engine died before
// ever observing a verdict. After a restart, reconstructTrainState's Route 2
// ("an open train PR with still-Queued members" + a backing branch present)
// must resume via resumeTrain rather than starting a brand new trial —
// proving no duplicate trial branch or duplicate draft PR is created, and
// that CI resolution (via this file's own verdict seeder, which reacts to
// the pre-existing open PR on its very first scan after being started) still
// lands the member normally.
func TestMergeTrainRestart_ResumesOpenTrialFromLiveBranch(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	num, _ := QueueMember(t, env, "restart-resume", map[string]string{"resume.txt": "resume\n"})

	const trialName = "merge-train-main-crashed-resume"
	const trialBranch = mergeTrainBranchPrefix + trialName
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, trialBranch, "main",
		map[string]string{"resume.txt": "resume\n"}, // same content the real assembly would have merged
		"trial assembly (simulated crash mid-CI-poll)")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding trial branch: %v", err)
	}

	body := fmt.Sprintf("🏭 **Fabrik merge-train trial integration for #%d**\n\nCloses #%d\n\n%s",
		num, num, mergeTrainBatchMarker)
	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{
		Head:  trialBranch,
		Base:  "main",
		Title: fmt.Sprintf("[merge-train] batch: #%d", num),
		Body:  body,
		Draft: true,
	})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding trial PR: %v", err)
	}

	draftPRsBefore := draftPRCount(env)

	restarted := restartMergeTrainEnv(t, env)
	startTrialVerdictSeeder(t, restarted, allGreenVerdict)

	RunPoll(t, restarted)

	WaitForProjectStatus(t, restarted, num, "Done", 20)
	WaitForIssueClosed(t, restarted, num, 5)

	// No duplicate trial: resumeTrain reused the seeded PR/branch rather than
	// assembling and opening a second draft CI PR. draftPRCount only counts
	// calls the engine itself made (the seeded PR went in via SeedPR, not
	// CreateDraftPR), so it should read exactly what it read before restart —
	// zero new draft PRs from this run.
	if got := draftPRCount(restarted); got != draftPRsBefore {
		t.Errorf("CreateDraftPR called %d time(s) after restart, want %d (no new draft PR — resumeTrain must reuse the seeded trial) — mutation log:\n%s",
			got, draftPRsBefore, restarted.Sim.Log().Dump(20))
	}

	// Exactly one merge — landMergeTrainBatch, reached via
	// resumeTrain->landGreenBatch, never doubles up.
	if got := len(restarted.Sim.Log().ByMethod("MergePR")); got != 1 {
		t.Errorf("MergePR called %d time(s), want exactly 1 — no double-merge", got)
	}

	final, err := restarted.WM.ListTrainBranchesOnOrigin()
	if err != nil {
		t.Fatalf("ListTrainBranchesOnOrigin (final): %v", err)
	}
	if len(final) != 0 {
		t.Errorf("expected zero merge-train branches on origin once the resumed trial landed, got %v", final)
	}
}

// --- Route 1: merged landing PR, member still Queued (completeDeferredLanding) ---

// TestMergeTrainRestart_CompletesDeferredLandingAfterMerge seeds an
// already-merged landing PR (batch marker, "Closes #N") whose member issue
// is still open and still sitting in Queued — the shape reconstructTrainState's
// own doc comment names directly: "a crash between MergePR succeeding and
// the engine's own follow-up board/issue bookkeeping running."
//
// This state is deliberately constructed with the seeded landing PR's base
// set to a non-default branch, not "main" — and that choice is itself worth
// explaining, not just declaring. GitHub (and simgh's faithful model of it,
// confirmed directly against simgh/prs.go and seed.go) auto-closes every
// issue a merged PR's body references via "Closes #N", but *only* when the
// merge lands on the repo's actual default branch. landMergeTrainBatch's own
// integration PR body unconditionally carries "Closes #N" for every
// survivor (buildIntegrationPRBody), so on the default branch that auto-close
// fires synchronously with the merge itself — before the engine's own
// subsequent advance/close calls ever run. That leaves no timing window in
// this harness where the member is genuinely still open after a real
// default-branch merge (mergetrain_landing_test.go's
// TestMergeTrainLanding_BatchCloseAsymmetryPin hit the identical fact from
// the landMergeTrainBatch-asymmetry angle). A non-default base sidesteps
// that auto-close specifically to construct the exact durable-state shape
// completeDeferredLanding exists to recover — reconstructTrainState's own
// Route-1 matching never inspects the found PR's base branch, so this is a
// faithful exercise of the real recovery code, not a workaround of it. In
// live production the equivalent trigger is understood to be a narrow
// GitHub-side propagation window between the merge API call returning and
// the auto-close side effect being processed — a race FIDELITY.md's Absent
// section already excludes from this model's scope.
func TestMergeTrainRestart_CompletesDeferredLandingAfterMerge(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	num, prNum := QueueMember(t, env, "restart-deferred", map[string]string{"deferred.txt": "deferred\n"})

	// A throwaway branch, distinct from the repo's real default ("main"), so
	// the seeded merged PR's base differs from it and GitHub's merge-time
	// auto-close (gated on base == the repo's actual default branch) does
	// not fire — see the doc comment above.
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, "side-base", "main",
		map[string]string{"side-base-marker.txt": "x\n"}, "throwaway non-default base for Route 1")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding side-base branch: %v", err)
	}

	const trialName = "merge-train-main-crashed-deferred"
	const trialBranch = mergeTrainBranchPrefix + trialName
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, trialBranch, "side-base",
		map[string]string{"deferred.txt": "deferred\n"}, "trial assembly for the deferred-landing PR")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding trial branch: %v", err)
	}

	body := fmt.Sprintf("🏭 **Fabrik merge-train landing PR**\n\n"+
		"This PR lands the following Queued issues via the internal merge train:\n\n"+
		"- #%d — merge-train member restart-deferred\n\nCloses #%d\n\n%s",
		num, num, mergeTrainBatchMarker)
	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{
		Head:   trialBranch,
		Base:   "side-base",
		Title:  fmt.Sprintf("[merge-train] batch: #%d", num),
		Body:   body,
		Merged: true,
	})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding merged landing PR: %v", err)
	}

	// Confirm the seeded state actually matches the scenario's premise before
	// touching the engine at all: member still open, still Queued.
	if projectItem(t, env, num).IsClosed {
		t.Fatal("member unexpectedly already closed by the seed itself — the non-default-base workaround did not prevent auto-close, this scenario proves nothing")
	}
	if got := projectItem(t, env, num).Status; got != "Queued" {
		t.Fatalf("member status = %q before restart, want Queued", got)
	}

	restarted := restartMergeTrainEnv(t, env)

	RunPoll(t, restarted)

	WaitForProjectStatus(t, restarted, num, "Done", 20)
	WaitForIssueClosed(t, restarted, num, 5)

	// The member's own PR is closed too — checked directly via
	// FetchPRDetails, not WaitForIssueClosed/projectItem: a member's own PR
	// is never itself placed on the project board (only its issue is), so
	// projectItem(prNum) would always report "not on the board" regardless
	// of the PR's real state.
	memberPR, err := restarted.Sim.FetchPRDetails(restarted.Owner, restarted.Repo, prNum)
	if err != nil {
		t.Fatalf("FetchPRDetails(#%d): %v", prNum, err)
	}
	if memberPR.State != "closed" {
		t.Errorf("member PR #%d state = %q, want closed", prNum, memberPR.State)
	}

	// The whole point: MergePR is never called for this crash-recovery
	// landing — the seeded PR was already merged, and completeDeferredLanding
	// must find alreadyMerged=true and skip straight to the bookkeeping.
	if got := len(restarted.Sim.Log().ByMethod("MergePR")); got != 0 {
		t.Errorf("MergePR called %d time(s), want 0 — completeDeferredLanding must never re-merge an already-merged landing PR (no double-merge):\n%s",
			got, restarted.Sim.Log().Dump(20))
	}
}
