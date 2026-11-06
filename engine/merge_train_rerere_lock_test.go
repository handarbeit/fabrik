package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/testenv"
)

func rlGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// conflictingRepo returns a repo on branch "main" with a second branch "other" whose
// change to f.txt conflicts with main's, rerere enabled.
func conflictingRepo(t *testing.T) (dir, otherSHA string) {
	t.Helper()
	skipIfNoGit(t)
	testenv.IsolateGit(t, "")
	dir = t.TempDir()
	rlGit(t, dir, "init", "-q", "-b", "main")
	rlGit(t, dir, "config", "user.email", "t@example.com")
	rlGit(t, dir, "config", "user.name", "t")
	rlGit(t, dir, "config", "rerere.enabled", "true")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rlGit(t, dir, "add", ".")
	rlGit(t, dir, "commit", "-qm", "base")
	rlGit(t, dir, "checkout", "-qb", "other")
	_ = os.WriteFile(filepath.Join(dir, "f.txt"), []byte("other\n"), 0o644)
	rlGit(t, dir, "commit", "-qam", "other")
	otherSHA = rlGit(t, dir, "rev-parse", "HEAD")
	rlGit(t, dir, "checkout", "-q", "main")
	_ = os.WriteFile(filepath.Join(dir, "f.txt"), []byte("main\n"), 0o644)
	rlGit(t, dir, "commit", "-qam", "main")
	return dir, otherSHA
}

// R4b (#2542): a leftover MERGE_RR.lock must not turn the replay into a fatal-looking
// failure. Without the tolerance git refuses the merge with "Unable to create …
// MERGE_RR.lock: File exists"; with it the lock is removed, the merge is retried and
// reports its ordinary conflict.
func TestMergeToleratingRerereLock(t *testing.T) {
	dir, other := conflictingRepo(t)
	gitDir := rlGit(t, dir, "rev-parse", "--absolute-git-dir")
	lock := filepath.Join(gitDir, rerereLockName)
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Reproduction: a plain merge fails on the lock.
	plain := exec.Command("git", "merge", "--no-ff", "--no-edit", other)
	plain.Dir = dir
	out, err := plain.CombinedOutput()
	if err == nil || !strings.Contains(string(out), rerereLockName) {
		t.Skipf("this git does not fail on a planted %s (err=%v): %s", rerereLockName, err, out)
	}
	rlGit(t, dir, "merge", "--abort")

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	out, err = eng.mergeToleratingRerereLock(context.Background(), dir, other)
	if err == nil {
		t.Fatal("the merge conflicts, so it still reports an error")
	}
	if strings.Contains(string(out), rerereLockName) || strings.Contains(string(out), "fatal:") {
		t.Fatalf("the retry must reach the real conflict, got: %s", out)
	}
	if !strings.Contains(string(out), "CONFLICT") {
		t.Fatalf("expected a content conflict, got: %s", out)
	}
	if _, statErr := os.Stat(lock); statErr == nil {
		t.Error("the leftover lock should have been removed")
	}
}

// With no lock the helper is a plain merge: one run, output unchanged.
func TestMergeToleratingRerereLock_NoLockIsPlainMerge(t *testing.T) {
	dir, other := conflictingRepo(t)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	out, err := eng.mergeToleratingRerereLock(context.Background(), dir, other)
	if err == nil || !strings.Contains(string(out), "CONFLICT") {
		t.Fatalf("want an ordinary conflict, got err=%v out=%s", err, out)
	}
}

func TestDropRerereLockLines(t *testing.T) {
	in := "Auto-merging f.txt\nCONFLICT (content): Merge conflict in f.txt\nfatal: Unable to create '/x/.git/worktrees/w/MERGE_RR.lock': File exists.\n\nAnother git process seems to be running in this repository, e.g.\nan editor opened by 'git commit'.\nIf no other git process is currently running, this probably means a\ngit process crashed in this repository earlier."
	got := dropRerereLockLines(in)
	if strings.Contains(got, "fatal:") || strings.Contains(got, "MERGE_RR") || strings.Contains(got, "Another git process") {
		t.Errorf("lock lines survived: %q", got)
	}
	if !strings.Contains(got, "CONFLICT (content)") {
		t.Errorf("other lines must survive: %q", got)
	}
}
