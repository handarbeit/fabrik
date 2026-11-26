package sim

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

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

// --- #2628: the Done cleanup of a closed item records a landed PR -------------

// seedMergedThenDone reproduces the #2628 shape: an item at Validate with a
// linked PR is merged outside Fabrik (a real merge, which closes the issue) and
// the board's "Item closed -> Done" automation moves it to Done before the
// engine's first poll. extraLabels are added at seed time (e.g. the pause pair).
func seedMergedThenDone(t *testing.T, env *Env, marker, prState string, extraLabels ...string) (num int) {
	t.Helper()
	num = FileIssue(t, env, "sim closed-cleanup-fill "+marker, "marker="+marker, "Validate", extraLabels...)
	if prState == "" {
		return num
	}
	branch := fmt.Sprintf("fabrik/issue-%d", num)
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, "", map[string]string{
		fmt.Sprintf("sim/closed-cleanup/%s-%d.md", marker, num): "marker=" + marker + "\n",
	}, "sim: closed cleanup fill")
	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{Title: "sim " + marker, Head: branch, IssueNumber: num, State: prState})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seed PR: %v", err)
	}
	return num
}

// moveItemToDone moves the issue's card to the Done column the way the board
// automation does, closing the issue first when closeIssue is set.
func moveItemToDone(t *testing.T, env *Env, num int) {
	t.Helper()
	item, err := env.Sim.FetchProjectItem(env.Owner, env.Repo, num)
	if err != nil || item == nil {
		t.Fatalf("FetchProjectItem #%d: %v", num, err)
	}
	board, err := env.Sim.FetchProjectBoard(env.Owner, env.Repo, env.ProjectNum, "User")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	sf, err := env.Sim.FetchStatusField(board.ProjectID)
	if err != nil {
		t.Fatalf("FetchStatusField: %v", err)
	}
	if err := env.Sim.UpdateProjectItemStatus(board.ProjectID, item.ItemID, sf.FieldID, sf.Options["Done"]); err != nil {
		t.Fatalf("UpdateProjectItemStatus: %v", err)
	}
}

func linkedPRNumber(t *testing.T, env *Env, num int) int {
	t.Helper()
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("FetchLinkedPR #%d: %v", num, err)
	}
	return pr.Number
}

func closedCleanupFillEnv(t *testing.T) *Env {
	t.Helper()
	return NewEnv(t, EnvOptions{Stages: pausedMergedPRRecoveryStages()})
}

// TestClosedCleanupFill_MergedPRRecorded is the #2628 acceptance scenario: the
// item first seen closed at Done, its linked PR merged, ends with the
// gate-checked completion labels filled alongside stage:Done:complete.
func TestClosedCleanupFill_MergedPRRecorded(t *testing.T) {
	t.Parallel()
	skipIfNoGit(t)
	for _, tc := range []struct {
		name   string
		labels []string
	}{
		{"not paused", nil},
		{"paused", []string{"fabrik:paused", "fabrik:awaiting-input"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := closedCleanupFillEnv(t)
			num := seedMergedThenDone(t, env, "merged-"+tc.name, "open", tc.labels...)
			if err := env.Sim.MergePR(env.Owner, env.Repo, linkedPRNumber(t, env, num)); err != nil {
				t.Fatalf("MergePR: %v", err)
			}
			moveItemToDone(t, env, num)
			comments := issueCommentCount(t, env, num)

			WaitForIssueLabel(t, env, num, "stage:Done:complete", 40)

			labels := IssueLabels(t, env, num)
			for _, l := range []string{"stage:Validate:complete", "fabrik:paused", "fabrik:awaiting-input"} {
				want := l == "stage:Validate:complete"
				if hasLabel(labels, l) != want {
					t.Errorf("label %s present=%v, want %v (labels %v)", l, !want, want, labels)
				}
			}
			if hasLabel(labels, "fabrik:awaiting-landing-verification") {
				t.Errorf("landing-verification marker added: %v", labels)
			}
			if got := issueCommentCount(t, env, num); got != comments {
				t.Errorf("comments %d -> %d, want none added", comments, got)
			}
		})
	}
}

// TestClosedCleanupFill_Neutralised is the non-vacuity check: with the fill
// switched off the same scenario ends without stage:Validate:complete.
func TestClosedCleanupFill_Neutralised(t *testing.T) {
	t.Parallel()
	skipIfNoGit(t)
	env := closedCleanupFillEnv(t)
	env.Engine.SetClosedCleanupFillDisabledForTest(true)
	num := seedMergedThenDone(t, env, "neutralised", "open", "fabrik:paused", "fabrik:awaiting-input")
	if err := env.Sim.MergePR(env.Owner, env.Repo, linkedPRNumber(t, env, num)); err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	moveItemToDone(t, env, num)

	WaitForIssueLabel(t, env, num, "stage:Done:complete", 40)

	if labels := IssueLabels(t, env, num); hasLabel(labels, "stage:Validate:complete") {
		t.Errorf("fill neutralised but stage:Validate:complete was applied (%v): the scenario is vacuous", labels)
	}
}

// TestClosedCleanupFill_FailuresRetry: a failed PR read or a failed fill leaves
// stage:Done:complete unapplied (and the item uncleaned), and the cleanup
// converges once the cooldown has passed.
func TestClosedCleanupFill_FailuresRetry(t *testing.T) {
	t.Parallel()
	skipIfNoGit(t)
	boom := errors.New("sim: injected failure")
	for _, tc := range []struct {
		name   string
		inject func(env *Env)
	}{
		{"PR read fails", func(env *Env) { env.Sim.Faults().FailOnce("FetchLinkedPR", boom) }},
		{"fill fails", func(env *Env) {
			env.Sim.Faults().FailWhen("AddLabelToIssue", func(a simgh.Args) bool {
				return a.Label == "stage:Validate:complete"
			}, 1, boom)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := closedCleanupFillEnv(t)
			num := seedMergedThenDone(t, env, "retry", "open", "fabrik:paused", "fabrik:awaiting-input")
			if err := env.Sim.MergePR(env.Owner, env.Repo, linkedPRNumber(t, env, num)); err != nil {
				t.Fatalf("MergePR: %v", err)
			}
			moveItemToDone(t, env, num)
			tc.inject(env)

			RunPoll(t, env)
			if hasLabel(IssueLabels(t, env, num), "stage:Done:complete") {
				t.Fatal("stage:Done:complete applied on the failing poll")
			}

			// Step past the periodic-re-eval cooldown and poll until it converges.
			AdvanceUntil(t, env, func(env *Env) bool {
				env.Clock.Advance(2 * time.Minute)
				return hasLabel(IssueLabels(t, env, num), "stage:Done:complete")
			}, 40)
			if labels := IssueLabels(t, env, num); !hasLabel(labels, "stage:Validate:complete") {
				t.Errorf("retry did not fill stage:Validate:complete: %v", labels)
			}
		})
	}
}

// TestClosedCleanupFill_NoFillWhenNothingLanded: no linked PR, or one closed
// unmerged, is cleaned up exactly as before — no fill.
func TestClosedCleanupFill_NoFillWhenNothingLanded(t *testing.T) {
	t.Parallel()
	skipIfNoGit(t)
	for _, tc := range []string{"no linked PR", "closed unmerged"} {
		t.Run(tc, func(t *testing.T) {
			t.Parallel()
			env := closedCleanupFillEnv(t)
			prState := ""
			if tc == "closed unmerged" {
				prState = "closed"
			}
			num := seedMergedThenDone(t, env, "nofill", prState)
			if err := env.Sim.CloseIssue(env.Owner, env.Repo, num); err != nil {
				t.Fatalf("CloseIssue: %v", err)
			}
			moveItemToDone(t, env, num)

			WaitForIssueLabel(t, env, num, "stage:Done:complete", 40)

			if labels := IssueLabels(t, env, num); hasLabel(labels, "stage:Validate:complete") {
				t.Errorf("stage:Validate:complete filled with no landed PR: %v", labels)
			}
		})
	}
}
