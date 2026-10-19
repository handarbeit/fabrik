package sim

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// Sim twins of #2394 (ADR 2394): a stage run that finished must be published, not run
// again, when its completion could not be written to GitHub.
//
// The seed is #2388: a Research stage ran about 150 times in 21 hours because its
// completion-label write kept failing, each repeat resuming a session that had grown to
// 13 MB. Here the one write that fails is the stage:Specify:complete label; reads and every
// other write work, so only the record can stand between the item and a second model call.

func stageOutcomeStages() []*stages.Stage {
	// No auto-advance: the item stays in Specify, which is where #2388's item stayed
	// because the advance's own writes failed too.
	noAdvance := false
	return []*stages.Stage{
		{Name: "Specify", Order: 1, AutoAdvance: &noAdvance},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
}

func stageOutcomeEnv(t *testing.T) *Env {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:    stageOutcomeStages(),
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			// Short enough that the in-memory attempt backoff has long expired when the
			// next poll runs: only the record can hold the stage back.
			cfg.RetryBackoff = time.Millisecond
			cfg.MaxRetries = 3
		},
	})
	EnableStageOutcomes(t, env)
	// The default script's output is only the completion marker, which posts no comment;
	// a real stage reports what it found, and that comment is part of what must be published.
	env.Claude.ForStage("Specify", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, c []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		_, done, usage, err := simclaude.DefaultScript(ctx, stage, issue, c, resume, workDir, opts)
		return "## Findings\n\nThe stage output.\n\nFABRIK_STAGE_COMPLETE\n", done, usage, err
	})
	return env
}

// failCompletionLabel makes every write of the Specify completion label fail with err.
func failCompletionLabel(env *Env, err error) {
	env.Sim.Faults().FailWhen("AddLabelToIssue", func(a simgh.Args) bool {
		return a.Label == "stage:Specify:complete"
	}, simgh.Always, err)
}

func stageComments(t *testing.T, env *Env, num int) int {
	t.Helper()
	comments, err := env.Sim.FetchIssueComments(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	n := 0
	for _, c := range comments {
		if strings.HasPrefix(c.Body, "🏭 **Fabrik — stage: Specify**") {
			n++
		}
	}
	return n
}

// runOutcomePolls runs n polls 30 s apart, tolerating poll errors.
func runOutcomePolls(t *testing.T, env *Env, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		pollTolerant(t, env, 30*time.Second)
	}
}

// R5 / FR-012 / SC-001, SC-002: a completed run whose completion-label write fails is
// published on a later poll with no second model call and no duplicate comment.
func TestStageOutcome_Sim_CompletionWriteFailsPublishedWithoutRerun(t *testing.T) {
	t.Parallel()
	env := stageOutcomeEnv(t)
	failCompletionLabel(env, simgh.ErrDeadlineExceeded())
	num := FileIssue(t, env, "completion write fails (#2394)", "body", "Specify")

	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 40)
	runOutcomePolls(t, env, 12)
	if got := env.Claude.StageCallCount("Specify"); got != 1 {
		t.Fatalf("the stage ran %d times while its completion write failed, want exactly 1\n\n%s", got, diagnostics(env))
	}
	if hasLabel(IssueLabels(t, env, num), "stage:Specify:complete") {
		t.Fatal("the completion label landed although every write of it fails — the scenario is vacuous")
	}

	env.Sim.Faults().ClearAll()
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 40)
	if got := env.Claude.StageCallCount("Specify"); got != 1 {
		t.Fatalf("the stage ran %d times in total, want exactly 1 (publication must not re-run it)", got)
	}
	if got := stageComments(t, env, num); got != 1 {
		t.Fatalf("%d stage comments on the issue, want exactly 1", got)
	}
}

// Neutralised, the same outage re-runs the stage (the #2388 loop), so the test above bites.
func TestStageOutcome_Sim_NeutralisedRerunsTheStage(t *testing.T) {
	t.Parallel()
	env := stageOutcomeEnv(t)
	env.Engine.SetStageOutcomeRecordDisabledForTest(true)
	failCompletionLabel(env, simgh.ErrDeadlineExceeded())
	FileIssue(t, env, "completion write fails, no record (#2394)", "body", "Specify")

	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 40)
	runOutcomePolls(t, env, 12)
	if got := env.Claude.StageCallCount("Specify"); got < 2 {
		t.Fatalf("with recording neutralised the stage ran %d time(s); want a re-run (>= 2) — the scenario no longer reproduces #2388\n\n%s", got, diagnostics(env))
	}
}

// FR-013 / SC-003: a restart between completion and publication still publishes the
// outcome, with no further model call.
func TestStageOutcome_Sim_RestartBeforePublicationStillPublishes(t *testing.T) {
	t.Parallel()
	env := stageOutcomeEnv(t)
	failCompletionLabel(env, simgh.ErrDeadlineExceeded())
	num := FileIssue(t, env, "restart before publication (#2394)", "body", "Specify")

	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 40)
	runOutcomePolls(t, env, 3)

	env = RestartEnv(t, env)
	env.Sim.Faults().ClearAll()
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 40)
	if got := env.Claude.StageCallCount("Specify"); got != 1 {
		t.Fatalf("the stage ran %d times across the restart, want exactly 1", got)
	}
	if got := stageComments(t, env, num); got != 1 {
		t.Fatalf("%d stage comments after the restart, want exactly 1", got)
	}
}

// A comment that failed to post is retried (and posted once) before the completion label is
// written: the label never lands ahead of the output it stands for.
func TestStageOutcome_Sim_FailedCommentIsPostedBeforeTheLabel(t *testing.T) {
	t.Parallel()
	env := stageOutcomeEnv(t)
	env.Sim.Faults().FailWhen("AddComment", func(a simgh.Args) bool { return true }, 4, simgh.ErrDeadlineExceeded())
	num := FileIssue(t, env, "comment write fails (#2394)", "body", "Specify")

	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 40)
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 60)
	if got := env.Claude.StageCallCount("Specify"); got != 1 {
		t.Fatalf("the stage ran %d times, want exactly 1", got)
	}
	if got := stageComments(t, env, num); got != 1 {
		t.Fatalf("%d stage comments, want exactly 1", got)
	}
}

// R4 / FR-008 / SC-005: a publication that keeps failing for a reason other than an outage
// escalates after MaxRetries — the item is paused and the model was handed it only once.
func TestStageOutcome_Sim_PersistentFailureEscalates(t *testing.T) {
	t.Parallel()
	env := stageOutcomeEnv(t)
	failCompletionLabel(env, errors.New("Resource not accessible by integration"))
	num := FileIssue(t, env, "persistent failure (#2394)", "body", "Specify")

	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 40)
	AdvanceUntil(t, env, func(env *Env) bool { return hasLabel(IssueLabels(t, env, num), "fabrik:paused") }, 30)
	if got := env.Claude.StageCallCount("Specify"); got != 1 {
		t.Fatalf("the stage ran %d times before escalating, want exactly 1", got)
	}
}

// R4 / FR-009: transport failures — an outage — never count toward the bound, however many
// polls they span.
func TestStageOutcome_Sim_OutageFailuresDoNotCount(t *testing.T) {
	t.Parallel()
	env := stageOutcomeEnv(t)
	failCompletionLabel(env, simgh.ErrDeadlineExceeded())
	num := FileIssue(t, env, "outage does not escalate (#2394)", "body", "Specify")

	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 40)
	runOutcomePolls(t, env, 20)
	if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
		t.Fatal("transport failures escalated the item to paused; an outage must not count against MaxRetries")
	}
	if got := env.Claude.StageCallCount("Specify"); got != 1 {
		t.Fatalf("the stage ran %d times, want exactly 1", got)
	}
}

// R3 / FR-007 / SC-004: a pause discards the record (logged), and normal dispatch resumes
// once the pause is lifted — the finished work is redone rather than publishing stale output.
func TestStageOutcome_Sim_PauseDiscardsTheRecord(t *testing.T) {
	t.Parallel()
	env := stageOutcomeEnv(t)
	failCompletionLabel(env, simgh.ErrDeadlineExceeded())
	num := FileIssue(t, env, "pause discards (#2394)", "body", "Specify")

	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 40)
	runOutcomePolls(t, env, 2)
	if env.Engine.StageOutcomeRecordsForTest() != 1 {
		t.Fatalf("setup: want exactly 1 live record, got %d", env.Engine.StageOutcomeRecordsForTest())
	}

	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, "fabrik:paused"); err != nil {
		t.Fatalf("pausing: %v", err)
	}
	runOutcomePolls(t, env, 2)
	if got := env.Engine.StageOutcomeRecordsForTest(); got != 0 {
		t.Fatalf("%d record(s) live after the item was paused, want 0", got)
	}

	env.Sim.Faults().ClearAll()
	if err := env.Sim.RemoveLabelFromIssue(env.Owner, env.Repo, num, "fabrik:paused"); err != nil {
		t.Fatalf("resuming: %v", err)
	}
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 2 }, 40)
}

// R3 / FR-006: a worktree head that moved since the run discards the record, and the stage
// is dispatched normally again.
func TestStageOutcome_Sim_MovedHeadDiscardsTheRecord(t *testing.T) {
	t.Parallel()
	env := stageOutcomeEnv(t)
	failCompletionLabel(env, simgh.ErrDeadlineExceeded())
	num := FileIssue(t, env, "head moved discards (#2394)", "body", "Specify")

	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 40)
	runOutcomePolls(t, env, 2)
	if env.Engine.StageOutcomeRecordsForTest() != 1 {
		t.Fatalf("setup: want exactly 1 live record, got %d", env.Engine.StageOutcomeRecordsForTest())
	}

	dir := env.WM.WorktreeDir(num)
	if out, err := exec.Command("git", "-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-m", "moves the head").CombinedOutput(); err != nil {
		t.Fatalf("committing in the worktree: %v: %s", err, out)
	}
	// The record is discarded (logged) and the stage dispatched normally again; with the
	// completion write still failing, that second run records afresh.
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 2 }, 40)
}

// seedCommentPassCompletion files an item whose Specify stage already ran (its stage comment
// is there, the completion label is not) with a human comment pending: the comment pass
// completes the stage, the audit case of ADR 2394.
func seedCommentPassCompletion(t *testing.T, env *Env) int {
	t.Helper()
	num := FileIssue(t, env, "comment pass completes (#2394)", "body", "Specify")
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "fabrik-bot", "🏭 **Fabrik — stage: Specify**\n\nEarlier output that asked a question.")
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "maintainer", "here is the answer")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	return num
}

// Audit finding (a), FR-011: a comment pass that completes the stage but cannot write the
// completion label must not make the whole stage run again.
func TestStageOutcome_Sim_CommentPassCompletionIsRecordedNotRerun(t *testing.T) {
	t.Parallel()
	env := stageOutcomeEnv(t)
	failCompletionLabel(env, simgh.ErrDeadlineExceeded())
	num := seedCommentPassCompletion(t, env)

	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.CommentCallCount("Specify") >= 1 }, 40)
	runOutcomePolls(t, env, 10)
	if got := env.Claude.StageCallCount("Specify"); got != 0 {
		t.Fatalf("the stage was dispatched %d time(s) after its comment pass completed it, want 0\n\n%s", got, diagnostics(env))
	}
	if got := env.Claude.CommentCallCount("Specify"); got != 1 {
		t.Fatalf("%d comment pass(es), want exactly 1", got)
	}

	env.Sim.Faults().ClearAll()
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 40)
	if got := env.Claude.StageCallCount("Specify"); got != 0 {
		t.Fatalf("the stage was dispatched %d time(s), want 0", got)
	}
}

// Neutralised, the same comment-pass completion loss re-dispatches the stage.
func TestStageOutcome_Sim_CommentPassNeutralisedRedispatches(t *testing.T) {
	t.Parallel()
	env := stageOutcomeEnv(t)
	env.Engine.SetStageOutcomeRecordDisabledForTest(true)
	failCompletionLabel(env, simgh.ErrDeadlineExceeded())
	seedCommentPassCompletion(t, env)

	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.CommentCallCount("Specify") >= 1 }, 40)
	runOutcomePolls(t, env, 10)
	if got := env.Claude.StageCallCount("Specify"); got < 1 {
		t.Fatalf("with recording neutralised the stage was never re-dispatched — the audit scenario does not reproduce the loss\n\n%s", diagnostics(env))
	}
}

// A failed issue-body write on the first attempt keeps the record: the comment, the push and
// the label all land, but the body update is still retried from the record rather than lost
// (review finding on #2394). Without the record the update is dropped for good.
func bodyUpdateEnv(t *testing.T, disabled bool) (*Env, int) {
	t.Helper()
	env := stageOutcomeEnv(t)
	env.Engine.SetStageOutcomeRecordDisabledForTest(disabled)
	env.Claude.ForStage("Specify", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, c []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		_, done, usage, err := simclaude.DefaultScript(ctx, stage, issue, c, resume, workDir, opts)
		return "## Findings\n\nThe stage output.\n\nFABRIK_ISSUE_UPDATE_BEGIN\nREWRITTEN SPEC BODY\nFABRIK_ISSUE_UPDATE_END\n\nFABRIK_STAGE_COMPLETE\n", done, usage, err
	})
	env.Sim.Faults().FailNTimes("UpdateIssueBody", 1, simgh.ErrDeadlineExceeded())
	num := FileIssue(t, env, "body write fails (#2394)", "original body", "Specify")
	return env, num
}

func TestStageOutcome_Sim_FailedBodyUpdateIsRetriedFromTheRecord(t *testing.T) {
	t.Parallel()
	env, num := bodyUpdateEnv(t, false)

	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 40)
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 60)
	runOutcomePolls(t, env, 6)
	body, err := env.Sim.GetIssueBody(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("GetIssueBody: %v", err)
	}
	if !strings.Contains(body, "REWRITTEN SPEC BODY") {
		t.Fatalf("the issue body was never updated from the recorded outcome:\n%s\n\n%s", body, diagnostics(env))
	}
	if got := env.Claude.StageCallCount("Specify"); got != 1 {
		t.Fatalf("the stage ran %d times, want exactly 1", got)
	}
}

// Neutralised, the failed body write is lost although the stage completed.
func TestStageOutcome_Sim_FailedBodyUpdateNeutralisedIsLost(t *testing.T) {
	t.Parallel()
	env, num := bodyUpdateEnv(t, true)

	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 40)
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 60)
	runOutcomePolls(t, env, 6)
	body, err := env.Sim.GetIssueBody(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("GetIssueBody: %v", err)
	}
	if strings.Contains(body, "REWRITTEN SPEC BODY") {
		t.Fatal("the body was updated without the record — the scenario no longer shows the loss the record prevents")
	}
}
