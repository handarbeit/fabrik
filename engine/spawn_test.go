package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// planCommentBody returns a fake Plan stage comment body containing the given spawn blocks.
func planCommentWithBlocks(blocksRaw string) string {
	return "🏭 **Fabrik — stage: Plan**\n\n" + blocksRaw
}

// ---- ParseSpawnBlocks unit tests ----

func TestParseSpawnBlocks_Empty(t *testing.T) {
	blocks := ParseSpawnBlocks("no spawn blocks here")
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks, got %d", len(blocks))
	}
}

func TestParseSpawnBlocks_SingleBlock(t *testing.T) {
	body := `
FABRIK_SPAWN_CHILD_BEGIN owner/child-repo
TITLE: Add authentication module
Implement OAuth2 authentication.
FABRIK_SPAWN_CHILD_END
`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	b := blocks[0]
	if b.Repo != "owner/child-repo" {
		t.Errorf("repo: got %q, want %q", b.Repo, "owner/child-repo")
	}
	if b.Title != "Add authentication module" {
		t.Errorf("title: got %q, want %q", b.Title, "Add authentication module")
	}
	if !strings.Contains(b.Body, "Implement OAuth2") {
		t.Errorf("body should contain body text, got: %q", b.Body)
	}
}

func TestParseSpawnBlocks_MultipleBlocks(t *testing.T) {
	body := `
FABRIK_SPAWN_CHILD_BEGIN owner/repo-a
TITLE: First child
First body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/repo-b
TITLE: Second child
Second body.
FABRIK_SPAWN_CHILD_END
`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Title != "First child" {
		t.Errorf("block[0] title: got %q", blocks[0].Title)
	}
	if blocks[1].Title != "Second child" {
		t.Errorf("block[1] title: got %q", blocks[1].Title)
	}
	if blocks[0].Repo != "owner/repo-a" {
		t.Errorf("block[0] repo: got %q", blocks[0].Repo)
	}
	if blocks[1].Repo != "owner/repo-b" {
		t.Errorf("block[1] repo: got %q", blocks[1].Repo)
	}
}

func TestParseSpawnBlocks_MissingRepo(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN
TITLE: No repo given
body
FABRIK_SPAWN_CHILD_END`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks for malformed BEGIN (no repo), got %d", len(blocks))
	}
}

func TestParseSpawnBlocks_RepoWithoutSlash(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN noslash
TITLE: Bad repo
body
FABRIK_SPAWN_CHILD_END`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks for repo without slash, got %d", len(blocks))
	}
}

func TestParseSpawnBlocks_MissingEnd(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: No end marker
body`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks for missing END, got %d", len(blocks))
	}
}

func TestParseSpawnBlocks_MissingTitle(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN owner/repo
just body, no TITLE: line
FABRIK_SPAWN_CHILD_END`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks for missing TITLE, got %d", len(blocks))
	}
}

func TestParseSpawnBlocks_EmptyTitle(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE:
body
FABRIK_SPAWN_CHILD_END`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks for empty TITLE, got %d", len(blocks))
	}
}

func TestParseSpawnBlocks_BodyEmpty(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: Title only
FABRIK_SPAWN_CHILD_END`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Body != "" {
		t.Errorf("expected empty body, got %q", blocks[0].Body)
	}
}

// ---- resolveSpecifyOptionID unit tests ----

func TestResolveSpecifyOptionID_Nil(t *testing.T) {
	if got := resolveSpecifyOptionID(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestResolveSpecifyOptionID_ExactMatch(t *testing.T) {
	sf := &gh.StatusField{
		Options:            map[string]string{"Backlog": "OPT_0", "Specify": "OPT_1", "Done": "OPT_3"},
		OrderedOptionNames: []string{"Backlog", "Specify", "Done"},
	}
	if got := resolveSpecifyOptionID(sf); got != "OPT_1" {
		t.Errorf("got %q, want OPT_1", got)
	}
}

func TestResolveSpecifyOptionID_Fallback(t *testing.T) {
	// No "Specify" column — fallback to first non-Backlog, non-terminal.
	sf := &gh.StatusField{
		Options:            map[string]string{"Backlog": "OPT_0", "Research": "OPT_1", "Done": "OPT_3"},
		OrderedOptionNames: []string{"Backlog", "Research", "Done"},
	}
	if got := resolveSpecifyOptionID(sf); got != "OPT_1" {
		t.Errorf("got %q, want OPT_1 (Research)", got)
	}
}

func TestResolveSpecifyOptionID_BacklogAndDoneOnly(t *testing.T) {
	// Only two columns — fallback skips Backlog and the last; nothing qualifies.
	sf := &gh.StatusField{
		Options:            map[string]string{"Backlog": "OPT_0", "Done": "OPT_1"},
		OrderedOptionNames: []string{"Backlog", "Done"},
	}
	if got := resolveSpecifyOptionID(sf); got != "" {
		t.Errorf("got %q, want empty (no viable column)", got)
	}
}

func TestResolveSpecifyOptionID_EmptyOrderedNames(t *testing.T) {
	sf := &gh.StatusField{
		Options:            map[string]string{"Research": "OPT_1"},
		OrderedOptionNames: nil,
	}
	if got := resolveSpecifyOptionID(sf); got != "" {
		t.Errorf("got %q, want empty when OrderedOptionNames is nil", got)
	}
}

func TestResolveSpecifyOptionID_SingleColumn(t *testing.T) {
	// Only one column — len(names) < 2, returns "".
	sf := &gh.StatusField{
		Options:            map[string]string{"Specify": "OPT_1"},
		OrderedOptionNames: []string{"Specify"},
	}
	// Even though "Specify" is in Options, len(names) == 1 so fallback returns "".
	// But we check exact match first, so it should return OPT_1.
	if got := resolveSpecifyOptionID(sf); got != "OPT_1" {
		t.Errorf("got %q, want OPT_1 (exact match wins)", got)
	}
}

// ---- preImplement integration tests ----

func spawnTestEngine(client *mockGitHubClient) *Engine {
	eng := testEngine(client, &mockClaudeInvoker{})
	// Register "owner/repo" and "owner/child" as managed repos.
	eng.worktreeManagers["owner/repo"] = NewWorktreeManager("/tmp/fake-parent")
	eng.worktreeManagers["owner/child"] = NewWorktreeManager("/tmp/fake-child")
	return eng
}

func planItemWithBlocks(blocksRaw string) gh.ProjectItem {
	return gh.ProjectItem{
		ID:     "I_parent",
		ItemID: "PVTI_parent",
		Number: 42,
		Repo:   "owner/repo",
		Title:  "Parent issue",
		Labels: []string{"stage:Plan:complete"},
		Comments: []gh.Comment{
			{
				DatabaseID: 1001,
				Author:     "testuser",
				Body:       planCommentWithBlocks(blocksRaw),
			},
		},
	}
}

func TestPreImplement_NoOp_ChildrenAlreadySpawned(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child issue
Body.
FABRIK_SPAWN_CHILD_END
`)
	item.Labels = append(item.Labels, "fabrik:children-spawned")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spawned {
		t.Error("expected spawned=false when children-spawned label present")
	}
	if len(client.createIssueCalls) != 0 {
		t.Error("CreateIssue should not be called when children already spawned")
	}
}

func TestPreImplement_NoOp_NoPlanComment(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(client)

	item := gh.ProjectItem{
		ID:     "I_parent",
		Number: 42,
		Repo:   "owner/repo",
		Labels: []string{"stage:Plan:complete"},
		// No comments at all.
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spawned {
		t.Error("expected spawned=false with no Plan comment")
	}
}

func TestPreImplement_NoOp_NoBlocks(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(client)

	item := planItemWithBlocks("No spawn blocks in here.")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spawned {
		t.Error("expected spawned=false when no spawn blocks in Plan comment")
	}
}

func TestPreImplement_HappyPath(t *testing.T) {
	childCounter := 0
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string) (int, string, error) {
			childCounter++
			return 100 + childCounter, fmt.Sprintf("I_child%d", childCounter), nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
	}
	eng := spawnTestEngine(client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child one
Child one body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child two
Child two body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true")
	}

	// Two issues created.
	if len(client.createIssueCalls) != 2 {
		t.Errorf("expected 2 CreateIssue calls, got %d", len(client.createIssueCalls))
	}
	if client.createIssueCalls[0].title != "Child one" {
		t.Errorf("first child title: got %q", client.createIssueCalls[0].title)
	}
	if client.createIssueCalls[1].title != "Child two" {
		t.Errorf("second child title: got %q", client.createIssueCalls[1].title)
	}

	// Footer injected into each child body.
	for i, c := range client.createIssueCalls {
		if !strings.Contains(c.body, "Spawned by Fabrik") {
			t.Errorf("child %d body missing footer: %q", i+1, c.body)
		}
	}

	// Added to project board twice.
	if len(client.addProjectV2ItemCalls) != 2 {
		t.Errorf("expected 2 AddProjectV2ItemById calls, got %d", len(client.addProjectV2ItemCalls))
	}

	// Linked as blockedBy twice.
	if len(client.addBlockedByIssueCalls) != 2 {
		t.Errorf("expected 2 AddBlockedByIssue calls, got %d", len(client.addBlockedByIssueCalls))
	}
	for _, c := range client.addBlockedByIssueCalls {
		if c.issueNodeID != "I_parent" {
			t.Errorf("AddBlockedByIssue issueNodeID: got %q, want %q", c.issueNodeID, "I_parent")
		}
	}

	// children-spawned label added.
	var spawnedLabelAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:children-spawned" {
			spawnedLabelAdded = true
		}
	}
	if !spawnedLabelAdded {
		t.Error("fabrik:children-spawned label not added")
	}

	// sub-issue label added to each child.
	subIssueCount := 0
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:sub-issue" {
			subIssueCount++
		}
	}
	if subIssueCount != 2 {
		t.Errorf("expected 2 fabrik:sub-issue labels, got %d", subIssueCount)
	}
}

// TestPreImplement_CloneFailure replaces the old TestPreImplement_UnmanagedRepo.
// With on-demand initialization, an unregistered target repo triggers a clone
// attempt. This test verifies the failure path when the clone cannot succeed.
func TestPreImplement_CloneFailure(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	eng := spawnTestEngine(client)
	// Point fabrikDir at a tempdir — ensureBareClone will fail to clone the nonexistent repo.
	eng.fabrikDir = t.TempDir()

	// "owner/newrepo" is NOT in worktreeManagers, and the clone will fail.
	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/newrepo
TITLE: Child in uncloneable repo
Body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err == nil {
		t.Fatal("expected error when clone fails")
	}
	if spawned {
		t.Error("expected spawned=false on clone failure")
	}
	if len(client.createIssueCalls) != 0 {
		t.Error("CreateIssue should not be called when clone fails")
	}

	// fabrik:paused and fabrik:awaiting-input must both be added.
	var pausedAdded, awaitingInputAdded bool
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:paused":
			pausedAdded = true
		case "fabrik:awaiting-input":
			awaitingInputAdded = true
		}
	}
	if !pausedAdded {
		t.Error("fabrik:paused label not added on clone failure")
	}
	if !awaitingInputAdded {
		t.Error("fabrik:awaiting-input label not added on clone failure")
	}

	// Error comment must be posted and must not mention the old "not in worktreeManagers" message.
	if len(client.addCommentCalls) == 0 {
		t.Error("expected error comment on clone failure")
	}
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "not in worktreeManagers") {
			t.Errorf("error comment must not mention 'not in worktreeManagers': %q", c.body)
		}
	}
}

// TestPreImplement_OnDemandClone_Success verifies that a spawn into a repo not
// yet in worktreeManagers succeeds when the bare clone directory already exists
// on disk (so ensureBareClone returns nil without hitting the network).
func TestPreImplement_OnDemandClone_Success(t *testing.T) {
	skipIfNoGit(t)

	// Create a tempdir to serve as fabrikDir.
	fabrikDir := t.TempDir()

	// Pre-create the bare clone directory that ensureBareClone will find.
	// When the directory exists, ensureBareClone skips the `git clone` step and
	// returns nil (best-effort fetch errors are silently ignored).
	targetOwner, targetRepoName := "testowner", "testrepo"
	bareDir := filepath.Join(fabrikDir, ".fabrik", "repos", targetOwner+"-"+targetRepoName+".git")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatalf("creating bare dir: %v", err)
	}
	initCmd := exec.Command("git", "init", "--bare", bareDir)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %s: %v", out, err)
	}

	childCounter := 0
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string) (int, string, error) {
			childCounter++
			return 200 + childCounter, fmt.Sprintf("I_newchild%d", childCounter), nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
	}
	eng := spawnTestEngine(client)
	eng.fabrikDir = fabrikDir

	// "testowner/testrepo" is NOT in worktreeManagers initially.
	item := planItemWithBlocks(fmt.Sprintf(`
FABRIK_SPAWN_CHILD_BEGIN %s/%s
TITLE: Child in on-demand-cloned repo
Body of the child issue.
FABRIK_SPAWN_CHILD_END
`, targetOwner, targetRepoName))
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true after on-demand clone")
	}

	// WorktreeManager must now be registered for the target repo.
	eng.mu.Lock()
	_, registered := eng.worktreeManagers[targetOwner+"/"+targetRepoName]
	eng.mu.Unlock()
	if !registered {
		t.Error("worktreeManagers should contain the on-demand-cloned target repo")
	}

	// CreateIssue must have been called for the child.
	if len(client.createIssueCalls) != 1 {
		t.Errorf("expected 1 CreateIssue call, got %d", len(client.createIssueCalls))
	}
}

func TestPreImplement_PartialFailure(t *testing.T) {
	callCount := 0
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string) (int, string, error) {
			callCount++
			if callCount == 2 {
				return 0, "", fmt.Errorf("github: 500 internal server error")
			}
			return 100 + callCount, fmt.Sprintf("I_child%d", callCount), nil
		},
	}
	eng := spawnTestEngine(client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child one
Body one.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child two
Body two.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err == nil {
		t.Fatal("expected error on partial failure")
	}
	if spawned {
		t.Error("expected spawned=false on error")
	}

	// Only one CreateIssue call succeeded before failure.
	if len(client.createIssueCalls) != 2 {
		t.Errorf("expected 2 CreateIssue attempts, got %d", len(client.createIssueCalls))
	}

	// children-spawned must NOT be added (partial state).
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:children-spawned" {
			t.Error("fabrik:children-spawned must not be added on partial failure")
		}
	}

	// Error comment and paused label should be added.
	if len(client.addCommentCalls) == 0 {
		t.Error("expected error comment on partial failure")
	}
	var pausedAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	if !pausedAdded {
		t.Error("fabrik:paused not added on partial failure")
	}
}
