package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
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
	if got := resolveSpecifyOptionID(nil, nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestResolveSpecifyOptionID_ExactMatch(t *testing.T) {
	sf := &gh.StatusField{
		Options:            map[string]string{"Backlog": "OPT_0", "Specify": "OPT_1", "Done": "OPT_3"},
		OrderedOptionNames: []string{"Backlog", "Specify", "Done"},
	}
	if got := resolveSpecifyOptionID(sf, nil); got != "OPT_1" {
		t.Errorf("got %q, want OPT_1", got)
	}
}

func TestResolveSpecifyOptionID_Fallback(t *testing.T) {
	// No "Specify" column — fallback to first non-Backlog, non-terminal.
	sf := &gh.StatusField{
		Options:            map[string]string{"Backlog": "OPT_0", "Research": "OPT_1", "Done": "OPT_3"},
		OrderedOptionNames: []string{"Backlog", "Research", "Done"},
	}
	if got := resolveSpecifyOptionID(sf, nil); got != "OPT_1" {
		t.Errorf("got %q, want OPT_1 (Research)", got)
	}
}

func TestResolveSpecifyOptionID_BacklogAndDoneOnly(t *testing.T) {
	// Only two columns — fallback skips Backlog and the last; nothing qualifies.
	sf := &gh.StatusField{
		Options:            map[string]string{"Backlog": "OPT_0", "Done": "OPT_1"},
		OrderedOptionNames: []string{"Backlog", "Done"},
	}
	if got := resolveSpecifyOptionID(sf, nil); got != "" {
		t.Errorf("got %q, want empty (no viable column)", got)
	}
}

func TestResolveSpecifyOptionID_EmptyOrderedNames(t *testing.T) {
	sf := &gh.StatusField{
		Options:            map[string]string{"Research": "OPT_1"},
		OrderedOptionNames: nil,
	}
	if got := resolveSpecifyOptionID(sf, nil); got != "" {
		t.Errorf("got %q, want empty when OrderedOptionNames is nil", got)
	}
}

func TestResolveSpecifyOptionID_SingleColumn(t *testing.T) {
	// Exact match on "Specify" fires before the fallback len(names) < 2 guard,
	// so a single-column board named "Specify" still returns its option ID.
	sf := &gh.StatusField{
		Options:            map[string]string{"Specify": "OPT_1"},
		OrderedOptionNames: []string{"Specify"},
	}
	if got := resolveSpecifyOptionID(sf, nil); got != "OPT_1" {
		t.Errorf("got %q, want OPT_1 (exact match wins)", got)
	}
}

// TestResolveSpecifyOptionID_SkipsDeclaredUnmanagedColumn is the regression
// guard for the PR review finding on issue #973: the fallback previously only
// ever skipped the literal name "Backlog", so a parking column declared
// `unmanaged: true` under any OTHER name (e.g. "Icebox") fell through and was
// returned as the "Specify" fallback target — landing spawned children in a
// column itemMayNeedWork/itemNeedsWork deliberately never dispatch, with no
// fabrik:awaiting-placement marker recorded to ever recover them.
func TestResolveSpecifyOptionID_SkipsDeclaredUnmanagedColumn(t *testing.T) {
	sf := &gh.StatusField{
		Options:            map[string]string{"Icebox": "OPT_0", "Research": "OPT_1", "Done": "OPT_3"},
		OrderedOptionNames: []string{"Icebox", "Research", "Done"},
	}
	stagesCfg := []*stages.Stage{
		{Name: "Icebox", Unmanaged: true},
		{Name: "Research"},
		{Name: "Done", CleanupWorktree: true},
	}
	if got := resolveSpecifyOptionID(sf, stagesCfg); got != "OPT_1" {
		t.Errorf("got %q, want OPT_1 (Research) — declared unmanaged column Icebox must be skipped", got)
	}
}

// TestResolveSpecifyOptionID_UnmanagedAndDoneOnly verifies the fallback still
// correctly yields "" when the only non-terminal option is a declared
// unmanaged column under a non-Backlog name — mirroring
// TestResolveSpecifyOptionID_BacklogAndDoneOnly for the generalized case.
func TestResolveSpecifyOptionID_UnmanagedAndDoneOnly(t *testing.T) {
	sf := &gh.StatusField{
		Options:            map[string]string{"Icebox": "OPT_0", "Done": "OPT_1"},
		OrderedOptionNames: []string{"Icebox", "Done"},
	}
	stagesCfg := []*stages.Stage{
		{Name: "Icebox", Unmanaged: true},
		{Name: "Done", CleanupWorktree: true},
	}
	if got := resolveSpecifyOptionID(sf, stagesCfg); got != "" {
		t.Errorf("got %q, want empty (no viable column)", got)
	}
}

// ---- preImplement integration tests ----

func spawnTestEngine(t *testing.T, client *mockGitHubClient) *Engine {
	t.Helper()
	eng := testEngine(t, client, &mockClaudeInvoker{})
	// Register "owner/repo" and "owner/child" as managed repos.
	eng.worktreeManagers["owner/repo"] = NewWorktreeManager(t.TempDir())
	eng.worktreeManagers["owner/child"] = NewWorktreeManager(t.TempDir())
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
	eng := spawnTestEngine(t, client)

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

// TestPreImplement_NoOp_NoPlanComment covers the #982 inconsistency: stage:Plan:complete
// is present but item.Comments has no Plan comment. This must NOT silently no-op —
// preImplement must attempt recovery via a live re-read before concluding there is
// nothing to spawn. This test's live re-read also finds no Plan comment, so the
// final outcome is still spawned=false, err=nil — but only after recovery genuinely ran.
func TestPreImplement_NoOp_NoPlanComment(t *testing.T) {
	var fetchCalls int
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			fetchCalls++
			return nil // fresh read still has no comments
		},
	}
	eng := spawnTestEngine(t, client)

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
	if fetchCalls != 1 {
		t.Errorf("expected live re-read to be attempted exactly once, got %d calls", fetchCalls)
	}
}

// TestPreImplement_RecoversViaLiveRead_SpawnsChildren covers the #982 recovery path
// where the live re-read finds the Plan comment (with spawn blocks) that was missing
// from the stale item.Comments snapshot. Children must be spawned exactly once.
func TestPreImplement_RecoversViaLiveRead_SpawnsChildren(t *testing.T) {
	childCounter := 0
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.Comments = []gh.Comment{
				{
					DatabaseID: 1001,
					Author:     "testuser",
					Body: planCommentWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Recovered child
Recovered body.
FABRIK_SPAWN_CHILD_END
`),
				},
			}
			return nil
		},
		createIssueFn: func(owner, repo, title, body string) (int, string, error) {
			childCounter++
			return 300 + childCounter, fmt.Sprintf("I_recovered%d", childCounter), nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
	}
	eng := spawnTestEngine(t, client)

	item := gh.ProjectItem{
		ID:     "I_parent",
		ItemID: "PVTI_parent",
		Number: 42,
		Repo:   "owner/repo",
		Labels: []string{"stage:Plan:complete"},
		// item.Comments is empty — simulates the stale-snapshot miss.
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true after recovery finds spawn blocks")
	}
	if len(client.createIssueCalls) != 1 {
		t.Fatalf("expected 1 CreateIssue call, got %d", len(client.createIssueCalls))
	}

	var spawnedLabelCount int
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:children-spawned" {
			spawnedLabelCount++
		}
	}
	if spawnedLabelCount != 1 {
		t.Errorf("expected fabrik:children-spawned added exactly once, got %d", spawnedLabelCount)
	}
}

// TestPreImplement_RecoversViaLiveRead_ConfirmsNothingToSpawn covers the #982 recovery
// path where the live re-read succeeds but confirms there really is nothing to spawn
// (Plan comment recovered, but with no spawn blocks).
func TestPreImplement_RecoversViaLiveRead_ConfirmsNothingToSpawn(t *testing.T) {
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.Comments = []gh.Comment{
				{DatabaseID: 1001, Author: "testuser", Body: planCommentWithBlocks("No spawn blocks in here.")},
			}
			return nil
		},
	}
	eng := spawnTestEngine(t, client)

	item := gh.ProjectItem{
		ID:     "I_parent",
		Number: 42,
		Repo:   "owner/repo",
		Labels: []string{"stage:Plan:complete"},
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spawned {
		t.Error("expected spawned=false when recovered Plan comment has no spawn blocks")
	}
	if len(client.createIssueCalls) != 0 {
		t.Error("CreateIssue should not be called when there is nothing to spawn")
	}
}

// TestPreImplement_RecoveryFails_DefersWithoutPausing covers the #982 outcome where the
// live re-read itself fails — preImplement must return errPreImplementDeferred (so the
// parent is retried on a subsequent poll) and must NOT pause the issue.
func TestPreImplement_RecoveryFails_DefersWithoutPausing(t *testing.T) {
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			return errors.New("simulated GraphQL failure")
		},
	}
	eng := spawnTestEngine(t, client)

	item := gh.ProjectItem{
		ID:     "I_parent",
		Number: 42,
		Repo:   "owner/repo",
		Labels: []string{"stage:Plan:complete"},
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if !errors.Is(err, errPreImplementDeferred) {
		t.Fatalf("expected errPreImplementDeferred, got %v", err)
	}
	if spawned {
		t.Error("expected spawned=false on deferred recovery")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused must not be added on the deferred outcome")
		}
	}
}

// TestPreImplement_RecoveryDeferred_CooldownSkipsLiveRead verifies that an active
// spawn-recovery cooldown short-circuits the live-read call entirely, bounding
// repeated GraphQL load during a sustained failure window (#971-style pressure).
func TestPreImplement_RecoveryDeferred_CooldownSkipsLiveRead(t *testing.T) {
	var fetchCalls int
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			fetchCalls++
			return nil
		},
	}
	eng := spawnTestEngine(t, client)
	eng.store.Apply(itemstate.CooldownRecorded{
		Repo:   "owner/repo",
		Number: 42,
		Reason: "spawn-recovery-deferred",
		Until:  time.Now().Add(10 * time.Minute),
	})

	item := gh.ProjectItem{
		ID:     "I_parent",
		Number: 42,
		Repo:   "owner/repo",
		Labels: []string{"stage:Plan:complete"},
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if !errors.Is(err, errPreImplementDeferred) {
		t.Fatalf("expected errPreImplementDeferred, got %v", err)
	}
	if spawned {
		t.Error("expected spawned=false while cooldown is active")
	}
	if fetchCalls != 0 {
		t.Errorf("expected live re-read to be skipped while cooldown is active, got %d calls", fetchCalls)
	}
}

func TestPreImplement_NoOp_NoBlocks(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)

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
	eng := spawnTestEngine(t, client)

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
	eng := spawnTestEngine(t, client)
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
	eng := spawnTestEngine(t, client)
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
	eng := spawnTestEngine(t, client)

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

// ---- status-set and label-inheritance tests ----

func spawnTestEngineWithSpecify(t *testing.T, client *mockGitHubClient) *Engine {
	t.Helper()
	eng := spawnTestEngine(t, client)
	eng.statusField = &gh.StatusField{
		FieldID: "FIELD_STATUS",
		Options: map[string]string{
			"Backlog": "OPT_0",
			"Specify": "OPT_1",
			"Done":    "OPT_9",
		},
		OrderedOptionNames: []string{"Backlog", "Specify", "Done"},
	}
	return eng
}

func TestPreImplement_SetsSpecifyStatus(t *testing.T) {
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string) (int, string, error) {
			return 101, "I_child101", nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_child101", nil
		},
	}
	eng := spawnTestEngineWithSpecify(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Test child
Body.
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

	// UpdateProjectItemStatus must be called with correct args.
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 UpdateProjectItemStatus call, got %d", len(client.updateStatusCalls))
	}
	call := client.updateStatusCalls[0]
	if call.projectID != "PVT_1" {
		t.Errorf("projectID = %q, want PVT_1", call.projectID)
	}
	if call.itemID != "PVTI_child101" {
		t.Errorf("itemID = %q, want PVTI_child101", call.itemID)
	}
	if call.fieldID != "FIELD_STATUS" {
		t.Errorf("fieldID = %q, want FIELD_STATUS", call.fieldID)
	}
	if call.optionID != "OPT_1" {
		t.Errorf("optionID = %q, want OPT_1 (Specify)", call.optionID)
	}
}

func TestPreImplement_StatusSetNilField(t *testing.T) {
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string) (int, string, error) {
			return 102, "I_child102", nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_child102", nil
		},
	}
	eng := spawnTestEngine(t, client)
	// statusField is nil by default in spawnTestEngine

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Test child no status
Body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true even when statusField is nil")
	}

	// No UpdateProjectItemStatus call should happen.
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected 0 UpdateProjectItemStatus calls, got %d", len(client.updateStatusCalls))
	}
}

func TestPreImplement_YoloInheritance(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngineWithSpecify(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child for yolo
Body.
FABRIK_SPAWN_CHILD_END
`)
	item.Labels = append(item.Labels, "fabrik:yolo")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var yoloAdded, cruiseAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:yolo" {
			yoloAdded = true
		}
		if c.labelName == "fabrik:cruise" {
			cruiseAdded = true
		}
	}
	if !yoloAdded {
		t.Error("fabrik:yolo not added to child when parent has it")
	}
	if cruiseAdded {
		t.Error("fabrik:cruise must not be added when parent does not have it")
	}
}

func TestPreImplement_CruiseInheritance(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngineWithSpecify(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child for cruise
Body.
FABRIK_SPAWN_CHILD_END
`)
	item.Labels = append(item.Labels, "fabrik:cruise")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var yoloAdded, cruiseAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:yolo" {
			yoloAdded = true
		}
		if c.labelName == "fabrik:cruise" {
			cruiseAdded = true
		}
	}
	if yoloAdded {
		t.Error("fabrik:yolo must not be added when parent does not have it")
	}
	if !cruiseAdded {
		t.Error("fabrik:cruise not added to child when parent has it")
	}
}

func TestPreImplement_BothLabelsInherited(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngineWithSpecify(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child for both
Body.
FABRIK_SPAWN_CHILD_END
`)
	item.Labels = append(item.Labels, "fabrik:yolo", "fabrik:cruise")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var yoloAdded, cruiseAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:yolo" {
			yoloAdded = true
		}
		if c.labelName == "fabrik:cruise" {
			cruiseAdded = true
		}
	}
	if !yoloAdded {
		t.Error("fabrik:yolo not added to child when parent has both labels")
	}
	if !cruiseAdded {
		t.Error("fabrik:cruise not added to child when parent has both labels")
	}
}

func TestPreImplement_NoAutonomyLabels(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngineWithSpecify(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child without autonomy labels
Body.
FABRIK_SPAWN_CHILD_END
`)
	// item.Labels has only "stage:Plan:complete" — no yolo or cruise
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:yolo" {
			t.Error("fabrik:yolo must not be added when parent does not have it")
		}
		if c.labelName == "fabrik:cruise" {
			t.Error("fabrik:cruise must not be added when parent does not have it")
		}
	}
}

// ---- #1263 regression: prose mentions must not destroy real blocks ----

// TestParseSpawnBlocks_ProseMentionDoesNotConsumeRealBlock reproduces the
// production failure from e2e TestCrossRepoSpawn (parent fabrik-test-alpha#3796).
// The Plan both described the marker in a markdown checklist AND emitted a real
// block. The backticked mention carried a slash-containing token
// ("handarbeit/fabrik-test-beta`"), so the pre-fix "contains a slash" check
// accepted it as a block opener; the parser then ran to the real END, consumed
// the authentic block as the phantom's content, failed the TITLE: check, and
// dropped it — yielding zero blocks and a silent no-op spawn.
func TestParseSpawnBlocks_ProseMentionDoesNotConsumeRealBlock(t *testing.T) {
	body := "## Approach\n" +
		"\n" +
		"1. Plan (this stage) emits a `FABRIK_SPAWN_CHILD_BEGIN` block scoped to the beta-side work.\n" +
		"\n" +
		"### Task checklist\n" +
		"\n" +
		"- [ ] Emit `FABRIK_SPAWN_CHILD_BEGIN handarbeit/fabrik-test-beta` block (this stage) scoping the function + test\n" +
		"\n" +
		"FABRIK_SPAWN_CHILD_BEGIN handarbeit/fabrik-test-beta\n" +
		"TITLE: Add HelloIssue3796() greeting function\n" +
		"\n" +
		"## Problem\n" +
		"\n" +
		"Beta-side half of the cross-repo spawn regression test.\n" +
		"FABRIK_SPAWN_CHILD_END\n"

	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 {
		t.Fatalf("expected exactly 1 block (the real one), got %d — a prose mention consumed it", len(blocks))
	}
	b := blocks[0]
	if b.Repo != "handarbeit/fabrik-test-beta" {
		t.Errorf("repo: got %q, want %q", b.Repo, "handarbeit/fabrik-test-beta")
	}
	if b.Title != "Add HelloIssue3796() greeting function" {
		t.Errorf("title: got %q, want %q", b.Title, "Add HelloIssue3796() greeting function")
	}
	if !strings.Contains(b.Body, "Beta-side half") {
		t.Errorf("body should carry the real block's content, got: %q", b.Body)
	}
}

// TestParseSpawnBlocks_ProseMentionAtLineStartRejected covers the variant the
// leading-whitespace tolerance could otherwise let through: the marker opens
// the line, but trailing prose follows the repo token. Real blocks carry the
// repo and nothing else.
func TestParseSpawnBlocks_ProseMentionAtLineStartRejected(t *testing.T) {
	body := `
FABRIK_SPAWN_CHILD_BEGIN owner/repo is the marker you emit to spawn a child.
TITLE: Not a real block
FABRIK_SPAWN_CHILD_END
`
	if blocks := ParseSpawnBlocks(body); len(blocks) != 0 {
		t.Fatalf("expected 0 blocks for a prose line with trailing text, got %d", len(blocks))
	}
}

// TestParseSpawnBlocks_BacktickedRepoRejected pins the specific token shape that
// defeated the old "contains a slash" validation.
func TestParseSpawnBlocks_BacktickedRepoRejected(t *testing.T) {
	body := "FABRIK_SPAWN_CHILD_BEGIN handarbeit/fabrik-test-beta`\nTITLE: Phantom\nFABRIK_SPAWN_CHILD_END\n"
	if blocks := ParseSpawnBlocks(body); len(blocks) != 0 {
		t.Fatalf("expected 0 blocks for a backtick-suffixed repo, got %d", len(blocks))
	}
}

// TestParseSpawnBlocks_IndentedBlockParses documents the deliberate tolerance:
// a real block nested under a list item still parses. Only trailing content
// after the repo disqualifies a line, not leading whitespace.
func TestParseSpawnBlocks_IndentedBlockParses(t *testing.T) {
	body := `
  FABRIK_SPAWN_CHILD_BEGIN owner/child
  TITLE: Indented but genuine
  Body text here.
  FABRIK_SPAWN_CHILD_END
`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Repo != "owner/child" {
		t.Errorf("repo: got %q, want %q", blocks[0].Repo, "owner/child")
	}
	if blocks[0].Title != "Indented but genuine" {
		t.Errorf("title: got %q, want %q", blocks[0].Title, "Indented but genuine")
	}
}

// TestParseSpawnBlocks_MalformedBlockDoesNotSwallowNext ensures a block that
// fails the TITLE: check does not re-open scanning inside its own content and
// consume the following well-formed block.
func TestParseSpawnBlocks_MalformedBlockDoesNotSwallowNext(t *testing.T) {
	body := `
FABRIK_SPAWN_CHILD_BEGIN owner/first
no title line here, so this block is malformed
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/second
TITLE: Second child
Second body.
FABRIK_SPAWN_CHILD_END
`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block (the well-formed second), got %d", len(blocks))
	}
	if blocks[0].Repo != "owner/second" {
		t.Errorf("repo: got %q, want %q", blocks[0].Repo, "owner/second")
	}
}
