package sim

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Sim twins for #2220 / ADR 2220: a stage entered with a pending comment runs
// the stage first; the comment pass follows, and the item never advances before
// it. The ordering is an assertion on an invocation log shared by the stage and
// comment scripts, because that is the only place "which skill ran first" is
// visible.

// stageFirstLog is the ordered record of invocations, with per-call probes.
type stageFirstLog struct {
	mu      sync.Mutex
	entries []string
	// atComment is the board Status and whether the pending comment carried a
	// rocket when the comment pass started; planCommentSeen is set when a Plan
	// stage comment existed at that moment.
	statusAtComment   []string
	rocketAtComment   []bool
	stageCommentSeen  []bool
	awaitingDoneAtCmt []bool
}

func (l *stageFirstLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, s)
}

func (l *stageFirstLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

// install registers recording scripts for stageName. stageOut is the stage
// script's output (it must end the stage); the comment script completes it.
func (l *stageFirstLog) install(env *Env, num int, stageName, stageOut string) {
	env.Claude.ForStage(stageName, func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		l.add("stage:" + stage.Name)
		out, completed, usage, err := simclaude.DefaultScript(ctx, stage, issue, newComments, resume, workDir, opts)
		if stageOut != "" {
			out = stageOut
		}
		return out, completed, usage, err
	})
	env.Claude.ForStageComments(stageName, func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		l.add("comment:" + stage.Name)
		item, _ := env.Sim.FetchProjectItem(env.Owner, env.Repo, num)
		rocket, stageComment := false, false
		for _, c := range item.Comments {
			if strings.HasPrefix(c.Body, "🏭 **Fabrik — stage: "+stage.Name+"**") {
				stageComment = true
			}
		}
		for _, c := range comments {
			if c.HasReaction("ROCKET") {
				rocket = true
			}
		}
		l.mu.Lock()
		l.statusAtComment = append(l.statusAtComment, item.Status)
		l.rocketAtComment = append(l.rocketAtComment, rocket)
		l.stageCommentSeen = append(l.stageCommentSeen, stageComment)
		l.awaitingDoneAtCmt = append(l.awaitingDoneAtCmt, hasLabel(item.Labels, "fabrik:awaiting-done"))
		l.mu.Unlock()
		return simclaude.DefaultCommentScript(ctx, stage, issue, comments, workDir, opts)
	})
}

func seedPendingComment(t *testing.T, env *Env, num int) {
	t.Helper()
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "maintainer", "also cover the offline case")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
}

func commentPassCount(env *Env, stage string) int { return env.Claude.CommentCallCount(stage) }

// TestStageFirst_SpecifyRunsBeforeCommentPass_Parked is SC-001 without an
// autonomy label: the stage runs, the item parks at complete with the comment
// still pending, and the next poll runs the one comment pass.
func TestStageFirst_SpecifyRunsBeforeCommentPass_Parked(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	log := &stageFirstLog{}
	num := FileIssue(t, env, "thin issue", "one line", "Specify")
	seedPendingComment(t, env, num)
	log.install(env, num, "Specify", "")

	AdvanceUntil(t, env, func(env *Env) bool { return commentPassCount(env, "Specify") >= 1 }, 40)
	RunPolls(t, env, 3) // nothing further may run

	if got, want := log.get(), []string{"stage:Specify", "comment:Specify"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("invocation order = %v, want %v", got, want)
	}
	if log.rocketAtComment[0] {
		t.Error("the pending comment was 🚀-marked by the stage run — it must reach the comment pass untouched")
	}
	if !hasLabel(IssueLabels(t, env, num), "stage:Specify:complete") {
		t.Error("stage:Specify:complete missing after the comment pass")
	}
	if got := projectItem(t, env, num).Status; got != "Specify" {
		t.Errorf("Status = %q, want Specify (no autonomy label — the item parks)", got)
	}
}

// TestStageFirst_SpecifyRunsBeforeCommentPass_Cruise is SC-001 with
// fabrik:cruise: feedbackGateBlocks must hold the advance after the stage run,
// so Research is entered only after the comment pass.
func TestStageFirst_SpecifyRunsBeforeCommentPass_Cruise(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	log := &stageFirstLog{}
	num := FileIssue(t, env, "thin issue", "one line", "Specify", "fabrik:cruise")
	seedPendingComment(t, env, num)
	log.install(env, num, "Specify", "")
	var researchStarted sync.Once
	env.Claude.ForStage("Research", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		researchStarted.Do(func() { log.add("stage:Research") })
		return simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
	})

	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Research") >= 1 }, 60)

	want := []string{"stage:Specify", "comment:Specify", "stage:Research"}
	if got := log.get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("invocation order = %v, want %v — the item advanced before the comment pass", got, want)
	}
	if log.statusAtComment[0] != "Specify" {
		t.Errorf("Status at the comment pass = %q, want Specify", log.statusAtComment[0])
	}
}

// TestStageFirst_NonVacuous restores today's routing: the comment pass runs
// first, the stage skill never runs, and the item advances on the comment
// skill's completion alone — the #2220 bug.
func TestStageFirst_NonVacuous(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	env.Engine.SetStageFirstRoutingDisabledForTest(true)
	log := &stageFirstLog{}
	num := FileIssue(t, env, "thin issue", "one line", "Specify", "fabrik:cruise")
	seedPendingComment(t, env, num)
	log.install(env, num, "Specify", "")

	AdvanceUntil(t, env, func(env *Env) bool { return projectItem(t, env, num).Status == "Research" }, 40)

	if got, want := log.get(), []string{"comment:Specify"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("with the seam on, invocations = %v, want %v", got, want)
	}
	if n := env.Claude.StageCallCount("Specify"); n != 0 {
		t.Errorf("Specify stage skill ran %d time(s) with the seam on, want 0", n)
	}
}

// TestStageFirst_PlanCommentPostedBeforeCommentPass is SC-002.
func TestStageFirst_PlanCommentPostedBeforeCommentPass(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	log := &stageFirstLog{}
	num := FileIssue(t, env, "plan me", "body", "Plan", "stage:Specify:complete", "stage:Research:complete")
	seedPendingComment(t, env, num)
	log.install(env, num, "Plan", "## Implementation Plan\n- [ ] Task 1\nFABRIK_STAGE_COMPLETE\n")

	AdvanceUntil(t, env, func(env *Env) bool { return commentPassCount(env, "Plan") >= 1 }, 40)

	if got, want := log.get(), []string{"stage:Plan", "comment:Plan"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("invocation order = %v, want %v", got, want)
	}
	if !log.stageCommentSeen[0] {
		t.Error("no 🏭 Plan stage comment existed when the comment pass started (SC-002)")
	}
}

// TestStageFirst_CommentAfterStageRanGoesToCommentPath is SC-003: the stage ran
// and parked; a comment posted afterwards is a plain comment pass, no re-run.
func TestStageFirst_CommentAfterStageRanGoesToCommentPath(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	log := &stageFirstLog{}
	num := FileIssue(t, env, "plain", "body", "Research", "stage:Specify:complete")
	log.install(env, num, "Research", "")

	WaitForIssueLabel(t, env, num, "stage:Research:complete", 40)
	RunPolls(t, env, 3)
	if got := log.get(); !reflect.DeepEqual(got, []string{"stage:Research"}) {
		t.Fatalf("before the comment, invocations = %v, want only the stage run", got)
	}

	env.Clock.Advance(1_000_000_000)
	seedPendingComment(t, env, num)
	AdvanceUntil(t, env, func(env *Env) bool { return commentPassCount(env, "Research") >= 1 }, 40)
	RunPolls(t, env, 3)

	if got, want := log.get(), []string{"stage:Research", "comment:Research"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("invocation order = %v, want %v — the stage must not re-run", got, want)
	}
}

// TestStageFirst_RestartBetweenStageAndCommentPass is SC-004: the "ran" decision
// is durable, so a restart before the comment pass does not run the stage again.
func TestStageFirst_RestartBetweenStageAndCommentPass(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	log := &stageFirstLog{}
	num := FileIssue(t, env, "thin issue", "one line", "Specify")
	seedPendingComment(t, env, num)
	log.install(env, num, "Specify", "")

	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 40)
	if n := commentPassCount(env, "Specify"); n != 0 {
		t.Fatalf("comment pass already ran (%d) before the restart point", n)
	}

	env = RestartEnv(t, env)
	AdvanceUntil(t, env, func(env *Env) bool { return commentPassCount(env, "Specify") >= 1 }, 40)
	RunPolls(t, env, 3)

	if got, want := log.get(), []string{"stage:Specify", "comment:Specify"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("invocation order across the restart = %v, want %v", got, want)
	}
}

// TestStageFirst_NoWorkNeededHeldByPendingComment covers FR-004's one ungated
// advance: FABRIK_NO_WORK_NEEDED with a pending comment must not reach Done
// before the comment pass.
func TestStageFirst_NoWorkNeededHeldByPendingComment(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	log := &stageFirstLog{}
	num := FileIssue(t, env, "moot", "body", "Research", "stage:Specify:complete")
	seedPendingComment(t, env, num)
	log.install(env, num, "Research", "nothing to do\nFABRIK_STAGE_COMPLETE\nFABRIK_NO_WORK_NEEDED\n")

	AdvanceUntil(t, env, func(env *Env) bool { return commentPassCount(env, "Research") >= 1 }, 40)

	if got, want := log.get(), []string{"stage:Research", "comment:Research"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("invocation order = %v, want %v", got, want)
	}
	if log.statusAtComment[0] == "Done" || log.awaitingDoneAtCmt[0] {
		t.Errorf("no-work decision ran ahead of the comment pass (status %q, awaiting-done %v)", log.statusAtComment[0], log.awaitingDoneAtCmt[0])
	}
}
