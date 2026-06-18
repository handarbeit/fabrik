//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCruiseFullPipeline is the e2e regression test for the fabrik:cruise
// pipeline contract.
//
// fabrik:cruise auto-advances through all stages (Specify → Research → Plan →
// Implement → Review → Validate) without merging the PR or advancing to Done.
// When a human merges the PR, the engine detects the terminal state and closes
// the issue / advances the board to Done.
//
// What this test verifies (R1–R5 from issue #898):
//   - R1: fabrik:cruise auto-advances to stage:Validate:complete without
//     any manual column moves.
//   - R2: at Validate-complete, the linked PR is non-draft (ready) and OPEN
//     (not merged or closed).
//   - R3: fabrik:auto-merge-enabled was never applied at any point — cruise
//     suppresses the auto-merge path entirely.
//   - R4: the issue is still OPEN and no stage:Done:* labels are present at
//     Validate-complete (proxy for "board column = Validate").
//   - R5: after a human merges the PR, the issue closes and the board
//     advances to Done.
//
// Prerequisites: handarbeit/fabrik-test-alpha must have the fabrik:cruise
// label seeded (it is a production label and should always exist).
//
// Wall-clock: ~30–50 min. Cost: ~$0.80–2.00.
func TestCruiseFullPipeline(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	stamp := time.Now().UTC().Format("20060102-150405")
	marker := fmt.Sprintf("cruise-pipeline-%s", stamp)
	body := fmt.Sprintf(cruiseBodyTemplate, "`", "`", "```", marker, "```")

	num := FileIssue(t, env, env.RepoAlpha,
		fmt.Sprintf("e2e cruise full pipeline (%s)", stamp),
		body, "fabrik:cruise")
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed %s#%d at Status=Specify with fabrik:cruise, marker=%s", env.RepoAlpha, num, marker)

	// R1: engine auto-advances through all stages to Validate-complete.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "stage:Validate:complete", 45*time.Minute)
	t.Logf("%s#%d reached stage:Validate:complete — checking cruise contract", env.RepoAlpha, num)

	// Discover the linked PR number. The PR is created during Implement; by
	// the time Validate completes it is guaranteed to exist.
	prNum := WaitForLinkedPR(t, env, env.RepoAlpha, num, 5*time.Minute)
	t.Logf("linked PR: #%d", prNum)

	// R2: PR must be non-draft (Implement marks it ready) and OPEN (not merged).
	prOut, err := ghOutput(env, "pr", "view", fmt.Sprint(prNum), "-R", env.RepoAlpha,
		"--json", "isDraft,state", "--jq", `{"isDraft":.isDraft,"state":.state}`)
	if err != nil {
		t.Fatalf("gh pr view %d in %s: %v\n%s", prNum, env.RepoAlpha, err, prOut)
	}
	var prState struct {
		IsDraft bool   `json:"isDraft"`
		State   string `json:"state"`
	}
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(prOut)), &prState); jsonErr != nil {
		t.Fatalf("parse pr state JSON: %v (raw: %s)", jsonErr, prOut)
	}
	if prState.IsDraft {
		t.Fatalf("PR #%d is still a draft at Validate-complete (expected non-draft) — R2", prNum)
	}
	if prState.State != "OPEN" {
		t.Fatalf("PR #%d is %q at Validate-complete (expected OPEN) — R2", prNum, prState.State)
	}
	t.Logf("PR #%d: isDraft=false, state=OPEN — R2 verified", prNum)

	// R3: fabrik:auto-merge-enabled must never have been applied. Cruise
	// suppresses the auto-merge path; the engine must not call
	// enablePullRequestAutoMerge for cruise items.
	AssertLabelWasNeverApplied(t, env, env.RepoAlpha, num, "fabrik:auto-merge-enabled")
	t.Logf("fabrik:auto-merge-enabled was never applied — cruise suppresses auto-merge — R3 verified")

	// R4 (proxy): issue is OPEN, no stage:Done:* labels present. The engine
	// advances label + board column atomically; absence of Done labels means
	// the board column is still Validate.
	if state := IssueState(t, env, env.RepoAlpha, num); state != "OPEN" {
		t.Fatalf("issue %s#%d is %q at Validate-complete (expected OPEN) — R4", env.RepoAlpha, num, state)
	}
	for _, l := range IssueLabels(t, env, env.RepoAlpha, num) {
		if strings.HasPrefix(l, "stage:Done:") {
			t.Fatalf("found %q label at Validate-complete — engine advanced to Done prematurely — R4", l)
		}
	}
	t.Logf("issue is OPEN, no stage:Done:* labels — board is Validate — R4 verified")

	// Merge the PR as a human (the action cruise is waiting for).
	MergePR(t, env, env.RepoAlpha, prNum)
	t.Logf("merged PR #%d — waiting for engine poll to advance to Done and close issue", prNum)

	// R5: after the human merge, the engine detects the terminal PR state,
	// advances the board to Done, and the issue closes (via GitHub's
	// Closes #N auto-close on merge).
	WaitForIssueClosed(t, env, env.RepoAlpha, num, 45*time.Minute)
	t.Logf("%s#%d closed after human merge — R5 verified (full cruise contract confirmed)", env.RepoAlpha, num)
}

// cruiseBodyTemplate is the issue body for TestCruiseFullPipeline. The five
// %s placeholders are: backtick, backtick, codefence, marker, codefence
// (Go raw strings can't contain backticks).
const cruiseBodyTemplate = `## Goal

End-to-end verification of the fabrik:cruise pipeline contract (#898).

## Trivial change

Append a single HTML comment line to %sREADME.md%s at the very end of the file:

%s
<!-- %s -->
%s

That is the entire change. One file, one line. Plan should NOT decompose.

## Scope

Single repo only. This issue verifies that cruise auto-advances through all
stages to Validate-complete without merging the PR, and that the issue closes
correctly after a human merges the PR.
`
