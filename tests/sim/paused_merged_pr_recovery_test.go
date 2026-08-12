package sim

import (
	"fmt"
	"testing"

	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// This file ports tests/e2e/paused_merged_pr_recovery_test.go's
// TestPausedMergedPRRecovery — the e2e regression guard for the #874 bug
// class: a paused item whose linked PR is merged externally must be healed
// by the settle-owner (advanceValidateTerminalItem /
// runValidatePRTerminalAdvance, ADR-056 D2) regardless of which gate label
// it carries. Ports all 3 sub-cases (R1: gate=fabrik:awaiting-ci, R2:
// gate=fabrik:awaiting-review, R3: no gate label/control) as t.Run
// sub-tests sharing one parent, mirroring the live structure (R6).
//
// Divergence from the live suite (R2): the live test drives a real
// Specify→Implement pipeline via Claude to get a genuine open PR, then
// forces the stuck state by hand. This port seeds the stuck state directly
// (issue at Validate, fabrik:paused + fabrik:awaiting-input + optional gate
// label, an open linked PR, stage:Validate:complete deliberately absent) —
// the zero-cost construction precedent this package uses throughout
// (review_gate_helpers.go). What's under test is the settle-owner's
// reaction to a merged PR on a stuck item, not how the item got stuck, so
// skipping the pipeline drive loses no assertion strength.
//
// Per Constraint 6 (Research/FIDELITY.md): the PR is seeded open and merged
// for real via env.Sim.MergePR, never SeedPR{Merged: true} directly — the
// latter only sets the bookkeeping flag with no merge commit and no
// auto-close, which would make R4's "issue closes" assertion pass for a
// reason production couldn't reproduce.
//
// The live test's fixed 45-second real-wall-clock sleep (a sync barrier so
// the engine's board cache observes the item at Validate before the
// external merge closes it) has no port here at all — RunPoll/AdvanceUntil
// drive the engine's own poll loop directly against the sim's synchronous
// state, so there is no board-cache-refetch race to guard against in the
// first place (R2's "sim can assert more, deterministically, not by
// replacing a race with a longer wait").

// pausedMergedPRRecoveryStages: Validate carries both wait_for_ci and
// wait_for_reviews — the live test's rationale (a) for why Validate is
// "gate-checked" and therefore eligible for the settle-owner's completion-
// label fill loop.
func pausedMergedPRRecoveryStages() []*stages.Stage {
	tr := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1, CreateDraftPR: true, MarkPRReadyOnComplete: true},
		{Name: "Validate", Order: 2, WaitForCI: &tr, WaitForReviews: &tr},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}
}

// seedPausedMergedPRItem seeds the #874-class stuck state directly: an
// issue at the Validate column carrying fabrik:paused + fabrik:awaiting-input
// (+ gateLabel, if non-empty), with an open linked PR and deliberately no
// stage:Validate:complete label — R5's assertion is that the settle-owner,
// not Validate itself, is what adds it.
func seedPausedMergedPRItem(t *testing.T, env *Env, marker, gateLabel string) (issueNum, prNum int) {
	t.Helper()

	labels := []string{"fabrik:paused", "fabrik:awaiting-input"}
	if gateLabel != "" {
		labels = append(labels, gateLabel)
	}
	title := fmt.Sprintf("sim paused-merged-pr %s", marker)
	num := FileIssue(t, env, title, fmt.Sprintf("sim paused_merged_pr_recovery test. marker=%s", marker), "Validate", labels...)

	branch := fmt.Sprintf("fabrik/issue-%d", num)
	path := fmt.Sprintf("sim/paused-merged-pr/%s-%d.md", marker, num)
	content := fmt.Sprintf("# sim paused-merged-pr marker\n\nmarker=%s\n", marker)
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, "", map[string]string{path: content}, "sim: "+title)
	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{
		Title:       title,
		Head:        branch,
		IssueNumber: num,
	})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seedPausedMergedPRItem: %v", err)
	}

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("seedPausedMergedPRItem: FetchLinkedPR: %v", err)
	}
	if pr == nil || pr.Number == 0 {
		t.Fatalf("seedPausedMergedPRItem: no linked PR for #%d", num)
	}

	t.Logf("seeded #874-class stuck item: issue #%d, PR #%d, Status=Validate, stuck-state labels=%v (gateLabel=%q)",
		num, pr.Number, labels, gateLabel)
	return num, pr.Number
}

// TestPausedMergedPRRecovery ports the live test of the same name (R1–R6,
// issue #896). Each sub-test seeds the stuck state directly, merges the PR
// for real, and asserts the settle-owner heals the issue to CLOSED — filling
// stage:Validate:complete, clearing every stuck-state/gate label — entirely
// independent of which gate label (or none) was present.
func TestPausedMergedPRRecovery(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		gateLabel string
	}{
		{"awaiting-ci", "fabrik:awaiting-ci"},         // R1
		{"awaiting-review", "fabrik:awaiting-review"}, // R2
		{"no-gate-label", ""},                         // R3 (control)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewEnv(t, EnvOptions{Stages: pausedMergedPRRecoveryStages()})

			num, prNum := seedPausedMergedPRItem(t, env, tc.name, tc.gateLabel)

			// stage:Validate:complete must be genuinely absent before the
			// merge — otherwise R5 ("the settle-owner fills it, Validate
			// never ran") would pass vacuously.
			if hasLabel(IssueLabels(t, env, num), "stage:Validate:complete") {
				t.Fatal("stage:Validate:complete present before the merge — seeding bug, R5 would be vacuous")
			}

			if err := env.Sim.MergePR(env.Owner, env.Repo, prNum); err != nil {
				t.Fatalf("MergePR: %v", err)
			}
			t.Logf("merged PR #%d for real — waiting for settle-owner to heal #%d", prNum, num)

			// R5: the settle-owner, not Validate, adds stage:Validate:complete.
			WaitForIssueLabel(t, env, num, "stage:Validate:complete", 80)
			t.Logf("stage:Validate:complete applied by settle-owner — R5")

			// R1/R2/R3: stuck-state and gate labels cleared.
			WaitForLabelAbsent(t, env, num, "fabrik:paused", 80)
			WaitForLabelAbsent(t, env, num, "fabrik:awaiting-input", 80)
			if tc.gateLabel != "" {
				WaitForLabelAbsent(t, env, num, tc.gateLabel, 80)
			}
			t.Logf("stuck-state labels cleared (gateLabel=%q) — R1/R2/R3", tc.gateLabel)

			// R4: issue must be CLOSED, and must not be stranded.
			WaitForIssueClosed(t, env, num, 80)
			t.Logf("#%d closed (variant=%s) — R4 verified", num, tc.name)
		})
	}
}
