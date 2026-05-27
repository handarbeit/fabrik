//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// TestYoloAutoMergeLabel is the regression test for #829 — replacing Fabrik's
// poll-merge loop with GitHub's native auto-merge for yolo issues.
//
// The headline observable behaviour change: after Validate completes for a
// fabrik:yolo issue, the engine calls enablePullRequestAutoMerge once and
// tags the issue with fabrik:auto-merge-enabled. GitHub then merges the PR
// atomically when CI passes and the PR is mergeable, eliminating the
// HTTP 405 spam and race window that the poll-merge loop produced.
//
// What this test verifies (FR-004, FR-005, SC-001 from the #829 spec):
//   - fabrik:auto-merge-enabled is applied at some point during the issue's
//     lifetime (observed via the issue timeline, so we don't miss a fast
//     add-then-remove cycle on a trivial PR).
//   - The issue ultimately closes — i.e. GitHub auto-merge actually merged
//     the PR end-to-end.
//   - fabrik:auto-merge-enabled is NOT present on the closed issue (FR-005:
//     removed once the PR merges).
//
// Out of scope here (better suited to unit/integration tests because
// provoking them deterministically in e2e is hard):
//   - convergence budget exhaustion (Story 3 / SC-003)
//   - mid-flight conflict triggering a bounded rebase (Story 2 / SC-002)
//   - cruise preservation (Story 4 / SC-004 — covered by unit tests of the
//     yolo/cruise gating logic)
//
// Wall-clock: ~20-40 min. Cost: ~$0.50-1.50.
func TestYoloAutoMergeLabel(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	stamp := time.Now().UTC().Format("20060102-150405")
	marker := fmt.Sprintf("auto-merge-yolo-%s", stamp)
	body := fmt.Sprintf(autoMergeBodyTemplate, "`", "`", "```", marker, "```")

	num := FileIssue(t, env, env.RepoAlpha,
		fmt.Sprintf("e2e yolo auto-merge (%s)", stamp),
		body, "fabrik:yolo")
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed %s#%d at Status=Specify, marker=%s", env.RepoAlpha, num, marker)

	// Wait for the full pipeline to land the merge. On trivial PRs with no
	// branch protection or required reviews, GitHub may merge within seconds
	// of auto-merge being enabled — so we don't poll for the transient
	// fabrik:auto-merge-enabled label here. We let the issue close and then
	// audit the timeline.
	WaitForIssueClosed(t, env, env.RepoAlpha, num, 45*time.Minute)
	t.Logf("%s#%d closed — checking the auto-merge path was taken", env.RepoAlpha, num)

	// Race-free verification that auto-merge enablement happened: scan the
	// issue's timeline for a labeled-with-fabrik:auto-merge-enabled event.
	AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:auto-merge-enabled")
	t.Logf("fabrik:auto-merge-enabled was applied at some point — GitHub native auto-merge path verified")

	// FR-005: the label must be removed when the PR merges. Fabrik needs
	// 1-2 poll cycles (~30-60s) after the merge to observe it and remove the
	// label — checking immediately races the cleanup poll.
	WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:auto-merge-enabled", 5*time.Minute)
	t.Logf("fabrik:auto-merge-enabled was cleaned up after merge — FR-005 verified")
}

// autoMergeBodyTemplate is the issue body for TestYoloAutoMergeLabel. The five
// %s placeholders are: backtick, backtick, codefence, marker, codefence
// (Go raw strings can't contain backticks).
const autoMergeBodyTemplate = `## Goal

End-to-end verification of the GitHub native auto-merge path for yolo issues (#829).

## Trivial change

Append a single HTML comment line to %sREADME.md%s at the very end of the file:

%s
<!-- %s -->
%s

That is the entire change. One file, one line. Plan should NOT decompose.

## Scope

Single repo only. This issue exists purely to drive a yolo PR through the new
post-Validate convergence flow and verify Fabrik enables GitHub auto-merge
(applying the fabrik:auto-merge-enabled label) rather than running the legacy
poll-merge loop.
`
