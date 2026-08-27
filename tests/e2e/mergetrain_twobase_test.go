//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// TestMergeTrainTwoBasesConcurrent is the AC1/AC2/AC3 e2e proof for #1648: Queued
// members on two different base branches in the same repo produce TWO independent
// merge trains, each forking from its own base and landing through its own
// integration PR targeting that base — not one train mis-based against whichever
// base happens to resolve first (the #1646 bug #1647 only guarded against, and this
// issue actually fixes).
//
// Setup mirrors TestMergeTrainHappyPathLanding (column-driven QueueMember/CreateMemberPR,
// no full pipeline run needed — ADR-059 D1), plus a throwaway non-default base branch
// (mirroring TestBaseBranchPipeline's CreateThrowawayBaseBranch) and QueueMemberOnBase
// to additionally apply the base:<branch> label the engine's own partitioning reads.
//
// Assertions:
//   - AC1: both batches land (two independent trains actually ran to completion).
//   - AC2: the maint-branch batch's integration PR targets the throwaway branch, not
//     main — and the main-branch batch's integration PR targets main, not the
//     throwaway branch (touches main not at all).
//   - AC3: the two integration PRs are distinct numbers (no cross-contamination via
//     findIntegrationPR / trial-branch identity — the #1617/#1614 regression shape).
//
// Wall-clock: ~10–25 min (two concurrent combined validations + integration-PR CI,
// running in parallel — not sequential). Cost: low (no Claude invocations — no
// conflicts to resolve on either base's clean batch).
func TestMergeTrainTwoBasesConcurrent(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	requireTrainBed(t, env)

	stamp := time.Now().UTC().Format("20060102-150405")
	maintBranch := fmt.Sprintf("e2e-maint-%s", stamp)
	CreateThrowawayBaseBranch(t, env, env.RepoAlpha, maintBranch)

	logStart := LogOffset(t, env)

	type member struct {
		issue int
		pr    int
	}

	// Two clean members on the default base (main).
	var mainMembers []member
	for _, m := range []struct{ marker, path, content string }{
		{"twobase-main-a", "e2e/train/twobase/main-a.txt", "main partition member a\n"},
		{"twobase-main-b", "e2e/train/twobase/main-b.txt", "main partition member b\n"},
	} {
		iss, pr := QueueMember(t, env, env.RepoAlpha, "main", m.marker, m.path, m.content)
		mainMembers = append(mainMembers, member{iss, pr})
	}

	// Two clean members on the throwaway non-default base — a distinct partition.
	var maintMembers []member
	for _, m := range []struct{ marker, path, content string }{
		{"twobase-maint-a", "e2e/train/twobase/maint-a.txt", "maint partition member a\n"},
		{"twobase-maint-b", "e2e/train/twobase/maint-b.txt", "maint partition member b\n"},
	} {
		iss, pr := QueueMemberOnBase(t, env, env.RepoAlpha, maintBranch, m.marker, m.path, m.content)
		maintMembers = append(maintMembers, member{iss, pr})
	}
	t.Logf("queued %d main-base member(s) and %d maint-base member(s); awaiting two independent trains",
		len(mainMembers), len(maintMembers))

	// Both trains must independently reach "landing complete" — proves two separate
	// worker runs, not one train that batched everything against a single base.
	WaitForLogLine(t, env, "landing complete", logStart, 30*time.Minute)
	t.Logf("at least one train landed a batch — waiting for both partitions' members individually")

	for _, m := range mainMembers {
		WaitForMemberLanded(t, env, env.RepoAlpha, m.issue, 15*time.Minute)
		waitForPRClosed(t, env, env.RepoAlpha, m.pr, 10*time.Minute)
		WaitForIssueClosed(t, env, env.RepoAlpha, m.issue, 10*time.Minute)
	}
	for _, m := range maintMembers {
		WaitForMemberLanded(t, env, env.RepoAlpha, m.issue, 15*time.Minute)
		waitForPRClosed(t, env, env.RepoAlpha, m.pr, 10*time.Minute)
		WaitForIssueClosed(t, env, env.RepoAlpha, m.issue, 10*time.Minute)
	}
	t.Logf("all members on both partitions landed")

	// AC2/AC3: resolve each partition's own integration/landing PR from its own
	// member's "landed via ..." comment (member-scoped, safe under concurrent
	// trains — see waitForLandingPRNumber's doc comment) and confirm it targets the
	// correct base, and the two partitions' landing PRs are distinct.
	mainLandingPR := waitForLandingPRNumber(t, env, env.RepoAlpha, mainMembers[0].pr, 2*time.Minute)
	maintLandingPR := waitForLandingPRNumber(t, env, env.RepoAlpha, maintMembers[0].pr, 2*time.Minute)

	if mainLandingPR == maintLandingPR {
		t.Fatalf("expected two distinct landing PRs (one per base partition), both members landed via PR #%d", mainLandingPR)
	}
	assertPRMerged(t, env, env.RepoAlpha, mainLandingPR)
	assertPRMerged(t, env, env.RepoAlpha, maintLandingPR)

	if got := PRBaseRef(t, env, env.RepoAlpha, mainLandingPR); got != "main" {
		t.Errorf("main-partition landing PR #%d targets %q, want \"main\"", mainLandingPR, got)
	}
	if got := PRBaseRef(t, env, env.RepoAlpha, maintLandingPR); got != maintBranch {
		t.Errorf("maint-partition landing PR #%d targets %q, want %q (AC2: must not fall back to / touch main)", maintLandingPR, got, maintBranch)
	}
	t.Logf("confirmed independent landings: main via PR #%d (base=main), %s via PR #%d (base=%s)",
		mainLandingPR, maintBranch, maintLandingPR, maintBranch)

	// No stale train branches/PRs should remain after both landings.
	WaitForNoStaleTrainArtifacts(t, env, env.RepoAlpha, 2*time.Minute)
	t.Logf("two-base concurrent landing verified: 2 partitions, 2 independent integration PRs, no cross-contamination")
}
