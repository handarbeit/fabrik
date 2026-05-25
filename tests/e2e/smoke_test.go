//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// TestSmokeSingleRepoDispatch is the minimal proof-of-life test: file an issue,
// verify a worker dispatches and Specify completes. Does NOT wait for the full
// pipeline. Always include this in any release-validation run; it's cheap and
// catches general pipeline breakage.
//
// Wall-clock: ~3-5 min. Cost: ~$0.10-0.20.
func TestSmokeSingleRepoDispatch(t *testing.T) {
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	num := FileIssue(t, env, env.RepoAlpha,
		fmt.Sprintf("e2e smoke dispatch (%s)", time.Now().UTC().Format("15:04:05")),
		`## Goal

Verify Fabrik dispatches a worker on a trivial issue.

## Trivial change

This is a dispatch smoke test. The test framework will close this issue before any actual work happens. Specify should just acknowledge the issue and signal complete; no Research/Plan/Implement is expected.

If you (the Specify agent) are reading this, the simplest spec is: "This is a smoke-test issue. No implementation required. Emit FABRIK_NO_WORK_NEEDED to short-circuit." That keeps cost minimal.`,
		"fabrik:yolo",
	)
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed %s#%d at Status=Specify", env.RepoAlpha, num)

	// Within 5 minutes Fabrik should have advanced past Specify.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "stage:Specify:complete", 5*time.Minute)
	t.Logf("Specify complete on %s#%d — dispatch path verified", env.RepoAlpha, num)

	// Don't wait for full pipeline; the t.Cleanup from FileIssue will close it.
}

// TestSmokeSingleRepoFullPipeline runs the full single-repo end-to-end flow:
// file an issue describing a trivial code change, expect Fabrik to take it
// from Specify all the way to Done with a merged PR.
//
// This is the "is the entire pipeline working end-to-end" check, complementary
// to TestCrossRepoSpawn (which exercises the cross-repo path). Useful when
// debugging "Implement onwards is broken" classes of issue.
//
// Wall-clock: ~20-40 min. Cost: ~$0.50-1.50.
func TestSmokeSingleRepoFullPipeline(t *testing.T) {
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	stamp := time.Now().UTC().Format("20060102-150405")
	marker := fmt.Sprintf("smoke-full-pipeline-%s", stamp)
	num := FileIssue(t, env, env.RepoAlpha,
		fmt.Sprintf("e2e smoke full-pipeline (%s)", stamp),
		`## Goal

End-to-end single-repo pipeline smoke. Verify Fabrik can take an issue from Specify all the way to Done with a merged PR.

## Trivial change

Append a single comment line to `+"`README.md`"+` at the very end of the file:

`+"```"+`
<!-- `+marker+` -->
`+"```"+`

That's the entire change. One file, one line.

## Scope

Single repo only — no cross-repo work. The Plan stage should NOT decompose.`,
		"fabrik:yolo",
	)
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed %s#%d at Status=Specify, marker=%s", env.RepoAlpha, num, marker)

	// Full pipeline can take up to ~30 min depending on Claude latency.
	WaitForIssueClosed(t, env, env.RepoAlpha, num, 45*time.Minute)
	t.Logf("%s#%d closed — full single-repo pipeline verified", env.RepoAlpha, num)
}
