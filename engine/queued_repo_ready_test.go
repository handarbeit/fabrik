package engine

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// restartedTrainRepo reproduces the state after a restart: the bare clone of owner/repo
// is on disk under fabrikDir (as ensureBareClone left it), with a maint/1.x branch on
// its origin, but no WorktreeManager is registered — nothing has run a stage since.
func restartedTrainRepo(t *testing.T) (fabrikDir string) {
	t.Helper()
	skipIfNoGit(t)
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	fabrikDir = filepath.Join(tmp, "fabrik")
	bareDir := filepath.Join(fabrikDir, ".fabrik", "repos", "owner-repo.git")

	mustGit(t, srcDir, "init", "-b", "main")
	mustGit(t, srcDir, "config", "user.email", "test@test.com")
	mustGit(t, srcDir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(srcDir, "f.txt"), "0\n")
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "initial")
	mustGit(t, srcDir, "branch", "maint/1.x")

	if out, err := exec.Command("git", "clone", "--bare", srcDir, bareDir).CombinedOutput(); err != nil {
		t.Fatalf("bare clone: %s: %v", out, err)
	}
	mustGitDir(t, bareDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	mustGitDir(t, bareDir, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
	mustGitDir(t, bareDir, "remote", "set-head", "origin", "--auto")
	return fabrikDir
}

// TestQueuedBaseMemberBatchesAfterRestart is the #2250 regression test. Before the
// helper runs, the base:-labelled member is excluded ("WorktreeManager not yet
// registered") — the pre-fix state that, with nothing else in the repo running a stage,
// lasted for ever. After ensureQueuedBaseReposReady it is registered and the member is
// partitioned on its own base.
func TestQueuedBaseMemberBatchesAfterRestart(t *testing.T) {
	fabrikDir := restartedTrainRepo(t)
	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo", MaxConcurrent: 1, Stages: testStages()}, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	eng.fabrikDir = fabrikDir

	items := []gh.ProjectItem{
		{Number: 1, Status: "BatchHold", Repo: "owner/repo"},
		{Number: 2, Status: "BatchHold", Repo: "owner/repo", Labels: []string{"base:maint/1.x"}},
	}

	// Precondition (the bug): no manager, so #2 is excluded.
	for _, g := range eng.groupQueuedByRepoAndBase(items, "BatchHold", "owner/repo") {
		for _, it := range g.items {
			if it.Number == 2 {
				t.Fatalf("precondition: #2 was partitioned (%q) with no WorktreeManager registered", g.base)
			}
		}
	}

	eng.ensureQueuedBaseReposReady(context.Background(), items, "BatchHold")

	eng.mu.Lock()
	_, registered := eng.worktreeManagers["owner/repo"]
	eng.mu.Unlock()
	if !registered {
		t.Fatal("ensureQueuedBaseReposReady did not register owner/repo")
	}
	var found bool
	for _, g := range eng.groupQueuedByRepoAndBase(items, "BatchHold", "owner/repo") {
		for _, it := range g.items {
			if it.Number == 2 {
				found = true
				if g.base != "maint/1.x" {
					t.Errorf("#2 partitioned on base %q, want maint/1.x", g.base)
				}
			}
		}
	}
	if !found {
		t.Fatal("#2 still excluded from batching after the repo was registered")
	}
}

// TestQueuedDefaultOnlyRepoIsNotRegistered keeps #1648's AC4: a repo whose Queued
// members carry no base: label needs no base resolution, so the helper touches no git
// and registers nothing for it.
func TestQueuedDefaultOnlyRepoIsNotRegistered(t *testing.T) {
	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo", MaxConcurrent: 1, Stages: testStages()}, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	var attempts []string
	eng.cloneAttemptHook = func(repo string) { attempts = append(attempts, repo) }

	items := []gh.ProjectItem{
		{Number: 1, Status: "BatchHold", Repo: "owner/repo"},
		{Number: 3, Status: "Validate", Repo: "other/repo", Labels: []string{"base:maint/1.x"}}, // not Queued
	}
	eng.ensureQueuedBaseReposReady(context.Background(), items, "BatchHold")

	if len(attempts) != 0 {
		t.Errorf("clone attempted for %v; want none (no base:-labelled Queued member)", attempts)
	}
	eng.mu.Lock()
	n := len(eng.worktreeManagers)
	eng.mu.Unlock()
	if n != 0 {
		t.Errorf("%d WorktreeManager(s) registered; want 0", n)
	}
}

// TestHandleMergeTrainBatchRegistersBeforeGrouping pins the wiring the unit tests above
// cannot see: handleMergeTrainBatch must call ensureQueuedBaseReposReady before it
// groups, or a restarted engine is back to skipping base:-labelled members for ever.
func TestHandleMergeTrainBatchRegistersBeforeGrouping(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "poll.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "handleMergeTrainBatch" {
			body = fd.Body
		}
	}
	if body == nil {
		t.Fatal("handleMergeTrainBatch not found in poll.go")
	}
	ensureAt, groupAt := token.NoPos, token.NoPos
	ast.Inspect(body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "ensureQueuedBaseReposReady":
				if ensureAt == token.NoPos {
					ensureAt = sel.Pos()
				}
			case "groupQueuedByRepoAndBase":
				if groupAt == token.NoPos {
					groupAt = sel.Pos()
				}
			}
		}
		return true
	})
	if ensureAt == token.NoPos {
		t.Fatal("handleMergeTrainBatch does not call ensureQueuedBaseReposReady (#2250)")
	}
	if groupAt == token.NoPos || ensureAt > groupAt {
		t.Fatal("handleMergeTrainBatch must call ensureQueuedBaseReposReady before groupQueuedByRepoAndBase (#2250)")
	}
}
