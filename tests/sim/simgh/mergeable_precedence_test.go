package simgh

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// The single-condition tests in mergeable_state_test.go prove each branch of
// deriveMergeableState is reachable. They do not prove the branches are
// *ordered* correctly: every one of them arranges exactly one condition, so a
// mutation that reshuffled the precedence would leave them all green.
//
// That matters more here than it would for a derivation copied from a spec.
// The order is the sim's own hand-picked design choice (FIDELITY.md says so,
// and rates the residual risk "medium, concentrated in untested
// combinations"), so nothing outside these tests constrains it.
//
// Each test below arranges two conditions that resolve to different states and
// asserts the higher-precedence one wins. Together they pin every *adjacent*
// link in the chain — draft > dirty > behind > blocked > unstable — which
// constrains the whole ordering transitively rather than spot-checking pairs.

// TestPrecedenceDraftOverDirty pins draft > dirty.
//
// The existing draft test arranges a draft PR with everything else green, so it
// would pass under any ordering. This one makes the PR genuinely conflict as
// well: FIDELITY.md promises "a draft PR that also conflicts resolves to draft,
// not dirty", and this is what holds that promise to account.
func TestPrecedenceDraftOverDirty(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedConflict(t, s)
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", Draft: true})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Precondition: the conflict is real, so "dirty" is the competing answer.
	mergeable, err := s.FetchPRMergeable("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRMergeable: %v", err)
	}
	if mergeable == nil || *mergeable {
		t.Fatal("precondition: the seeded head must genuinely conflict with main")
	}

	assertState(t, s, stateDraft)
}

// TestPrecedenceDirtyOverBehind pins dirty > behind.
//
// seedConflict advances main past the fork point, so the head is both behind
// and conflicting. With up-to-date heads required, "behind" is a live competing
// answer — a conflict must still win, since rebasing cannot be the engine's
// first move when the merge itself is impossible.
func TestPrecedenceDirtyOverBehind(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedConflict(t, s)
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main"}).
		SeedRequireUpToDate(repoName, "main", true)
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	behind, err := s.FetchCommitsBehind("acme", "widgets", "main", headBranch)
	if err != nil {
		t.Fatalf("FetchCommitsBehind: %v", err)
	}
	if behind == 0 {
		t.Fatal("precondition: head must be behind main for this to test precedence")
	}

	assertState(t, s, stateDirty)
}

// TestPrecedenceBehindOverBlocked pins behind > blocked.
//
// This is the combination Pruefer called out by name. Both conditions are
// arranged: main has advanced with up-to-date heads required, and a required
// context is failing. The engine treats these very differently — "behind" is
// rebase territory, "blocked" carries ErrNotMergeableCI and must never enter
// the rebase path — so which one the model reports is load-bearing.
func TestPrecedenceBehindOverBlocked(t *testing.T) {
	s, _ := seedBasicBoard(t)
	sha := seedPRForState(t, s, false)

	s.SeedCommit(repoName, "main", map[string]string{"unrelated.txt": "moved on\n"}, "main advances").
		SeedRequireUpToDate(repoName, "main", true).
		SeedRequiredContexts(repoName, "main", []string{"ci/build"}).
		SeedCheckRun(repoName, sha, gh.CheckRun{Name: "ci/build", Status: "completed", Conclusion: "failure"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	behind, err := s.FetchCommitsBehind("acme", "widgets", "main", headBranch)
	if err != nil {
		t.Fatalf("FetchCommitsBehind: %v", err)
	}
	if behind == 0 {
		t.Fatal("precondition: head must be behind main for this to test precedence")
	}

	assertState(t, s, stateBehind)
}

// TestPrecedenceBlockedOverUnstable pins blocked > unstable.
//
// The other combination Pruefer named. A failing required context and a failing
// non-required one are both present. Reporting "unstable" here would be a
// merge-safety bug of exactly ADR-072's kind: MergeableStateAccepted admits
// unstable, so the PR would merge with a required check red.
func TestPrecedenceBlockedOverUnstable(t *testing.T) {
	s, _ := seedBasicBoard(t)
	sha := seedPRForState(t, s, false)

	s.SeedRequiredContexts(repoName, "main", []string{"ci/build"}).
		SeedCheckRun(repoName, sha, gh.CheckRun{Name: "ci/build", Status: "completed", Conclusion: "failure"}).
		SeedCheckRun(repoName, sha, gh.CheckRun{Name: "ci/lint", Status: "completed", Conclusion: "failure"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	assertState(t, s, stateBlocked)

	// And the refusal must carry the CI-readiness error, not the conflict one —
	// the engine routes only ErrNotMergeable into the rebase path.
	err := s.MergePR("acme", "widgets", 42)
	if !isNotMergeableCI(err) {
		t.Fatalf("MergePR error = %v, want ErrNotMergeableCI", err)
	}
}

// TestPrecedenceUnstableOverClean pins unstable > clean: a green required
// context does not mask a red non-required one.
func TestPrecedenceUnstableOverClean(t *testing.T) {
	s, _ := seedBasicBoard(t)
	sha := seedPRForState(t, s, false)

	s.SeedRequiredContexts(repoName, "main", []string{"ci/build"}).
		SeedCheckRun(repoName, sha, gh.CheckRun{Name: "ci/build", Status: "completed", Conclusion: "success"}).
		SeedCheckRun(repoName, sha, gh.CheckRun{Name: "ci/lint", Status: "completed", Conclusion: "failure"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	assertState(t, s, stateUnstable)
}
