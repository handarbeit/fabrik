package simclaude

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/claudeerr"
	"github.com/handarbeit/fabrik/stages"
)

// skipIfNoGit follows the repo-wide convention (engine/worktree_test.go,
// tests/sim/simgh/helpers_test.go) of exercising the real git binary rather
// than reimplementing git semantics.
func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
}

// newGitWorkDir creates a fresh, real git repository at t.TempDir() — a
// stand-in for the real worktree the engine hands a ClaudeInvoker.
func newGitWorkDir(t *testing.T) string {
	t.Helper()
	skipIfNoGit(t)
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

func gitLog(t *testing.T, workDir string) string {
	t.Helper()
	cmd := exec.Command("git", "log", "--oneline")
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v: %s", err, out)
	}
	return string(out)
}

func gitShowFiles(t *testing.T, workDir, rev string) string {
	t.Helper()
	cmd := exec.Command("git", "show", "--stat", "--name-only", rev)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git show: %v: %s", err, out)
	}
	return string(out)
}

var testStage = &stages.Stage{Name: "Implement", Order: 4}

var testIssue = gh.ProjectItem{Number: 42, Title: "test issue", Repo: "acme/widgets"}

// TestDefaultScript_CommitIsReal_NotJustMarkerText is the seed for AC10's
// non-vacuity requirement: it proves DefaultScript's commit is a real git
// commit inside workDir, not merely marker text in the returned output. A
// regression that removed the commit (kept only the FABRIK_STAGE_COMPLETE
// return) would make this fail while still "passing" any test that only
// checks the marker string — which is exactly the false-pass this layer
// exists to prevent (see issue #1449 R1/AC10).
func TestDefaultScript_CommitIsReal_NotJustMarkerText(t *testing.T) {
	workDir := newGitWorkDir(t)

	output, completed, usage, err := DefaultScript(context.Background(), testStage, testIssue, nil, false, workDir, engine.InvokeOptions{})
	if err != nil {
		t.Fatalf("DefaultScript: %v", err)
	}
	if !completed {
		t.Error("DefaultScript should report completed=true")
	}
	if !strings.Contains(output, "FABRIK_STAGE_COMPLETE") {
		t.Errorf("output = %q, want it to contain FABRIK_STAGE_COMPLETE", output)
	}
	if usage.TurnsUsed == 0 {
		t.Error("DefaultScript should report non-zero usage (it 'ran')")
	}

	log := gitLog(t, workDir)
	if strings.TrimSpace(log) == "" {
		t.Fatal("DefaultScript did not create any git commit — this is the exact false-pass AC10 guards against: a scripted invoker returning markers without touching the worktree")
	}
	if !strings.Contains(log, "Implement") {
		t.Errorf("git log = %q, want a commit message mentioning the stage name", log)
	}

	// Confirm the commit actually contains a file (not an empty/allow-empty commit).
	files := gitShowFiles(t, workDir, "HEAD")
	if !strings.Contains(files, ".simclaude/Implement.md") {
		t.Errorf("HEAD commit's files = %q, want it to include .simclaude/Implement.md", files)
	}
}

// TestDefaultScript_RepeatedCalls_EachProducesANewCommit confirms repeated
// invocations (retries) don't collide into a single empty-diff commit —
// each call must vary its content so `git commit` never has "nothing to
// commit."
func TestDefaultScript_RepeatedCalls_EachProducesANewCommit(t *testing.T) {
	workDir := newGitWorkDir(t)

	for i := 0; i < 3; i++ {
		if _, _, _, err := DefaultScript(context.Background(), testStage, testIssue, nil, false, workDir, engine.InvokeOptions{}); err != nil {
			t.Fatalf("DefaultScript call %d: %v", i, err)
		}
	}

	log := gitLog(t, workDir)
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) != 3 {
		t.Errorf("git log has %d commits after 3 calls, want 3:\n%s", len(lines), log)
	}
}

// TestDefaultCommentScript_CommitIsReal mirrors the Invoke-side non-vacuity
// guard for InvokeForComments.
func TestDefaultCommentScript_CommitIsReal(t *testing.T) {
	workDir := newGitWorkDir(t)

	_, completed, _, err := DefaultCommentScript(context.Background(), testStage, testIssue, nil, workDir, engine.InvokeOptions{})
	if err != nil {
		t.Fatalf("DefaultCommentScript: %v", err)
	}
	if !completed {
		t.Error("DefaultCommentScript should report completed=true")
	}
	log := gitLog(t, workDir)
	if strings.TrimSpace(log) == "" {
		t.Fatal("DefaultCommentScript did not create any git commit")
	}
}

// TestInvoker_Invoke_UsesDefaultScriptWhenUnregistered confirms an Invoker
// with no ForStage registration falls through to DefaultScript rather than
// erroring or returning a zero value.
func TestInvoker_Invoke_UsesDefaultScriptWhenUnregistered(t *testing.T) {
	workDir := newGitWorkDir(t)
	inv := New()

	output, completed, _, err := inv.Invoke(context.Background(), testStage, testIssue, nil, false, workDir, engine.InvokeOptions{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !completed || !strings.Contains(output, "FABRIK_STAGE_COMPLETE") {
		t.Errorf("Invoke() with no registered script should behave like DefaultScript, got completed=%v output=%q", completed, output)
	}
	if strings.TrimSpace(gitLog(t, workDir)) == "" {
		t.Fatal("Invoke() with no registered script did not commit anything")
	}
}

// TestInvoker_ForStage_SelectsByInvocationCountAndRepeatsLast confirms R1's
// selection rule: script[0] on the 1st call, script[1] on the 2nd, and the
// last script repeats for every call beyond the registered list.
func TestInvoker_ForStage_SelectsByInvocationCountAndRepeatsLast(t *testing.T) {
	workDir := newGitWorkDir(t)
	inv := New()

	var calls []string
	scriptNamed := func(name string) Script {
		return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
			calls = append(calls, name)
			return "FABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{}, nil
		}
	}
	inv.ForStage("Review", scriptNamed("first"), scriptNamed("second"))

	for i := 0; i < 4; i++ {
		if _, _, _, err := inv.Invoke(context.Background(), &stages.Stage{Name: "Review"}, testIssue, nil, false, workDir, engine.InvokeOptions{}); err != nil {
			t.Fatalf("Invoke call %d: %v", i, err)
		}
	}

	want := []string{"first", "second", "second", "second"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q (full: %v)", i, calls[i], want[i], calls)
		}
	}
	if got := inv.StageCallCount("Review"); got != 4 {
		t.Errorf("StageCallCount(Review) = %d, want 4", got)
	}
}

// TestInvoker_ForStage_DoesNotAffectOtherStages confirms script selection is
// keyed per stage name, not global.
func TestInvoker_ForStage_DoesNotAffectOtherStages(t *testing.T) {
	workDir := newGitWorkDir(t)
	inv := New()
	var scripted bool
	inv.ForStage("Review", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		scripted = true
		return "FABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{}, nil
	})

	if _, _, _, err := inv.Invoke(context.Background(), &stages.Stage{Name: "Implement"}, testIssue, nil, false, workDir, engine.InvokeOptions{}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if scripted {
		t.Error("Review's registered script fired for an Implement invocation")
	}
}

// --- Failure-shape contract tests (Task 6 / R2 / AC5 seed) ---
//
// Each test pins a scripted failure shape against RealClaudeInvoker's
// documented (output, completed, usage, err) contract for that exit
// condition (see engine/claude.go and CLAUDE.md's label reference), so a
// mismatch is caught here rather than silently testing the wrong engine
// branch in a scenario test built on top.

func TestTurnLimitExhausted_MatchesProductionContract(t *testing.T) {
	workDir := newGitWorkDir(t)
	script := TurnLimitExhausted(50)

	output, completed, usage, err := script(context.Background(), testStage, testIssue, nil, false, workDir, engine.InvokeOptions{})
	if completed {
		t.Error("a turn-limit exit must report completed=false — the stage did not finish")
	}
	if strings.Contains(output, "FABRIK_STAGE_COMPLETE") {
		t.Error("a turn-limit exit's output must not contain the completion marker")
	}
	if usage.TurnsUsed == 0 {
		t.Error("a turn-limit exit consumed real turns — usage.TurnsUsed must be non-zero")
	}
	var turnErr *claudeerr.TurnLimitError
	if !errors.As(err, &turnErr) {
		t.Fatalf("err = %v, want *claudeerr.TurnLimitError (errors.As must match — this is exactly what engine/item.go's turnLimited detection relies on)", err)
	}
	if turnErr.NumTurns != 50 {
		t.Errorf("NumTurns = %d, want 50", turnErr.NumTurns)
	}
	// No commit for a turn-limit exit script by design (it models a run that
	// consumed turns without necessarily producing a durable change); nothing
	// to assert about git state here.
}

func TestUsageLimitExit_MatchesProductionContract(t *testing.T) {
	workDir := newGitWorkDir(t)
	script := UsageLimitExit()

	output, completed, usage, err := script(context.Background(), testStage, testIssue, nil, false, workDir, engine.InvokeOptions{})
	if completed {
		t.Error("a usage-limit exit must report completed=false — the stage never ran")
	}
	if output != "" {
		t.Errorf("output = %q, want empty — the stage never ran", output)
	}
	if usage.TurnsUsed != 0 || usage.CostUSD != 0 {
		t.Errorf("usage = %+v, want zero turns/cost — classifyUsageLimitExit's own structural signal", usage)
	}
	var limitErr *claudeerr.UsageLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("err = %v, want *claudeerr.UsageLimitError", err)
	}
	if limitErr.ResetTime != "" {
		t.Errorf("ResetTime = %q, want empty (the structural detector never parses a reset time — see claudeerr.UsageLimitError's doc comment)", limitErr.ResetTime)
	}
}

func TestAPIErrorExit_MatchesProductionContract(t *testing.T) {
	workDir := newGitWorkDir(t)
	script := APIErrorExit()

	output, completed, usage, err := script(context.Background(), testStage, testIssue, nil, false, workDir, engine.InvokeOptions{})
	if completed {
		t.Error("an api_error exit must report completed=false — the stage never ran")
	}
	if output != "" {
		t.Errorf("output = %q, want empty", output)
	}
	if usage.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0 per #1458 R5's belt-and-suspenders", usage.CostUSD)
	}
	var apiErr *claudeerr.APIErrorExit
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *claudeerr.APIErrorExit", err)
	}
	if apiErr.TerminalReason != "api_error" {
		t.Errorf("TerminalReason = %q, want %q", apiErr.TerminalReason, "api_error")
	}
}

func TestPartialOutputThenKilled_MatchesProductionContract(t *testing.T) {
	workDir := newGitWorkDir(t)
	script := PartialOutputThenKilled()

	output, completed, _, err := script(context.Background(), testStage, testIssue, nil, false, workDir, engine.InvokeOptions{})
	if !completed {
		t.Error("PartialOutputThenKilled must report completed=true — the marker was present in output before the non-zero exit (engine/claude.go's stageCompleteRE branch)")
	}
	if !strings.Contains(output, "FABRIK_STAGE_COMPLETE") {
		t.Errorf("output = %q, want it to contain FABRIK_STAGE_COMPLETE", output)
	}
	if err == nil {
		t.Fatal("PartialOutputThenKilled must still return a non-nil error — the process exited non-zero")
	}
	// Deliberately NOT one of the claudeerr types — production returns a
	// generic wrapped error here, not a classified type (see doc comment).
	var usageErr *claudeerr.UsageLimitError
	var turnErr *claudeerr.TurnLimitError
	var apiErr *claudeerr.APIErrorExit
	if errors.As(err, &usageErr) || errors.As(err, &turnErr) || errors.As(err, &apiErr) {
		t.Errorf("err = %v (%T), must NOT match any claudeerr type — production's stageCompleteRE branch returns a generic wrapped error", err, err)
	}
	// The commit must be real (same non-vacuity concern as DefaultScript).
	if strings.TrimSpace(gitLog(t, workDir)) == "" {
		t.Fatal("PartialOutputThenKilled did not create any git commit")
	}
}

func TestCommentReviewCompleted_CommitsAndSignalsCompletion(t *testing.T) {
	workDir := newGitWorkDir(t)
	script := CommentReviewCompleted()

	output, completed, _, err := script(context.Background(), testStage, testIssue, nil, workDir, engine.InvokeOptions{})
	if err != nil {
		t.Fatalf("CommentReviewCompleted: %v", err)
	}
	if !completed || !strings.Contains(output, "FABRIK_STAGE_COMPLETE") {
		t.Errorf("completed=%v output=%q, want completed=true with the completion marker", completed, output)
	}
	if strings.TrimSpace(gitLog(t, workDir)) == "" {
		t.Fatal("CommentReviewCompleted did not create any git commit")
	}
}

// TestInvoker_ForStageComments_SelectsIndependentlyFromForStage confirms the
// comment-script selection ladder is tracked separately from the stage
// ladder — a scenario scripting InvokeForComments must not perturb Invoke's
// own call count for the same stage, and vice versa.
func TestInvoker_ForStageComments_SelectsIndependentlyFromForStage(t *testing.T) {
	workDir := newGitWorkDir(t)
	inv := New()

	if _, _, _, err := inv.Invoke(context.Background(), testStage, testIssue, nil, false, workDir, engine.InvokeOptions{}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if _, _, _, err := inv.InvokeForComments(context.Background(), testStage, testIssue, nil, workDir, engine.InvokeOptions{}); err != nil {
		t.Fatalf("InvokeForComments: %v", err)
	}

	if got := inv.StageCallCount(testStage.Name); got != 1 {
		t.Errorf("StageCallCount = %d, want 1 (InvokeForComments must not increment it)", got)
	}
	if got := inv.CommentCallCount(testStage.Name); got != 1 {
		t.Errorf("CommentCallCount = %d, want 1", got)
	}
}
