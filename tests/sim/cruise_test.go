package sim

import (
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// boolPtr returns a pointer to b — EnvOptions.Yolo takes *bool so a scenario
// can distinguish "unset" (defaults true) from an explicit false.
func boolPtr(b bool) *bool { return &b }

// TestCruiseFullPipeline ports tests/e2e/cruise_test.go's TestCruiseFullPipeline
// (R1-R5 from issue #898) — the fabrik:cruise pipeline contract.
//
// fabrik:cruise auto-advances through every stage (Specify -> Research -> Plan
// -> Implement -> Review -> Validate) without merging the PR or advancing to
// Done. Only once a human merges the PR does the engine detect the terminal
// state and close the issue / advance the board to Done. What this test
// verifies, restated from the live test's own doc comment so this file is
// self-contained:
//
//   - R1: fabrik:cruise auto-advances to stage:Validate:complete without any
//     manual column moves.
//   - R2: at Validate-complete, the linked PR is non-draft (ready) and OPEN
//     (not merged or closed).
//   - R3: fabrik:auto-merge-enabled was never applied at any point — cruise
//     suppresses the auto-merge path entirely.
//   - R4: the issue is still OPEN and no stage:Done:* labels are present at
//     Validate-complete (proxy for "board column = Validate").
//   - R5: after a human merges the PR, the issue closes and the board
//     advances to Done.
//
// Engine mechanics behind R3/R5 (engine/stages.go, engine/pr_terminal_advance.go):
// attemptMergeOnValidate is only ever called when yoloActive is true
// (handleStageComplete's `if yoloActive && stage.Name == "Validate"` guard) —
// a pure-cruise item (fabrik:cruise, no fabrik:yolo) never reaches it at all,
// so fabrik:auto-merge-enabled can never be applied (R3). Separately,
// hasCruiseLabel && stage.Name == "Validate" forces shouldAdvance = false,
// which is what stops the board at Validate instead of Done (R1/R4). Once a
// human merges the PR (this test's env.Sim.MergePR call, standing in for the
// live test's `gh pr merge`), advanceValidateTerminalItem — reached via the
// open/closed-item settle-scan pair (runValidatePRTerminalAdvance /
// settleClosedValidateAdvance), which runs unconditionally regardless of
// yolo/cruise — detects the merged PR, fills any missing gate-checked
// completion labels, and advances the board to Done (R5). simgh's own
// MergePR auto-closes the linked issue on a default-branch merge, mirroring
// GitHub's closing-keyword behavior (see simgh/prs.go).
func TestCruiseFullPipeline(t *testing.T) {
	t.Parallel() // R8: this package's dominant real-time cost — see poll.go's workerYield doc comment
	// Yolo must be off so cruiseActive (engine/stages.go: !yoloActive &&
	// hasCruiseLabel) is actually true — NewEnv defaults Yolo to true, which
	// would make yoloActive win and exercise the wrong code path entirely.
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), Yolo: boolPtr(false)})

	num := FileIssue(t, env, "e2e cruise full pipeline (ported)",
		`## Goal

End-to-end verification of the fabrik:cruise pipeline contract (#898):
cruise auto-advances through all stages to Validate-complete without merging
the PR, and the issue closes correctly after a human merges the PR.`,
		"Specify", "fabrik:cruise")

	// R1: engine auto-advances through every stage to Validate-complete with
	// no manual column moves (only clock/poll advancement, per R8). Waited
	// stage-by-stage (mirroring smoke_test.go's TestSmoke_FullPipelineToDone)
	// rather than a single combined budget for stage:Validate:complete, so
	// each stage transition gets its own maxPolls allowance.
	for _, name := range []string{"Specify", "Research", "Plan", "Implement", "Review", "Validate"} {
		WaitForIssueLabel(t, env, num, "stage:"+name+":complete", 80)
	}

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr == nil || pr.Number == 0 {
		t.Fatal("expected a linked PR by Validate-complete (created during Implement)")
	}

	// R2: PR must be non-draft (Implement marks it ready) and OPEN (not merged).
	if pr.Draft {
		t.Errorf("PR #%d is still a draft at Validate-complete (expected non-draft) — R2", pr.Number)
	}
	if pr.Merged || pr.State != "open" {
		t.Errorf("PR #%d state=%q merged=%v at Validate-complete (expected open, unmerged) — R2", pr.Number, pr.State, pr.Merged)
	}

	// R3: fabrik:auto-merge-enabled must never have been applied — cruise
	// suppresses the auto-merge path entirely (attemptMergeOnValidate is
	// gated on yoloActive, which is false here). Checked against the mutation
	// log rather than current labels: a label that was applied and then
	// removed would pass a snapshot check but still be the exact regression
	// R3 exists to catch.
	if entries := env.Sim.Log().Find(simgh.LabelAdded(num, "fabrik:auto-merge-enabled")); len(entries) != 0 {
		t.Errorf("fabrik:auto-merge-enabled was applied %d time(s) — cruise must suppress auto-merge entirely — R3\nentries: %v", len(entries), entries)
	}

	// R4 (proxy): issue is OPEN, no stage:Done:* labels present — the board
	// column is still Validate, not Done.
	item := projectItem(t, env, num)
	if item.IsClosed {
		t.Error("issue is closed at Validate-complete (expected OPEN) — R4")
	}
	for _, l := range item.Labels {
		if strings.HasPrefix(l, "stage:Done:") {
			t.Errorf("found %q label at Validate-complete — engine advanced to Done prematurely — R4", l)
		}
	}
	if item.Status != "Validate" {
		t.Errorf("board status = %q at Validate-complete, want %q — R4", item.Status, "Validate")
	}

	// Merge the PR as a human — the action cruise is waiting for.
	if err := env.Sim.MergePR(env.Owner, env.Repo, pr.Number); err != nil {
		t.Fatalf("MergePR: %v", err)
	}

	// R5: after the human merge, the engine detects the terminal PR state,
	// advances the board to Done, and the issue closes (simgh's MergePR
	// auto-closes it, mirroring GitHub's Closes #N keyword on merge).
	WaitForIssueClosed(t, env, num, 80)
	WaitForProjectStatus(t, env, num, "Done", 80)
}

// TestCruiseFullPipeline_NonVacuous is AC10/R2's non-vacuity companion for
// TestCruiseFullPipeline's R3 assertion. It neutralizes cruise by filing the
// otherwise-identical issue under plain fabrik:yolo instead — the case R3's
// own doc comment (and engine/stages.go) says takes the *other* branch —
// and confirms fabrik:auto-merge-enabled DOES get applied at
// Validate-complete. This proves TestCruiseFullPipeline's "never applied"
// assertion is not vacuously true: an ordinary yolo (non-cruise) item hits
// this label every time.
func TestCruiseFullPipeline_NonVacuous(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), Yolo: boolPtr(true)})
	// Auto-merge must actually be allowed here (unlike the smoke test's
	// direct-merge-fallback setup) so attemptMergeOnValidate takes the
	// EnablePullRequestAutoMerge branch this test is probing, not the
	// unrelated fallback path.
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: true, CanPush: true})

	num := FileIssue(t, env, "Cruise non-vacuity probe (yolo, not cruise)", "body", "Specify")

	for _, name := range []string{"Specify", "Research", "Plan", "Implement", "Review", "Validate"} {
		WaitForIssueLabel(t, env, num, "stage:"+name+":complete", 80)
	}
	WaitForIssueLabel(t, env, num, "fabrik:auto-merge-enabled", 20)
	if entries := env.Sim.Log().Find(simgh.LabelAdded(num, "fabrik:auto-merge-enabled")); len(entries) == 0 {
		t.Fatal("expected fabrik:auto-merge-enabled to have been applied for a plain-yolo (non-cruise) item — if this fails, TestCruiseFullPipeline's R3 assertion is not discriminating")
	}
}
