package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
}

// initBareRepo creates a minimal git repo with one commit in a temp dir.
func initBareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %s: %v", args, out, err)
		}
	}
	return dir
}

func TestNewWorktreeManager(t *testing.T) {
	wm := NewWorktreeManager("/some/repo")
	if wm.baseDir != "/some/repo" {
		t.Errorf("baseDir = %q", wm.baseDir)
	}
	if wm.rootDir != "/some/repo/.fabrik/worktrees" {
		t.Errorf("rootDir = %q", wm.rootDir)
	}
}

func TestWorktreeDir(t *testing.T) {
	wm := NewWorktreeManager("/repo")
	dir := wm.WorktreeDir(42)
	if !strings.HasSuffix(dir, "issue-42") {
		t.Errorf("WorktreeDir(42) = %q", dir)
	}
}

func TestEnsureWorktree_CreatesAndReturns(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	wtDir, err := wm.EnsureWorktree(99, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	// Check the worktree directory exists
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Fatal("worktree directory not created")
	}

	// Check the branch is correct
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = wtDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("checking branch: %v", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch != "fabrik/issue-99" {
		t.Errorf("branch = %q, want fabrik/issue-99", branch)
	}
}

func TestEnsureWorktree_Idempotent(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	dir1, err := wm.EnsureWorktree(1, "main", false)
	if err != nil {
		t.Fatalf("first EnsureWorktree: %v", err)
	}

	dir2, err := wm.EnsureWorktree(1, "main", false)
	if err != nil {
		t.Fatalf("second EnsureWorktree: %v", err)
	}

	if dir1 != dir2 {
		t.Errorf("idempotent call returned different dirs: %q vs %q", dir1, dir2)
	}
}

func TestCleanupWorktree(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	wtDir, err := wm.EnsureWorktree(5, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	if err := wm.CleanupWorktree(5, true); err != nil {
		t.Fatalf("CleanupWorktree: %v", err)
	}

	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Error("worktree directory should be removed")
	}

	// Branch should be deleted
	if wm.branchExists("fabrik/issue-5") {
		t.Error("branch should be deleted")
	}
}

func TestCleanupWorktree_KeepBranch(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	_, err := wm.EnsureWorktree(7, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	if err := wm.CleanupWorktree(7, false); err != nil {
		t.Fatalf("CleanupWorktree: %v", err)
	}

	// Branch should still exist
	if !wm.branchExists("fabrik/issue-7") {
		t.Error("branch should still exist when deleteBranch=false")
	}
}

func TestDefaultBaseBranch_Main(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	// The default init branch is typically "main" in modern git
	branch, err := wm.DefaultBaseBranch()
	if err != nil {
		t.Fatalf("DefaultBaseBranch error: %v", err)
	}
	if branch != "main" && branch != "master" {
		t.Errorf("DefaultBaseBranch = %q, expected main or master", branch)
	}
}

func TestBranchExists(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	// Default branch should exist
	defBranch, err := wm.DefaultBaseBranch()
	if err != nil {
		t.Fatalf("DefaultBaseBranch error: %v", err)
	}
	if !wm.branchExists(defBranch) {
		t.Errorf("expected %q to exist", defBranch)
	}

	// Nonexistent branch
	if wm.branchExists("fabrik/nonexistent-branch-xyz") {
		t.Error("expected nonexistent branch to not exist")
	}
}

// TestRemoteBranchExists verifies the ls-remote probe against origin: true for a
// branch present on origin, false for one absent, and false (fail-safe) when no
// origin remote is configured at all (the ls-remote command itself errors).
func TestRemoteBranchExists(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)

	mustGit(t, srcDir, "branch", "release/present")

	if !wm.remoteBranchExists("release/present") {
		t.Error("expected release/present to exist on origin")
	}
	if wm.remoteBranchExists("release/absent") {
		t.Error("expected release/absent to not exist on origin")
	}

	// No origin remote configured at all — fail safe to false.
	noOriginWM := NewWorktreeManager(initBareRepo(t))
	if noOriginWM.remoteBranchExists("main") {
		t.Error("expected remoteBranchExists to fail safe to false with no origin configured")
	}
}

// TestResolveBaseLabelBranch_LocalHit verifies the fast path: a branch already
// present in the local clone's refs/remotes/origin/* is confirmed without any
// remote probe or fetch.
func TestResolveBaseLabelBranch_LocalHit(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	mustGitDir(t, wm.baseDir, "update-ref", "refs/remotes/origin/develop", "HEAD")

	if !wm.resolveBaseLabelBranch("develop", 1) {
		t.Error("expected local hit to resolve true")
	}
}

// TestResolveBaseLabelBranch_RemoteOnly verifies the miss path: a branch absent
// from the local clone but present on origin is fetched and then resolves true,
// and the local clone ends up with a resolvable refs/remotes/origin/<branch>.
func TestResolveBaseLabelBranch_RemoteOnly(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)
	mustGit(t, srcDir, "branch", "release/only-on-remote")

	if wm.branchExists("origin/release/only-on-remote") {
		t.Fatal("test setup invariant violated: branch should not yet be in the local clone")
	}

	if !wm.resolveBaseLabelBranch("release/only-on-remote", 1) {
		t.Error("expected remote-only branch to resolve true after fetch")
	}
	if !wm.branchExists("origin/release/only-on-remote") {
		t.Error("expected fetch to populate refs/remotes/origin/release/only-on-remote locally")
	}
}

// TestResolveBaseLabelBranch_Absent verifies a branch absent both locally and on
// origin resolves false (the genuine-fallback case).
func TestResolveBaseLabelBranch_Absent(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)

	if wm.resolveBaseLabelBranch("nonexistent-anywhere", 1) {
		t.Error("expected absent branch to resolve false")
	}
}

// TestFetchOrigin_SerializedUnderMu is a direct regression guard (found in review,
// #1648) for a newly-introduced race: since #1648, worktreesFor(repoKey) returns the
// SAME *WorktreeManager for every base partition of a repo, so two sibling-base
// merge-train workers now share one wm.baseDir. Every other mutation of that shared
// directory (resolveBaseLabelBranch, EnsureWorktree, PushBranch,
// ensureTrainWorktreeFromRef, ...) already serializes under wm.mu; FetchOrigin must
// too, or two concurrent sibling-base workers' fetches (or a fetch racing a
// worktree-creation call) can run against the bare clone unguarded. This test proves
// FetchOrigin actually blocks on wm.mu rather than merely documenting that it should:
// it holds the lock itself, launches FetchOrigin in a goroutine, and confirms the
// call does not return until the lock is released.
func TestFetchOrigin_SerializedUnderMu(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)

	wm.mu.Lock()
	done := make(chan struct{})
	go func() {
		wm.FetchOrigin()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("FetchOrigin returned before wm.mu was released — it is not serialized under wm.mu")
	case <-time.After(150 * time.Millisecond):
		// Still blocked, as expected.
	}

	wm.mu.Unlock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("FetchOrigin did not complete after wm.mu was released")
	}
}

// TestResolveBaseLabelBranch_RejectsOptionInjection verifies that a
// base:<branch> label value beginning with "-" (an attempted argument
// injection, e.g. "--upload-pack=<script>") is treated as a literal,
// nonexistent ref rather than parsed as a git option by the underlying
// `ls-remote`/`fetch` subprocess calls. Without the "--" separator ahead of
// the untrusted value, --upload-pack causes git to execute the given local
// program as a subprocess — this test fails loudly if that regresses.
func TestResolveBaseLabelBranch_RejectsOptionInjection(t *testing.T) {
	skipIfNoGit(t)
	tmp := t.TempDir()
	marker := filepath.Join(tmp, "pwned")
	script := filepath.Join(tmp, "pwn.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, _, _, wm := setupTrainRepo(t)
	malicious := "--upload-pack=" + script

	if wm.resolveBaseLabelBranch(malicious, 1) {
		t.Error("expected option-injection candidate to resolve false (not a real branch)")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("SECURITY: malicious --upload-pack argument executed a local script via git subprocess")
	}
}

func TestEnsureWorktree_StaleDirectoryPreserved(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	// Create a stale directory (not a git worktree)
	wtDir := wm.WorktreeDir(50)
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Write a dummy file so it's not empty
	os.WriteFile(filepath.Join(wtDir, "dummy"), []byte("stale"), 0644)

	// EnsureWorktree should preserve the stale dir (may contain partial work)
	resultDir, err := wm.EnsureWorktree(50, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree with stale dir: %v", err)
	}
	if resultDir != wtDir {
		t.Errorf("dir = %q, want %q", resultDir, wtDir)
	}

	// Verify the dummy file still exists (not destroyed)
	if _, err := os.Stat(filepath.Join(resultDir, "dummy")); err != nil {
		t.Error("stale directory contents should be preserved")
	}
}

func TestBranchName(t *testing.T) {
	wm := NewWorktreeManager("/repo")
	if name := wm.branchName(42); name != "fabrik/issue-42" {
		t.Errorf("branchName(42) = %q", name)
	}
}

func TestCleanupWorktree_NonexistentWorktree(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	// Cleaning up a worktree that doesn't exist should error
	err := wm.CleanupWorktree(999, false)
	if err == nil {
		t.Error("expected error for cleaning up nonexistent worktree")
	}
}

func TestDefaultBaseBranch_FallbackToMaster(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	// Init with master branch
	cmds := [][]string{
		{"git", "init", "-b", "master"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %s: %v", args, out, err)
		}
	}

	wm := NewWorktreeManager(dir)
	branch, err := wm.DefaultBaseBranch()
	if err != nil {
		t.Fatalf("DefaultBaseBranch error: %v", err)
	}
	if branch != "master" {
		t.Errorf("DefaultBaseBranch = %q, want master", branch)
	}
}

func TestDefaultBaseBranch_NeitherMainNorMaster(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	cmds := [][]string{
		{"git", "init", "-b", "develop"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %s: %v", args, out, err)
		}
	}

	wm := NewWorktreeManager(dir)
	branch, err := wm.DefaultBaseBranch()
	if err != nil {
		t.Fatalf("DefaultBaseBranch error: %v", err)
	}
	// git symbolic-ref HEAD should return the actual branch ("develop"), not a hardcoded fallback.
	if branch != "develop" {
		t.Errorf("DefaultBaseBranch = %q, want develop", branch)
	}
}

func TestDefaultBaseBranch_WithOriginHead(t *testing.T) {
	skipIfNoGit(t)

	// Create a bare "remote" repo with an initial commit
	remoteDir := t.TempDir()
	remoteCmds := [][]string{
		{"git", "init", "--bare"},
	}
	for _, args := range remoteCmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = remoteDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup remote %v: %s: %v", args, out, err)
		}
	}

	// Create a temporary repo, commit, and push to remote
	tmpDir := t.TempDir()
	setupCmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "remote", "add", "origin", remoteDir},
		{"git", "commit", "--allow-empty", "-m", "initial"},
		{"git", "push", "-u", "origin", "HEAD"},
	}
	for _, args := range setupCmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = tmpDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("setup %v: %s: %v", args, out, err)
		}
	}

	// Clone into the actual test directory (so origin/HEAD is set)
	localDir := filepath.Join(t.TempDir(), "repo")
	cmd := exec.Command("git", "clone", remoteDir, localDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("clone failed: %s: %v", out, err)
	}

	wm := NewWorktreeManager(localDir)
	branch, err := wm.DefaultBaseBranch()
	if err != nil {
		t.Fatalf("DefaultBaseBranch error: %v", err)
	}
	if branch != "main" && branch != "master" {
		t.Errorf("DefaultBaseBranch with origin = %q", branch)
	}
}

func TestDefaultBaseBranch_SymbolicRefHEAD(t *testing.T) {
	skipIfNoGit(t)

	// Create a "remote" repo with develop as the default branch.
	remoteDir := t.TempDir()
	remoteCmds := [][]string{
		{"git", "init", "-b", "develop"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range remoteCmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = remoteDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup remote %v: %s: %v", args, out, err)
		}
	}

	// Bare-clone it — simulates what ensureBareClone does.
	// git clone --bare sets the bare clone's HEAD to the remote's default branch
	// (refs/heads/develop) but does NOT populate refs/remotes/origin/HEAD.
	bareDir := filepath.Join(t.TempDir(), "repo.git")
	cloneCmd := exec.Command("git", "clone", "--bare", remoteDir, bareDir)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Skipf("bare clone failed: %s: %v", out, err)
	}

	wm := NewWorktreeManager(bareDir)
	branch, err := wm.DefaultBaseBranch()
	if err != nil {
		t.Fatalf("DefaultBaseBranch error: %v", err)
	}
	if branch != "develop" {
		t.Errorf("DefaultBaseBranch = %q, want develop (via git symbolic-ref HEAD on bare clone)", branch)
	}
}

func TestIsAlreadyGoneWorktreeError(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "already gone",
			out:  "fatal: '/some/path/merge-train-main-1785120046' is not a working tree",
			want: true,
		},
		{
			name: "genuine failure",
			out:  "fatal: '/some/path/merge-train-main-1785120046' contains modified or untracked files, use --force to delete it",
			want: false,
		},
		{
			name: "empty",
			out:  "",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAlreadyGoneWorktreeError([]byte(tt.out)); got != tt.want {
				t.Errorf("isAlreadyGoneWorktreeError(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

func TestIsAlreadyGoneRemoteBranchError(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "already gone",
			out:  "error: unable to delete 'fabrik/merge-train/merge-train-main-1785120046': remote ref does not exist",
			want: true,
		},
		{
			name: "genuine failure",
			out:  "error: unable to delete 'fabrik/merge-train/merge-train-main-1785120046': remote rejected (protected branch)",
			want: false,
		},
		{
			name: "empty",
			out:  "",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAlreadyGoneRemoteBranchError([]byte(tt.out)); got != tt.want {
				t.Errorf("isAlreadyGoneRemoteBranchError(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

func TestQuietGitSSHEnv(t *testing.T) {
	env := quietGitSSHEnv()
	var sshCmd string
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_SSH_COMMAND=") {
			sshCmd = kv
		}
	}
	if sshCmd == "" {
		t.Fatal("quietGitSSHEnv did not set GIT_SSH_COMMAND")
	}
	if !strings.Contains(sshCmd, "-o LogLevel=ERROR") {
		t.Errorf("GIT_SSH_COMMAND = %q, want -o LogLevel=ERROR to suppress known-hosts chatter", sshCmd)
	}
	if !strings.Contains(sshCmd, "-o StrictHostKeyChecking=accept-new") {
		t.Errorf("GIT_SSH_COMMAND = %q, want StrictHostKeyChecking=accept-new unchanged (host-key verification must not weaken)", sshCmd)
	}
}

// TestCleanupTrainWorktree_AlreadyGoneNoWarn locks in the false-positive fix: calling
// CleanupTrainWorktree a second time against already-removed artifacts (the reported
// "every successful landing" scenario, where an earlier cleanup already ran) must not
// emit warn: lines for the worktree or the remote branch.
func TestCleanupTrainWorktree_AlreadyGoneNoWarn(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)

	const trialName = "already-gone-trial"
	if _, err := wm.EnsureTrainWorktree(trialName, "main"); err != nil {
		t.Fatalf("EnsureTrainWorktree: %v", err)
	}
	if err := wm.PushTrainBranch(trialName); err != nil {
		t.Fatalf("PushTrainBranch: %v", err)
	}

	var firstLogs, secondLogs []string
	wm.logfFn = func(n int, tag, format string, args ...any) {
		firstLogs = append(firstLogs, fmt.Sprintf("[%s] "+format, append([]any{tag}, args...)...))
	}
	if err := wm.CleanupTrainWorktree(trialName, true); err != nil {
		t.Fatalf("first CleanupTrainWorktree: %v", err)
	}
	for _, line := range firstLogs {
		if strings.Contains(line, "warn:") {
			t.Errorf("first cleanup (artifacts genuinely present) unexpectedly warned: %s", line)
		}
	}

	wm.logfFn = func(n int, tag, format string, args ...any) {
		secondLogs = append(secondLogs, fmt.Sprintf("[%s] "+format, append([]any{tag}, args...)...))
	}
	if err := wm.CleanupTrainWorktree(trialName, true); err != nil {
		t.Fatalf("second CleanupTrainWorktree: %v", err)
	}
	for _, line := range secondLogs {
		if strings.Contains(line, "could not remove train worktree") {
			t.Errorf("second cleanup against an already-gone worktree warned: %s", line)
		}
		if strings.Contains(line, "could not delete remote trial branch") {
			t.Errorf("second cleanup against an already-gone remote branch warned: %s", line)
		}
	}
}

func TestEnsureWorktree_ExistingBranch(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	// Pre-create the branch
	cmd := exec.Command("git", "branch", "fabrik/issue-20", "main")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("creating branch: %s: %v", out, err)
	}

	// EnsureWorktree should use existing branch
	wtDir, err := wm.EnsureWorktree(20, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Fatal("worktree dir not created")
	}
}
