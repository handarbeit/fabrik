package simgh

import (
	"strings"
	"testing"
)

// These tests are the proof that the model is git-backed rather than
// git-shaped. Every conflict below is produced by real commits that genuinely
// cannot be merged; nothing declares a mergeability answer. A fake that let a
// test assert "this PR conflicts" could not catch a mergeability bug, so these
// cases construct the conflict and let git decide.

const (
	repoName    = "acme/widgets"
	headBranch  = "fabrik/issue-7"
	otherBranch = "sibling"
)

// seedConflict builds a genuine three-way conflict: both branches edit the
// same line of the same file after diverging from a common ancestor.
func seedConflict(t *testing.T, s *Sim) {
	t.Helper()
	s.SeedCommit(repoName, "main", map[string]string{"shared.txt": "original\n"}, "common ancestor").
		SeedCommit(repoName, headBranch, map[string]string{"shared.txt": "feature edit\n"}, "feature edits the line").
		SeedCommit(repoName, "main", map[string]string{"shared.txt": "main edit\n"}, "main edits the same line")
	if err := s.Err(); err != nil {
		t.Fatalf("seeding conflict: %v", err)
	}
}

// seedCleanDivergence builds branches that diverge but touch different files,
// so the merge succeeds.
func seedCleanDivergence(t *testing.T, s *Sim) {
	t.Helper()
	s.SeedCommit(repoName, "main", map[string]string{"shared.txt": "original\n"}, "common ancestor").
		SeedCommit(repoName, headBranch, map[string]string{"feature.txt": "feature\n"}, "feature adds its own file").
		SeedCommit(repoName, "main", map[string]string{"mainonly.txt": "main\n"}, "main adds its own file")
	if err := s.Err(); err != nil {
		t.Fatalf("seeding clean divergence: %v", err)
	}
}

func TestFetchPRMergeableFalseOnRealConflict(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedConflict(t, s)

	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", Title: "conflicting"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding PR: %v", err)
	}

	mergeable, err := s.FetchPRMergeable("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRMergeable: %v", err)
	}
	if mergeable == nil {
		t.Fatal("FetchPRMergeable returned nil for a PR with no recompute window pending")
	}
	if *mergeable {
		t.Fatal("FetchPRMergeable = true for branches that genuinely conflict in the backing repo")
	}

	state, err := s.FetchPRMergeableState("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRMergeableState: %v", err)
	}
	if state != stateDirty {
		t.Fatalf("mergeable state = %q, want %q", state, stateDirty)
	}
}

func TestFetchPRMergeableTrueWhenBranchesDoNotConflict(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedCleanDivergence(t, s)

	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", Title: "clean"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding PR: %v", err)
	}

	mergeable, err := s.FetchPRMergeable("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRMergeable: %v", err)
	}
	if mergeable == nil || !*mergeable {
		t.Fatalf("FetchPRMergeable = %v, want true for non-conflicting branches", mergeable)
	}
}

// TestMergeableProbeIsRepeatableAndNonDestructive guards the throwaway-worktree
// design: a trial merge must not leave conflict state behind that poisons the
// next probe, and must never move the base branch.
func TestMergeableProbeIsRepeatableAndNonDestructive(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedConflict(t, s)
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	baseBefore := mustHeadSHA(t, s, repoName, "main")
	for i := range 3 {
		mergeable, err := s.FetchPRMergeable("acme", "widgets", 42)
		if err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
		if mergeable == nil || *mergeable {
			t.Fatalf("probe %d: mergeable = %v, want false every time", i, mergeable)
		}
	}
	if after := mustHeadSHA(t, s, repoName, "main"); after != baseBefore {
		t.Fatalf("a read-only mergeability probe moved main from %s to %s", baseBefore, after)
	}
}

// TestMergePRProducesRealMergeCommit is acceptance criterion 4: the merge must
// be an actual commit on the base ref, and a sibling branch's commits-behind
// count must reflect it.
func TestMergePRProducesRealMergeCommit(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedCleanDivergence(t, s)

	// A sibling branch pinned at main's pre-merge tip, used to observe the
	// merge landing.
	s.SeedBranch(repoName, otherBranch, "main").
		SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", Title: "clean"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	behindBefore, err := s.FetchCommitsBehind("acme", "widgets", "main", otherBranch)
	if err != nil {
		t.Fatalf("FetchCommitsBehind before merge: %v", err)
	}
	if behindBefore != 0 {
		t.Fatalf("sibling is %d commits behind main before the merge, want 0", behindBefore)
	}

	baseBefore := mustHeadSHA(t, s, repoName, "main")
	if err := s.MergePR("acme", "widgets", 42); err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	baseAfter := mustHeadSHA(t, s, repoName, "main")
	if baseAfter == baseBefore {
		t.Fatal("MergePR did not move the base branch — no merge commit was written")
	}

	// The new tip must be a real merge commit: two parents, the old base tip
	// and the PR head.
	r, err := s.repoByKey(repoName)
	if err != nil {
		t.Fatalf("repoByKey: %v", err)
	}
	r.gitMu.Lock()
	parents, gitErr := runGit(r.bareDir, "rev-list", "--parents", "-n", "1", "refs/heads/main")
	r.gitMu.Unlock()
	if gitErr != nil {
		t.Fatalf("rev-list --parents: %v", gitErr)
	}
	fields := strings.Fields(parents)
	if len(fields) != 3 {
		t.Fatalf("main tip has %d parents (%q), want a two-parent merge commit", len(fields)-1, parents)
	}
	headSHA := mustHeadSHA(t, s, repoName, headBranch)
	if fields[1] != baseBefore || fields[2] != headSHA {
		t.Fatalf("merge commit parents = %v, want [%s %s]", fields[1:], baseBefore, headSHA)
	}

	// Acceptance criterion 4's second half: another branch's commits-behind
	// count reflects the merge.
	behindAfter, err := s.FetchCommitsBehind("acme", "widgets", "main", otherBranch)
	if err != nil {
		t.Fatalf("FetchCommitsBehind after merge: %v", err)
	}
	if behindAfter <= behindBefore {
		t.Fatalf("sibling is %d commits behind after the merge, want more than %d", behindAfter, behindBefore)
	}
	// The feature commit plus the merge commit.
	if behindAfter != 2 {
		t.Fatalf("sibling is %d commits behind, want 2 (feature commit + merge commit)", behindAfter)
	}
}

// TestMergePRRefusesRealConflict proves the merge path and the probe agree: a
// PR the probe reports unmergeable cannot be merged, and the refusal carries
// the error kind production uses for a conflict.
func TestMergePRRefusesRealConflict(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedConflict(t, s)
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	baseBefore := mustHeadSHA(t, s, repoName, "main")
	err := s.MergePR("acme", "widgets", 42)
	if err == nil {
		t.Fatal("MergePR succeeded on genuinely conflicting branches")
	}
	if !isNotMergeable(err) {
		t.Fatalf("MergePR error = %v, want gh.ErrNotMergeable", err)
	}
	if after := mustHeadSHA(t, s, repoName, "main"); after != baseBefore {
		t.Fatalf("a refused merge still moved main from %s to %s", baseBefore, after)
	}
	merged, mergeErr := s.FetchPRMerged("acme", "widgets", 42)
	if mergeErr != nil {
		t.Fatalf("FetchPRMerged: %v", mergeErr)
	}
	if merged {
		t.Fatal("PR reports merged after a refused merge")
	}
}

// TestFetchCommitsBehindCountsRealCommits pins the direction of the count:
// it is commits on base that head lacks, matching GitHub's behind_by.
func TestFetchCommitsBehindCountsRealCommits(t *testing.T) {
	s, _ := seedBasicBoard(t)
	s.SeedCommit(repoName, "main", map[string]string{"a.txt": "1\n"}, "one").
		SeedBranch(repoName, headBranch, "main").
		SeedCommit(repoName, "main", map[string]string{"b.txt": "2\n"}, "two").
		SeedCommit(repoName, "main", map[string]string{"c.txt": "3\n"}, "three")
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	behind, err := s.FetchCommitsBehind("acme", "widgets", "main", headBranch)
	if err != nil {
		t.Fatalf("FetchCommitsBehind: %v", err)
	}
	if behind != 2 {
		t.Fatalf("head is %d commits behind main, want 2", behind)
	}

	// The reverse direction is a different quantity: main lacks nothing that
	// the head branch has.
	ahead, err := s.FetchCommitsBehind("acme", "widgets", headBranch, "main")
	if err != nil {
		t.Fatalf("FetchCommitsBehind reversed: %v", err)
	}
	if ahead != 0 {
		t.Fatalf("main is %d commits behind head, want 0", ahead)
	}
}

// TestHeadSHATracksNewCommits proves PR head SHAs are derived from the repo on
// every read rather than frozen at PR creation — the engine keys check runs by
// head SHA, so a stale value would silently read the wrong CI results.
func TestHeadSHATracksNewCommits(t *testing.T) {
	s, _ := seedBasicBoard(t)
	s.SeedCommit(repoName, headBranch, map[string]string{"a.txt": "1\n"}, "first").
		SeedPR(repoName, PRSeed{Number: 42, Head: headBranch})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	first, err := s.FetchLinkedPR("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}

	s.SeedCommit(repoName, headBranch, map[string]string{"a.txt": "2\n"}, "second")
	if err := s.Err(); err != nil {
		t.Fatalf("second commit: %v", err)
	}

	second, err := s.FetchLinkedPR("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchLinkedPR after push: %v", err)
	}
	if second.HeadSHA == first.HeadSHA {
		t.Fatalf("head SHA did not change after a new commit (%s)", first.HeadSHA)
	}
	if second.HeadSHA != mustHeadSHA(t, s, repoName, headBranch) {
		t.Fatalf("head SHA %s does not match the branch tip", second.HeadSHA)
	}
}
