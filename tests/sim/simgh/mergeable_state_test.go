package simgh

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// The engine branches hard on mergeable_state: MergeableStateAccepted admits
// only {clean, unstable}, and ErrNotMergeableCI fires specifically on
// blocked/unknown and must never be routed into the rebase-reinvoke path. A
// model that could only say dirty-or-clean would make four of the six values
// unreachable and ADR-072's merge-safety incident unreproducible here.
//
// Each case below builds its state from real conditions — a real conflict, a
// real commits-behind count, a real red check against a real head SHA — rather
// than declaring the answer.

// seedPRForState builds a repo with a head branch off main and a PR on it,
// returning the head SHA that check runs must be keyed by.
func seedPRForState(t *testing.T, s *Sim, draft bool) string {
	t.Helper()
	s.SeedCommit(repoName, "main", map[string]string{"base.txt": "base\n"}, "base").
		SeedCommit(repoName, headBranch, map[string]string{"feature.txt": "feature\n"}, "feature").
		SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", Draft: draft})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return mustHeadSHA(t, s, repoName, headBranch)
}

func TestMergeableStateClean(t *testing.T) {
	s, _ := seedBasicBoard(t)
	sha := seedPRForState(t, s, false)
	s.SeedRequiredContexts(repoName, "main", []string{"ci/build"}).
		SeedCheckRun(repoName, sha, gh.CheckRun{Name: "ci/build", Status: "completed", Conclusion: "success"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	assertState(t, s, stateClean)
}

func TestMergeableStateBlockedOnFailingRequiredContext(t *testing.T) {
	s, _ := seedBasicBoard(t)
	sha := seedPRForState(t, s, false)
	s.SeedRequiredContexts(repoName, "main", []string{"ci/build"}).
		SeedCheckRun(repoName, sha, gh.CheckRun{Name: "ci/build", Status: "completed", Conclusion: "failure"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	assertState(t, s, stateBlocked)
}

func TestMergeableStateBlockedOnMissingRequiredContext(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedPRForState(t, s, false)
	// The required context has simply never reported — GitHub blocks on this
	// exactly as it blocks on a failure.
	s.SeedRequiredContexts(repoName, "main", []string{"ci/build"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	assertState(t, s, stateBlocked)
}

func TestMergeableStateBlockedOnPendingRequiredContext(t *testing.T) {
	s, _ := seedBasicBoard(t)
	sha := seedPRForState(t, s, false)
	s.SeedRequiredContexts(repoName, "main", []string{"ci/build"}).
		SeedCheckRun(repoName, sha, gh.CheckRun{Name: "ci/build", Status: "in_progress"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	assertState(t, s, stateBlocked)
}

// TestMergeableStateUnstableOnFailingNonRequiredContext is the distinction the
// whole required-context mechanism exists for: the same red check that yields
// "blocked" when required yields "unstable" when it is not, and "unstable" is
// mergeable.
func TestMergeableStateUnstableOnFailingNonRequiredContext(t *testing.T) {
	s, _ := seedBasicBoard(t)
	sha := seedPRForState(t, s, false)
	s.SeedRequiredContexts(repoName, "main", []string{"ci/build"}).
		SeedCheckRun(repoName, sha, gh.CheckRun{Name: "ci/build", Status: "completed", Conclusion: "success"}).
		SeedCheckRun(repoName, sha, gh.CheckRun{Name: "cleanup", Status: "completed", Conclusion: "failure"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	assertState(t, s, stateUnstable)
	if !gh.MergeableStateAccepted(stateUnstable) {
		t.Fatal("precondition: production must accept unstable as mergeable")
	}
}

// TestMergeableStateBlockedViaClassicCommitStatus covers ADR-933's path: a
// required context posted through the classic Statuses API rather than as a
// check run must block just the same. This is why the model keeps the two
// collections genuinely separate.
func TestMergeableStateBlockedViaClassicCommitStatus(t *testing.T) {
	s, _ := seedBasicBoard(t)
	sha := seedPRForState(t, s, false)
	s.SeedRequiredContexts(repoName, "main", []string{"local-ci"}).
		SeedCommitStatus(repoName, sha, gh.CommitStatus{Context: "local-ci", State: "failure"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	assertState(t, s, stateBlocked)

	// FetchCombinedStatus must surface it, and FetchCheckRuns must not — a
	// model that conflated them would show the status as a check run.
	statuses, err := s.FetchCombinedStatus("acme", "widgets", sha)
	if err != nil {
		t.Fatalf("FetchCombinedStatus: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Context != "local-ci" {
		t.Fatalf("combined status = %+v, want one local-ci entry", statuses)
	}
	runs, err := s.FetchCheckRuns("acme", "widgets", sha)
	if err != nil {
		t.Fatalf("FetchCheckRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("check runs = %+v, want none — a commit status is not a check run", runs)
	}
}

// TestMergeableStateCleanViaClassicCommitStatus is the discriminating half of
// the ADR-933 pair. Blocking on a *failing* commit status proves nothing on its
// own: a derivation that ignored commit statuses entirely would also report
// "blocked", because the required context would simply be missing. Only a
// *passing* commit status can distinguish "read and green" from "not read at
// all".
func TestMergeableStateCleanViaClassicCommitStatus(t *testing.T) {
	s, _ := seedBasicBoard(t)
	sha := seedPRForState(t, s, false)
	s.SeedRequiredContexts(repoName, "main", []string{"local-ci"}).
		SeedCommitStatus(repoName, sha, gh.CommitStatus{Context: "local-ci", State: "success"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	assertState(t, s, stateClean)
}

// TestMergeableStateUnstableViaNonRequiredCommitStatus is the same
// discrimination for the unstable branch: a red commit status that is not
// required must make the PR unstable, which is only reachable if commit
// statuses reach the derivation at all.
func TestMergeableStateUnstableViaNonRequiredCommitStatus(t *testing.T) {
	s, _ := seedBasicBoard(t)
	sha := seedPRForState(t, s, false)
	s.SeedCommitStatus(repoName, sha, gh.CommitStatus{Context: "optional-lint", State: "failure"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	assertState(t, s, stateUnstable)
}

func TestMergeableStateDraftTakesPrecedence(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedPRForState(t, s, true)

	// A draft PR resolves to "draft" even with everything else green; the
	// precedence order is documented on FetchPRMergeableFields.
	assertState(t, s, stateDraft)

	if err := s.MarkPRReady("acme", "widgets", 42); err != nil {
		t.Fatalf("MarkPRReady: %v", err)
	}
	assertState(t, s, stateClean)
}

// TestMergeableStateBehindRequiresBranchProtection pins the model's most
// consequential fidelity choice: GitHub reports "behind" only where branch
// protection requires up-to-date heads. Without that setting a conflict-free
// but out-of-date PR is "clean", and a model that always said "behind" would
// make "clean" nearly unreachable and misroute the engine's rebase logic.
func TestMergeableStateBehindRequiresBranchProtection(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedPRForState(t, s, false)

	// Advance main past the PR's fork point, with a non-conflicting change.
	s.SeedCommit(repoName, "main", map[string]string{"unrelated.txt": "moved on\n"}, "main advances")
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	behind, err := s.FetchCommitsBehind("acme", "widgets", "main", headBranch)
	if err != nil {
		t.Fatalf("FetchCommitsBehind: %v", err)
	}
	if behind == 0 {
		t.Fatal("precondition: head should be behind main after main advanced")
	}

	// Without the requirement, being behind does not block.
	assertState(t, s, stateClean)

	s.SeedRequireUpToDate(repoName, "main", true)
	if err := s.Err(); err != nil {
		t.Fatalf("seeding requirement: %v", err)
	}
	assertState(t, s, stateBehind)
}

func TestMergeableStateDirtyOnRealConflict(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedConflict(t, s)
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	assertState(t, s, stateDirty)
}

// TestMergePRGatesOnDerivedState is the ADR-072 guard: a PR in a blocked state
// must not merge, and the refusal must carry ErrNotMergeableCI rather than
// ErrNotMergeable — the engine treats those differently, and only the latter
// may enter the rebase path.
func TestMergePRGatesOnDerivedState(t *testing.T) {
	s, _ := seedBasicBoard(t)
	sha := seedPRForState(t, s, false)
	s.SeedRequiredContexts(repoName, "main", []string{"ci/build"}).
		SeedCheckRun(repoName, sha, gh.CheckRun{Name: "ci/build", Status: "completed", Conclusion: "failure"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	baseBefore := mustHeadSHA(t, s, repoName, "main")
	err := s.MergePR("acme", "widgets", 42)
	if err == nil {
		t.Fatal("MergePR merged a PR in the blocked state")
	}
	if !isNotMergeableCI(err) {
		t.Fatalf("MergePR error = %v, want gh.ErrNotMergeableCI for a blocked PR", err)
	}
	if isNotMergeable(err) {
		t.Fatalf("a CI refusal must not also satisfy ErrNotMergeable — it would be routed into the rebase path: %v", err)
	}
	if after := mustHeadSHA(t, s, repoName, "main"); after != baseBefore {
		t.Fatalf("a refused merge still moved main from %s to %s", baseBefore, after)
	}
}

// TestMergePRSucceedsOnUnstable is the other side of the gate: "unstable" is
// in MergeableStateAccepted, so a red *non-required* check must not stop a
// merge.
func TestMergePRSucceedsOnUnstable(t *testing.T) {
	s, _ := seedBasicBoard(t)
	sha := seedPRForState(t, s, false)
	s.SeedRequiredContexts(repoName, "main", []string{"ci/build"}).
		SeedCheckRun(repoName, sha, gh.CheckRun{Name: "ci/build", Status: "completed", Conclusion: "success"}).
		SeedCheckRun(repoName, sha, gh.CheckRun{Name: "cleanup", Status: "completed", Conclusion: "failure"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	assertState(t, s, stateUnstable)
	if err := s.MergePR("acme", "widgets", 42); err != nil {
		t.Fatalf("MergePR refused an unstable PR: %v", err)
	}
	merged, err := s.FetchPRMerged("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRMerged: %v", err)
	}
	if !merged {
		t.Fatal("PR does not report merged after a successful merge")
	}
}

// TestMergeableRecomputeWindow covers GitHub's "still computing" window: both
// fields report unresolved together, each read drains one, and the real
// git-derived answer surfaces afterwards. MergePR returning ErrNotMergeable on
// a null read is what made a healthy issue (#1087) look wedged, so this path
// has to be reachable rather than merely documented.
func TestMergeableRecomputeWindow(t *testing.T) {
	s, _ := seedBasicBoard(t)
	s.SeedCommit(repoName, "main", map[string]string{"base.txt": "base\n"}, "base").
		SeedCommit(repoName, headBranch, map[string]string{"feature.txt": "feature\n"}, "feature").
		SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", MergeableRecomputeReads: 2})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	for i := range 2 {
		mergeable, state, err := s.FetchPRMergeableFields("acme", "widgets", 42)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if mergeable != nil {
			t.Fatalf("read %d: mergeable = %v, want nil during the recompute window", i, *mergeable)
		}
		if state != stateUnknown {
			t.Fatalf("read %d: state = %q, want %q", i, state, stateUnknown)
		}
	}

	mergeable, state, err := s.FetchPRMergeableFields("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("read after window: %v", err)
	}
	if mergeable == nil || !*mergeable {
		t.Fatalf("after the window, mergeable = %v, want true", mergeable)
	}
	if state != stateClean {
		t.Fatalf("after the window, state = %q, want %q", state, stateClean)
	}
}

// TestMergePRRefusesDuringRecomputeWindow proves the window is not merely
// cosmetic: while it is open, the merge is refused as not-mergeable, which is
// the production behaviour that made #1087 look stuck.
func TestMergePRRefusesDuringRecomputeWindow(t *testing.T) {
	s, _ := seedBasicBoard(t)
	s.SeedCommit(repoName, "main", map[string]string{"base.txt": "base\n"}, "base").
		SeedCommit(repoName, headBranch, map[string]string{"feature.txt": "feature\n"}, "feature").
		SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", MergeableRecomputeReads: 1})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	err := s.MergePR("acme", "widgets", 42)
	if err == nil {
		t.Fatal("MergePR succeeded during the recompute window")
	}
	if !isNotMergeable(err) {
		t.Fatalf("MergePR error = %v, want gh.ErrNotMergeable during recompute", err)
	}

	// The window has now drained, so the retry the engine would make succeeds.
	if err := s.MergePR("acme", "widgets", 42); err != nil {
		t.Fatalf("MergePR after the window closed: %v", err)
	}
}

// TestMergedPRReportsUnknown covers the merged/closed case: there is no
// mergeability left to report.
func TestMergedPRReportsUnknown(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedPRForState(t, s, false)

	if err := s.MergePR("acme", "widgets", 42); err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	mergeable, state, err := s.FetchPRMergeableFields("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRMergeableFields: %v", err)
	}
	if mergeable != nil {
		t.Fatalf("merged PR reports mergeable = %v, want nil", *mergeable)
	}
	if state != stateUnknown {
		t.Fatalf("merged PR reports state %q, want %q", state, stateUnknown)
	}
}

// assertState checks the derived mergeable_state for PR 42 in the standard
// fixture repo.
func assertState(t *testing.T, s *Sim, want string) {
	t.Helper()
	got, err := s.FetchPRMergeableState("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRMergeableState: %v", err)
	}
	if got != want {
		t.Fatalf("mergeable state = %q, want %q", got, want)
	}
}
