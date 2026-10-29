package sim

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Sim twin of the declined at-head finding decision pause (#2466, ADR 2466).
//
// The reported sequence: a declared bot requests changes on the PR head, Review
// answers with a deliberate no-change (no commit), and the bot — which reviews
// only on push — never reviews again. Before the fix the gate waited out
// review-wait timeouts; now the item is paused with a decision comment on the
// first poll after the no-op reinvoke. Every scenario runs as shipped and, as its
// non-vacuity control, with SetDeclinedReviewPauseDisabledForTest(true).

const (
	declinedDecisionPrefix = "🏭 **Fabrik — review decision needed**"
	declinedBot            = "pruefer-bot"
)

func newDeclinedEnv(t *testing.T, off bool, declared ...string) *Env {
	t.Helper()
	env := newHeadAwareEnv(t, declared...)
	t.Cleanup(env.Engine.RegisterObservers())
	env.Engine.SetDeclinedReviewPauseDisabledForTest(off)
	return env
}

// seedDeclinedCR seeds the actionable at-head change request and returns the
// issue, PR and the review's database ID.
func seedDeclinedCR(t *testing.T, env *Env, marker, author, body string, extraLabels ...string) (num, prNum int) {
	t.Helper()
	num, prNum = seedReviewGateItem(t, env, "Review", marker, append([]string{"review-authority:authoritative"}, extraLabels...)...)
	preCreateWorktree(t, env, num)
	head := headOf(t, env, prNum)
	id := int(reviewDatabaseIDSeq.Add(1))
	env.Sim.Sim().SeedReview(env.OwnerRepo, prNum, gh.PRReview{
		Author: author, State: "CHANGES_REQUESTED", Body: body, DatabaseID: id, CommitID: head,
	})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seed CR: %v", err)
	}
	return num, prNum
}

// runUntilReinvoked drives polls until the no-op Review comment pass has run.
func runUntilReinvoked(t *testing.T, env *Env) {
	t.Helper()
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.CommentCallCount("Review") >= 1 }, 20)
}

func TestDeclinedFinding_DeclaredBotAtHeadBecomesADecision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		off  bool
		want bool
	}{{"as shipped", false, true}, {"neutralised", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newDeclinedEnv(t, tc.off, declinedBot)
			env.Claude.ForStageComments("Review", simclaude.NoOpCommentReview())
			num, _ := seedDeclinedCR(t, env, "decl-main", declinedBot+"[bot]", "Possible nil dereference in `foo()` — @maintainer could not confirm.")
			runUntilReinvoked(t, env)
			RunPolls(t, env, 6) // far less than ReviewWaitTimeout (15m of fake time)

			labels := IssueLabels(t, env, num)
			item := projectItem(t, env, num)
			paused := hasLabel(labels, "fabrik:paused")
			if paused != tc.want {
				t.Fatalf("paused = %v, want %v (labels %v)", paused, tc.want, labels)
			}
			if got := countCommentsWithPrefix(item.Comments, declinedDecisionPrefix); (got == 1) != tc.want || got > 1 {
				t.Fatalf("decision comments = %d, want %v", got, tc.want)
			}
			if hasCommentWithPrefix(item.Comments, "🏭 **Fabrik — review wait timeout**") {
				t.Fatal("a review wait timeout comment was posted; the decision must not read as a timeout")
			}
			if !tc.want {
				return
			}
			if !hasLabel(labels, "fabrik:pause:question") {
				t.Errorf("missing fabrik:pause:question in %v", labels)
			}
			// R2: names the reviewer and the declined finding, mention neutralised.
			var body string
			for _, c := range item.Comments {
				if strings.HasPrefix(c.Body, declinedDecisionPrefix) {
					body = c.Body
				}
			}
			for _, want := range []string{declinedBot, "Possible nil dereference", "dismiss", "review-authority:advisory"} {
				if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
					t.Errorf("decision comment lacks %q:\n%s", want, body)
				}
			}
			if strings.Contains(body, "@maintainer") || strings.Contains(body, "`foo()`") {
				t.Errorf("reviewer text was not neutralised:\n%s", body)
			}
			// R4: the worker's reply did not clear the gate.
			if !hasLabel(labels, "fabrik:awaiting-review") {
				t.Errorf("fabrik:awaiting-review was removed: the gate was cleared on the worker's say-so (%v)", labels)
			}
		})
	}
}

func TestDeclinedFinding_ResumeWithoutChangeResurfacesOnce(t *testing.T) {
	t.Parallel()
	env := newDeclinedEnv(t, false, declinedBot)
	env.Claude.ForStageComments("Review", simclaude.NoOpCommentReview())
	num, _ := seedDeclinedCR(t, env, "decl-resume", declinedBot+"[bot]", "Handle the empty slice case.")
	runUntilReinvoked(t, env)
	RunPolls(t, env, 4)
	if !hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
		t.Fatal("not paused after the declined reinvoke")
	}
	if err := env.Sim.RemoveLabelFromIssue(env.Owner, env.Repo, num, "fabrik:paused"); err != nil {
		t.Fatal(err)
	}
	RunPolls(t, env, 6)
	labels := IssueLabels(t, env, num)
	item := projectItem(t, env, num)
	if !hasLabel(labels, "fabrik:paused") {
		t.Fatalf("a resume that changed nothing must re-surface the decision (labels %v)", labels)
	}
	if got := countCommentsWithPrefix(item.Comments, declinedDecisionPrefix); got != 1 {
		t.Fatalf("decision comments = %d, want exactly 1 (no duplicate for the same head and review)", got)
	}
	if hasCommentWithPrefix(item.Comments, "🏭 **Fabrik — review wait timeout**") {
		t.Fatal("review wait timeout posted on resume")
	}
}

func TestDeclinedFinding_ReviewerOrHumanAction_ClearsTheGateNormally(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"dismissal", "fresh review on a new head"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			env := newDeclinedEnv(t, false, declinedBot)
			env.Claude.ForStageComments("Review", simclaude.NoOpCommentReview())
			num, prNum := seedDeclinedCR(t, env, "decl-clear-"+strings.ReplaceAll(mode, " ", "-"), declinedBot+"[bot]", "Add a test for the boundary.")
			runUntilReinvoked(t, env)
			RunPolls(t, env, 4)
			if !hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
				t.Fatal("not paused")
			}
			switch mode {
			case "dismissal":
				head := headOf(t, env, prNum)
				env.Sim.Sim().SeedReview(env.OwnerRepo, prNum, gh.PRReview{
					Author: declinedBot + "[bot]", State: "DISMISSED", CommitID: head,
					DatabaseID: int(reviewDatabaseIDSeq.Add(1)), SubmittedAt: time.Now().Add(time.Minute),
				})
			default:
				b := pushFix(t, env, num, prNum)
				seedBareReview(t, env, prNum, declinedBot+"[bot]", "COMMENTED", b)
			}
			if err := env.Sim.Sim().Err(); err != nil {
				t.Fatal(err)
			}
			if err := env.Sim.RemoveLabelFromIssue(env.Owner, env.Repo, num, "fabrik:paused"); err != nil {
				t.Fatal(err)
			}
			RunPolls(t, env, 8)
			labels := IssueLabels(t, env, num)
			if hasLabel(labels, "fabrik:paused") {
				t.Fatalf("still paused after %s (labels %v)", mode, labels)
			}
		})
	}
}

func TestDeclinedFinding_OutOfScopeShapesKeepTheOldBehaviour(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		declared []string
		author   string
		labels   []string
	}{
		{"undeclared reviewer", []string{declinedBot}, "carol", nil},
		{"stage declares nobody", nil, declinedBot + "[bot]", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newDeclinedEnv(t, false, tc.declared...)
			env.Claude.ForStageComments("Review", simclaude.NoOpCommentReview())
			num, _ := seedDeclinedCR(t, env, "decl-oos-"+strings.ReplaceAll(tc.name, " ", "-"), tc.author, "Please reconsider.", tc.labels...)
			runUntilReinvoked(t, env)
			RunPolls(t, env, 6)
			item := projectItem(t, env, num)
			if hasCommentWithPrefix(item.Comments, declinedDecisionPrefix) || hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
				t.Fatalf("decision pause raised for %s", tc.name)
			}
		})
	}
}

func TestDeclinedFinding_ReinvokeThatCommitsRaisesNoDecision(t *testing.T) {
	t.Parallel()
	env := newDeclinedEnv(t, false, declinedBot)
	// The scripted pass commits in the worktree; the sim's PR head lives on the
	// simgh model (the engine's push goes to the local origin clone), so the
	// script also advances the PR branch there — what a real push does.
	var issueNum int
	commits := simclaude.CommentReviewCompleted()
	env.Claude.ForStageComments("Review", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		out, done, usage, err := commits(ctx, stage, issue, comments, workDir, opts)
		env.Sim.Sim().SeedCommit(env.OwnerRepo, fmt.Sprintf("fabrik/issue-%d", issueNum),
			map[string]string{"sim/pushed-fix.md": "fix\n"}, "sim: address review")
		return out, done, usage, err
	})
	num, _ := seedDeclinedCR(t, env, "decl-commits", declinedBot+"[bot]", "Fix the off-by-one.")
	issueNum = num
	runUntilReinvoked(t, env)
	RunPolls(t, env, 6)
	if hasCommentWithPrefix(projectItem(t, env, num).Comments, declinedDecisionPrefix) {
		t.Fatal("decision pause raised although the reinvoke landed a commit (the verdict is no longer at head)")
	}
}
