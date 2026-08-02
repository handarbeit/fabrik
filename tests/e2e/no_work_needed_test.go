//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestNoWorkNeeded verifies the FABRIK_NO_WORK_NEEDED short-circuit.
//
// Files an issue describing work that is already done. Plan should determine
// nothing actually needs implementing, emit FABRIK_NO_WORK_NEEDED, and the
// engine should mark all downstream stages complete and CLOSE the issue
// (per #742) — without ever opening a PR.
//
// This is the regression test for #733 (the marker itself) and #742 (the
// close-on-no-work fix).
//
// handarbeit/fabrik#1355 (R2): WaitForIssueClosed alone cannot distinguish
// the no-work-needed path from an ordinary close — a broken
// settleNoWorkNeeded that somehow still closed the issue some other way
// would pass this test just as easily as a correctly-working one. The
// assertion below additionally requires the engine's own no-work-needed
// skip comment (noWorkNeededSkipComment, engine/no_work_needed_settle.go),
// matched by body prefix via hasNoWorkNeededSkipComment
// (no_work_needed_marker_test.go) — an observable only settleNoWorkNeeded
// produces — closing the vacuity gap. settleNoWorkNeeded posts this comment
// before it moves the item to Done and closes the issue, so once
// WaitForIssueClosed returns, the comment is guaranteed to already exist if
// the no-work-needed path actually ran; no polling race is needed.
//
// Wall-clock: ~10-15 min. Cost: ~$0.30-0.50.
func TestNoWorkNeeded(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	startedAt := time.Now()
	num := FileIssue(t, env, env.RepoAlpha,
		fmt.Sprintf("e2e no-work-needed (%s)", startedAt.Format("15:04:05")),
		`## Goal

Verify that the FABRIK_NO_WORK_NEEDED short-circuit closes the issue without creating a PR.

## What this asks for

`+"`README.md` in this repo already exists and contains a description of the test bed. Please verify that the README.md file is present, contains the substring \"`+`fabrik-test-alpha`+`\", and has the Apache 2.0 LICENSE pointer."+`

## Hint to Plan

This issue is purely a verification request. There is no code change to make. The Plan stage should recognise that and emit FABRIK_NO_WORK_NEEDED on a line of its own (instead of decomposing or proceeding to Implement).

This is an e2e regression test for #733 (the marker) and #742 (close-on-no-work).`,
		"fabrik:yolo",
	)
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed %s#%d at Status=Specify", env.RepoAlpha, num)

	// Should close within Plan + a few minutes for downstream stage cascade.
	WaitForIssueClosed(t, env, env.RepoAlpha, num, 20*time.Minute)
	t.Logf("%s#%d closed without PR — short-circuit verified", env.RepoAlpha, num)

	// The engine's own no-work-needed skip comment must be present, matched
	// by body prefix — the discriminating assertion for R2 (see doc comment
	// above). settleNoWorkNeeded posts it before closing, so it is already
	// there by the time WaitForIssueClosed returns; a single fetch suffices.
	out, err := ghOutput(env, "issue", "view", fmt.Sprint(num), "-R", env.RepoAlpha,
		"--json", "comments", "--jq", "[.comments[].body]")
	if err != nil {
		t.Fatalf("could not fetch comments on %s#%d: %v", env.RepoAlpha, num, err)
	}
	var bodies []string
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(out)), &bodies); jsonErr != nil {
		t.Fatalf("could not parse comments on %s#%d: %v (raw: %s)", env.RepoAlpha, num, jsonErr, out)
	}
	if !hasNoWorkNeededSkipComment(bodies) {
		t.Fatalf("no engine-authored no-work-needed skip comment (prefix %q) found on %s#%d — "+
			"the issue closed, but not provably via the no-work-needed path",
			noWorkNeededSkipCommentPrefix, env.RepoAlpha, num)
	}
	t.Logf("no-work-needed skip comment confirmed on %s#%d — no-work-needed path genuinely exercised", env.RepoAlpha, num)

	// Spot-check: should NOT have any open PR referencing this issue.
	out, err = ghOutput(env, "pr", "list", "-R", env.RepoAlpha, "--state", "open",
		"--search", fmt.Sprintf("in:body \"Closes #%d\"", num),
		"--json", "number", "--jq", "length")
	if err != nil {
		t.Logf("could not query PRs (non-fatal): %v", err)
	} else if out != "" && out != "0\n" {
		t.Errorf("expected 0 open PRs referencing #%d, got: %s", num, out)
	}
}
