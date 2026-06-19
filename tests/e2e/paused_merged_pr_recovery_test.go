//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// TestPausedMergedPRRecovery is the e2e regression guard for the #874 class of
// stuck issues: a paused item whose linked PR merges externally while gate
// labels are present must be healed by the settle-owner
// (runValidatePRTerminalAdvance, introduced in #887/ADR-056 D2) without any
// manual intervention.
//
// Each sub-test drives a cruise issue through Implement (creating an open PR),
// then forces the #874-class stuck state by applying fabrik:paused +
// fabrik:awaiting-input + an optional gate label before moving the board to
// Validate. After the PR is merged externally, the test asserts that the
// settle-owner heals the issue: stage:Validate:complete is added, all stuck
// labels are removed, and the issue closes.
//
// Sub-tests (sequential — see R6):
//   - "awaiting-ci":      gate label fabrik:awaiting-ci    (R1)
//   - "awaiting-review":  gate label fabrik:awaiting-review (R2)
//   - "no-gate-label":    no gate label — control case       (R3)
//
// R4: No variant ends stranded with fabrik:paused after the poll budget.
// R5: stage:Validate:complete is added by the settle-owner even though the
//
//	Validate stage was never invoked (Validate never ran; the label is absent
//	until the settle-owner fills it).
//
// R6: Sub-tests run sequentially (no t.Parallel inside t.Run) to avoid
//
//	label-mutation races on the shared test board.
//
// Prerequisites: gate labels fabrik:awaiting-ci and fabrik:awaiting-review
// must be seeded in handarbeit/fabrik-test-alpha (AddLabel fatals if not).
//
// Wall-clock: ~60–90 min (3 sequential sub-tests, ~20–30 min each).
// Use E2E_TIMEOUT=3h when running this test in isolation:
//
//	E2E_TIMEOUT=3h scripts/e2e/run.sh -run TestPausedMergedPRRecovery
//
// Cost: ~$1.50–4.50 (three cruise pipelines through Specify → Implement).
//
// References: #874 (original stuck-state bug class), #887 (settle-owner),
// ADR-056 D2 (single-owner PR-terminal advance).
func TestPausedMergedPRRecovery(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	cases := []struct {
		name      string
		gateLabel string
	}{
		{"awaiting-ci", "fabrik:awaiting-ci"},
		{"awaiting-review", "fabrik:awaiting-review"},
		{"no-gate-label", ""},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Sequential — no t.Parallel() here (R6).
			stamp := time.Now().UTC().Format("20060102-150405")
			marker := fmt.Sprintf("paused-merged-pr-%s-%s", tc.name, stamp)
			body := fmt.Sprintf(pausedMergedPRBodyTemplate, "`", "`", "```", marker, "```")

			num := FileIssue(t, env, env.RepoAlpha,
				fmt.Sprintf("e2e paused-merged-pr recovery (%s/%s)", tc.name, stamp),
				body, "fabrik:cruise")
			itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
			SetIssueStatus(t, env, itemID, "Specify")
			t.Logf("filed %s#%d at Status=Specify with fabrik:cruise, variant=%s, marker=%s",
				env.RepoAlpha, num, tc.name, marker)

			// Step 1: Wait for Implement to complete. The PR is created and open at
			// this point; stage:Validate:complete does not yet exist.
			WaitForIssueLabel(t, env, env.RepoAlpha, num, "stage:Implement:complete", 60*time.Minute)
			t.Logf("%s#%d reached stage:Implement:complete", env.RepoAlpha, num)

			// Step 2: Discover the linked PR. It was created during Implement.
			prNum := WaitForLinkedPR(t, env, env.RepoAlpha, num, 5*time.Minute)
			t.Logf("linked PR: %s#%d", env.RepoAlpha, prNum)

			// Step 3: Force the #874-class stuck state.
			// fabrik:paused first — the Phase 1/2 dispatch loop skips paused items,
			// closing the race window before Review can be dispatched.
			AddLabel(t, env, env.RepoAlpha, num, "fabrik:paused")
			AddLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-input")
			if tc.gateLabel != "" {
				AddLabel(t, env, env.RepoAlpha, num, tc.gateLabel)
			}
			// Move the board to Validate: the settle-owner only processes items
			// whose board column is Validate.
			SetIssueStatus(t, env, itemID, "Validate")
			t.Logf("stuck state applied to %s#%d: fabrik:paused + fabrik:awaiting-input + gate=%q, board=Validate",
				env.RepoAlpha, num, tc.gateLabel)

			// Step 4: Merge the PR externally. This is the trigger for the
			// settle-owner. stage:Validate:complete is absent at this point — Validate
			// was never invoked — so the settle-owner will fill it from scratch (R5).
			MergePR(t, env, env.RepoAlpha, prNum)
			t.Logf("merged PR %s#%d — waiting for settle-owner to heal the issue", env.RepoAlpha, prNum)

			// Step 5: Assert recovery. The settle-owner runs on the next Fabrik
			// poll cycle (up to ~30s after the merge).

			// R5: stage:Validate:complete must be added by the settle-owner.
			// WaitForIssueLabel polls until the label appears (up to 15 min).
			WaitForIssueLabel(t, env, env.RepoAlpha, num, "stage:Validate:complete", 15*time.Minute)
			// AssertLabelWasApplied provides timeline-level confirmation (survives
			// the case where Fabrik adds and removes a label faster than polling).
			AssertLabelWasApplied(t, env, env.RepoAlpha, num, "stage:Validate:complete")
			t.Logf("stage:Validate:complete confirmed on %s#%d — R5 verified", env.RepoAlpha, num)

			// R1/R2/R3: stuck labels must be cleared.
			WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:paused", 5*time.Minute)
			t.Logf("fabrik:paused cleared on %s#%d — R%s verified", env.RepoAlpha, num, rNumber(tc.name))
			WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:awaiting-input", 5*time.Minute)
			if tc.gateLabel != "" {
				WaitForLabelAbsent(t, env, env.RepoAlpha, num, tc.gateLabel, 5*time.Minute)
				t.Logf("gate label %q cleared on %s#%d", tc.gateLabel, env.RepoAlpha, num)
			}

			// R4: issue must close (settle-owner advances board to Done; GitHub
			// also auto-closes via Closes #N on merge).
			WaitForIssueClosed(t, env, env.RepoAlpha, num, 5*time.Minute)
			t.Logf("%s#%d closed — R4 verified (variant=%s full recovery confirmed)", env.RepoAlpha, num, tc.name)
		})
	}
}

// rNumber maps a sub-test name to the spec requirement number for log output.
func rNumber(name string) string {
	switch name {
	case "awaiting-ci":
		return "1"
	case "awaiting-review":
		return "2"
	default:
		return "3"
	}
}

// pausedMergedPRBodyTemplate is the issue body for TestPausedMergedPRRecovery.
// The five %s placeholders are: backtick, backtick, codefence, marker, codefence
// (Go raw strings can't contain backticks).
const pausedMergedPRBodyTemplate = `## Goal

End-to-end regression guard for the #874 class of stuck issues (paused item
with merged PR; settle-owner recovery path from #887/ADR-056 D2).

## Trivial change

Append a single HTML comment line to %sREADME.md%s at the very end of the file:

%s
<!-- %s -->
%s

That is the entire change. One file, one line. Plan should NOT decompose.

## Scope

Single repo only. This issue verifies that the settle-owner correctly heals
a paused-and-merged item regardless of which gate label it carries.
`
