//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCIFixReinvoke is the positive-path regression test for the CI-fix
// reinvoke loop (engine/ci.go). The sentinel CI job "ci-fix-sentinel" in
// handarbeit/fabrik-test-alpha fails on the first push but passes after a
// trivial fix commit — exercising the full failure → reinvoke → recovery path.
//
// Pass criteria:
//   - fabrik:awaiting-ci appears after Validate (CI gate holds).
//   - stage:Validate:complete does NOT appear during the 2-minute withheld window.
//   - A second "🏭 **Fabrik — stage: Validate**" PR comment appears (reinvoke fired).
//   - CI eventually passes and the issue closes.
//   - The PR has exactly baseCommits+1 commits (one CI-fix commit; no rebase storm).
//
// Prerequisite: "ci-fix-sentinel" must be enrolled as a required status check
// on handarbeit/fabrik-test-alpha/main. The test skips gracefully if not.
//
// Wall-clock: ~75–90 min when run in isolation. Use E2E_TIMEOUT=3h.
// Cost: ~$1–3.
func TestCIFixReinvoke(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	assertSentinelCheckRequired(t, env, env.RepoAlpha)

	stamp := time.Now().UTC().Format("20060102-150405")
	title := fmt.Sprintf("e2e ci-fix-reinvoke (%s)", stamp)

	num := FileIssue(t, env, env.RepoAlpha, title, ciFixReinvokeBody, "fabrik:yolo")
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed %s#%d", env.RepoAlpha, num)

	// Wait for Implement to complete, then capture baseline commit count.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "stage:Implement:complete", 60*time.Minute)
	prNumber := LinkedPRNumber(t, env, env.RepoAlpha, num)
	baseCommits := PRCommitCount(t, env, env.RepoAlpha, prNumber)
	t.Logf("PR #%d has %d commits at baseline (before Validate)", prNumber, baseCommits)

	// Validate fires, CI fails, engine adds fabrik:awaiting-ci.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-ci", 30*time.Minute)
	AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-ci")
	t.Logf("fabrik:awaiting-ci confirmed on %s#%d", env.RepoAlpha, num)

	// 2-minute withheld window: Validate must NOT complete while CI is failing.
	withheldDeadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(withheldDeadline) {
		labels, err := tryIssueLabels(env, env.RepoAlpha, num)
		if err != nil {
			t.Logf("transient error fetching labels during withheld window: %v (retrying)", err)
			time.Sleep(15 * time.Second)
			continue
		}
		for _, l := range labels {
			if l == "stage:Validate:complete" {
				t.Fatalf("stage:Validate:complete appeared during withheld window — CI gate did not hold on %s#%d",
					env.RepoAlpha, num)
			}
		}
		time.Sleep(15 * time.Second)
	}
	t.Logf("withheld window passed: CI gate held for 2 minutes on %s#%d", env.RepoAlpha, num)

	// A second Validate comment on the PR confirms the CI-fix reinvoke fired.
	// (The first "🏭 **Fabrik — stage: Validate**" comment is from the initial
	// Validate run; the reinvoke posts a second one.)
	WaitForPRCommentContaining(t, env, env.RepoAlpha, prNumber,
		"🏭 **Fabrik — stage: Validate**", 10*time.Minute)
	t.Logf("CI-fix reinvoke confirmed via second Validate PR comment on #%d", prNumber)

	// Poll until all CI check conclusions are "success" (pending = still running).
	ciDeadline := time.Now().Add(20 * time.Minute)
	ciPassed := false
	for time.Now().Before(ciDeadline) {
		conclusions, err := tryPRCheckRunConclusions(env, env.RepoAlpha, prNumber)
		if err != nil {
			time.Sleep(30 * time.Second)
			continue
		}
		if len(conclusions) > 0 {
			allSuccess := true
			for _, c := range conclusions {
				if c != "success" {
					allSuccess = false
					break
				}
			}
			if allSuccess {
				t.Logf("all CI checks passed on PR #%d: %v", prNumber, conclusions)
				ciPassed = true
				break
			}
		}
		time.Sleep(30 * time.Second)
	}
	if !ciPassed {
		t.Fatalf("timed out waiting for all CI checks to pass on PR #%d", prNumber)
	}

	// Issue must close (Validate completes and yolo auto-merges the PR).
	WaitForIssueClosed(t, env, env.RepoAlpha, num, 30*time.Minute)
	AssertLabelWasApplied(t, env, env.RepoAlpha, num, "stage:Validate:complete")
	t.Logf("%s#%d closed with stage:Validate:complete", env.RepoAlpha, num)

	// Rebase storm guard: expect exactly one CI-fix commit beyond baseline.
	finalCommits := PRCommitCount(t, env, env.RepoAlpha, prNumber)
	if finalCommits > baseCommits+1 {
		t.Fatalf("rebase storm on PR #%d: expected %d commits, got %d",
			prNumber, baseCommits+1, finalCommits)
	}
	if finalCommits != baseCommits+1 {
		t.Fatalf("expected exactly 1 CI-fix commit on PR #%d: baseCommits=%d, finalCommits=%d",
			prNumber, baseCommits, finalCommits)
	}
	t.Logf("commit count guard passed: %d commits (base=%d + 1 CI-fix)", finalCommits, baseCommits)
}

// TestCIFixReinvokeCycleLimit is the negative-path regression test for the
// CI-fix reinvoke cycle limit (engine/ci.go: pauseForCIFixCycleLimit).
// The sentinel CI job is permanently failing for this issue — Claude cannot fix
// it — so the engine must exhaust MaxCiFixCycles and pause the issue.
//
// Pass criteria:
//   - fabrik:awaiting-ci appears (CI gate fires).
//   - fabrik:paused and fabrik:awaiting-input appear (cycle limit reached).
//   - Issue remains OPEN.
//   - A "🏭 **Fabrik — CI fix cycle limit reached**" comment appears on the issue.
//
// Prerequisite: "ci-fix-sentinel" must be enrolled as a required status check
// AND FABRIK_MAX_CI_FIX_CYCLES must be ≤ 3 (ideally 2) in the test bed .env.
// The test skips with an instructional message if either condition is not met.
//
// Wall-clock: ~30–60 min. Cost: ~$0.50–1.50.
func TestCIFixReinvokeCycleLimit(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	assertSentinelCheckRequired(t, env, env.RepoAlpha)

	maxCycles := readEnvFileMaxCiFixCycles(t, env)
	if maxCycles > 3 {
		t.Skipf("FABRIK_MAX_CI_FIX_CYCLES=%d in test bed .env (must be ≤3 for this test — set to 2 and restart Fabrik)", maxCycles)
	}
	t.Logf("FABRIK_MAX_CI_FIX_CYCLES=%d — cycle limit test will fire after %d failed attempts", maxCycles, maxCycles)

	stamp := time.Now().UTC().Format("20060102-150405")
	title := fmt.Sprintf("e2e ci-fix-cycle-limit (%s)", stamp)

	num := FileIssue(t, env, env.RepoAlpha, title, ciFixCycleLimitBody, "fabrik:yolo")
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed %s#%d", env.RepoAlpha, num)

	// CI gate fires, then engine exhausts reinvoke cycles and pauses.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-ci", 90*time.Minute)
	t.Logf("fabrik:awaiting-ci appeared on %s#%d", env.RepoAlpha, num)

	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:paused", 90*time.Minute)
	t.Logf("fabrik:paused appeared on %s#%d (cycle limit reached)", env.RepoAlpha, num)

	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-input", 5*time.Minute)
	t.Logf("fabrik:awaiting-input appeared on %s#%d", env.RepoAlpha, num)

	if state := IssueState(t, env, env.RepoAlpha, num); state != "OPEN" {
		t.Fatalf("expected issue OPEN after cycle limit, got %s", state)
	}

	// The engine posts the cycle-limit message directly to the issue (not the PR).
	cycleDeadline := time.Now().Add(5 * time.Minute)
	found := false
	for time.Now().Before(cycleDeadline) {
		out, err := ghOutput(env, "issue", "view", fmt.Sprint(num), "-R", env.RepoAlpha,
			"--json", "comments", "--jq", "[.comments[].body]")
		if err == nil {
			var bodies []string
			if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(out)), &bodies); jsonErr == nil {
				for _, b := range bodies {
					if strings.Contains(b, "🏭 **Fabrik — CI fix cycle limit reached**") {
						found = true
						break
					}
				}
			}
		}
		if found {
			break
		}
		time.Sleep(15 * time.Second)
	}
	if !found {
		t.Fatalf("cycle limit comment not found on %s#%d after 5 minutes", env.RepoAlpha, num)
	}
	t.Logf("cycle limit comment confirmed on %s#%d — CI-fix cycle limit regression guard passed", env.RepoAlpha, num)
}

// ciFixReinvokeBody is the issue body for TestCIFixReinvoke. The PR body must
// contain "ci-fix-sentinel-required" so the test-alpha CI workflow triggers the
// sentinel check (which fails on the first push, requiring a fix commit).
//
// Claude is instructed to make a deterministic, reversible change to README.md
// on the initial push (which will fail the sentinel), then push a second commit
// that satisfies the sentinel condition when reinvoked.
const ciFixReinvokeBody = `## Goal

End-to-end regression test for the Fabrik CI-fix reinvoke loop (handarbeit/fabrik#900).

## The change

Add exactly one new HTML comment to README.md, on its own line, immediately
after the line containing "# fabrik-test-alpha". The comment must be:

    <!-- ci-fix-reinvoke-initial -->

**On the initial Implement commit**: add only the HTML comment above. Do NOT
make any other changes. The CI sentinel will fail on this commit.

**When the CI-fix reinvoke fires** (you will be prompted with a message about
CI failure): push a second commit that adds exactly one more line immediately
below the first comment:

    <!-- ci-fix-sentinel-satisfied -->

This second commit makes the sentinel pass. Do not rebase or squash — the
two commits must remain distinct so the e2e test can verify the commit count.

## CI behaviour required

The PR body MUST carry the literal marker below so the test repo's CI sentinel
check fires (and initially fails):

ci-fix-sentinel-required

## Scope

Single file (README.md). No other changes. No decomposition. Plan and Implement
should be minimal — one commit on the initial push, one CI-fix commit.
`

// ciFixCycleLimitBody is the issue body for TestCIFixReinvokeCycleLimit. The
// PR body must contain "ci-fix-sentinel-unfixable" so the test-alpha CI
// workflow runs a permanently-failing sentinel check regardless of content.
// Claude will attempt to fix CI on each reinvoke but cannot succeed.
const ciFixCycleLimitBody = `## Goal

End-to-end regression test for the Fabrik CI-fix cycle limit
(handarbeit/fabrik#900). This issue is designed to exhaust MaxCiFixCycles
by running an unfixable CI check.

## The change

Add exactly one new HTML comment to README.md, on its own line, immediately
after the line containing "# fabrik-test-alpha". The comment must be:

    <!-- ci-fix-cycle-limit-test -->

This is the only change needed. Do NOT attempt to remove or alter the CI
sentinel marker in the PR body — the sentinel is intentionally unfixable
at the code level. Make your best effort on each CI-fix reinvoke, but the
test expects the cycle limit to be reached.

## CI behaviour required

The PR body MUST carry the literal marker below so the test repo's CI
permanently-failing sentinel fires:

ci-fix-sentinel-unfixable

## Scope

Single file (README.md). Minimal change. No decomposition.
`
