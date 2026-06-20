//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// TestPausedMergedPRRecovery is the e2e regression guard for the #874 bug
// class: a paused item whose linked PR is merged externally must be healed by
// the settle-owner (runValidatePRTerminalAdvance, ADR-056 D2) regardless of
// which gate label it carries.
//
// Each sub-test drives a cruise issue through Implement (creating an open PR),
// forces the #874-class stuck state (fabrik:paused + fabrik:awaiting-input +
// optional gate label, board at Validate), merges the PR externally, and
// asserts the settle-owner heals the issue.
//
// Requirements covered (R1–R6 from issue #896):
//   - R1: gate=fabrik:awaiting-ci  → stage:Validate:complete added, gate + pause labels removed, issue CLOSED.
//   - R2: gate=fabrik:awaiting-review → same recovery as R1.
//   - R3: no gate label (control) → same recovery; proves advancement is gate-label-agnostic.
//   - R4: no variant ends stranded after the poll budget.
//   - R5: stage:Validate:complete is added by the settle-owner (Validate was never invoked).
//   - R6: three t.Run sub-tests sharing a single TestPausedMergedPRRecovery function.
//
// Sub-tests run sequentially (no t.Parallel inside t.Run) to avoid label-
// mutation races on the shared test board.
//
// Prerequisites: handarbeit/fabrik-test-alpha must have the labels
// fabrik:cruise, fabrik:paused, fabrik:awaiting-input, fabrik:awaiting-ci, and
// fabrik:awaiting-review seeded (all are production labels and should exist).
//
// Wall-clock: ~60–90 min (3 sequential sub-tests, ~20–30 min each).
// Run with: E2E_TIMEOUT=3h scripts/e2e/run.sh -run TestPausedMergedPRRecovery
// Cost: ~$1.50–4.50.
func TestPausedMergedPRRecovery(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	cases := []struct {
		name      string
		gateLabel string
	}{
		{"awaiting-ci", "fabrik:awaiting-ci"},       // R1
		{"awaiting-review", "fabrik:awaiting-review"}, // R2
		{"no-gate-label", ""},                         // R3 (control)
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			stamp := time.Now().UTC().Format("20060102-150405")
			marker := fmt.Sprintf("paused-merged-pr-%s-%s", tc.name, stamp)
			body := fmt.Sprintf(pausedMergedPRBodyTemplate, "`", "`", "```", marker, "```")

			// File WITHOUT an auto-advance label. With global yolo off and
			// auto_advance unset (nil, not true) on Research..Validate, the engine
			// advances NOTHING on its own (poll.go:1286) — it runs only the stage
			// whose column we set. This gives the test total control of the board
			// position and removes the cruise race that previously let the issue
			// reach Review before we could pin it at Validate.
			num := FileIssue(t, env, env.RepoAlpha,
				fmt.Sprintf("e2e paused merged-PR recovery (%s %s)", tc.name, stamp),
				body)
			itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
			t.Logf("filed %s#%d (no auto-advance label), variant=%s marker=%s",
				env.RepoAlpha, num, tc.name, marker)

			// Step 1: Drive Specify→Implement one stage at a time. Because nothing
			// auto-advances, the engine stops after each stage's :complete, so it
			// never runs past where we intend to pin the item. The draft PR is
			// created during Implement and is open once Implement completes.
			for _, st := range []string{"Specify", "Research", "Plan", "Implement"} {
				SetIssueStatus(t, env, itemID, st)
				WaitForIssueLabel(t, env, env.RepoAlpha, num, "stage:"+st+":complete", 30*time.Minute)
			}
			t.Logf("%s#%d reached stage:Implement:complete (manual drive)", env.RepoAlpha, num)

			// Step 2: Discover the linked PR. The PR is created during Implement;
			// 5 minutes is generous for the GraphQL query to surface it.
			prNum := WaitForLinkedPR(t, env, env.RepoAlpha, num, 5*time.Minute)
			t.Logf("linked PR: #%d", prNum)

			// Step 3–5: Force the stuck state. Nothing is auto-advancing (no
			// cruise/yolo label), so the item sits at Implement until we move it —
			// there is no race with stage advancement. Apply fabrik:paused first
			// (it also halts any dispatch as a belt-and-suspenders), then the
			// remaining stuck-state labels.
			AddLabel(t, env, env.RepoAlpha, num, "fabrik:paused")
			AddLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-input")
			if tc.gateLabel != "" {
				AddLabel(t, env, env.RepoAlpha, num, tc.gateLabel)
			}
			t.Logf("applied stuck-state labels (fabrik:paused, fabrik:awaiting-input, gateLabel=%q)", tc.gateLabel)

			// Step 6: Move the board to Validate. The settle-owner
			// (runValidatePRTerminalAdvance) only processes items with
			// item.Status == "Validate". The item is cleanly parked at Implement,
			// so this is an uncontended move.
			SetIssueStatus(t, env, itemID, "Validate")
			t.Logf("moved board to Validate column")

			// Sync barrier: confirm GitHub has propagated the Validate column, then
			// dwell ~1.5 poll cycles so the engine's board cache refetches the OPEN
			// item at Validate BEFORE the external merge closes it. Without this the
			// merge can close the issue while the engine still sees the prior column,
			// so the Validate-scoped settle-owner never observes it at Validate and
			// the heal is missed (the failure mode this test previously exhibited).
			WaitForProjectStatus(t, env, env.RepoAlpha, num, "Validate", 3*time.Minute)
			time.Sleep(45 * time.Second)
			t.Logf("engine had a poll window to observe %s#%d at Validate before merge", env.RepoAlpha, num)

			// Step 7: Merge the PR externally, simulating a human merge.
			MergePR(t, env, env.RepoAlpha, prNum)
			t.Logf("merged PR #%d — waiting for settle-owner to heal", prNum)

			// Step 8 (R5): Wait for stage:Validate:complete.
			// The settle-owner fills this label because:
			//   (a) Validate has wait_for_ci: true and wait_for_reviews: true,
			//       so it is "gate-checked" in the fill loop;
			//   (b) the label is absent — Validate never ran; and
			//   (c) the PR is now merged (terminal).
			// 15 minutes is generous for a single poll cycle to fire.
			WaitForIssueLabel(t, env, env.RepoAlpha, num, "stage:Validate:complete", 15*time.Minute)
			t.Logf("stage:Validate:complete applied by settle-owner — R5")

			// Step 9 (R5 timeline confirmation): Verify via issue timeline that
			// stage:Validate:complete was actually applied (not already present).
			AssertLabelWasApplied(t, env, env.RepoAlpha, num, "stage:Validate:complete")
			t.Logf("timeline confirms stage:Validate:complete was applied — R5 verified")

			// Steps 10–12 (R1/R2/R3): Assert the settle-owner cleared the stuck-state labels.
			WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:paused", 5*time.Minute)
			t.Logf("fabrik:paused absent — R1/R2/R3")

			WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:awaiting-input", 5*time.Minute)
			t.Logf("fabrik:awaiting-input absent — R1/R2/R3")

			if tc.gateLabel != "" {
				WaitForLabelAbsent(t, env, env.RepoAlpha, num, tc.gateLabel, 5*time.Minute)
				t.Logf("%q absent — R1/R2", tc.gateLabel)
			}

			// Step 13 (R4): Issue must be CLOSED.
			// GitHub auto-closes via "Closes #N" on merge, so this typically
			// completes within seconds of MergePR.
			WaitForIssueClosed(t, env, env.RepoAlpha, num, 5*time.Minute)
			t.Logf("%s#%d closed — R4 verified (variant=%s)", env.RepoAlpha, num, tc.name)
		})
	}
}

// pausedMergedPRBodyTemplate is the issue body for TestPausedMergedPRRecovery.
// The five %s placeholders are: backtick, backtick, codefence, marker, codefence
// (Go raw strings can't contain backticks).
const pausedMergedPRBodyTemplate = `## Goal

End-to-end regression guard for the #874 bug class (paused item + merged PR
recovery via the settle-owner, ADR-056 D2).

## Trivial change

Append a single HTML comment line to %sREADME.md%s at the very end of the file:

%s
<!-- %s -->
%s

That is the entire change. One file, one line. Plan should NOT decompose.

## Scope

Single repo only. This issue verifies that when a cruise issue's linked PR is
merged externally while the issue is in the stuck state (fabrik:paused +
fabrik:awaiting-input + optional gate label at Validate), the settle-owner
heals the issue to CLOSED without Validate having been invoked.
`
