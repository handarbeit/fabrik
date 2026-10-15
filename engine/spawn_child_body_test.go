package engine

import (
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// The #1784 fix is guidance to the spawning stage (the plan/review/validate
// skills), not an engine change: the engine passes a block's body through
// verbatim and cannot know which artifacts exist only on the parent's branch.
// These tests pin that contract for the reported shape — a mid-flight spawn
// while the parent has an open PR — and for every origin's independent spawn,
// so the guidance stays the only control and an independent spawn is
// unchanged (AC4).

const branchOnlySpecBody = "Add retry handling to the client.\n\n" +
	"Context (in flight, not required): the parent's open PR adds internal/retry/policy.go.\n\n" +
	"## Acceptance\n- The client retries on 503 using only what exists on the base branch."

func spawnBodyStages(origin string) []*stages.Stage {
	if origin == "Validate" {
		return validateStages()
	}
	return reviewStages()
}

func runMidflightSpawn(t *testing.T, origin string, output string, client *mockGitHubClient) {
	t.Helper()
	skipIfNoGit(t)
	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })

	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return output, true, TokenUsage{TurnsUsed: 3, MaxTurns: 30}, nil
		},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, spawnBodyStages(origin))
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{ID: "I_parent", Number: 102, Title: "Parent with open PR", Status: origin, ItemID: "PVTI_102"}
	if err := eng.processItem(t.Context(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
}

// TestMidflightSpawn_ChildBodyIsStageTextVerbatim: for a Review- or
// Validate-origin spawn while the parent has an open PR, the child body is
// exactly the stage's text plus the fixed footer — no engine-added notice —
// and the child is linked as a blocker of the parent. That second fact is why
// a child written to depend on its parent's unmerged PR could never complete.
func TestMidflightSpawn_ChildBodyIsStageTextVerbatim(t *testing.T) {
	for _, origin := range []string{"Review", "Validate"} {
		t.Run(origin, func(t *testing.T) {
			client := midflightSpawnClient("I_child1", 201, 42)
			output := "Found a blocker.\n" +
				"FABRIK_SPAWN_CHILD_BEGIN owner/repo\n" +
				"TITLE: Add retry handling\n" +
				branchOnlySpecBody + "\n" +
				"FABRIK_SPAWN_CHILD_END\n"
			if origin == "Review" {
				output += "FABRIK_STAGE_COMPLETE\n"
			}
			runMidflightSpawn(t, origin, output, client)

			if len(client.createIssueCalls) != 1 {
				t.Fatalf("expected 1 CreateIssue call, got %d", len(client.createIssueCalls))
			}
			want := branchOnlySpecBody + childFooter("owner", "repo", 102)
			if got := client.createIssueCalls[0].body; strings.TrimSpace(got) != strings.TrimSpace(want) {
				t.Errorf("child body must be the stage text + footer verbatim.\n got: %q\nwant: %q", got, want)
			}
			if len(client.addBlockedByIssueCalls) != 1 ||
				client.addBlockedByIssueCalls[0].issueNodeID != "I_parent" ||
				client.addBlockedByIssueCalls[0].blockerNodeID != "I_child1" {
				t.Errorf("expected the child to block the parent, got %v", client.addBlockedByIssueCalls)
			}
		})
	}
}

// TestMidflightSpawn_DependsOnWiresSiblingEdge: DEPENDS_ON works for Review and
// Validate blocks (shared parser and spawnChildren), which is what the skills
// now document. Block 2 waits on block 1; both block the parent.
func TestMidflightSpawn_DependsOnWiresSiblingEdge(t *testing.T) {
	for _, origin := range []string{"Review", "Validate"} {
		t.Run(origin, func(t *testing.T) {
			client := midflightSpawnClient("", 0, 42)
			n := 200
			client.createIssueFn = func(owner, repo, title, body string, assignees []string) (int, string, error) {
				n++
				return n, "I_child" + title[len(title)-1:], nil
			}
			output := "Two blockers.\n" +
				"FABRIK_SPAWN_CHILD_BEGIN owner/repo\nTITLE: First 1\nDo first.\nFABRIK_SPAWN_CHILD_END\n" +
				"FABRIK_SPAWN_CHILD_BEGIN owner/repo\nTITLE: Second 2\nDEPENDS_ON: 1\nDo second.\nFABRIK_SPAWN_CHILD_END\n"
			if origin == "Review" {
				output += "FABRIK_STAGE_COMPLETE\n"
			}
			runMidflightSpawn(t, origin, output, client)

			if len(client.createIssueCalls) != 2 {
				t.Fatalf("expected 2 CreateIssue calls, got %d", len(client.createIssueCalls))
			}
			var sibling bool
			for _, c := range client.addBlockedByIssueCalls {
				if c.issueNodeID == "I_child2" && c.blockerNodeID == "I_child1" {
					sibling = true
				}
			}
			if !sibling {
				t.Errorf("expected sibling edge child2 blockedBy child1, got %v", client.addBlockedByIssueCalls)
			}
		})
	}
}
