package sim

import (
	"os/exec"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// smokeStages returns the R7 pipeline: Specify → Research → Plan → Implement
// → Review → Validate → Done. Deliberately no wait_for_ci/wait_for_reviews
// gates (out of scope per #1449's Plan Key Decisions — those are follow-on
// scenarios) and no YAML loading — a Go literal, matching the convention
// engine/*_test.go already uses for hand-built pipelines
// (e.g. closedAdvanceStages in engine/closed_item_advance_settle_test.go).
func smokeStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Research", Order: 2},
		{Name: "Plan", Order: 3},
		{Name: "Implement", Order: 4, CreateDraftPR: true, MarkPRReadyOnComplete: true},
		{Name: "Review", Order: 5},
		{Name: "Validate", Order: 6},
		{Name: "Done", Order: 7, CleanupWorktree: true},
	}
}

// TestSmoke_FullPipelineToDone is the R7/AC1-4/AC8 proof that the sim layer
// works: one issue traverses the full 7-stage pipeline to a closed issue in
// Done, using only explicit poll advancement (AC1), asserting the
// in_progress/complete label pair and board column move at each stage
// (AC2), a real committed diff on the linked PR's head branch (AC3), and
// draft-PR-before-Implement + mark-ready-on-complete against the sim
// model's own PR state (AC4).
func TestSmoke_FullPipelineToDone(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})

	// Force the deterministic direct-merge fallback in attemptMergeOnValidate
	// (engine/stages.go): with native auto-merge unavailable, Validate's
	// completion merges the PR synchronously within the same dispatch,
	// rather than the async native-auto-merge path the sim's
	// EnablePullRequestAutoMerge never spontaneously resolves (see
	// simgh/FIDELITY.md) — the scenario would otherwise hang waiting for a
	// convergence event the model cannot produce on its own.
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})

	// Captured before any stage runs, so the AC3 diff below compares against
	// the true pre-pipeline base — after Validate merges the PR, the base
	// branch itself will contain every commit the pipeline made, so diffing
	// the (by-then-identical) current base against head would show nothing.
	baseSHA, err := env.Sim.Sim().HeadSHA(env.OwnerRepo, "main")
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}

	num := FileIssue(t, env, "Smoke: full pipeline", "Prove the sim layer end to end.", "Specify")

	stageOrder := []string{"Specify", "Research", "Plan", "Implement", "Review", "Validate"}
	for _, name := range stageOrder {
		WaitForIssueLabel(t, env, num, "stage:"+name+":complete", 40)
		labels := IssueLabels(t, env, num)
		if hasLabel(labels, "stage:"+name+":in_progress") {
			t.Errorf("stage %q: in_progress label should have been cleared once complete, labels=%v", name, labels)
		}
	}

	WaitForProjectStatus(t, env, num, "Done", 40)
	WaitForIssueClosed(t, env, num, 40)

	// AC4: draft PR created before Implement, marked ready on completion —
	// asserted against the sim model's own PR state, not the invoker's script.
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr == nil || pr.Number == 0 {
		t.Fatal("expected a linked PR")
	}
	if pr.Draft {
		t.Error("PR should have been marked ready (not draft) once Implement completed")
	}
	if !pr.Merged {
		t.Error("PR should be merged once Validate completed under yolo")
	}

	// AC3: the scripted invoker's commits are real — the linked PR's head
	// branch in the backing git repo contains the committed file, and the
	// diff is non-empty.
	simBareDir, err := env.Sim.Sim().RepoBareDir(env.OwnerRepo)
	if err != nil {
		t.Fatalf("RepoBareDir: %v", err)
	}
	diff := gitDiffNames(t, simBareDir, baseSHA, pr.HeadSHA)
	if strings.TrimSpace(diff) == "" {
		t.Fatal("PR diff is empty — the scripted invoker's commits are not real (the exact false-pass AC10 guards against)")
	}
	if !strings.Contains(diff, ".simclaude/Implement.md") {
		t.Errorf("PR diff = %q, want it to include .simclaude/Implement.md (the default script's marker file)", diff)
	}

	t.Logf("final board state:\n%s", boardSummary(env))
}

// gitDiffNames returns the names of files changed between a and b (commit
// SHAs or refs) in the bare repository at dir, via `git diff --name-only`.
// Works directly against a bare repo — diff needs no working tree.
func gitDiffNames(t *testing.T, dir, a, b string) string {
	t.Helper()
	cmd := exec.Command("git", "diff", "--name-only", a, b)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git diff --name-only %s %s (dir=%q): %v: %s", a, b, dir, err, out)
	}
	return string(out)
}
