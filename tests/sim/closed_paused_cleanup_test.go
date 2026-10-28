package sim

import (
	"os"
	"testing"

	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// seedDoneItem seeds an item in Done with the given state and labels and, when
// withWorktree is set, a real worktree on disk.
func seedDoneItem(t *testing.T, env *Env, closed, withWorktree bool, labels ...string) int {
	t.Helper()
	const num = 2459
	state := "OPEN"
	if closed {
		state = "CLOSED"
	}
	env.Sim.Sim().SeedIssue(env.OwnerRepo, simgh.IssueSeed{
		Number: num, Title: "paused then closed", Status: "Done", State: state, Labels: labels,
	})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedIssue: %v", err)
	}
	if withWorktree {
		preCreateWorktree(t, env, num)
	}
	return num
}

func worktreeExists(env *Env, num int) bool {
	_, err := os.Stat(env.WM.WorktreeDir(num))
	return err == nil
}

func issueCommentCount(t *testing.T, env *Env, num int) int {
	t.Helper()
	cs, err := env.Sim.Sim().FetchIssueComments(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	return len(cs)
}

// TestClosedPausedCleanup_ClosedItemIsCleanedUp is the #2459 acceptance
// scenario: a closed issue carrying the pause pair, placed in Done with an
// existing worktree, ends with the worktree removed, both pause labels gone,
// stage:Done:complete applied and no comment posted.
func TestClosedPausedCleanup_ClosedItemIsCleanedUp(t *testing.T) {
	t.Parallel()
	skipIfNoGit(t)
	for _, tc := range []struct {
		name         string
		withWorktree bool
		labels       []string
	}{
		{"with worktree", true, []string{"fabrik:paused", "fabrik:awaiting-input"}},
		{"no worktree", false, []string{"fabrik:paused", "fabrik:awaiting-input"}},
		{"paused only", true, []string{"fabrik:paused"}},
		{"awaiting-input only", true, []string{"fabrik:awaiting-input"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := NewEnv(t, EnvOptions{Stages: smokeStages()})
			num := seedDoneItem(t, env, true, tc.withWorktree, tc.labels...)
			if tc.withWorktree && !worktreeExists(env, num) {
				t.Fatal("setup: worktree was not created")
			}
			comments := issueCommentCount(t, env, num)

			WaitForIssueLabel(t, env, num, "stage:Done:complete", 40)

			labels := IssueLabels(t, env, num)
			for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
				if hasLabel(labels, l) {
					t.Errorf("%s still present after cleanup: %v", l, labels)
				}
			}
			if worktreeExists(env, num) {
				t.Error("worktree not removed")
			}
			if got := issueCommentCount(t, env, num); got != comments {
				t.Errorf("comments %d -> %d, want no comment for clearing the pause (FR-004)", comments, got)
			}
			if got := env.Claude.CommentCallCount("Done"); got != 0 {
				t.Errorf("Claude comment pass ran %d time(s) on a closed cleanup item", got)
			}
		})
	}
}

// TestClosedPausedCleanup_OpenItemIsLeftAlone is FR-003: the same scenario with
// the issue open leaves the worktree, the labels and the absence of the
// completion label untouched across repeated polls.
func TestClosedPausedCleanup_OpenItemIsLeftAlone(t *testing.T) {
	t.Parallel()
	skipIfNoGit(t)
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	num := seedDoneItem(t, env, false, true, "fabrik:paused", "fabrik:awaiting-input")

	RunPolls(t, env, 15)

	labels := IssueLabels(t, env, num)
	for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
		if !hasLabel(labels, l) {
			t.Errorf("%s was removed from an open paused item: %v", l, labels)
		}
	}
	if hasLabel(labels, "stage:Done:complete") {
		t.Errorf("completion label applied to an open paused item: %v", labels)
	}
	if !worktreeExists(env, num) {
		t.Error("worktree of an open paused item was removed")
	}
}

// TestClosedPausedCleanup_Neutralised is the non-vacuity check: with the fix
// switched off the closed scenario is left stuck, so the assertions above bite.
func TestClosedPausedCleanup_Neutralised(t *testing.T) {
	t.Parallel()
	skipIfNoGit(t)
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Engine.SetClosedPausedCleanupDisabledForTest(true)
	num := seedDoneItem(t, env, true, true, "fabrik:paused", "fabrik:awaiting-input")

	RunPolls(t, env, 15)

	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "stage:Done:complete") || !hasLabel(labels, "fabrik:paused") || !worktreeExists(env, num) {
		t.Errorf("fix neutralised but the item was still cleaned up (labels %v, worktree %v): the scenario is vacuous",
			labels, worktreeExists(env, num))
	}
}
