package sim

import (
	"fmt"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// Sim twins of the review gate on the catch-up advance (#2491, ADR 2491).
//
// The reported bypass: a stage with wait_for_ci + wait_for_reviews +
// review_authority: authoritative left Review while a reviewer's change request
// still stood. handleReviewGate runs checkReviewGate only when stage:X:complete was
// already present before the handler chain (#617), so on the poll where CI clears
// and checkCIGate adds the label the verdict was never asked and Phase 2 advanced
// the item. Both reported shapes reach that poll:
//
//   - the post-fix-round shape: a fix round finishes (a new head), CI goes green on
//     it, and the change request is still unanswered;
//   - the not-mergeable shape: a base conflict dispatches an in-stage rebase, the
//     rebase pushes a new head, CI goes green on it, and the change request is
//     still unanswered.
//
// Each scenario runs as shipped and, as its non-vacuity control, with
// engine.SetReviewGateAdvanceDisabledForTest(true): the neutralised run must reach
// Validate while the verdict is still blocking.

const advanceGateCheck = "ci"

// newAdvanceGateEnv builds a Review stage that waits for both CI and reviewers
// (authoritative, one declared bot), followed by Validate. The item carries
// fabrik:cruise so Phase 2 would advance it as soon as no gate holds it.
func newAdvanceGateEnv(t *testing.T) *Env {
	t.Helper()
	tr := true
	declared := []string{"pruefer-bot"}
	env := NewEnv(t, EnvOptions{
		Stages: []*stages.Stage{
			{Name: "Implement", Order: 1, CreateDraftPR: true, MarkPRReadyOnComplete: true},
			{Name: "Review", Order: 2, WaitForCI: &tr, WaitForReviews: &tr, ReviewAuthority: "authoritative", ExpectedReviewers: &declared},
			{Name: "Validate", Order: 3},
			{Name: "Done", Order: 4, CleanupWorktree: true},
		},
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.ReviewWaitTimeout = 15 * time.Minute
			cfg.MaxReviewCycles = 5
			cfg.MaxRebaseCycles = 3
		},
	})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{advanceGateCheck})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return env
}

// seedAwaitingCIReviewItem files an item at Review that has finished its stage run
// and is waiting for CI (fabrik:awaiting-ci, no completion label yet), with a PR.
func seedAwaitingCIReviewItem(t *testing.T, env *Env, marker string, files map[string]string) (num, prNum int) {
	t.Helper()
	num = FileIssue(t, env, "sim advance gate "+marker, "advance gate "+marker, "Review",
		"fabrik:awaiting-ci", "fabrik:cruise")
	branch := fmt.Sprintf("fabrik/issue-%d", num)
	if files == nil {
		files = map[string]string{fmt.Sprintf("sim/advance-gate/%s-%d.md", marker, num): "x\n"}
	}
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, "", files, "sim: "+marker)
	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{Title: "sim " + marker, Head: branch, IssueNumber: num})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil || pr.Number == 0 {
		t.Fatalf("FetchLinkedPR: %v %+v", err, pr)
	}
	return num, pr.Number
}

func seedGreenCI(t *testing.T, env *Env, sha string) {
	t.Helper()
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: advanceGateCheck, Status: "completed", Conclusion: "success"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}
}

func boardStatusOf(t *testing.T, env *Env, num int) string {
	t.Helper()
	return projectItem(t, env, num).Status
}

// leftReview drives polls until the item leaves Review or the budget is
// spent, and reports whether it left.
func leftReview(t *testing.T, env *Env, num, polls int) bool {
	t.Helper()
	for i := 0; i < polls; i++ {
		RunPolls(t, env, 1)
		if boardStatusOf(t, env, num) != "Review" {
			return true
		}
	}
	return false
}

func TestReviewGateAdvance_PostFixRoundDoesNotBypassBlockingGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		disabled     bool
		wantAdvanced bool
	}{{"as shipped", false, false}, {"neutralised (pre-#2491)", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newAdvanceGateEnv(t)
			env.Engine.SetReviewGateAdvanceDisabledForTest(tc.disabled)
			num, prNum := seedAwaitingCIReviewItem(t, env, "post-fix", nil)

			// The bot requested changes on the first head; a fix round pushed a
			// second head; CI is green on it; the bot has not re-reviewed.
			a := headOf(t, env, prNum)
			seedBareReview(t, env, prNum, "pruefer-bot[bot]", "CHANGES_REQUESTED", a)
			b := pushFix(t, env, num, prNum)
			seedGreenCI(t, env, b)

			if got := leftReview(t, env, num, 25); got != tc.wantAdvanced {
				t.Fatalf("left Review = %v, want %v (status %q, labels %v)", got, tc.wantAdvanced, boardStatusOf(t, env, num), IssueLabels(t, env, num))
			}
			if tc.wantAdvanced {
				return
			}
			labels := IssueLabels(t, env, num)
			if !hasLabel(labels, "fabrik:awaiting-review") {
				t.Errorf("a held item carries fabrik:awaiting-review (the wait clock anchor), got %v", labels)
			}
			if hasLabel(labels, "fabrik:paused") {
				t.Errorf("a hold is not a pause, got %v", labels)
			}

			// The verdict clears when the bot re-reviews the new head: the item
			// advances through the normal path.
			seedBareReview(t, env, prNum, "pruefer-bot[bot]", "COMMENTED", b)
			if !leftReview(t, env, num, 40) {
				t.Fatalf("item did not advance once the gate cleared (status %q, labels %v)", boardStatusOf(t, env, num), IssueLabels(t, env, num))
			}
		})
	}
}

func TestReviewGateAdvance_NotMergeableDoesNotBypassBlockingGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		disabled     bool
		wantAdvanced bool
	}{{"as shipped", false, false}, {"neutralised (pre-#2491)", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newAdvanceGateEnv(t)
			env.Engine.SetReviewGateAdvanceDisabledForTest(tc.disabled)
			env.Claude.ForStageComments("Review", rebaseResolveScript())
			num, prNum := seedAwaitingCIReviewItem(t, env, "not-mergeable",
				map[string]string{conflictingPath: "pr content\n"})

			a := headOf(t, env, prNum)
			seedBareReview(t, env, prNum, "pruefer-bot[bot]", "CHANGES_REQUESTED", a)
			// The base moves under the PR: a genuine add/add conflict.
			env.Sim.Sim().SeedCommit(env.OwnerRepo, "main", map[string]string{conflictingPath: "main content\n"}, "external: conflicting change")
			if err := env.Sim.Sim().Err(); err != nil {
				t.Fatalf("SeedCommit: %v", err)
			}

			// The base conflict is handled in the stage: a rebase reinvoke pushes a
			// new head and the item has not left Review.
			WaitForIssueLabel(t, env, num, "fabrik:rebase-needed", 40)
			var c string
			AdvanceUntil(t, env, func(env *Env) bool {
				c = headOf(t, env, prNum)
				return c != a
			}, 60)
			if st := boardStatusOf(t, env, num); st != "Review" {
				t.Fatalf("item left Review (%q) during the rebase", st)
			}
			seedGreenCI(t, env, c)

			if got := leftReview(t, env, num, 25); got != tc.wantAdvanced {
				t.Fatalf("left Review = %v, want %v (status %q, labels %v)", got, tc.wantAdvanced, boardStatusOf(t, env, num), IssueLabels(t, env, num))
			}
			if tc.wantAdvanced {
				return
			}
			if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
				t.Errorf("a hold is not a pause, labels %v", IssueLabels(t, env, num))
			}
			seedBareReview(t, env, prNum, "pruefer-bot[bot]", "COMMENTED", c)
			if !leftReview(t, env, num, 40) {
				t.Fatalf("item did not advance once the gate cleared (status %q, labels %v)", boardStatusOf(t, env, num), IssueLabels(t, env, num))
			}
		})
	}
}
