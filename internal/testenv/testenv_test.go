package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsolateUnsetsAndRestores(t *testing.T) {
	t.Setenv("FABRIK_TOKEN", "secret")
	t.Setenv("GH_TOKEN", "tok")
	t.Setenv("GIT_CONFIG_COUNT", "5")
	t.Setenv("GIT_CONFIG_KEY_4", "a.b")
	t.Setenv("GIT_CONFIG_VALUE_4", "c")

	t.Run("inner", func(t *testing.T) {
		Isolate(t)
		for _, k := range []string{"FABRIK_TOKEN", "GH_TOKEN", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_4", "GIT_CONFIG_VALUE_4", "HOOKDECK_API_KEY"} {
			if v, ok := os.LookupEnv(k); ok {
				t.Errorf("%s still set to %q (must be truly unset, not empty)", k, v)
			}
		}
	})
	if got := os.Getenv("FABRIK_TOKEN"); got != "secret" {
		t.Errorf("FABRIK_TOKEN not restored: %q", got)
	}
	if got := os.Getenv("GIT_CONFIG_KEY_4"); got != "a.b" {
		t.Errorf("GIT_CONFIG_KEY_4 not restored: %q", got)
	}
}

func TestNamesEnumeratesAnyGitConfigIndex(t *testing.T) {
	names := Names([]string{"GIT_CONFIG_KEY_97=x", "GIT_CONFIG_VALUE_97=y", "UNRELATED=1"})
	joined := strings.Join(names, " ")
	for _, want := range []string{"GIT_CONFIG_KEY_97", "GIT_CONFIG_VALUE_97", "FABRIK_REVIEWER_TOKEN", "GH_HOST"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Names missing %s", want)
		}
	}
	if strings.Contains(joined, "UNRELATED") {
		t.Error("Names must not include unrelated variables")
	}
}

func TestIsolateGitIgnoresInheritedCommandScopeConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// What the ADR-1846 helper injection looks like in a worker.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "fabrik.workertest")
	t.Setenv("GIT_CONFIG_VALUE_0", "leaked")
	IsolateGit(t, "[fabrik]\n\tworkertest = from-global\n")
	out, err := exec.Command("git", "config", "--get", "fabrik.workertest").Output()
	if err != nil {
		t.Fatalf("git config: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "from-global" {
		t.Fatalf("got %q; inherited command-scope entry outranked the isolated global file", got)
	}
}

// newLeakyRepo builds a repository whose *local* config sets a credential
// helper and an SSH url rewrite, plus a linked worktree of it, and returns both
// directories. It must run after IsolateGit so the developer's own global
// config cannot touch the fixture.
func newLeakyRepo(t *testing.T) (repo, worktree string) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "main")
	worktree = filepath.Join(root, "linked")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git(repo, "init", "-q")
	git(repo, "config", "--local", "user.name", "fixture")
	git(repo, "config", "--local", "user.email", "fixture@example.invalid")
	git(repo, "config", "--local", "commit.gpgsign", "false")
	git(repo, "config", "--local", "credential.helper", "store")
	git(repo, "config", "--local", "url.git@github.com:.insteadOf", "https://github.com/")
	git(repo, "commit", "-q", "--allow-empty", "-m", "init")
	git(repo, "worktree", "add", "-q", "-b", "fixture-wt", worktree)
	return repo, worktree
}

// gitConfigIn runs `git config` in dir ("" = the process's working directory)
// and returns its trimmed stdout; "no match" (exit 1) is the empty string.
func gitConfigIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"config"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return ""
		}
		t.Fatalf("git config %v in %q: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

// TestIsolateGitIgnoresRepoAndWorktreeLocalConfig (#2262): a repository's own
// local config — including the main repository's config seen from a linked
// worktree — must not be visible to a test that called IsolateGit, even when
// the test started inside that repository.
func TestIsolateGitIgnoresRepoAndWorktreeLocalConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	IsolateGit(t, "") // env only, so the fixture is built without ambient config
	repo, worktree := newLeakyRepo(t)

	const insteadOf = `url\..*\.insteadof`
	// Non-vacuity: from inside the fixture, with only the env isolated, the
	// repository's config is what git reports. Without this the immunity
	// assertions below could pass because the fixture never leaked at all.
	for _, dir := range []string{repo, worktree} {
		if got := gitConfigIn(t, dir, "credential.helper"); got != "store" {
			t.Fatalf("fixture %s: credential.helper = %q, want store (fixture does not leak)", dir, got)
		}
		if got := gitConfigIn(t, dir, "--get-regexp", insteadOf); got == "" {
			t.Fatalf("fixture %s: no insteadOf visible (fixture does not leak)", dir)
		}
	}

	for name, dir := range map[string]string{"repo": repo, "worktree": worktree} {
		t.Run(name, func(t *testing.T) {
			t.Chdir(dir)
			if got := gitConfigIn(t, "", "credential.helper"); got != "store" {
				t.Fatalf("started in fixture but credential.helper = %q, want store", got)
			}
			IsolateGit(t, "")
			if cwd, err := os.Getwd(); err != nil {
				t.Fatal(err)
			} else if resolved, _ := filepath.EvalSymlinks(cwd); strings.HasPrefix(resolved, evalSymlinks(t, dir)) {
				t.Fatalf("working directory %q is still inside the fixture", cwd)
			}
			if got := gitConfigIn(t, "", "credential.helper"); got != "" {
				t.Errorf("credential.helper = %q after IsolateGit; repo-local config leaked", got)
			}
			if got := gitConfigIn(t, "", "--get-regexp", insteadOf); got != "" {
				t.Errorf("insteadOf = %q after IsolateGit; repo-local config leaked", got)
			}
		})
	}
}

func evalSymlinks(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
