package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// Tests for #2612 (ADR 2612): the fabrik:spawned-child markers are read before
// the servability guard, so a child created by hand in a repo this instance does
// not serve resumes the parent instead of pausing it for ever. They use the
// repo-scoped testEngine (owner/repo), never spawnTestEngine, which widens the
// engine to multi-repo and would serve every target.

const markerFirstBlock = `
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Cross-repo child created by hand
Body.
FABRIK_SPAWN_CHILD_END
`

// markerFirstEngine builds a repo-scoped engine whose client resolves the
// hand-made child owner/child#101 (closed when closed is true) and whose live
// re-read of the parent reports blockedBy.
func markerFirstEngine(t *testing.T, closed bool, blockedBy []gh.Dependency) (*Engine, *mockGitHubClient) {
	t.Helper()
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			t.Errorf("CreateIssue(%s/%s) must not be called for a marked block", owner, repo)
			return 0, "", fmt.Errorf("unexpected CreateIssue")
		},
		fetchProjectItemFn: func(owner, repo string, n int) (*gh.ProjectItem, error) {
			if owner == "owner" && repo == "child" && n == 101 {
				return &gh.ProjectItem{ID: "I_child101", Number: 101, Repo: "owner/child", IsClosed: closed}, nil
			}
			return nil, fmt.Errorf("not found: %s/%s#%d", owner, repo, n)
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.BlockedBy = blockedBy
			return nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.worktreeManagers["owner/repo"] = NewWorktreeManager(t.TempDir())
	return eng, client
}

func hasLabelCall(c *mockGitHubClient, label string) bool {
	for _, l := range c.addLabelCalls {
		if l.labelName == label {
			return true
		}
	}
	return false
}

// assertNoWriteToUnservedRepo fails on any write against owner/child or this
// instance's board for the marked child (FR-002, SC-005).
func assertNoWriteToUnservedRepo(t *testing.T, c *mockGitHubClient) {
	t.Helper()
	if len(c.createIssueCalls) != 0 {
		t.Errorf("CreateIssue calls = %d, want 0", len(c.createIssueCalls))
	}
	if len(c.addProjectV2ItemCalls) != 0 {
		t.Errorf("AddProjectV2ItemById calls = %d, want 0", len(c.addProjectV2ItemCalls))
	}
	if len(c.lookupIssueProjectItemCalls) != 0 {
		t.Errorf("LookupIssueProjectItem calls = %d, want 0", len(c.lookupIssueProjectItemCalls))
	}
	if len(c.updateStatusCalls) != 0 {
		t.Errorf("UpdateProjectItemStatus calls = %d, want 0", len(c.updateStatusCalls))
	}
	for _, l := range c.addLabelCalls {
		if l.owner == "owner" && l.repo == "child" {
			t.Errorf("label %q written to the unserved repo", l.labelName)
		}
	}
}

func TestSpawnChildren_MarkedCrossRepoChild_ResumesWithoutPause(t *testing.T) {
	edge := []gh.Dependency{{Repo: "owner/child", Number: 101, State: "CLOSED"}}
	eng, client := markerFirstEngine(t, true, edge)

	item := planItemWithBlocks(markerFirstBlock)
	item.Labels = append(item.Labels, spawnChildLabel(1, 101))

	spawned, err := eng.preImplement(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item)
	if err != nil {
		t.Fatalf("preImplement: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true")
	}
	if hasLabelCall(client, "fabrik:paused") {
		t.Error("parent was paused")
	}
	assertNoWriteToUnservedRepo(t, client)
	if len(client.addBlockedByIssueCalls) != 0 {
		t.Errorf("AddBlockedByIssue calls = %d, want 0 (edge already present)", len(client.addBlockedByIssueCalls))
	}
	if !hasLabelCall(client, "fabrik:children-spawned") {
		t.Error("fabrik:children-spawned not applied")
	}
	var markerRemoved bool
	for _, c := range client.removeLabelCalls {
		if c.labelName == spawnChildLabel(1, 101) {
			markerRemoved = true
		}
	}
	if !markerRemoved {
		t.Error("marker not removed after the spawn completed")
	}

	snap, err := eng.store.Get("owner/repo", item.Number)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	deps := snap.State().BlockedBy
	if len(deps) == 0 || deps[len(deps)-1].State != "CLOSED" {
		t.Errorf("BlockedBy = %+v; want the resumed edge written through as CLOSED", deps)
	}
}

// Non-vacuity: with the marker-first ordering neutralised the same setup pauses.
func TestSpawnChildren_MarkedCrossRepoChild_NeutralisedPauses(t *testing.T) {
	edge := []gh.Dependency{{Repo: "owner/child", Number: 101, State: "CLOSED"}}
	eng, client := markerFirstEngine(t, true, edge)
	eng.SetSpawnMarkerFirstDisabledForTest(true)

	item := planItemWithBlocks(markerFirstBlock)
	item.Labels = append(item.Labels, spawnChildLabel(1, 101))

	spawned, err := eng.preImplement(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item)
	if err == nil || spawned {
		t.Fatalf("expected the not-served refusal, got spawned=%v err=%v", spawned, err)
	}
	if !hasLabelCall(client, "fabrik:paused") {
		t.Error("expected fabrik:paused with the fix neutralised")
	}
}

func TestSpawnChildren_UnmarkedCrossRepo_PausesWithManualPath(t *testing.T) {
	eng, client := markerFirstEngine(t, false, nil)

	item := planItemWithBlocks(markerFirstBlock)
	spawned, err := eng.preImplement(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item)
	if err == nil || spawned {
		t.Fatalf("expected refusal, got spawned=%v err=%v", spawned, err)
	}
	if !hasLabelCall(client, "fabrik:paused") {
		t.Error("fabrik:paused not added")
	}
	assertNoWriteToUnservedRepo(t, client)

	var body string
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "not served by this Fabrik instance") {
			body = c.body
		}
	}
	if body == "" {
		t.Fatal("no not-served comment posted")
	}
	// R4: the supported manual path.
	for _, want := range []string{
		"create it in the target repo",
		"`fabrik:spawned-child:<i>:<n>`",
		"then remove `fabrik:paused`",
		"`fabrik:spawned-child:1:<n>`",
		"owner/child",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pause comment lacks %q:\n%s", want, body)
		}
	}
}

func TestSpawnChildren_MarkedCrossRepoChild_Unresolvable_Pauses(t *testing.T) {
	eng, client := markerFirstEngine(t, false, nil)

	item := planItemWithBlocks(markerFirstBlock)
	item.Labels = append(item.Labels, spawnChildLabel(1, 999)) // not resolvable

	spawned, err := eng.preImplement(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item)
	if err == nil || spawned {
		t.Fatalf("expected refusal, got spawned=%v err=%v", spawned, err)
	}
	var sawResolve bool
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "could not be found") && strings.Contains(c.body, spawnChildLabel(1, 999)) {
			sawResolve = true
		}
		if strings.Contains(c.body, "not served by this Fabrik instance") {
			t.Error("an unresolvable marker must not surface as the servability pause")
		}
	}
	if !sawResolve {
		t.Error("expected the 'recorded child can no longer be resolved' pause")
	}
	assertNoWriteToUnservedRepo(t, client)
}

func TestSpawnChildren_MarkedCrossRepoChild_EdgeAbsent_WritesParentEdgeOnly(t *testing.T) {
	eng, client := markerFirstEngine(t, false, nil)

	item := planItemWithBlocks(markerFirstBlock)
	item.Labels = append(item.Labels, spawnChildLabel(1, 101))

	spawned, err := eng.preImplement(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item)
	if err != nil || !spawned {
		t.Fatalf("preImplement: spawned=%v err=%v", spawned, err)
	}
	assertNoWriteToUnservedRepo(t, client)
	if len(client.addBlockedByIssueCalls) != 1 {
		t.Fatalf("AddBlockedByIssue calls = %d, want 1", len(client.addBlockedByIssueCalls))
	}
	c := client.addBlockedByIssueCalls[0]
	if c.issueNodeID != "I_parent" || c.blockerNodeID != "I_child101" {
		t.Errorf("AddBlockedByIssue = %+v; want the edge written on the parent only", c)
	}
	snap, _ := eng.store.Get("owner/repo", item.Number)
	deps := snap.State().BlockedBy
	if len(deps) == 0 || deps[len(deps)-1].State != "OPEN" {
		t.Errorf("BlockedBy = %+v; want an OPEN edge for the open child", deps)
	}
}

const markerFirstTwoBlocks = markerFirstBlock + `
FABRIK_SPAWN_CHILD_BEGIN owner/other
TITLE: Second cross-repo child
Body two.
FABRIK_SPAWN_CHILD_END
`

func TestSpawnChildren_MixedBatch_UnmarkedUnservedBlockStillPauses(t *testing.T) {
	edge := []gh.Dependency{{Repo: "owner/child", Number: 101, State: "CLOSED"}}
	eng, client := markerFirstEngine(t, true, edge)

	item := planItemWithBlocks(markerFirstTwoBlocks)
	item.Labels = append(item.Labels, spawnChildLabel(1, 101))

	spawned, err := eng.preImplement(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item)
	if err == nil || spawned {
		t.Fatalf("expected the not-served refusal for block 2, got spawned=%v err=%v", spawned, err)
	}
	assertNoWriteToUnservedRepo(t, client)
	var body string
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "not served by this Fabrik instance") {
			body = c.body
		}
	}
	if !strings.Contains(body, "`owner/other`") || !strings.Contains(body, "`fabrik:spawned-child:2:<n>`") {
		t.Errorf("pause comment should name block 2 only:\n%s", body)
	}
	if strings.Contains(body, "`fabrik:spawned-child:1:<n>`") {
		t.Errorf("pause comment must not ask for a marker on the already-marked block 1:\n%s", body)
	}
}

func TestSpawnChildren_MixedBatch_ServedCreateAndMarkedUnserved(t *testing.T) {
	edge := []gh.Dependency{{Repo: "owner/child", Number: 101, State: "CLOSED"}}
	eng, client := markerFirstEngine(t, true, edge)
	client.createIssueFn = func(owner, repo, title, body string, assignees []string) (int, string, error) {
		if owner != "owner" || repo != "repo" {
			t.Errorf("CreateIssue(%s/%s): only the served repo may be written", owner, repo)
		}
		return 300, "I_served300", nil
	}
	client.addProjectV2ItemByIdFn = func(projectID, contentNodeID string) (string, error) {
		return "PVTI_" + contentNodeID, nil
	}

	item := planItemWithBlocks(markerFirstBlock + `
FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: Served child
Body two.
FABRIK_SPAWN_CHILD_END
`)
	item.Labels = append(item.Labels, spawnChildLabel(1, 101))

	spawned, err := eng.preImplement(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item)
	if err != nil || !spawned {
		t.Fatalf("preImplement: spawned=%v err=%v", spawned, err)
	}
	if len(client.createIssueCalls) != 1 {
		t.Errorf("CreateIssue calls = %d, want 1 (the served block only)", len(client.createIssueCalls))
	}
	if len(client.addProjectV2ItemCalls) != 1 || client.addProjectV2ItemCalls[0].contentNodeID != "I_served300" {
		t.Errorf("AddProjectV2ItemById calls = %+v, want only the served child", client.addProjectV2ItemCalls)
	}
	for _, l := range client.addLabelCalls {
		if l.owner == "owner" && l.repo == "child" {
			t.Errorf("label %q written to the unserved repo", l.labelName)
		}
	}
}

func TestSpawnChildren_RemoteMarkedDependent_SkipsSiblingEdge(t *testing.T) {
	edge := []gh.Dependency{{Repo: "owner/child", Number: 101, State: "CLOSED"}}
	eng, client := markerFirstEngine(t, true, edge)
	client.createIssueFn = func(owner, repo, title, body string, assignees []string) (int, string, error) {
		return 300, "I_served300", nil
	}
	client.addProjectV2ItemByIdFn = func(projectID, contentNodeID string) (string, error) {
		return "PVTI_" + contentNodeID, nil
	}

	// Block 2 (served, created) is the blocker; block 1 (marked, unserved)
	// declares DEPENDS_ON: 2 — an edge that would be written on the unserved child.
	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: Served blocker
Body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Marked dependent
DEPENDS_ON: 1
Body two.
FABRIK_SPAWN_CHILD_END
`)
	// Block 2 is the marked, unserved one.
	item.Labels = append(item.Labels, spawnChildLabel(2, 101))

	spawned, err := eng.preImplement(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item)
	if err != nil || !spawned {
		t.Fatalf("preImplement: spawned=%v err=%v", spawned, err)
	}
	for _, c := range client.addBlockedByIssueCalls {
		if c.issueNodeID == "I_child101" {
			t.Errorf("sibling edge written on the unserved child: %+v", c)
		}
	}
}

func TestSpawnChildren_NonResumable_IgnoresMarkerAndPauses(t *testing.T) {
	eng, client := markerFirstEngine(t, true, nil)

	item := planItemWithBlocks(markerFirstBlock)
	item.Labels = append(item.Labels, spawnChildLabel(1, 101))
	blocks := ParseSpawnBlocks(planCommentWithBlocks(markerFirstBlock))

	spawned, _, err := eng.spawnChildren(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, "owner", "repo", blocks, false)
	if err == nil {
		t.Fatal("expected the not-served refusal for a non-resumable origin")
	}
	_ = spawned
	if !hasLabelCall(client, "fabrik:paused") {
		t.Error("expected fabrik:paused")
	}
	assertNoWriteToUnservedRepo(t, client)
}

func TestSpawnChildren_OutOfRangeMarkerIsIgnored(t *testing.T) {
	eng, client := markerFirstEngine(t, true, nil)

	item := planItemWithBlocks(markerFirstBlock)
	item.Labels = append(item.Labels, spawnChildLabel(7, 101)) // no block 7

	spawned, err := eng.preImplement(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item)
	if err == nil || spawned {
		t.Fatalf("expected the not-served refusal, got spawned=%v err=%v", spawned, err)
	}
	if !hasLabelCall(client, "fabrik:paused") {
		t.Error("expected fabrik:paused")
	}
}
