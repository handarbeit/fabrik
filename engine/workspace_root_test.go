package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/workspace"
	"github.com/handarbeit/fabrik/stages"
)

// symlinkedRoot returns a workspace root reached through a symlink: the real
// directory and the link to it.
func symlinkedRoot(t *testing.T) (real, link string) {
	t.Helper()
	base := t.TempDir()
	real = filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	return real, link
}

func TestLayoutDefaultsToInBedForEnginesBuiltWithoutNew(t *testing.T) {
	eng := NewWithDeps(Config{}, nil, nil, nil)
	eng.fabrikDir = t.TempDir()
	if got, want := eng.layout().Dir(workspace.Worktrees), filepath.Join(eng.fabrikDir, ".fabrik", "worktrees"); got != want {
		t.Fatalf("layout worktrees = %s, want %s", got, want)
	}
}

func TestSymlinkEnv_ExternalRootGetsAbsoluteLink(t *testing.T) {
	bed := t.TempDir()
	root := filepath.Join(t.TempDir(), "ws")
	work := filepath.Join(root, "worktrees", "o-r", "issue-1")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bed, ".env"), []byte("K=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eng := NewWithDeps(Config{SymlinkEnv: true}, nil, nil, nil)
	eng.fabrikDir = bed
	eng.ws = workspace.Layout{Bed: bed, Root: root}
	eng.symlinkEnvIfEnabled(1, work)

	dst := filepath.Join(work, ".env")
	target, err := os.Readlink(dst)
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(bed, ".env") {
		t.Errorf("link target = %q, want the absolute bed .env", target)
	}
	if data, err := os.ReadFile(dst); err != nil || string(data) != "K=v\n" {
		t.Errorf("link must resolve to the bed .env: %v %q", err, data)
	}
}

func TestSymlinkEnv_DefaultLayoutKeepsRelativeLink(t *testing.T) {
	bed := t.TempDir()
	work := filepath.Join(bed, ".fabrik", "worktrees", "o-r", "issue-1")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bed, ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	eng := NewWithDeps(Config{SymlinkEnv: true}, nil, nil, nil)
	eng.fabrikDir = bed
	eng.symlinkEnvIfEnabled(1, work)
	target, err := os.Readlink(filepath.Join(work, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(target) {
		t.Errorf("the unset case must keep the relative link, got %q", target)
	}
}

func TestLinkEnv_RepairsBrokenLinkAndRespectsRealFiles(t *testing.T) {
	bed := t.TempDir()
	work := t.TempDir()
	src := filepath.Join(bed, ".env")
	if err := os.WriteFile(src, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(work, ".env")

	// A dangling link (the bed moved, or the root changed) is repaired.
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone", ".env"), dst); err != nil {
		t.Fatal(err)
	}
	repaired, err := linkEnv(src, dst, work, true)
	if err != nil || !repaired {
		t.Fatalf("broken link must be repaired: repaired=%v err=%v", repaired, err)
	}
	if data, err := os.ReadFile(dst); err != nil || string(data) != "A=1\n" {
		t.Fatalf("repaired link must resolve: %v %q", err, data)
	}
	if left, _ := filepath.Glob(filepath.Join(work, ".env.fabrik-repair-*")); len(left) != 0 {
		t.Errorf("repair must leave no temporary link behind: %v", left)
	}

	// A working link is left alone.
	if repaired, err := linkEnv(src, dst, work, true); err != nil || repaired {
		t.Errorf("a resolving link must be untouched: repaired=%v err=%v", repaired, err)
	}

	// A real file is never overwritten.
	real := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(real, []byte("MINE=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if repaired, err := linkEnv(src, real, filepath.Dir(real), true); err != nil || repaired {
		t.Errorf("real file: repaired=%v err=%v", repaired, err)
	}
	if data, _ := os.ReadFile(real); string(data) != "MINE=1\n" {
		t.Errorf("a real .env was modified: %q", data)
	}
}

func TestCheckNotInWorktree_RelocatedRoot(t *testing.T) {
	bed := t.TempDir()
	real, link := symlinkedRoot(t)
	ws := workspace.Layout{Bed: bed, Root: real}
	wt := filepath.Join(real, "worktrees", "o-r", "issue-3")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	// The relocated worktree has no ".fabrik/worktrees" segment, so only the
	// layout comparison can refuse it.
	if err := checkNotInWorktreeIn(wt, ws); err == nil {
		t.Error("starting inside a relocated worktree must be refused")
	}
	// And so must its symlinked spelling.
	if err := checkNotInWorktreeIn(filepath.Join(link, "worktrees", "o-r", "issue-3"), ws); err == nil {
		t.Error("starting inside a relocated worktree through a symlink must be refused")
	}
	if err := checkNotInWorktreeIn(bed, ws); err != nil {
		t.Errorf("the bed itself must be accepted: %v", err)
	}
	if err := checkNotInWorktreeIn(filepath.Join(real, "repos"), ws); err != nil {
		t.Errorf("a sibling of worktrees must be accepted: %v", err)
	}
}

func TestEnsureBareCloneAs_UsesGivenReposDir(t *testing.T) {
	skipIfNoGit(t)
	bed := t.TempDir()
	reposDir := filepath.Join(t.TempDir(), "ws", "repos")
	bare := filepath.Join(reposDir, "o-r.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	got, err := ensureBareCloneAs(reposDir, "o", "r", "operator", nil, false, "")
	if err != nil || got != bare {
		t.Fatalf("ensureBareCloneAs = %q, %v; want %q", got, err, bare)
	}
	if _, err := os.Stat(filepath.Join(bed, ".fabrik")); !os.IsNotExist(err) {
		t.Error("nothing may be created in the bed when the repos dir is elsewhere")
	}
}

func TestJanitorReapsWorktreeUnderSymlinkedExternalRoot(t *testing.T) {
	eng, wm, _ := janitorTestSetup(t)
	real, link := symlinkedRoot(t)

	// The worktree manager works on the real path; the engine's layout reaches
	// the same directory through the symlink.
	wm.rootDir = filepath.Join(real, "worktrees")
	eng.ws = workspace.Layout{Bed: eng.fabrikDir, Root: link}

	eng.client.(*mockGitHubClient).fetchIssueFn = func(owner, repo string, num int) (*gh.IssueData, error) {
		return &gh.IssueData{Number: num, State: "closed"}, nil
	}
	wtDir := createCleanWorktree(t, wm, 7)

	eng.runWorktreeJanitor(t.Context())

	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("a closed off-board worktree under a symlinked root must be reaped, still exists (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(eng.fabrikDir, ".fabrik", "worktrees")); !os.IsNotExist(err) {
		// janitorTestSetup pre-creates the in-bed dir; it must not hold the worktree.
		if entries, _ := os.ReadDir(filepath.Join(eng.fabrikDir, ".fabrik", "worktrees")); len(entries) != 0 {
			t.Errorf("the in-bed worktrees directory must not be the one scanned: %v", entries)
		}
	}
}

func TestJanitorKeepsOpenWorktreeUnderExternalRoot(t *testing.T) {
	eng, wm, _ := janitorTestSetup(t)
	root := filepath.Join(t.TempDir(), "ws")
	wm.rootDir = filepath.Join(root, "worktrees")
	eng.ws = workspace.Layout{Bed: eng.fabrikDir, Root: root}
	seedOpenOnStage(eng, "testowner/testrepo", 8, "Implement")
	wtDir := createCleanWorktree(t, wm, 8)

	eng.runWorktreeJanitor(t.Context())

	if _, err := os.Stat(wtDir); err != nil {
		t.Errorf("an open issue's worktree must survive the janitor: %v", err)
	}
}

// R5: the cwd reaper attributes a process to a worktree reached through a
// symlinked root, and still leaves an unrelated process alone.
func TestReapWorktreeProcesses_SymlinkedWorkspaceRoot(t *testing.T) {
	useRealLsofReaper(t)
	real, link := symlinkedRoot(t)
	wtReal := filepath.Join(real, "worktrees", "o-r", "issue-9")
	if err := os.MkdirAll(wtReal, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := spawnSetsidProcess(t, wtReal)
	outside := spawnSetsidProcess(t, t.TempDir())
	t.Cleanup(func() { _ = inside.Process.Kill(); _ = outside.Process.Kill() })
	waitForPGID(t, inside.Process.Pid)
	waitForPGID(t, outside.Process.Pid)

	var logs []string
	reapWorktreeProcesses(filepath.Join(link, "worktrees", "o-r", "issue-9"), 9, testLogf(&logs))

	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(inside.Process.Pid, 0) != syscall.ESRCH {
		if time.Now().After(deadline) {
			t.Fatalf("process in the moved worktree was not reaped through the symlink; logs: %v", logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(outside.Process.Pid, 0); err != nil {
		t.Errorf("an unrelated process must not be reaped: %v", err)
	}
}

// SC-002: with a workspace root set, an invocation's session, log and debug
// files land under the root and nothing is created in the bed.
func TestRealClaudeInvoker_WorkspaceRootKeepsChurnOutOfTheBed(t *testing.T) {
	bed := t.TempDir()
	t.Chdir(bed)
	binDir := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"result\":\"ok\",\"session_id\":\"sess_ws\",\"num_turns\":1,\"total_cost_usd\":0.001}'\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	root := filepath.Join(t.TempDir(), "ws")
	ws := workspace.Layout{Bed: bed, Root: root}
	if err := ws.Prepare(); err != nil {
		t.Fatal(err)
	}
	invoker := &RealClaudeInvoker{DebugOutput: true, WS: ws}
	stage := &stages.Stage{Name: "Test", Prompt: "p", Completion: stages.CompletionCriteria{Type: "claude"}}
	issue := gh.ProjectItem{Number: 12, Repo: "o/r", Title: "t"}
	if _, _, _, err := invoker.Invoke(context.Background(), stage, issue, nil, false, t.TempDir(), InvokeOptions{}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if data, err := os.ReadFile(filepath.Join(root, "sessions", "o-r", "issue-12", "Test.session")); err != nil || string(data) != "sess_ws" {
		t.Errorf("session file under the root: %v %q", err, data)
	}
	if got := ReadSessionID(ws, "o/r", 12, "Test"); got != "sess_ws" {
		t.Errorf("ReadSessionID through the layout = %q", got)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "debug")); err != nil || len(entries) == 0 {
		t.Errorf("debug output under the root: %v %v", entries, err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "logs", "o-r", "issue-12")); err != nil || len(entries) == 0 {
		t.Errorf("log output under the root: %v %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(bed, ".fabrik")); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(filepath.Join(bed, ".fabrik"))
		t.Errorf("nothing may be created under the bed's .fabrik, found %v", entries)
	}
}

// R6 at the engine boundary: New refuses a root change that would orphan live
// worktrees, before it touches anything under the new root.
func TestNew_RefusesWorkspaceRootChangeWithLiveWorktrees(t *testing.T) {
	bed := t.TempDir()
	t.Chdir(bed)
	wt := filepath.Join(bed, ".fabrik", "worktrees", "o-r", "issue-5")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	newRoot := filepath.Join(t.TempDir(), "ws")
	_, err := New(Config{WorkspaceRoot: newRoot})
	if err == nil {
		t.Fatal("New must refuse")
	}
	for _, want := range []string{wt, newRoot} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s: %v", want, err)
		}
	}
	if _, serr := os.Stat(newRoot); !os.IsNotExist(serr) {
		t.Error("the refused root must not be created")
	}
	if _, serr := os.Stat(wt); serr != nil {
		t.Error("the live worktree must be untouched")
	}
}

func TestNew_RefusesUnusableWorkspaceRoot(t *testing.T) {
	bed := t.TempDir()
	t.Chdir(bed)
	missing := filepath.Join(t.TempDir(), "unmounted", "ws")
	_, err := New(Config{WorkspaceRoot: missing})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("want an error naming %s, got %v", missing, err)
	}
	if _, serr := os.Stat(filepath.Dir(missing)); !os.IsNotExist(serr) {
		t.Error("a missing mount path must never be created")
	}
	if _, serr := os.Stat(filepath.Join(bed, ".fabrik")); !os.IsNotExist(serr) {
		t.Error("there must be no silent fallback to the in-bed workspace")
	}
}
