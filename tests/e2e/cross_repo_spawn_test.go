//go:build e2e

package e2e

import (
	"fmt"
	"math/rand/v2"
	"strings"
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
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	startedAt := time.Now()

	// Per-run-unique token so the fixture isn't satisfied by a prior run's
	// beta-side work (#1308). The stamp alone is only second-granularity, so
	// two runs starting in the same UTC second (a manual re-run right after a
	// failure, or racing CI shards) would collide; a random nonce is appended
	// to rule that out. idSuffix is a legal Go exported-identifier suffix
	// (digits only, from the punctuation-stripped token); sentinel is the
	// human-readable variant used as the function's returned string.
	stamp := time.Now().UTC().Format("20060102-150405")
	nonce := fmt.Sprintf("%04d", rand.IntN(10000))
	token := stamp + "-" + nonce
	idSuffix := strings.ReplaceAll(token, "-", "")
	sentinel := fmt.Sprintf("e2e-cross-repo-spawn-%s", token)

	body := fmt.Sprintf(crossRepoBodyTemplate,
		env.RepoBeta, env.RepoBeta, idSuffix, sentinel, env.RepoAlpha, idSuffix)
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
	WaitForIssueClosedWithReviewCheck(t, env, env.RepoBeta, child, 45*time.Minute)
	t.Logf("child closed — parent should resume")

	// Parent unblocks, runs Implement → Review → Validate → closes.
	WaitForIssueClosedWithReviewCheck(t, env, env.RepoAlpha, parent, 30*time.Minute)
	t.Logf("parent closed — cross-repo flow complete")

	// fabrik:children-spawned only proves Plan emitted a spawn block — it
	// doesn't prove the beta-side change was implemented correctly. Read the
	// merged beta content back and confirm the run-specific sentinel landed,
	// so the scenario still fails if the cross-repo spawn mechanism itself
	// regresses (#1308).
	content := FetchRepoFileContent(t, env, env.RepoBeta, "pkg/greeting/greeting.go")
	if !strings.Contains(content, sentinel) {
		t.Fatalf("expected sentinel %q not found in %s/pkg/greeting/greeting.go; fetched content:\n%s",
			sentinel, env.RepoBeta, content)
	}
	t.Logf("sentinel %q confirmed in %s/pkg/greeting/greeting.go — beta-side work verified", sentinel, env.RepoBeta)
}

// crossRepoBodyTemplate is the issue body for TestCrossRepoSpawn. Written
// without raw-string backticks (Go raw strings can't contain them); markdown
// code spans use *italics* style instead. The %s placeholders, in order, are
// (1) beta repo name, (2) beta repo name, (3) idSuffix (the per-run
// identifier-safe token, in the beta function name), (4) sentinel (the
// per-run human-readable token, as the returned string literal), (5) alpha
// repo name, (6) idSuffix again (the same token, in the alpha call site —
// must match (3) exactly since it's the same function).
const crossRepoBodyTemplate = `## Goal

Verify cross-repo decomposition works end-to-end. Plan should spawn a sub-issue in %s because the work requires changes in both repos in strict order.

## What needs to change

### In %s (the beta repo)
- Add an exported function HelloE2E%s() string to pkg/greeting/greeting.go returning the literal string "%s".
- Add a test for it in pkg/greeting/greeting_test.go.

### In %s (this repo)
- Update main.go to call greeting.HelloE2E%s() (in addition to whatever it already does) and print the result.
- Run go mod tidy if needed.

## Constraint

Beta must land first because alpha cannot import a function that does not yet exist. Plan should emit a FABRIK_SPAWN_CHILD_BEGIN block targeting the beta repo to decompose. Use the exact function name and string literal given above verbatim — do not invent your own; a per-run-unique token is already baked into them, and this scenario's final assertion depends on that exact text landing in the beta repo.

This is an e2e regression test for #803 (on-demand spawn-target repo init) and #1308 (per-run-unique fixture token).
`
