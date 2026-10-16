package simgh

import (
	"errors"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// #2342: the pinned writes refuse a head that moved, with gh.ErrHeadMoved, and
// the BeforeNthCall hook runs its side effect between two engine calls.

func movedPR(t *testing.T) (*Sim, string, string) {
	t.Helper()
	s, _ := seedBasicBoard(t)
	old := seedPRForState(t, s, false)
	s.SeedCommit(repoName, headBranch, map[string]string{"late.txt": "late\n"}, "late push")
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return s, old, mustHeadSHA(t, s, repoName, headBranch)
}

func TestPinnedWritesRefuseAMovedHead(t *testing.T) {
	s, old, cur := movedPR(t)
	s.SeedMergeQueueEnabled(repoName, true)

	if err := s.EnablePullRequestAutoMerge("acme", "widgets", 42, "MERGE", old); !errors.Is(err, gh.ErrHeadMoved) {
		t.Errorf("enable at stale head: err = %v, want ErrHeadMoved", err)
	}
	if err := s.EnqueuePullRequest("acme", "widgets", 42, old); !errors.Is(err, gh.ErrHeadMoved) {
		t.Errorf("enqueue at stale head: err = %v, want ErrHeadMoved", err)
	}
	err := s.MergePRAtHeadSHA("acme", "widgets", 42, old)
	if !errors.Is(err, gh.ErrHeadMoved) || !errors.Is(err, gh.ErrConflict) {
		t.Errorf("merge at stale head: err = %v, want both ErrHeadMoved and ErrConflict", err)
	}
	if got := mustHeadSHA(t, s, repoName, "main"); got == cur {
		t.Error("a refused merge must not move main")
	}

	if err := s.EnablePullRequestAutoMerge("acme", "widgets", 42, "MERGE", cur); err != nil {
		t.Errorf("enable at the live head: %v", err)
	}
	if err := s.EnablePullRequestAutoMerge("acme", "widgets", 42, "MERGE", ""); err == nil {
		t.Error("an empty expected head must be refused, never treated as unpinned")
	}
}

func TestAutoMergeRefusedCleanIsSeedable(t *testing.T) {
	s, _ := seedBasicBoard(t)
	sha := seedPRForState(t, s, false)
	s.SeedAutoMergeRefusedClean(repoName, 42)
	if err := s.EnablePullRequestAutoMerge("acme", "widgets", 42, "MERGE", sha); !errors.Is(err, gh.ErrAutoMergeAlreadyClean) {
		t.Fatalf("err = %v, want ErrAutoMergeAlreadyClean", err)
	}
}

func TestBeforeNthCallRunsOnceBeforeTheNthCall(t *testing.T) {
	in, _ := newInstrumented(t)
	ran := 0
	in.Faults().BeforeNthCall("GetIssueBody", 2, func() {
		ran++
		// The hook may call back into the model: the table's mutex is not held.
		if _, err := in.sim.GetIssueBody("acme", "widgets", 7); err != nil {
			t.Errorf("hook call into the Sim: %v", err)
		}
	})
	for i := 1; i <= 3; i++ {
		if _, err := in.GetIssueBody("acme", "widgets", 7); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if want := map[int]int{1: 0, 2: 1, 3: 1}[i]; ran != want {
			t.Errorf("after call %d the hook has run %d time(s), want %d", i, ran, want)
		}
	}
}
