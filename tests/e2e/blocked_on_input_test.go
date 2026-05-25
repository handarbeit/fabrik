//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// TestBlockedOnInput verifies the FABRIK_BLOCKED_ON_INPUT pause-and-resume flow.
//
// Files an issue with deliberately ambiguous requirements. Specify should
// recognise the ambiguity, emit FABRIK_BLOCKED_ON_INPUT, and the engine should
// apply fabrik:paused + fabrik:awaiting-input. We then post a clarifying
// comment that resolves the ambiguity; Fabrik should resume the stage on the
// next poll and complete Specify (verified by stage:Specify:complete + label
// clearing).
//
// Also verifies #ed46b7fc — the awaiting-input label clear on stage complete.
//
// Wall-clock: ~10-15 min. Cost: ~$0.30-0.50.
func TestBlockedOnInput(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	startedAt := time.Now()
	num := FileIssue(t, env, env.RepoAlpha,
		fmt.Sprintf("e2e blocked-on-input (%s)", startedAt.Format("15:04:05")),
		`## Goal

Verify FABRIK_BLOCKED_ON_INPUT pause + resume.

## Deliberately ambiguous

Make the README "better".

That is all. Specify should immediately notice this is wildly under-specified and emit FABRIK_BLOCKED_ON_INPUT asking for clarification (instead of proceeding to Research with assumptions). The engine should then pause the issue with `+"`fabrik:paused`"+` and `+"`fabrik:awaiting-input`"+`.

This issue is filed by the e2e harness, which will post a clarifying comment to resume.

Regression coverage:
- #733 / FABRIK_BLOCKED_ON_INPUT marker
- ed46b7fc — fabrik:awaiting-input clears on FABRIK_STAGE_COMPLETE`,
		"fabrik:yolo",
	)
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed %s#%d at Status=Specify", env.RepoAlpha, num)

	// Wait for the pause to land.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-input", 15*time.Minute)
	t.Logf("issue paused awaiting input — emitting clarifying comment")

	// Post a resolving comment.
	CommentOnIssue(t, env, env.RepoAlpha, num,
		"Clarification: please just close this issue with FABRIK_NO_WORK_NEEDED. "+
			"The README already covers what it needs to cover and this issue was "+
			"posted by the e2e test purely to exercise the BLOCKED_ON_INPUT flow.")

	// On next poll, Fabrik should re-invoke Specify. Specify should now see
	// the comment + a clear answer + react accordingly.
	WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:awaiting-input", 15*time.Minute)
	t.Logf("fabrik:awaiting-input cleared — resume verified")

	// The issue should ultimately close (via NO_WORK_NEEDED, per our clarifying comment).
	WaitForIssueClosed(t, env, env.RepoAlpha, num, 20*time.Minute)
	t.Logf("%s#%d closed — pause/resume/short-circuit verified end-to-end", env.RepoAlpha, num)
}
