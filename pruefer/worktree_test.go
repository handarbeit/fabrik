package pruefer

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
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

func TestRedactSecrets(t *testing.T) {
	const tok = "ghs_Tok+/en_0123"
	raw := []byte("x-access-token:" + tok)
	forms := map[string]string{
		"raw token":        tok,
		"std base64":       base64.StdEncoding.EncodeToString(raw),
		"raw std base64":   base64.RawStdEncoding.EncodeToString(raw),
		"url base64":       base64.URLEncoding.EncodeToString(raw),
		"raw url base64":   base64.RawURLEncoding.EncodeToString(raw),
		"header line":      "Authorization: Basic " + basicAuthValue(tok),
		"header line, URL": "Authorization: Basic " + base64.URLEncoding.EncodeToString(raw),
	}
	for name, form := range forms {
		got := redactSecrets("before "+form+" after", tok)
		if strings.Contains(got, tok) || strings.Contains(got, form) {
			t.Errorf("%s: secret left in place: %q", name, got)
		}
		if got != "before *** after" {
			t.Errorf("%s: redacted = %q, want %q (the header line must be masked whole)", name, got, "before *** after")
		}
	}
	if got := redactSecrets("nothing secret here", tok); got != "nothing secret here" {
		t.Errorf("non-secret text modified: %q", got)
	}
	if got := redactSecrets("anything", ""); got != "anything" {
		t.Errorf("empty token must be a no-op, got %q", got)
	}
}

func TestRedactArgs(t *testing.T) {
	const tok = "secret123"
	args := []string{"fetch", "-c", "x=" + basicAuthValue(tok), "https://github.com/o/r.git?t=" + tok, "refs/pull/1/head"}
	got := redactArgs(args, tok)
	for _, a := range got {
		if strings.Contains(a, tok) || strings.Contains(a, basicAuthValue(tok)) {
			t.Errorf("redactArgs left a secret in place: %v", got)
		}
	}
	if got[0] != "fetch" || got[4] != "refs/pull/1/head" {
		t.Errorf("redactArgs modified non-secret args: %v", got)
	}
}

func TestCloneURL_NoUserinfo(t *testing.T) {
	url := cloneURL("owner", "repo")
	want := "https://github.com/owner/repo.git"
	if url != want {
		t.Errorf("cloneURL = %q, want %q", url, want)
	}
	if strings.Contains(url, "@") {
		t.Errorf("cloneURL carries userinfo: %q", url)
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
	remote := "https://example.invalid/o/r.git"
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

// TestCloneForReview_NoCredentialInArgv (#2622): no argument of any git
// command on the clone path, the head-SHA fallback included, carries the
// token, its encoded form or a userinfo URL. Restoring the userinfo URL in
// cloneURL makes this fail.
func TestCloneForReview_NoCredentialInArgv(t *testing.T) {
	const tok = "ghs_distinctive_argv_token"
	sha := strings.Repeat("ab", 20)
	var argvs [][]string
	origRun, origOut := runGitFn, gitOutputFn
	t.Cleanup(func() { runGitFn, gitOutputFn = origRun, origOut })
	runGitFn = func(_ context.Context, _, _ string, args ...string) error {
		argvs = append(argvs, append([]string(nil), args...))
		if args[0] == "fetch" && strings.HasPrefix(args[len(args)-1], "refs/pull/") {
			return errors.New("fatal: couldn't find remote ref refs/pull/77/head")
		}
		return nil
	}
	gitOutputFn = func(_ context.Context, _, _ string, args ...string) (string, error) {
		argvs = append(argvs, append([]string(nil), args...))
		return sha, nil
	}

	_, cleanup, err := CloneForReview(context.Background(), "o", "r", tok, 77, sha)
	defer cleanup()
	if err != nil {
		t.Fatalf("CloneForReview: %v", err)
	}
	fetches := 0
	for _, argv := range argvs {
		for _, a := range argv {
			for _, bad := range []string{tok, basicAuthValue(tok), "x-access-token", "@"} {
				if strings.Contains(a, bad) {
					t.Errorf("argv %v carries %q", argv, bad)
				}
			}
		}
		if argv[0] == "fetch" {
			fetches++
			if got := argv[len(argv)-2]; got != "https://github.com/o/r.git" {
				t.Errorf("fetch remote = %q, want the plain URL", got)
			}
		}
	}
	if fetches != 2 {
		t.Errorf("fetches = %d, want the pull-ref fetch and the head SHA fallback", fetches)
	}
}

func envHasEntry(env []string, entry string) bool {
	for _, e := range env {
		if e == entry {
			return true
		}
	}
	return false
}

func TestGitCredentialEnv(t *testing.T) {
	const tok = "ghs_env_token"
	header := "Authorization: Basic " + basicAuthValue(tok)
	fetch := []string{"fetch", "--depth", "1", "-q", "https://github.com/o/r.git", "refs/pull/1/head"}

	t.Run("https fetch gets a scoped header", func(t *testing.T) {
		base := []string{"PATH=/usr/bin"}
		got := gitCredentialEnv(base, fetch, tok)
		for _, want := range []string{
			"PATH=/usr/bin",
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader",
			"GIT_CONFIG_VALUE_0=" + header,
		} {
			if !envHasEntry(got, want) {
				t.Errorf("env lacks %q: %v", want, got)
			}
		}
	})
	t.Run("unchanged cases", func(t *testing.T) {
		base := []string{"PATH=/usr/bin"}
		cases := map[string]struct {
			args  []string
			token string
		}{
			"empty token":    {fetch, ""},
			"init":           {[]string{"init", "-q"}, tok},
			"checkout":       {[]string{"checkout", "-q", "FETCH_HEAD"}, tok},
			"rev-parse":      {[]string{"rev-parse", "HEAD"}, tok},
			"local path":     {[]string{"fetch", "-q", "/tmp/some/repo", "refs/pull/1/head"}, tok},
			"file url":       {[]string{"fetch", "-q", "file:///tmp/some/repo", "refs/pull/1/head"}, tok},
			"cleartext http": {[]string{"fetch", "-q", "http://github.com/o/r.git", "refs/pull/1/head"}, tok},
			"no args":        {nil, tok},
		}
		for name, c := range cases {
			if got := gitCredentialEnv(base, c.args, c.token); !reflect.DeepEqual(got, base) {
				t.Errorf("%s: env changed: %v", name, got)
			}
		}
	})
	t.Run("existing config entries are kept", func(t *testing.T) {
		base := []string{"A=1", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=k0", "GIT_CONFIG_VALUE_0=v0", "GIT_CONFIG_KEY_1=k1", "GIT_CONFIG_VALUE_1=v1"}
		orig := append([]string(nil), base...)
		got := gitCredentialEnv(base, fetch, tok)
		if !reflect.DeepEqual(base, orig) {
			t.Errorf("input mutated: %v", base)
		}
		counts := 0
		for _, e := range got {
			if strings.HasPrefix(e, "GIT_CONFIG_COUNT=") {
				counts++
			}
		}
		if counts != 1 || !envHasEntry(got, "GIT_CONFIG_COUNT=3") {
			t.Errorf("want exactly one GIT_CONFIG_COUNT=3: %v", got)
		}
		for _, want := range []string{"GIT_CONFIG_KEY_0=k0", "GIT_CONFIG_VALUE_1=v1", "GIT_CONFIG_KEY_2=http.https://github.com/.extraHeader", "GIT_CONFIG_VALUE_2=" + header} {
			if !envHasEntry(got, want) {
				t.Errorf("env lacks %q: %v", want, got)
			}
		}
	})
	t.Run("malformed or negative count is zero", func(t *testing.T) {
		for _, c := range []string{"GIT_CONFIG_COUNT=abc", "GIT_CONFIG_COUNT=-4", "GIT_CONFIG_COUNT="} {
			got := gitCredentialEnv([]string{c}, fetch, tok)
			if !envHasEntry(got, "GIT_CONFIG_COUNT=1") || !envHasEntry(got, "GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader") || envHasEntry(got, c) {
				t.Errorf("%s: env = %v", c, got)
			}
		}
	})
	t.Run("port is part of the scope", func(t *testing.T) {
		got := gitCredentialEnv(nil, []string{"fetch", "https://127.0.0.1:8443/o/r.git", "x"}, tok)
		if !envHasEntry(got, "GIT_CONFIG_KEY_0=http.https://127.0.0.1:8443/.extraHeader") {
			t.Errorf("env = %v", got)
		}
	})
}

// installShellGit installs a `git` shell script first on PATH that logs its argv and
// GIT_CONFIG_* environment to a file and, for fetch, fails echoing the
// credential in every encoded form. It returns the log path.
func installShellGit(t *testing.T, tok string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake git")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "git.log")
	script := `#!/bin/sh
{ echo "ARGV $*"; env | grep '^GIT_CONFIG_' | sed 's/^/ENV /'; echo END; } >> "$FAKE_LOG"
if [ "$1" = fetch ]; then
  echo "fatal: Authorization: Basic $FAKE_B64 raw $FAKE_TOKEN std $FAKE_B64 url $FAKE_B64URL rawurl $FAKE_B64RAWURL" >&2
  exit 128
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte("x-access-token:" + tok)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", log)
	t.Setenv("FAKE_TOKEN", tok)
	t.Setenv("FAKE_B64", base64.StdEncoding.EncodeToString(raw))
	t.Setenv("FAKE_B64URL", base64.URLEncoding.EncodeToString(raw))
	t.Setenv("FAKE_B64RAWURL", base64.RawURLEncoding.EncodeToString(raw))
	return log
}

// TestRunGit_RealExecCredentialEnvAndRedaction exercises the actual exec path:
// the fetch process gets the header in its environment and nothing in argv,
// the other commands get none, the daemon's environment is untouched, and the
// error text carries no form of the credential on either wrapper.
func TestRunGit_RealExecCredentialEnvAndRedaction(t *testing.T) {
	const tok = "ghs_fake_exec_Tok+/en"
	t.Setenv("GIT_CONFIG_COUNT", "") // hermetic; also lets us see it is not mutated
	log := installShellGit(t, tok)
	dir := t.TempDir()
	remote := "https://github.com/o/r.git"

	_ = runGit(context.Background(), dir, tok, "init", "-q")
	errRun := runGit(context.Background(), dir, tok, "fetch", "--depth", "1", "-q", remote, "refs/pull/1/head")
	_ = runGit(context.Background(), dir, tok, "checkout", "-q", "FETCH_HEAD")
	_, errOut := gitOutput(context.Background(), dir, tok, "fetch", "-q", remote, "refs/pull/1/head")

	for name, err := range map[string]error{"runGit": errRun, "gitOutput": errOut} {
		if err == nil {
			t.Fatalf("%s: expected the fake fetch to fail", name)
		}
		raw := []byte("x-access-token:" + tok)
		for _, form := range []string{tok, base64.StdEncoding.EncodeToString(raw), base64.URLEncoding.EncodeToString(raw), base64.RawURLEncoding.EncodeToString(raw)} {
			if strings.Contains(err.Error(), form) {
				t.Errorf("%s: error leaked %q: %v", name, form, err)
			}
		}
		if !strings.Contains(err.Error(), "fatal:") {
			t.Errorf("%s: error lost git's output: %v", name, err)
		}
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	header := "Authorization: Basic " + basicAuthValue(tok)
	var cur string
	var hasHeader bool
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "ARGV "):
			cur, hasHeader = strings.TrimPrefix(line, "ARGV "), false
			if strings.Contains(line, tok) || strings.Contains(line, basicAuthValue(tok)) {
				t.Errorf("argv carries the credential: %s", line)
			}
		case strings.HasPrefix(line, "ENV ") && strings.Contains(line, "GIT_CONFIG_VALUE_") && strings.HasSuffix(line, header):
			hasHeader = true
		case line == "END":
			verb := strings.Fields(cur)[0]
			seen[verb] = true
			if want := verb == "fetch"; hasHeader != want {
				t.Errorf("git %s: credential env present = %v, want %v", verb, hasHeader, want)
			}
		}
	}
	for _, verb := range []string{"init", "fetch", "checkout"} {
		if !seen[verb] {
			t.Errorf("fake git never saw %s", verb)
		}
	}
	if got := os.Getenv("GIT_CONFIG_COUNT"); got != "" {
		t.Errorf("daemon GIT_CONFIG_COUNT mutated to %q", got)
	}
	for _, e := range os.Environ() {
		// FAKE_* are this test's own channel to the fake git script.
		if !strings.HasPrefix(e, "FAKE_") && (strings.Contains(e, tok) || strings.Contains(e, basicAuthValue(tok))) {
			t.Errorf("daemon environment holds the credential: %s", e)
		}
	}
}

// serveGitTLS serves bare repository src (refs fetched with refspec) over
// HTTPS through git-http-backend, answering 401 unless every request carries
// the exact Basic header for tok. It returns the base URL and a pointer to
// the number of authorised requests served.
func serveGitTLS(t *testing.T, src, refspec, tok string) (baseURL string, authorised *atomic.Int64) {
	t.Helper()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not installed")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "o", "r.git")
	for _, args := range [][]string{{"init", "-q", "--bare", bare}, {"-C", bare, "fetch", "-q", src, refspec}} {
		if o, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, o)
		}
	}
	want := "Basic " + basicAuthValue(tok)
	authorised = new(atomic.Int64)
	cgiH := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		authorised.Add(1)
		cgiH.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GIT_SSL_NO_VERIFY", "true")
	return srv.URL, authorised
}

// walkFilesFor reports every file under root (including .git) containing any
// of the needles.
func walkFilesFor(t *testing.T, root string, needles ...string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil // unreadable special file: nothing to search
		}
		for _, n := range needles {
			if bytes.Contains(data, []byte(n)) {
				hits = append(hits, p)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

// TestCloneRefAt_CredentialDeliveredOverHTTPSAndLeftNowhere is the end-to-end
// proof: a real git fetch over HTTPS authenticates through the environment
// header alone, on the pull-ref route and on the head-SHA fallback route, and
// no file under the clone ever holds the credential.
func TestCloneRefAt_CredentialDeliveredOverHTTPSAndLeftNowhere(t *testing.T) {
	skipIfNoGit(t)
	const tok = "ghs_e2e_Tok+/en_9"
	needles := []string{tok, basicAuthValue(tok), base64.URLEncoding.EncodeToString([]byte("x-access-token:" + tok)), "x-access-token"}

	t.Run("pull ref", func(t *testing.T) {
		src, sha := initSourceRepoWithPullRef(t, 21)
		base, authorised := serveGitTLS(t, src, "refs/pull/*:refs/pull/*", tok)
		dir, cleanup, err := cloneRefAt(context.Background(), base+"/o/r.git", tok, "refs/pull/21/head", sha, 21)
		defer cleanup()
		if err != nil {
			t.Fatalf("cloneRefAt over https: %v", err)
		}
		if headOf(t, dir) != sha {
			t.Error("HEAD is not the PR head")
		}
		if authorised.Load() == 0 {
			t.Error("server never saw the Authorization header")
		}
		if hits := walkFilesFor(t, dir, needles...); len(hits) > 0 {
			t.Errorf("credential found on disk: %v", hits)
		}
		if _, err := os.Stat(filepath.Join(dir, ".git", "FETCH_HEAD")); err == nil {
			t.Error("FETCH_HEAD left in the clone")
		}
	})

	t.Run("head SHA fallback", func(t *testing.T) {
		src, sha := initSourceRepoNoPullRef(t)
		base, authorised := serveGitTLS(t, src, "refs/heads/*:refs/heads/*", tok)
		dir, cleanup, err := cloneRefAt(context.Background(), base+"/o/r.git", tok, "refs/pull/22/head", sha, 22)
		defer cleanup()
		if err != nil {
			t.Fatalf("cloneRefAt over https (fallback): %v", err)
		}
		if headOf(t, dir) != sha {
			t.Error("HEAD is not the selected SHA")
		}
		if authorised.Load() < 2 {
			t.Errorf("authorised requests = %d, want the credential on both fetches", authorised.Load())
		}
		if hits := walkFilesFor(t, dir, needles...); len(hits) > 0 {
			t.Errorf("credential found on disk: %v", hits)
		}
	})
}
