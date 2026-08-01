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
	// Skipped pending handarbeit/fabrik#916. A capable Implement-stage agent makes
	// CI green on the first push (it satisfies the sentinel during Implement), so
	// the sentinel never fails and the reinvoke loop never fires. Forcing a
	// deterministic first failure needs the engine change (surface failing-check
	// output in buildCIFixComment) + a per-run-nonce sentinel the agent cannot
	// pre-satisfy. The harness hardening below (SENTINEL_FIX body, no-workflow-edit
	// guard, sentinel-failure precondition) is retained for when #916 unblocks this.
	t.Skip("blocked on #916: agent satisfies ci-fix-sentinel during Implement; first failure not yet deterministically forceable")
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

	// Guard: the agent must NOT have modified the CI workflow. The ci-fix-sentinel
	// job is immutable test infrastructure; if the agent edited it (e.g. to relax
	// the failing condition) the whole CI-fix scenario is invalid.
	AssertPRDidNotTouchWorkflows(t, env, env.RepoAlpha, prNumber)

	// Validate fires, CI fails, engine adds fabrik:awaiting-ci.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-ci", 30*time.Minute)
	AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-ci")
	t.Logf("fabrik:awaiting-ci confirmed on %s#%d", env.RepoAlpha, num)

	// Establish the precondition for the withheld-window assertion: confirm the
	// sentinel check has GENUINELY FAILED. fabrik:awaiting-ci is applied
	// unconditionally on every wait_for_ci Validate completion (engine/stages.go)
	// — it is NOT proof that CI is failing. Without this guard, a sentinel that
	// passed (e.g. because the agent satisfied it prematurely) would let
	// stage:Validate:complete appear and be misread as a CI-gate leak.
	WaitForCheckConclusion(t, env, env.RepoAlpha, prNumber, "ci-fix-sentinel", "failure", 10*time.Minute)
	t.Logf("ci-fix-sentinel confirmed FAILED on PR #%d — withheld window is now meaningful", prNumber)

	// 2-minute withheld window: Validate must NOT complete while CI is failing.
	withheldDeadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(withheldDeadline) {
		labels, err := tryIssueLabels(env, env.RepoAlpha, num)
		if err != nil {
			t.Logf("transient error fetching labels during withheld window: %v (retrying)", err)
			pollSleep(pollBase())
			continue
		}
		for _, l := range labels {
			if l == "stage:Validate:complete" {
				t.Fatalf("stage:Validate:complete appeared during withheld window — CI gate did not hold on %s#%d",
					env.RepoAlpha, num)
			}
		}
		pollSleep(pollBase())
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

// cycleLimitMarker is the literal prefix of the engine's own CI-fix
// cycle-limit pause comment (engine/ci.go:pauseForCIFixCycleLimit). Matching
// on HasPrefix rather than Contains ensures hasEngineCycleLimitComment can
// only be satisfied by a genuine engine-authored comment, not by a
// stage-output comment where an agent quotes the marker text in prose while
// reasoning about the feature — this happened on handarbeit/fabrik-test-alpha#4049,
// where the Research stage output was the only comment containing the
// marker and the engine never posted one at all. See handarbeit/fabrik#1320.
const cycleLimitMarker = "🏭 **Fabrik — CI fix cycle limit reached**"

// ciWaitTimeoutMarker is the literal prefix of the engine's competing CI
// wait-timeout pause comment (engine/ci.go:pauseForCITimeout). Used only to
// produce a diagnostic that distinguishes "the cycle-limit path never fired
// because the fixed wait-timeout timer won the race" from "no pause
// happened at all" in TestCIFixReinvokeCycleLimit's failure message.
const ciWaitTimeoutMarker = "🏭 **Fabrik — CI wait timeout**"

// hasEngineCycleLimitComment reports whether bodies contains a genuine
// engine-authored CI-fix cycle-limit pause comment. It matches on
// strings.HasPrefix, mirroring the body-prefix convention findNewComments
// (engine/comments.go) uses to distinguish Fabrik's own output from prose —
// a strings.Contains scan would also match a stage-output comment where an
// agent quotes the marker text while reasoning about the feature.
func hasEngineCycleLimitComment(bodies []string) bool {
	for _, b := range bodies {
		if strings.HasPrefix(b, cycleLimitMarker) {
			return true
		}
	}
	return false
}

// hasEngineCIWaitTimeoutComment reports whether bodies contains a genuine
// engine-authored CI wait-timeout pause comment, using the same HasPrefix
// convention as hasEngineCycleLimitComment.
func hasEngineCIWaitTimeoutComment(bodies []string) bool {
	for _, b := range bodies {
		if strings.HasPrefix(b, ciWaitTimeoutMarker) {
			return true
		}
	}
	return false
}

// TestCIFixReinvokeCycleLimit is the negative-path regression test for the
// CI-fix reinvoke cycle limit (engine/ci.go: pauseForCIFixCycleLimit).
// The sentinel CI job is permanently failing for this issue — Claude cannot fix
// it — so the engine must exhaust MaxCiFixCycles and pause the issue.
//
// handarbeit/fabrik#1323 superseded #1320's timer-race diagnosis: the
// previous fixture never guaranteed a new commit per CI-fix reinvoke, so the
// `#958 leg 2` no-op guard (engine/catch_up_handlers.go) latched after the
// first reinvoke and CIFixCycleIncremented never advanced — the cycle-limit
// path was structurally unreachable, at any FABRIK_CI_WAIT_TIMEOUT. The
// fixture below (ciFixCycleLimitBody) instead forces an unconditional
// scratch-file commit+push on every single reinvoke, independent of any
// belief about fixability, which is the only thing the no-op guard actually
// checks (head SHA equality). With that fixed, the cycle-limit path is
// reached through genuine repeated dispatch and no bespoke timer
// coordination with FABRIK_CI_WAIT_TIMEOUT is needed — the engine default
// applies.
//
// Pass criteria:
//   - fabrik:awaiting-ci appears (CI gate fires).
//   - fabrik:paused and fabrik:awaiting-input appear (cycle limit reached).
//   - Issue remains OPEN.
//   - A genuine engine-authored "🏭 **Fabrik — CI fix cycle limit reached**"
//     comment appears on the issue (matched by prefix, not substring — see
//     hasEngineCycleLimitComment).
//   - The PR gained at least maxCycles commits beyond its pre-Validate
//     baseline, proving the cycle-limit path was reached via genuine
//     repeated dispatch rather than a single latched no-op (non-vacuous
//     per #1323's Requirements — see PRCommitCount check below).
//
// Prerequisite: "ci-fix-sentinel" must be enrolled as a required status
// check, and FABRIK_MAX_CI_FIX_CYCLES must be ≤ 3 (ideally 2) — see
// "Additional prerequisites for TestCIFixReinvoke and
// TestCIFixReinvokeCycleLimit" in tests/e2e/README.md. The test skips with
// an instructional message if either condition is not met.
//
// Manual, one-off, non-vacuous verification (not run by this test): to
// confirm the cycle-limit comment assertion above would actually fail if
// pauseForCIFixCycleLimit were broken, temporarily neutralize it locally
// (e.g. make it a no-op) and re-run this scenario against a live bed —
// expect a timeout waiting for fabrik:paused instead of a pass. This is a
// deliberate, costly (~$0.50–1.50, 30-90 min) live-bed run per #1323's Risks
// section, not something to repeat routinely.
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

	// Wait for Implement to complete, then capture baseline commit count —
	// mirrors TestCIFixReinvoke's pattern, needed here for the non-vacuous
	// commit-count check below.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "stage:Implement:complete", 60*time.Minute)
	prNumber := LinkedPRNumber(t, env, env.RepoAlpha, num)
	baseCommits := PRCommitCount(t, env, env.RepoAlpha, prNumber)
	t.Logf("PR #%d has %d commits at baseline (before Validate)", prNumber, baseCommits)

	// Guard: the agent must NOT have modified the CI workflow.
	AssertPRDidNotTouchWorkflows(t, env, env.RepoAlpha, prNumber)

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

	// The engine posts the cycle-limit message directly to the issue (not the
	// PR). hasEngineCycleLimitComment matches by HasPrefix so this can only be
	// satisfied by a genuine engine-authored comment — never by a stage-output
	// comment where an agent quotes the marker text in prose (#1320).
	cycleDeadline := time.Now().Add(5 * time.Minute)
	found := false
	var lastBodies []string
	for time.Now().Before(cycleDeadline) {
		out, err := ghOutput(env, "issue", "view", fmt.Sprint(num), "-R", env.RepoAlpha,
			"--json", "comments", "--jq", "[.comments[].body]")
		if err == nil {
			var bodies []string
			if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(out)), &bodies); jsonErr == nil {
				lastBodies = bodies
				if hasEngineCycleLimitComment(bodies) {
					found = true
					break
				}
			}
		}
		pollSleep(pollBase())
	}
	if !found {
		if hasEngineCIWaitTimeoutComment(lastBodies) {
			t.Fatalf("cycle limit comment not found on %s#%d after 5 minutes — instead the engine posted a CI "+
				"wait-timeout pause comment. This means the fixed CI-wait-timeout timer won the race against the "+
				"cycle-limit path — the bed's FABRIK_CI_WAIT_TIMEOUT may be misconfigured too low relative to how "+
				"long %d genuine CI-fix cycles actually take, or fix cycles are taking longer than expected. This "+
				"is a test timer-coordination problem, not necessarily an engine regression.",
				env.RepoAlpha, num, maxCycles)
		}
		t.Fatalf("cycle limit comment not found on %s#%d after 5 minutes, and no CI wait-timeout comment either — "+
			"the engine may not have paused at all", env.RepoAlpha, num)
	}
	t.Logf("cycle limit comment confirmed on %s#%d — CI-fix cycle limit regression guard passed", env.RepoAlpha, num)

	// Non-vacuous check (#1323): the cycle-limit path must have been reached
	// via genuine repeated dispatch, not a single no-op latch. Each CI-fix
	// reinvoke that actually dispatched pushed exactly one scratch-file
	// commit (see ciFixCycleLimitBody), so the PR must have gained at least
	// maxCycles commits beyond baseline.
	finalCommits := PRCommitCount(t, env, env.RepoAlpha, prNumber)
	if finalCommits < baseCommits+maxCycles {
		t.Fatalf("cycle-limit path was not genuinely exercised on PR #%d: expected at least %d commits "+
			"(baseCommits=%d + maxCycles=%d), got %d — this is consistent with the #958 leg 2 no-op guard "+
			"latching after a single reinvoke rather than the engine dispatching %d genuine CI-fix cycles "+
			"(handarbeit/fabrik#1323)", prNumber, baseCommits+maxCycles, baseCommits, maxCycles, finalCommits, maxCycles)
	}
	t.Logf("commit count guard passed: %d commits (base=%d + >=%d CI-fix cycles) — cycle-limit path genuinely exercised",
		finalCommits, baseCommits, maxCycles)

	// Re-check the workflow guard now that the CI-fix reinvoke commits exist.
	// The pre-Validate check above only covers the initial Implement commit;
	// this fixture's whole point is to force genuine repeated dispatch, so
	// the commits actually at risk of violating the "never touch .github/"
	// hard constraint are the ones produced during the cycle-limit loop, not
	// the baseline one.
	AssertPRDidNotTouchWorkflows(t, env, env.RepoAlpha, prNumber)
}

// ciFixReinvokeBody is the issue body for TestCIFixReinvoke. The PR body must
// contain "ci-fix-sentinel-required" so the test-alpha CI workflow triggers the
// ci-fix-sentinel check. That check FAILS until a file named SENTINEL_FIX
// (at the repo root) contains the line "ci-fix-satisfied" — the only sanctioned
// way to satisfy it. Claude must create that file ONLY on the CI-fix reinvoke,
// producing the failure → reinvoke → recovery sequence the test exercises.
//
// The SENTINEL_FIX mechanism is deliberately the sole satisfaction path and the
// workflow is off-limits (see hard constraints) so the agent cannot satisfy or
// weaken the sentinel prematurely — the historical failure mode that made the
// sentinel pass before the CI-fix loop ever ran.
const ciFixReinvokeBody = `## Goal

End-to-end regression test for the Fabrik CI-fix reinvoke loop (handarbeit/fabrik#900).

## The change

On the **initial Implement commit**, add exactly one new HTML comment to
README.md, on its own line, immediately after the line containing
"# fabrik-test-alpha":

    <!-- ci-fix-reinvoke-initial -->

Make NO other changes on this commit. In particular do NOT create the
SENTINEL_FIX file yet. The ci-fix-sentinel check WILL fail on this first push —
that failure is expected and required.

**When the CI-fix reinvoke fires** (you will be prompted with a message about
the CI failure), and ONLY then, push a second commit that creates a new file
named exactly ` + "`SENTINEL_FIX`" + ` at the repository root, containing exactly this
single line:

    ci-fix-satisfied

That second commit makes the sentinel pass. Do not rebase or squash — the two
commits must remain distinct so the e2e test can verify the commit count.

## Hard constraints (do NOT violate)

- NEVER modify any file under ` + "`.github/`" + `. The CI workflow — including the
  ci-fix-sentinel job — is immutable test infrastructure. Editing it invalidates
  the test and will fail the run.
- Do NOT create the SENTINEL_FIX file on the initial commit; create it ONLY on
  the CI-fix reinvoke.
- The PR body MUST contain the literal marker ` + "`ci-fix-sentinel-required`" + ` so the
  sentinel fires.

## CI behaviour required

The PR body MUST carry the literal marker below so the test repo's CI sentinel
check fires (and initially fails):

ci-fix-sentinel-required

## Scope

README.md on the initial commit; a new SENTINEL_FIX file on the CI-fix reinvoke.
No other files. No decomposition. One commit on the initial push, one CI-fix
commit.
`

// ciFixCycleLimitBody is the issue body for TestCIFixReinvokeCycleLimit. The
// PR body must contain "ci-fix-sentinel-unfixable" so the test-alpha CI
// workflow runs a permanently-failing sentinel check regardless of content.
//
// The sentinel cannot be satisfied by any commit content, so this fixture's
// job is not to fix CI — it is to guarantee a NEW commit on every single
// CI-fix reinvoke, unconditionally, independent of whatever the agent
// believes about fixability. The `#958 leg 2` no-op guard
// (engine/catch_up_handlers.go) latches whenever a CI-fix reinvoke completes
// without changing the PR's head SHA — which is exactly what the previous
// "make your best effort" wording invited (handarbeit/fabrik#1323: the
// no-op guard latched after the first reinvoke and the cycle-limit path was
// structurally unreachable). Appending a line to a scratch file and pushing
// is a mechanically verifiable action, not a judgment call, so
// headAfter != headBefore holds every round and CIFixCycleIncremented
// genuinely advances to MaxCiFixCycles.
const ciFixCycleLimitBody = `## Goal

End-to-end regression test for the Fabrik CI-fix cycle limit
(handarbeit/fabrik#900, handarbeit/fabrik#1323). This issue is designed to
exhaust MaxCiFixCycles by running an unfixable CI check while forcing a new
commit on every single CI-fix reinvoke.

## The change

On the initial commit, add exactly one new HTML comment to README.md, on its
own line, immediately after the line containing "# fabrik-test-alpha":

    <!-- ci-fix-cycle-limit-test -->

Also create a new file named exactly ` + "`.ci-fix-attempts.log`" + ` at the
repository root, containing exactly one line:

    attempt-1

## MANDATORY action on EVERY CI-fix reinvoke (no exceptions)

The CI sentinel check on this PR is PERMANENTLY UNFIXABLE — no code change,
file edit, or workaround can make it pass. Do NOT attempt to diagnose or fix
it, and do NOT decide there is "nothing to do" and skip pushing.

Instead, on EVERY CI-fix reinvoke, unconditionally:

1. Append exactly one new line to ` + "`.ci-fix-attempts.log`" + ` at the
   repository root, reading ` + "`attempt-N`" + ` where N is one greater than
   the highest attempt number already in the file (e.g. if the file's last
   line is ` + "`attempt-1`" + `, append ` + "`attempt-2`" + `).
2. Commit that change and push it.

This applies to the first CI-fix reinvoke and every reinvoke after it, with
no exceptions and regardless of any belief about whether the check is
fixable — the point of this test is that it is not. A reinvoke that pushes
no commit defeats the test.

## Hard constraints (do NOT violate)

- NEVER modify any file under ` + "`.github/`" + `. The CI workflow — including the
  ci-fix-sentinel job — is immutable test infrastructure. It is intentionally
  unfixable here; editing it invalidates the test.
- NEVER remove or alter the CI sentinel marker in the PR body.
- On every CI-fix reinvoke, ALWAYS append to ` + "`.ci-fix-attempts.log`" + ` and
  push, even though the check cannot be fixed. Skipping the push is the one
  way to invalidate this test.

## CI behaviour required

The PR body MUST carry the literal marker below so the test repo's CI
permanently-failing sentinel fires:

ci-fix-sentinel-unfixable

## Scope

README.md and ` + "`.ci-fix-attempts.log`" + ` only. Minimal change per commit. No
decomposition.
`
