package pruefer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
}

// initSourceRepoWithPullRef creates a local git repo with one commit on
// main, and a second commit reachable only via refs/pull/<prNumber>/head
// (simulating a GitHub PR head ref that isn't on any branch). Returns the
// repo's path and the SHA of the PR-head commit.
func initSourceRepoWithPullRef(t *testing.T, prNumber int) (repoPath, headSHA string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "initial commit")

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("pr change\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "feature.txt")
	run("commit", "-q", "-m", "pr commit")
	sha := run("rev-parse", "HEAD")

	// Detach the PR commit from main and expose it only as refs/pull/N/head,
	// mirroring how GitHub exposes PR heads that aren't on a real branch.
	run("update-ref", "refs/pull/"+strconv.Itoa(prNumber)+"/head", sha)
	run("reset", "-q", "--hard", "HEAD~1")

	return dir, sha
}

func TestCloneRef_ChecksOutPRHead(t *testing.T) {
	skipIfNoGit(t)
	repoPath, headSHA := initSourceRepoWithPullRef(t, 42)

	dir, cleanup, err := cloneRef(context.Background(), repoPath, "", "refs/pull/42/head", 42)
	defer cleanup()
	if err != nil {
		t.Fatalf("cloneRef: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "feature.txt")); err != nil {
		t.Errorf("expected feature.txt (only present at the PR head) to exist in clone: %v", err)
	}

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != headSHA {
		t.Errorf("checked-out HEAD = %s, want %s", got, headSHA)
	}
}

func TestCloneRef_CleanupRemovesDir(t *testing.T) {
	skipIfNoGit(t)
	repoPath, _ := initSourceRepoWithPullRef(t, 7)

	dir, cleanup, err := cloneRef(context.Background(), repoPath, "", "refs/pull/7/head", 7)
	if err != nil {
		t.Fatalf("cloneRef: %v", err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("expected clone dir to be removed after cleanup(), stat err = %v", err)
	}
}

func TestCloneRef_MissingRefFails(t *testing.T) {
	skipIfNoGit(t)
	repoPath, _ := initSourceRepoWithPullRef(t, 7)

	_, cleanup, err := cloneRef(context.Background(), repoPath, "", "refs/pull/999/head", 999)
	defer cleanup()
	if err == nil {
		t.Fatal("expected an error when the ref does not exist")
	}
}

func TestRunGit_RedactsTokenFromErrorOutput(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	const secretToken = "ghs_super_secret_token_value"
	err := runGit(context.Background(), dir, secretToken, "fetch", "--depth", "1", "https://x-access-token:"+secretToken+"@example.invalid/owner/repo.git", "refs/pull/1/head")
	if err == nil {
		t.Fatal("expected an error fetching from a nonexistent host")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Errorf("error message leaked the token: %v", err)
	}
}

func TestRedactArgs(t *testing.T) {
	args := []string{"fetch", "https://x-access-token:secret123@github.com/o/r.git", "refs/pull/1/head"}
	got := redactArgs(args, "secret123")
	for _, a := range got {
		if strings.Contains(a, "secret123") {
			t.Errorf("redactArgs left the token in place: %v", got)
		}
	}
	if got[0] != "fetch" || got[2] != "refs/pull/1/head" {
		t.Errorf("redactArgs modified non-token args: %v", got)
	}
}

func TestCloneURL_EmbedsCredentials(t *testing.T) {
	url := cloneURL("owner", "repo", "tok123")
	want := "https://x-access-token:tok123@github.com/owner/repo.git"
	if url != want {
		t.Errorf("cloneURL = %q, want %q", url, want)
	}
}
