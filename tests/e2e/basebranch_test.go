//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// TestBaseBranchPipeline is the e2e regression test for the base:<branch>
// (non-default base branch) pipeline contract. It closes the e2e coverage
// gap that let #1046 escape to a community user in v0.0.74: GitHub only
// populates closingIssuesReferences / closedByPullRequestsReferences (and the
// review data nested inside them) for PRs targeting a repo's *default*
// branch, so any code path that unconditionally trusts those GraphQL fields
// silently breaks for base:<branch> issues — a defect class unit tests
// structurally cannot catch, since they mock GitHub's responses.
//
// Two fixes shipped for #1046: #1047 (issue<->PR linkage verification via
// verifyAndHealLinkageByBody) and #1050 (base-independent review-gate data
// feed via FetchPRReviews/FetchPRReviewRequests). Both are unit-tested but
// had no end-to-end validation before this scenario.
//
// The scenario creates a throwaway branch off main, files a base:<branch>
// issue targeting it under fabrik:cruise, and asserts:
//   - the PR actually targets the throwaway branch (not a silent fallback to
//     main from baseBranchForItem — that fallback only fires if the branch
//     is missing on the remote, which it isn't here);
//   - the pipeline does NOT pause at end of Implement (the #1046 symptom;
//     validates #1047 — verifyAndHealLinkageByBody must not false-fail on the
//     always-empty GraphQL closingIssuesReferences for a non-default-base PR);
//   - the review gate clears naturally rather than degrading to the timeout
//     path (validates #1050) — a base:<branch> PR's GraphQL-sourced
//     LinkedPRReviews/LinkedPRReviewRequests are structurally always empty,
//     so a natural clear is only possible if checkReviewGate's REST fallback
//     is correctly supplying real review data. This relies on
//     gemini-code-assist auto-reviewing the PR (the same bot-availability
//     dependency already accepted by TestConjunctiveCIReviewGate) — a real
//     regression in the REST feed would make the gate time out and pause,
//     which this test would catch.
//
// The scenario then continues (bonus, non-blocking for the core assertions)
// to stage:Validate:complete and a human merge, mirroring
// TestCruiseFullPipeline, to exercise the full pipeline end-to-end.
//
// Wall-clock: ~35–55 min. Cost: ~$0.80–2.00.
func TestBaseBranchPipeline(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	stamp := time.Now().UTC().Format("20060102-150405")
	branchName := fmt.Sprintf("e2e-base-branch-%s", stamp)
	CreateThrowawayBaseBranch(t, env, env.RepoAlpha, branchName)

	// base:<branch> is a fresh, unpredictable label value every run — unlike
	// gh issue create's other flags, --label requires the label to already
	// exist in the repo (the engine's own AddLabel path creates labels on
	// demand via github.Client.ensureLabel; the gh CLI does not).
	baseLabel := "base:" + branchName
	ensureLabelExists(t, env, env.RepoAlpha, baseLabel)

	marker := fmt.Sprintf("base-branch-pipeline-%s", stamp)
	body := fmt.Sprintf(baseBranchPipelineBodyTemplate, "`", "`", "```", marker, "```", branchName)

	num := FileIssue(t, env, env.RepoAlpha,
		fmt.Sprintf("e2e base-branch pipeline (%s)", stamp),
		body, baseLabel, "fabrik:cruise")
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed %s#%d at Status=Specify with %s + fabrik:cruise, marker=%s",
		env.RepoAlpha, num, baseLabel, marker)

	// Core assertion (#1046 / #1047): the pipeline must reach end of Implement
	// without a false pause. If verifyAndHealLinkageByBody regressed to
	// trusting the always-empty GraphQL closingIssuesReferences on this
	// non-default-base PR, the linkage heal would false-fail right here.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "stage:Implement:complete", 60*time.Minute)
	AssertLabelWasNeverApplied(t, env, env.RepoAlpha, num, "fabrik:paused")
	t.Logf("%s#%d reached stage:Implement:complete without fabrik:paused — #1046/#1047 regression check passed",
		env.RepoAlpha, num)

	// Confirm Fabrik forked from / targeted the PR at the throwaway branch —
	// not a silent fallback to main.
	prNum := WaitForLinkedPR(t, env, env.RepoAlpha, num, 5*time.Minute)
	if got := PRBaseRef(t, env, env.RepoAlpha, prNum); got != branchName {
		t.Fatalf("PR #%d base ref = %q, want %q (fell back to default branch?)", prNum, got, branchName)
	}
	t.Logf("PR #%d targets %s as expected", prNum, branchName)

	// Review-gate assertion (#1050): wait for the gate to engage, then confirm
	// it clears naturally (not via timeout).
	reviewWaitTimeout := readEnvFileReviewWaitTimeout(t, env)
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 15*time.Minute)
	t.Logf("fabrik:awaiting-review appeared on %s#%d — review gate engaged", env.RepoAlpha, num)
	WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:awaiting-review",
		time.Duration(reviewWaitTimeout+10)*time.Minute)
	AssertLabelWasNeverApplied(t, env, env.RepoAlpha, num, "fabrik:paused")
	AssertLabelWasNeverApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-input")
	t.Logf("fabrik:awaiting-review cleared naturally on %s#%d (no pause, no awaiting-input) — #1050 regression check passed",
		env.RepoAlpha, num)

	// Bonus: drive the rest of the pipeline to completion, mirroring
	// TestCruiseFullPipeline.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "stage:Validate:complete", 30*time.Minute)
	t.Logf("%s#%d reached stage:Validate:complete", env.RepoAlpha, num)

	MergePR(t, env, env.RepoAlpha, prNum)
	t.Logf("merged PR #%d — waiting for engine poll to advance to Done and close issue", prNum)
	WaitForIssueClosed(t, env, env.RepoAlpha, num, 30*time.Minute)
	t.Logf("%s#%d closed after human merge — full base:<branch> pipeline verified", env.RepoAlpha, num)
}

// ensureLabelExists creates label on repo if it doesn't already exist
// (idempotent via --force). Needed because base:<branch> labels are minted
// fresh per run and gh issue create --label fails if the label is missing.
func ensureLabelExists(t *testing.T, env *Env, repo, label string) {
	t.Helper()
	if out, err := ghOutput(env, "label", "create", label, "-R", repo,
		"--color", "5319e7", "--force"); err != nil {
		t.Fatalf("ensure label %q exists on %s: %v\n%s", label, repo, err, out)
	}
}

// baseBranchPipelineBodyTemplate is the issue body for TestBaseBranchPipeline.
// The six %s placeholders are: backtick, backtick, codefence, marker,
// codefence, branch name (Go raw strings can't contain backticks).
const baseBranchPipelineBodyTemplate = `## Goal

End-to-end verification of the base:<branch> (non-default base branch)
pipeline contract — regression coverage for handarbeit/fabrik#1046,
validating the #1047 (issue<->PR linkage) and #1050 (review-gate data feed)
fixes.

## Trivial change

Append a single HTML comment line to %sREADME.md%s at the very end of the file:

%s
<!-- %s -->
%s

That is the entire change. One file, one line. Plan should NOT decompose.

## Scope

Single repo only, targeting the non-default base branch %s (already set via
the base:<branch> label applied at filing — do not add or remove labels).
`
