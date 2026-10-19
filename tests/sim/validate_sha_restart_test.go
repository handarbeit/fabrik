package sim

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// This file is the sim twin of #2343 (ADR-2343): the Validate completion SHA is engine
// memory, so a restart used to wipe it and a later push to the PR no longer invalidated
// stage:Validate:complete. The engine now rebuilds it from the Validate stage comment's
// commit: header before the landing decision.
//
// The Validate completion is driven through the real addCompleteLabelAndRemoveCI path
// (not seeded), so the stage comment's commit: header is genuine; RestartEnv then gives
// the engine an empty Store over unchanged GitHub state.
//
// Neutralisation: Engine.SetValidateSHARebuildDisabledForTest restores the old FR-5
// "empty SHA means do nothing" behaviour — PushAfterRestartInvalidatesValidate fails
// with it (TestValidateSHARestart_NonVacuity asserts exactly that).

func validateSHARestartEnv(t *testing.T) (env *Env, num int, firstHead string) {
	t.Helper()
	env = NewEnv(t, EnvOptions{
		Stages:             ciFixStages(),
		StartTime:          time.Now(),
		ValidateSHARebuild: true,
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxCiFixCycles = 5
			// simgh posts as simgh-bot; the rebuild trusts only the engine's own comments.
			cfg.User = "simgh-bot"
		},
	})
	// A real Validate run always reports; the default script emits only the marker, which
	// the engine would not post as a stage comment.
	env.Claude.ForStage("Validate", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		out, ok, usage, err := simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
		return "Validation report: all requirements met.\n\n" + out, ok, usage, err
	})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	num = FileIssue(t, env, "validate sha restart", "Prove a push after a restart still invalidates Validate.", "Implement", "fabrik:cruise")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	firstHead = pr.HeadSHA
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, firstHead, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: "success"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}
	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 80)
	if got := env.Claude.StageCallCount("Validate"); got != 1 {
		t.Fatalf("StageCallCount(Validate) = %d before restart, want 1", got)
	}
	return env, num, firstHead
}

// pushToPRBranch moves the PR branch head, as an external push would.
func pushToPRBranch(t *testing.T, env *Env, num int) string {
	t.Helper()
	branch := memberBranch(num)
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, branch, map[string]string{"external.txt": "pushed after restart\n"}, "external push after restart")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("pushing to %s: %v", branch, err)
	}
	head, err := env.Sim.Sim().HeadSHA(env.OwnerRepo, branch)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	return head
}

func TestValidateSHARestart_PushAfterRestartInvalidatesValidate(t *testing.T) {
	t.Parallel()
	env, num, firstHead := validateSHARestartEnv(t)

	restarted := RestartEnv(t, env)
	newHead := pushToPRBranch(t, restarted, num)
	if newHead == firstHead {
		t.Fatal("the push did not move the head")
	}

	for i := 0; i < 3; i++ {
		RunPoll(t, restarted)
	}
	if hasLabel(IssueLabels(t, restarted, num), validateCompleteLabel) {
		t.Fatalf("%s survived a post-restart push to the PR branch\n\n%s", validateCompleteLabel, diagnostics(restarted))
	}
	// ... and Validate runs again on the new head.
	for i := 0; i < 40 && restarted.Claude.StageCallCount("Validate") < 2; i++ {
		RunPoll(t, restarted)
	}
	if got := restarted.Claude.StageCallCount("Validate"); got < 2 {
		t.Errorf("StageCallCount(Validate) = %d after a post-restart push, want Validate re-dispatched (>=2)\n\n%s", got, diagnostics(restarted))
	}
}

// With the rebuild disabled the same scenario must not invalidate: this is what makes
// PushAfterRestartInvalidatesValidate non-vacuous.
func TestValidateSHARestart_NonVacuity(t *testing.T) {
	t.Parallel()
	env, num, _ := validateSHARestartEnv(t)

	restarted := RestartEnv(t, env)
	defer restarted.Engine.SetValidateSHARebuildDisabledForTest()()
	pushToPRBranch(t, restarted, num)

	for i := 0; i < 3; i++ {
		RunPoll(t, restarted)
	}
	if got := restarted.Claude.StageCallCount("Validate"); got != 1 {
		t.Errorf("StageCallCount(Validate) = %d with the rebuild disabled, want 1 (old behaviour: push goes unnoticed)", got)
	}
	if !hasLabel(IssueLabels(t, restarted, num), "stage:Validate:complete") {
		t.Error("stage:Validate:complete was removed with the rebuild disabled — the twin is vacuous")
	}
}

// An unchanged head after a restart must keep Validate complete and not re-run it.
func TestValidateSHARestart_UnchangedHeadKeepsValidate(t *testing.T) {
	t.Parallel()
	env, num, _ := validateSHARestartEnv(t)

	restarted := RestartEnv(t, env)
	for i := 0; i < 3; i++ {
		RunPoll(t, restarted)
	}
	if got := restarted.Claude.StageCallCount("Validate"); got != 1 {
		t.Errorf("StageCallCount(Validate) = %d after a restart with no push, want 1", got)
	}
	if !hasLabel(IssueLabels(t, restarted, num), "stage:Validate:complete") {
		t.Errorf("stage:Validate:complete lost on an unchanged head\n\n%s", diagnostics(restarted))
	}
}

// ---- #2378 / ADR 2378: a comment pass must not satisfy the forced re-run --------------

// forcedRerunLog records the invocation order of the Validate stage and its comment pass.
type forcedRerunLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *forcedRerunLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, s)
}

func (l *forcedRerunLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

// makeValidateCommentUnusable rewrites the commit: header of the Validate stage comment to
// "unknown", so the #2343 rebuild finds no SHA (R3 fires) while the comment itself still
// exists — the state in which stageRanOnEntry used to send a pending comment to the
// comment path (the v0.0.88 gate incident).
func makeValidateCommentUnusable(t *testing.T, env *Env, num int) {
	t.Helper()
	item := projectItem(t, env, num)
	n := 0
	for _, c := range item.Comments {
		if !strings.HasPrefix(c.Body, "🏭 **Fabrik — stage: Validate**") {
			continue
		}
		body := regexp.MustCompile(`commit: [0-9a-f]+`).ReplaceAllString(c.Body, "commit: unknown")
		if body == c.Body {
			t.Fatalf("no commit: header to rewrite in %q", c.Body)
		}
		if err := env.Sim.Sim().UpdateComment(env.Owner, env.Repo, c.DatabaseID, body); err != nil {
			t.Fatalf("UpdateComment: %v", err)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no Validate stage comment found")
	}
}

// installForcedRerunScripts makes the Validate comment pass report completion, as the
// incident's "acknowledge only" pass did, and logs the order of both invocations.
func installForcedRerunScripts(env *Env, log *forcedRerunLog) {
	installForcedRerunScriptsMode(env, log, true)
}

// installForcedRerunScriptsMode is installForcedRerunScripts with the comment pass's outcome
// chosen: completes=false ends the pass without FABRIK_STAGE_COMPLETE (the turn cap, an
// error, ...), the non-completing exit of the rework bracket.
func installForcedRerunScriptsMode(env *Env, log *forcedRerunLog, completes bool) {
	env.Claude.ForStage("Validate", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		log.add("stage:Validate")
		out, ok, usage, err := simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
		return "Validation report: all requirements met.\n\n" + out, ok, usage, err
	})
	env.Claude.ForStageComments("Validate", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		log.add("comment:Validate")
		if !completes {
			return "Looking into it.\n", false, engine.TokenUsage{TurnsUsed: 15}, nil
		}
		return "Acknowledged.\n\nFABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{TurnsUsed: 4}, nil
	})
}

// runForcedRerunScenario runs one quiet poll after the restart (so the PR head is known),
// then lands a pending human comment. The comment admits the item, so in that same poll
// #2343's R3 finds no recoverable SHA and strips the completion label, and the comment pass
// is dispatched from the poll-start snapshot — the v0.0.88 incident's order. It then polls
// to quiescence, answering CI on every new head, and reports whether the completion label
// was ever present while no Validate stage run had happened since the restart.
func runForcedRerunScenario(t *testing.T, restarted *Env, num int, log *forcedRerunLog) (completeBeforeStageRun bool) {
	t.Helper()
	RunPoll(t, restarted)
	restarted.Sim.Sim().SeedComment(restarted.OwnerRepo, num, "maintainer", "please acknowledge this")
	if err := restarted.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	green := map[string]bool{}
	disturbed := false
	for i := 0; i < 40; i++ {
		RunPoll(t, restarted)
		if pr, err := restarted.Sim.FetchLinkedPR(restarted.Owner, restarted.Repo, num); err == nil && pr != nil && !green[pr.HeadSHA] {
			green[pr.HeadSHA] = true
			restarted.Sim.Sim().SeedCheckRun(restarted.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: "success"})
		}
		stageRan := false
		for _, e := range log.get() {
			if e == "stage:Validate" {
				stageRan = true
			}
			if e == "comment:Validate" {
				disturbed = true
			}
		}
		present := hasLabel(IssueLabels(t, restarted, num), validateCompleteLabel)
		if !present {
			disturbed = true // R3 stripped it
		}
		// Idle polls before the comment admits the item legitimately show the original
		// label; only what follows the strip or the comment pass counts.
		if disturbed && present && !stageRan {
			completeBeforeStageRun = true
		}
	}
	return completeBeforeStageRun
}

func forcedRerunRestart(t *testing.T) (*Env, int, *forcedRerunLog) {
	t.Helper()
	return forcedRerunRestartMode(t, true)
}

func forcedRerunRestartMode(t *testing.T, commentCompletes bool) (*Env, int, *forcedRerunLog) {
	t.Helper()
	env, num, _ := validateSHARestartEnv(t)
	restarted := RestartEnv(t, env)
	log := &forcedRerunLog{}
	installForcedRerunScriptsMode(restarted, log, commentCompletes)
	makeValidateCommentUnusable(t, restarted, num)
	return restarted, num, log
}

// With a pending comment and no recoverable SHA, a Validate stage run executes before the
// completion label is restored; the comment is still processed (once), after it.
func TestValidateSHARestart_PendingCommentDoesNotSatisfyForcedRerun(t *testing.T) {
	t.Parallel()
	restarted, num, log := forcedRerunRestart(t)

	if early := runForcedRerunScenario(t, restarted, num, log); early {
		t.Errorf("%s was present before any Validate stage run — a comment pass satisfied the forced re-run\n\n%s", validateCompleteLabel, diagnostics(restarted))
	}
	entries := log.get()
	// Stage first or comment first are both fine (#2220); what matters is that the stage
	// ran at all, and that the early-label check above held across the comment pass.
	hasStage := false
	for _, e := range entries {
		if e == "stage:Validate" {
			hasStage = true
		}
	}
	if !hasStage {
		t.Fatalf("no Validate stage run happened; invocations = %v\n\n%s", entries, diagnostics(restarted))
	}
	if got := restarted.Claude.CommentCallCount("Validate"); got != 1 {
		t.Errorf("CommentCallCount(Validate) = %d, want exactly 1 (comment processed once)", got)
	}
	labels := IssueLabels(t, restarted, num)
	if !hasLabel(labels, validateCompleteLabel) {
		t.Errorf("%s missing after the Validate stage run and the comment pass: %v\n\n%s", validateCompleteLabel, labels, diagnostics(restarted))
	}
	if hasLabel(labels, "fabrik:validate-rerun") {
		t.Errorf("fabrik:validate-rerun left behind after the Validate stage run: %v", labels)
	}
}

// Neutralising the fix makes the scenario fail: this is what makes it non-vacuous.
func TestValidateSHARestart_PendingCommentForcedRerun_NonVacuity(t *testing.T) {
	t.Parallel()
	restarted, num, log := forcedRerunRestart(t)
	restarted.Engine.SetValidateRerunGuardDisabledForTest(true)

	if early := runForcedRerunScenario(t, restarted, num, log); !early {
		t.Errorf("with the guard disabled the completion label never preceded the Validate stage run — the twin is vacuous; invocations = %v\n\n%s", log.get(), diagnostics(restarted))
	}
}

// The same restart, but the Validate comment pass ends WITHOUT the completion marker (the
// turn cap here). It was dispatched from the poll-start snapshot that still carried
// stage:Validate:complete, so its rework bracket's non-completing exit used to restore the
// label R3 had just withdrawn. The label must stay absent until a Validate stage run.
func TestValidateSHARestart_NonCompletingCommentPassDoesNotRestoreForcedRerun(t *testing.T) {
	t.Parallel()
	restarted, num, log := forcedRerunRestartMode(t, false)

	if early := runForcedRerunScenario(t, restarted, num, log); early {
		t.Errorf("%s was present before any Validate stage run — a non-completing comment pass restored it\n\n%s", validateCompleteLabel, diagnostics(restarted))
	}
	hasStage := false
	for _, e := range log.get() {
		if e == "stage:Validate" {
			hasStage = true
		}
	}
	if !hasStage {
		t.Fatalf("no Validate stage run happened; invocations = %v\n\n%s", log.get(), diagnostics(restarted))
	}
	labels := IssueLabels(t, restarted, num)
	if !hasLabel(labels, validateCompleteLabel) {
		t.Errorf("%s missing after the Validate stage run: %v\n\n%s", validateCompleteLabel, labels, diagnostics(restarted))
	}
	if hasLabel(labels, "fabrik:validate-rerun") {
		t.Errorf("fabrik:validate-rerun left behind after the Validate stage run: %v", labels)
	}
}

// Disabling the rework-bracket guard alone makes the scenario above fail.
func TestValidateSHARestart_NonCompletingCommentPass_NonVacuity(t *testing.T) {
	t.Parallel()
	restarted, num, log := forcedRerunRestartMode(t, false)
	restarted.Engine.SetValidateReworkRestoreGuardDisabledForTest(true)
	// The scenario's window needs the pre-#2350 timing: with the strip mirrored into
	// the Store of this cacheless engine, the dispatch re-read sees it and runs the
	// Validate stage in the very poll that stripped the label, before the comment
	// exists, so the rework bracket this twin targets is never reached.
	restarted.Engine.SetCachelessWriteThroughDisabledForTest(true)

	if early := runForcedRerunScenario(t, restarted, num, log); !early {
		t.Errorf("with the rework guard disabled the completion label never preceded the Validate stage run — the twin is vacuous; invocations = %v\n\n%s", log.get(), diagnostics(restarted))
	}
}
