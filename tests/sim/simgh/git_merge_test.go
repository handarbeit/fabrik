package simgh

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// TestMergePRCreatesMergeCommitEvenWhenFastForwardable pins GitHub's default
// "Create a merge commit" strategy: when the head is a strict descendant of the
// base, git would happily fast-forward, but GitHub still writes a two-parent
// merge commit. Without this case the divergent-branch test above cannot tell
// --no-ff from --ff, since divergent branches force a merge commit either way.
func TestMergePRCreatesMergeCommitEvenWhenFastForwardable(t *testing.T) {
	s, _ := seedBasicBoard(t)
	// The head branch descends directly from main, which does not move.
	s.SeedCommit(repoName, "main", map[string]string{"base.txt": "base\n"}, "base").
		SeedCommit(repoName, headBranch, map[string]string{"feature.txt": "feature\n"}, "feature").
		SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	baseBefore := mustHeadSHA(t, s, repoName, "main")
	headSHA := mustHeadSHA(t, s, repoName, headBranch)

	// Precondition: this merge really is fast-forwardable.
	behind, err := s.FetchCommitsBehind("acme", "widgets", "main", headBranch)
	if err != nil {
		t.Fatalf("FetchCommitsBehind: %v", err)
	}
	if behind != 0 {
		t.Fatalf("precondition: head is %d commits behind main, want 0", behind)
	}

	if err := s.MergePR("acme", "widgets", 42); err != nil {
		t.Fatalf("MergePR: %v", err)
	}

	after := mustHeadSHA(t, s, repoName, "main")
	if after == headSHA {
		t.Fatal("MergePR fast-forwarded main onto the head commit; GitHub's merge-commit strategy writes a merge commit")
	}

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
	if fields[1] != baseBefore || fields[2] != headSHA {
		t.Fatalf("merge commit parents = %v, want [%s %s]", fields[1:], baseBefore, headSHA)
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

// TestMergePRRefusesWhenNothingToMerge pins that a merge which would write no
// commit is refused rather than recorded.
//
// `git merge --no-ff` prints "Already up to date." and exits *zero* when the
// head is already contained in the base, leaving the tip untouched. Publishing
// that tip would flip merged=true having written no merge commit at all —
// silently breaking the invariant this package and FIDELITY.md both state
// unconditionally ("MergePR writes a real merge commit onto the base ref").
//
// The state is ordinary, not contrived: merging a branch and then merging it
// again reaches it, which a scenario replaying a merge-train batch can do.
func TestMergePRRefusesWhenNothingToMerge(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedCleanDivergence(t, s)

	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", Title: "first"}).
		SeedPR(repoName, PRSeed{Number: 43, Head: headBranch, Base: "main", Title: "again"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := s.MergePR("acme", "widgets", 42); err != nil {
		t.Fatalf("first MergePR: %v", err)
	}

	// main now contains headBranch, so #43 has nothing left to merge.
	tipBefore := mustHeadSHA(t, s, repoName, "main")

	err := s.MergePR("acme", "widgets", 43)
	if err == nil {
		t.Fatal("MergePR succeeded with nothing to merge — it would have recorded a merge that wrote no commit")
	}
	if !isNotMergeable(err) {
		t.Fatalf("MergePR error = %v, want it to wrap gh.ErrNotMergeable", err)
	}

	// The refusal must leave no trace: no moved ref, and no merged flag.
	if tipAfter := mustHeadSHA(t, s, repoName, "main"); tipAfter != tipBefore {
		t.Fatalf("main moved from %s to %s on a refused merge", tipBefore, tipAfter)
	}
	merged, ferr := s.FetchPRMerged("acme", "widgets", 43)
	if ferr != nil {
		t.Fatalf("FetchPRMerged: %v", ferr)
	}
	if merged {
		t.Fatal("PR #43 reports merged after a refused merge")
	}

	// The read-only probe is deliberately unaffected: there is no conflict, so
	// mergeability still reports true. Only the merge itself refuses, matching
	// GitHub, where "nothing to merge" is not a conflict.
	mergeable, perr := s.FetchPRMergeable("acme", "widgets", 43)
	if perr != nil {
		t.Fatalf("FetchPRMergeable: %v", perr)
	}
	if mergeable == nil || !*mergeable {
		t.Fatalf("FetchPRMergeable = %v, want true — nothing-to-merge is not a conflict", mergeable)
	}
}

// TestTrialWorktreeStaysInsideBaseDir pins that throwaway worktrees are created
// under Sim.baseDir.
//
// The deferred cleanup in withWorktree covers every ordinary path, but a
// process killed mid-merge (OOM, SIGKILL, a CI timeout) never runs it. Under
// the OS temp dir that orphan outlives both the sandbox the package doc
// promises and t.TempDir()'s cleanup; under baseDir the test framework is the
// backstop.
func TestTrialWorktreeStaysInsideBaseDir(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedCleanDivergence(t, s)

	r, err := s.repoByKey(repoName)
	if err != nil {
		t.Fatalf("repoByKey: %v", err)
	}

	// Resolve inside the callback: the deferred cleanup deletes the directory
	// before withWorktree returns, and EvalSymlinks cannot resolve a path that
	// no longer exists.
	var seen, got string
	r.gitMu.Lock()
	wtErr := r.withWorktree("refs/heads/main", func(wt string) error {
		seen = wt
		// The checkout must be real, not just a path: a directory that is not
		// a worktree would make the containment assertion meaningless.
		if _, statErr := os.Stat(filepath.Join(wt, ".git")); statErr != nil {
			return fmt.Errorf("worktree has no .git entry: %w", statErr)
		}
		resolved, resErr := filepath.EvalSymlinks(wt)
		if resErr != nil {
			return fmt.Errorf("EvalSymlinks(worktree): %w", resErr)
		}
		got = resolved
		return nil
	})
	r.gitMu.Unlock()
	if wtErr != nil {
		t.Fatalf("withWorktree: %v", wtErr)
	}

	base, err := filepath.EvalSymlinks(s.baseDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(baseDir): %v", err)
	}
	rel, err := filepath.Rel(base, got)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("worktree %s is outside baseDir %s (rel=%q, err=%v)", got, base, rel, err)
	}

	// And it must not survive the call.
	if _, statErr := os.Stat(seen); !os.IsNotExist(statErr) {
		t.Fatalf("worktree %s still exists after withWorktree returned (stat err = %v)", seen, statErr)
	}
}

// TestSeedBranchRefusesExistingBranch pins that SeedBranch creates a branch
// rather than repointing one.
//
// The raw `update-ref` underneath would happily move an existing branch, so
// seeding a branch that already carries commits would discard them and still
// report success — a scenario author would only notice by wondering where
// their commits went. GitHub's create-ref endpoint refuses the same case
// (422 "Reference already exists"), so refusing is both the faithful answer
// and the safe one. Growing a branch is SeedCommit's job.
func TestSeedBranchRefusesExistingBranch(t *testing.T) {
	s, _ := seedBasicBoard(t)
	s.SeedCommit(repoName, headBranch, map[string]string{"feature.txt": "work\n"}, "seeded work")
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	before := mustHeadSHA(t, s, repoName, headBranch)

	s.SeedBranch(repoName, headBranch, "main")

	err := s.Err()
	if err == nil {
		t.Fatal("SeedBranch onto an existing branch reported success; it silently discarded the seeded commits")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("SeedBranch error = %v, want an 'already exists' refusal", err)
	}
	// The refusal must leave the branch alone. An error raised only after the
	// ref had already moved would still have lost the commits.
	if after := mustHeadSHA(t, s, repoName, headBranch); after != before {
		t.Fatalf("refused SeedBranch still moved %s from %s to %s", headBranch, before, after)
	}
}

// TestSeedPRMergedTruePerformsRealMerge pins item 4's fix: SeedPR{Merged: true}
// must not merely flip a bookkeeping flag. It has to write the same merge
// commit MergePR would, and run the same closing-keyword auto-close, or a
// merge-train scenario asserting "this member already landed" would be
// asserting against git history and issue state that never actually
// happened. See FIDELITY.md, "SeedPR{Merged: true}", and #1498.
func TestSeedPRMergedTruePerformsRealMerge(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedCleanDivergence(t, s)

	baseBefore := mustHeadSHA(t, s, repoName, "main")
	headSHA := mustHeadSHA(t, s, repoName, headBranch)

	// Linked via IssueNumber, the same as CreateDraftPR would link it — issue
	// #7 is the one seedBasicBoard already seeds.
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", Title: "already landed", IssueNumber: 7, Merged: true})
	if err := s.Err(); err != nil {
		t.Fatalf("SeedPR(Merged: true): %v", err)
	}

	baseAfter := mustHeadSHA(t, s, repoName, "main")
	if baseAfter == baseBefore {
		t.Fatal("SeedPR(Merged: true) did not move the base branch — no merge commit was written")
	}

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
	if fields[1] != baseBefore || fields[2] != headSHA {
		t.Fatalf("merge commit parents = %v, want [%s %s]", fields[1:], baseBefore, headSHA)
	}

	merged, err := s.FetchPRMerged("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRMerged: %v", err)
	}
	if !merged {
		t.Fatal("PR reports merged = false after SeedPR(Merged: true)")
	}

	// The linked issue must be auto-closed, exactly as MergePR's own
	// closing-keyword loop would do it.
	iss, err := s.FetchIssue("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	if iss.State != "closed" {
		t.Fatalf("linked issue #7 state = %q, want %q after SeedPR(Merged: true) on the default branch", iss.State, "closed")
	}
}

// TestSeedPRMergedTrueRefusesConflict pins the other half of item 4: seeding
// is authoritative about *state*, not about git history it cannot represent.
// Merged: true against branches that genuinely conflict is refused rather
// than silently recorded — GitHub could never have produced that PR as
// merged either.
func TestSeedPRMergedTrueRefusesConflict(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedConflict(t, s)

	baseBefore := mustHeadSHA(t, s, repoName, "main")

	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", Title: "conflicting", Merged: true})

	err := s.Err()
	if err == nil {
		t.Fatal("SeedPR(Merged: true) accepted branches that genuinely conflict")
	}
	if !strings.Contains(err.Error(), "does not merge cleanly") {
		t.Fatalf("error = %v, want it to name the conflict", err)
	}

	// The refusal must leave no trace: no moved ref, and no PR record at all.
	if after := mustHeadSHA(t, s, repoName, "main"); after != baseBefore {
		t.Fatalf("a refused SeedPR(Merged: true) still moved main from %s to %s", baseBefore, after)
	}
	if _, ferr := s.FetchPRMerged("acme", "widgets", 42); ferr == nil {
		t.Fatal("a refused SeedPR(Merged: true) still recorded a PR")
	}
}

// countLooseObjects parses `git count-objects -v`'s "count:" line — the
// number of loose objects sitting outside any pack, which is exactly where an
// orphaned probe merge commit lives before it is packed or pruned. Caller
// must hold gitMu.
func countLooseObjects(t *testing.T, bareDir string) int {
	t.Helper()
	out, err := runGit(bareDir, "count-objects", "-v")
	if err != nil {
		t.Fatalf("count-objects: %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if name, val, ok := strings.Cut(line, ": "); ok && name == "count" {
			n, convErr := strconv.Atoi(val)
			if convErr != nil {
				t.Fatalf("parsing count-objects line %q: %v", line, convErr)
			}
			return n
		}
	}
	t.Fatalf("count-objects -v output %q has no \"count:\" line", out)
	return 0
}

// TestMergeableProbeBoundsUnreferencedObjects pins item 5's fix: a read-only
// mergeability probe runs a real `git merge --no-ff` and discards the result
// without moving a ref, so the merge commit it wrote is orphaned in the
// object store the instant the call returns. Left unchecked that grows
// without bound across a long, poll-heavy scenario — merge-train bisection
// is the worst case, since it re-probes mergeability across a bisection
// sequence on top of a per-trial-SHA FetchCheckRuns poll. tryMerge now runs a
// periodic `git gc --prune=now` to reclaim them. See FIDELITY.md, "Trial
// merges leave unreachable objects", and #1498.
func TestMergeableProbeBoundsUnreferencedObjects(t *testing.T) {
	s, _ := seedBasicBoard(t)
	r, err := s.repoByKey(repoName)
	if err != nil {
		t.Fatalf("repoByKey: %v", err)
	}

	// probeGCThreshold-1 distinct probes, each merging a genuinely different
	// branch into main. Distinct branches matter: repeating one identical
	// probe would recreate byte-identical commit content each time (same
	// tree, same parents, same message), and git's content-addressable store
	// would just recognise the object already exists rather than writing a
	// new one — which would prove nothing about accumulation.
	for i := 0; i < probeGCThreshold-1; i++ {
		branch := fmt.Sprintf("feature-%d", i)
		s.SeedCommit(repoName, branch, map[string]string{fmt.Sprintf("f%d.txt", i): "x"}, fmt.Sprintf("commit %d", i))
		if err := s.Err(); err != nil {
			t.Fatalf("seeding branch %d: %v", i, err)
		}
		r.gitMu.Lock()
		_, conflict, mergeErr := r.tryMerge("main", branch, false, "simgh trial merge")
		r.gitMu.Unlock()
		if mergeErr != nil {
			t.Fatalf("probe %d: %v", i, mergeErr)
		}
		if conflict {
			t.Fatalf("probe %d: unexpected conflict", i)
		}
	}

	r.gitMu.Lock()
	beforeGC := countLooseObjects(t, r.bareDir)
	counterBeforeThreshold := r.probesSinceGC
	r.gitMu.Unlock()
	if beforeGC == 0 {
		t.Fatal("no loose objects accumulated from probeGCThreshold-1 distinct probes; the probe path may not be writing real merge commits")
	}
	if counterBeforeThreshold != probeGCThreshold-1 {
		t.Fatalf("probesSinceGC = %d before the threshold-crossing probe, want %d", counterBeforeThreshold, probeGCThreshold-1)
	}

	// One more probe crosses the threshold and must trigger the housekeeping
	// gc within this same call.
	branch := fmt.Sprintf("feature-%d", probeGCThreshold-1)
	s.SeedCommit(repoName, branch, map[string]string{"last.txt": "x"}, "final commit")
	if err := s.Err(); err != nil {
		t.Fatalf("seeding final branch: %v", err)
	}
	r.gitMu.Lock()
	_, conflict, mergeErr := r.tryMerge("main", branch, false, "simgh trial merge")
	r.gitMu.Unlock()
	if mergeErr != nil {
		t.Fatalf("threshold-crossing probe: %v", mergeErr)
	}
	if conflict {
		t.Fatal("threshold-crossing probe: unexpected conflict")
	}

	r.gitMu.Lock()
	afterGC := countLooseObjects(t, r.bareDir)
	counterAfterThreshold := r.probesSinceGC
	r.gitMu.Unlock()
	if afterGC >= beforeGC {
		t.Fatalf("loose object count after crossing the gc threshold = %d, want fewer than the %d accumulated before it — "+
			"the housekeeping gc did not reclaim the unreferenced merge commits", afterGC, beforeGC)
	}
	if counterAfterThreshold != 0 {
		t.Fatalf("probesSinceGC = %d after crossing the threshold, want 0 (reset by the gc)", counterAfterThreshold)
	}

	// The gc must not have disturbed anything reachable: main's tip is
	// unaffected — a probe never moves it in the first place, and the gc
	// prunes only what nothing points at.
	r.gitMu.Lock()
	_, mainErr := r.resolveRef("refs/heads/main")
	r.gitMu.Unlock()
	if mainErr != nil {
		t.Fatalf("main is unreadable after the housekeeping gc: %v", mainErr)
	}
}
