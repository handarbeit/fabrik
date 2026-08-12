package sim

import (
	"testing"

	"github.com/handarbeit/fabrik/stages"
)

// autoAdvanceNoYoloStages is smokeStages with per-stage auto_advance: true
// on every stage up to (but not including) Validate — instead of the
// fabrik:yolo/fabrik:cruise label — used only by
// TestYoloAutoMergeLabel_NonVacuous to drive an issue to Validate-complete
// via a route that never sets yoloActive, isolating whether
// attemptMergeOnValidate's gate really checks the yolo label/config
// specifically (not just "did the item advance"). Validate itself is left
// with AutoAdvance unset (nil) so the pipeline parks there rather than
// continuing on to Done/cleanup/archival, which would take the item off the
// board before this test's final label check runs.
func autoAdvanceNoYoloStages() []*stages.Stage {
	tr := true
	stgs := smokeStages()
	for _, s := range stgs {
		if s.Name == "Validate" || s.Name == "Done" {
			continue
		}
		s.AutoAdvance = &tr
	}
	return stgs
}

// TestYoloAutoMergeLabel ports tests/e2e/auto_merge_test.go's
// TestYoloAutoMergeLabel — TRAIN-MODE-OFF LEG ONLY (#829/FR-004/FR-005/
// SC-001). The train-mode-on leg is merge-train (ADR-059), out of scope for
// this port per the issue's own Scope section — see the coverage matrix in
// README.md.
//
// Train-off contract: after Validate completes under fabrik:yolo, the
// engine calls EnablePullRequestAutoMerge once and tags the issue with
// fabrik:auto-merge-enabled (attemptMergeOnValidate, engine/stages.go).
// GitHub then merges the PR asynchronously, whenever it decides CI has
// passed and the PR is mergeable — an event this port cannot make happen on
// its own (see the FIDELITY.md gap below). The engine's own reaction to that
// eventual merge is what checkAutoMergeConvergence (engine/merge_gate.go)
// implements, and that reaction is what this port actually proves.
//
// R5 fidelity finding (FIDELITY.md's native-auto-merge entry, "Simplified
// (flag only)"): simgh's EnablePullRequestAutoMerge only sets a bookkeeping
// flag — it never watches checks and completes the merge on its own the way
// real GitHub's native auto-merge does. This port therefore does not — and
// cannot — prove that GitHub would actually complete the merge once
// fabrik:auto-merge-enabled is applied; it simulates that completion via a
// direct env.Sim.MergePR call and proves only the engine's *reaction*:
// checkAutoMergeConvergence detects the now-merged PR on a subsequent poll
// and clears the label (FR-005), and the issue reaches Done (FR-004/SC-001).
// A regression in the *enablement* call itself (FR-004's "applied at some
// point") is still fully provable, and is asserted before the simulated
// merge.
func TestYoloAutoMergeLabel(t *testing.T) {
	t.Parallel()
	// AllowAutoMerge stays at simgh's true default (unlike smoke_test.go's
	// override to false) — this scenario is specifically about the native
	// auto-merge path attemptMergeOnValidate takes when auto-merge is
	// available, not the direct-merge fallback smoke_test.go exercises.
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})

	num := FileIssue(t, env, "sim yolo auto-merge", "Prove the GitHub native auto-merge path for yolo issues (#829).",
		"Specify", "fabrik:yolo")

	for _, name := range []string{"Specify", "Research", "Plan", "Implement", "Review", "Validate"} {
		WaitForIssueLabel(t, env, num, "stage:"+name+":complete", 80)
	}

	// FR-004: fabrik:auto-merge-enabled applied at Validate-complete — the
	// engine took the native-auto-merge branch, not the direct-merge fallback.
	WaitForIssueLabel(t, env, num, "fabrik:auto-merge-enabled", 20)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr == nil || pr.Number == 0 {
		t.Fatal("expected a linked PR")
	}
	if pr.Merged {
		t.Fatal("PR is already merged before the simulated GitHub auto-merge completion — the direct-merge fallback ran instead of native auto-merge")
	}
	t.Logf("PR #%d: fabrik:auto-merge-enabled applied, not yet merged — native auto-merge path confirmed", pr.Number)

	// Simulate GitHub's async native-auto-merge completing on its own — the
	// one step simgh cannot do spontaneously (FIDELITY.md).
	if err := env.Sim.MergePR(env.Owner, env.Repo, pr.Number); err != nil {
		t.Fatalf("MergePR: %v", err)
	}

	// FR-005/SC-001: the engine's checkAutoMergeConvergence detects the
	// now-merged PR and clears the label; the issue reaches Done.
	WaitForLabelAbsent(t, env, num, "fabrik:auto-merge-enabled", 80)
	WaitForIssueClosed(t, env, num, 80)
	WaitForProjectStatus(t, env, num, "Done", 80)
	t.Logf("%s#%d: fabrik:auto-merge-enabled cleared after simulated merge convergence, issue closed and Done", env.OwnerRepo, num)
}

// TestYoloAutoMergeLabel_NonVacuous proves fabrik:auto-merge-enabled's
// application is actually gated on yolo specifically (attemptMergeOnValidate
// is only ever called when yoloActive is true) rather than on "did the item
// advance to Validate-complete" in general: an issue driven to
// Validate-complete via per-stage auto_advance: true — with neither
// fabrik:yolo nor fabrik:cruise present, so yoloActive stays false the whole
// way — must never get the label.
func TestYoloAutoMergeLabel_NonVacuous(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: autoAdvanceNoYoloStages(), Yolo: boolPtr(false)})

	num := FileIssue(t, env, "sim auto-merge non-vacuity probe (no yolo)", "body", "Specify")

	for _, name := range []string{"Specify", "Research", "Plan", "Implement", "Review", "Validate"} {
		WaitForIssueLabel(t, env, num, "stage:"+name+":complete", 80)
	}
	// Give the catch-up loop a few extra polls to prove the label really
	// never shows up, not just that we didn't wait long enough.
	RunPolls(t, env, 10)
	if labels := IssueLabels(t, env, num); hasLabel(labels, "fabrik:auto-merge-enabled") {
		t.Fatalf("fabrik:auto-merge-enabled applied without fabrik:yolo — attemptMergeOnValidate's yoloActive gate is not discriminating: labels=%v", labels)
	}
	t.Logf("no fabrik:yolo (auto_advance:true drove the pipeline instead) -> fabrik:auto-merge-enabled never applied, confirming the label is yolo-gated, not advance-gated")
}
