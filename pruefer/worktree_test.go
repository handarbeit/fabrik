package pruefer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

// TestCloneRef_NoFetchHeadInClone (#2192): FETCH_HEAD records the fetched URL,
// which for a real review carries the installation token; the clone is
// readable by the reviewer, so cloneRef must not leave the file behind.
func TestCloneRef_NoFetchHeadInClone(t *testing.T) {
	skipIfNoGit(t)
	repoPath, _ := initSourceRepoWithPullRef(t, 43)

	dir, cleanup, err := cloneRef(context.Background(), repoPath, "tok-123", "refs/pull/43/head", 43)
	defer cleanup()
	if err != nil {
		t.Fatalf("cloneRef: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "FETCH_HEAD")); err == nil {
		t.Error("FETCH_HEAD left in the review clone")
	}
}

// initSourceRepoNoPullRef creates a local repo whose head commit is reachable
// from main and has no refs/pull/* ref at all (GitHub failed to create one).
// Returns the repo path and the head commit's SHA.
func initSourceRepoNoPullRef(t *testing.T) (repoPath, headSHA string) {
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
	for _, f := range []string{"README.md", "feature.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		run("add", f)
		run("commit", "-q", "-m", "add "+f)
	}
	return dir, run("rev-parse", "HEAD")
}

// recordGit wraps the runGitFn seam, recording every git argv, and restores
// the original on cleanup. rewrite may alter or fail an invocation.
func recordGit(t *testing.T, rewrite func(args []string) ([]string, error)) *[][]string {
	t.Helper()
	var calls [][]string
	orig := runGitFn
	t.Cleanup(func() { runGitFn = orig })
	runGitFn = func(ctx context.Context, dir, token string, args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		if rewrite != nil {
			newArgs, err := rewrite(args)
			if err != nil {
				return err
			}
			args = newArgs
		}
		return orig(ctx, dir, token, args...)
	}
	return &calls
}

// captureCloneLog captures Logf lines for the duration of the test.
func captureCloneLog(t *testing.T) *[]string {
	t.Helper()
	var lines []string
	orig := Logf
	t.Cleanup(func() { Logf = orig })
	Logf = func(prNumber int, tag, format string, args ...any) {
		lines = append(lines, tag+": "+strings.TrimSpace(fmt.Sprintf(format, args...)))
	}
	return &lines
}

func countRoute(lines []string, route string) int {
	n := 0
	for _, l := range lines {
		if l == "clone: route="+route {
			n++
		}
	}
	return n
}

func headOf(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestCloneRefAt_SHAFallbackWhenPullRefMissing(t *testing.T) {
	skipIfNoGit(t)
	repoPath, sha := initSourceRepoNoPullRef(t)
	logs := captureCloneLog(t)

	dir, cleanup, err := cloneRefAt(context.Background(), repoPath, "", "refs/pull/12/head", sha, 12)
	defer cleanup()
	if err != nil {
		t.Fatalf("cloneRefAt: %v", err)
	}
	if got := headOf(t, dir); got != sha {
		t.Errorf("HEAD = %s, want %s", got, sha)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "FETCH_HEAD")); err == nil {
		t.Error("FETCH_HEAD left in the clone on the fallback route")
	}
	if n := countRoute(*logs, routeHeadSHAFall); n != 1 {
		t.Errorf("fallback route logged %d times, want 1: %v", n, *logs)
	}
	if n := countRoute(*logs, routePullRef); n != 0 {
		t.Errorf("pull ref route logged on a fallback clone: %v", *logs)
	}
}

func TestCloneRefAt_FallbackFetchesDifferentCommitFails(t *testing.T) {
	skipIfNoGit(t)
	repoPath, sha := initSourceRepoNoPullRef(t)
	cmd := exec.Command("git", "rev-parse", "HEAD~1")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	other := strings.TrimSpace(string(out))

	// The SHA fetch is rewritten to request a different commit than the one
	// selected; verification must refuse it.
	recordGit(t, func(args []string) ([]string, error) {
		if len(args) > 0 && args[0] == "fetch" && args[len(args)-1] == sha {
			rewritten := append([]string(nil), args...)
			rewritten[len(rewritten)-1] = other
			return rewritten, nil
		}
		return args, nil
	})

	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "pruefer-pr-12-*"))
	dir, cleanup, err := cloneRefAt(context.Background(), repoPath, "", "refs/pull/12/head", sha, 12)
	defer cleanup()
	if err == nil {
		t.Fatal("expected a mismatch error")
	}
	if dir != "" {
		t.Errorf("dir = %q on failure, want empty", dir)
	}
	if !strings.Contains(err.Error(), "not the selected head") {
		t.Errorf("error does not report the mismatch: %v", err)
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "pruefer-pr-12-*"))
	if len(after) > len(before) {
		t.Errorf("clone dir leaked after mismatch: before=%v after=%v", before, after)
	}
}

func TestCloneRefAt_PullRefRouteMismatchFails(t *testing.T) {
	skipIfNoGit(t)
	repoPath, _ := initSourceRepoWithPullRef(t, 5)
	otherSHA := strings.Repeat("a", 40)

	_, cleanup, err := cloneRefAt(context.Background(), repoPath, "", "refs/pull/5/head", otherSHA, 5)
	defer cleanup()
	if err == nil || !strings.Contains(err.Error(), "not the selected head") {
		t.Fatalf("expected a mismatch error on the pull ref route, got %v", err)
	}
}

func TestCloneRefAt_PullRefExistsPinsPrimaryPath(t *testing.T) {
	skipIfNoGit(t)
	repoPath, sha := initSourceRepoWithPullRef(t, 9)
	logs := captureCloneLog(t)
	calls := recordGit(t, nil)
	var gitOut [][]string
	orig := gitOutputFn
	t.Cleanup(func() { gitOutputFn = orig })
	gitOutputFn = func(ctx context.Context, dir, token string, args ...string) (string, error) {
		gitOut = append(gitOut, args)
		return orig(ctx, dir, token, args...)
	}

	dir, cleanup, err := cloneRefAt(context.Background(), repoPath, "", "refs/pull/9/head", sha, 9)
	defer cleanup()
	if err != nil {
		t.Fatalf("cloneRefAt: %v", err)
	}
	if headOf(t, dir) != sha {
		t.Error("HEAD is not the selected SHA")
	}
	want := [][]string{
		{"init", "-q"},
		{"fetch", "--depth", "1", "-q", repoPath, "refs/pull/9/head"},
		{"checkout", "-q", "FETCH_HEAD"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("git invocations = %v, want %v", *calls, want)
	}
	if !reflect.DeepEqual(gitOut, [][]string{{"rev-parse", "HEAD"}}) {
		t.Errorf("read-only invocations = %v, want only rev-parse HEAD", gitOut)
	}
	if countRoute(*logs, routePullRef) != 1 || countRoute(*logs, routeHeadSHAFall) != 0 {
		t.Errorf("route log = %v", *logs)
	}
}

func TestCloneRefAt_OtherFetchFailureDoesNotFallBack(t *testing.T) {
	skipIfNoGit(t)
	repoPath, sha := initSourceRepoNoPullRef(t)
	calls := recordGit(t, func(args []string) ([]string, error) {
		if len(args) > 0 && args[0] == "fetch" {
			return nil, errors.New("git fetch: exit status 128: fatal: Authentication failed")
		}
		return args, nil
	})

	_, cleanup, err := cloneRefAt(context.Background(), repoPath, "", "refs/pull/12/head", sha, 12)
	defer cleanup()
	if err == nil || !strings.Contains(err.Error(), "fetching refs/pull/12/head") || !strings.Contains(err.Error(), "Authentication failed") {
		t.Fatalf("expected the primary failure unchanged, got %v", err)
	}
	fetches := 0
	for _, c := range *calls {
		if c[0] == "fetch" {
			fetches++
		}
	}
	if fetches != 1 {
		t.Errorf("fetch issued %d times, want 1 (no fallback)", fetches)
	}
}

func TestCloneRefAt_NoUsableSHASkipsFallback(t *testing.T) {
	skipIfNoGit(t)
	repoPath, _ := initSourceRepoNoPullRef(t)
	for _, sha := range []string{"", "abc123", strings.Repeat("g", 40)} {
		calls := recordGit(t, nil)
		_, cleanup, err := cloneRefAt(context.Background(), repoPath, "", "refs/pull/12/head", sha, 12)
		cleanup()
		if err == nil || !isMissingRefErr(err) {
			t.Errorf("sha %q: expected the original missing-ref failure, got %v", sha, err)
		}
		for _, c := range *calls {
			if c[0] == "fetch" && c[len(c)-1] == sha && sha != "" {
				t.Errorf("sha %q: fallback fetch issued", sha)
			}
		}
	}
}

func TestCloneRefAt_EmptySHAWithPullRefSkipsVerification(t *testing.T) {
	skipIfNoGit(t)
	repoPath, sha := initSourceRepoWithPullRef(t, 3)
	orig := gitOutputFn
	t.Cleanup(func() { gitOutputFn = orig })
	gitOutputFn = func(context.Context, string, string, ...string) (string, error) {
		t.Error("rev-parse issued with no selected SHA")
		return "", nil
	}
	dir, cleanup, err := cloneRefAt(context.Background(), repoPath, "", "refs/pull/3/head", "", 3)
	defer cleanup()
	if err != nil {
		t.Fatalf("cloneRefAt: %v", err)
	}
	if headOf(t, dir) != sha {
		t.Error("HEAD is not the PR head")
	}
}

func TestCloneRefAt_FallbackFetchFailureNamesBothSteps(t *testing.T) {
	skipIfNoGit(t)
	repoPath, _ := initSourceRepoNoPullRef(t)
	missing := strings.Repeat("deadbeef", 5)

	_, cleanup, err := cloneRefAt(context.Background(), repoPath, "", "refs/pull/12/head", missing, 12)
	defer cleanup()
	if err == nil {
		t.Fatal("expected the fallback to fail for a SHA the remote does not have")
	}
	if !strings.Contains(err.Error(), "head SHA fallback fetch") || !strings.Contains(err.Error(), "couldn't find remote ref refs/pull/12/head") {
		t.Errorf("error should name the fallback and the original missing ref: %v", err)
	}
}

func TestCloneRefAt_FallbackUsesSameURLAndRedactsToken(t *testing.T) {
	skipIfNoGit(t)
	repoPath, _ := initSourceRepoNoPullRef(t)
	const tok = "ghs_secret_fallback_token"
	missing := strings.Repeat("deadbeef", 5)
	// Record the URL of each fetch, then point the real invocation at the
	// local repo so no test touches the network.
	remote := "https://x-access-token:" + tok + "@example.invalid/o/r.git"
	var urls []string
	recordGit(t, func(args []string) ([]string, error) {
		if args[0] == "fetch" {
			urls = append(urls, args[len(args)-2])
			rewritten := append([]string(nil), args...)
			rewritten[len(rewritten)-2] = repoPath
			return rewritten, nil
		}
		return args, nil
	})

	_, cleanup, err := cloneRefAt(context.Background(), remote, tok, "refs/pull/12/head", missing, 12)
	defer cleanup()
	if err == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(err.Error(), tok) {
		t.Errorf("error leaked the token: %v", err)
	}
	if len(urls) != 2 || urls[0] != remote || urls[1] != remote {
		t.Errorf("fetch URLs = %v, want both the base-repository URL", urls)
	}
}
