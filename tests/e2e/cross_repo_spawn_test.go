//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// TestCrossRepoSpawn verifies that a multi-repo issue filed against alpha
// successfully decomposes into a sub-issue in beta, the parent is gated until
// the child closes, the child flows through its own pipeline, and the parent
// resumes and completes.
//
// This is the codified version of the bootstrap test that surfaced #797/#803
// today. It is THE regression test for the cross-repo spawn machinery
// shipped in v0.0.66.
//
// Wall-clock: ~45-60 min. Cost: ~$1-2.
//
// Requires: #803 fix (on-demand spawn-target repo init) shipped in v0.0.67+.
// Will fail-via-timeout on earlier versions because the spawn aborts with
// fabrik:paused and the child is never created.
func TestCrossRepoSpawn(t *testing.T) {
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	startedAt := time.Now()
	body := fmt.Sprintf(crossRepoBodyTemplate, env.RepoBeta, env.RepoBeta, env.RepoAlpha)
	parent := FileIssue(t, env, env.RepoAlpha,
		fmt.Sprintf("e2e cross-repo spawn (%s)", startedAt.Format("15:04:05")),
		body,
		"fabrik:yolo",
	)
	itemID := AddIssueToProject(t, env, env.RepoAlpha, parent)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed parent: %s#%d", env.RepoAlpha, parent)

	// Plan should produce a spawn block. Once the engine sees it, the parent
	// gets fabrik:children-spawned and a child appears in beta.
	WaitForIssueLabel(t, env, env.RepoAlpha, parent, "fabrik:children-spawned", 20*time.Minute)
	t.Logf("parent has fabrik:children-spawned — spawn step ran")

	child := WaitForChildIssueInRepo(t, env, env.RepoBeta, startedAt, 5*time.Minute)
	t.Logf("child appeared: %s#%d", env.RepoBeta, child)
	t.Cleanup(func() { CloseIssue(env, env.RepoBeta, child) })

	// The Issue Dependencies API should show parent blockedBy child.
	AssertBlockedBy(t, env, env.RepoAlpha, parent, env.RepoBeta, child)
	t.Logf("blockedBy linkage confirmed via API")

	// Sub-issue may need a manual yolo bump until #806 lands. Do it for the test.
	labels := IssueLabels(t, env, env.RepoBeta, child)
	hasYolo := false
	for _, l := range labels {
		if l == "fabrik:yolo" {
			hasYolo = true
			break
		}
	}
	if !hasYolo {
		t.Logf("child lacks fabrik:yolo (pre-#806 behaviour) — adding manually so the test can proceed")
		AddLabel(t, env, env.RepoBeta, child, "fabrik:yolo")
	}
	// Move child to Specify if it has no status (also a #806 manual step).
	childItemID := AddIssueToProject(t, env, env.RepoBeta, child)
	SetIssueStatus(t, env, childItemID, "Specify")

	// Wait for child to close (its PR merges).
	WaitForIssueClosed(t, env, env.RepoBeta, child, 45*time.Minute)
	t.Logf("child closed — parent should resume")

	// Parent unblocks, runs Implement → Review → Validate → closes.
	WaitForIssueClosed(t, env, env.RepoAlpha, parent, 30*time.Minute)
	t.Logf("parent closed — cross-repo flow complete")
}

// crossRepoBodyTemplate is the issue body for TestCrossRepoSpawn. Written
// without raw-string backticks (Go raw strings can't contain them); markdown
// code spans use *italics* style instead. The two %s placeholders are
// (1) beta repo name, (2) alpha repo name.
const crossRepoBodyTemplate = `## Goal

Verify cross-repo decomposition works end-to-end. Plan should spawn a sub-issue in %s because the work requires changes in both repos in strict order.

## What needs to change

### In %s (the beta repo)
- Add an exported function HelloE2E() string to pkg/greeting/greeting.go returning the literal string "e2e-cross-repo-spawn".
- Add a test for it in pkg/greeting/greeting_test.go.

### In %s (this repo)
- Update main.go to call greeting.HelloE2E() (in addition to whatever it already does) and print the result.
- Run go mod tidy if needed.

## Constraint

Beta must land first because alpha cannot import a function that does not yet exist. Plan should emit a FABRIK_SPAWN_CHILD_BEGIN block targeting the beta repo to decompose.

This is an e2e regression test for #803 (on-demand spawn-target repo init).
`
