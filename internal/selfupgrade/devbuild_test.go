package selfupgrade

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
}

// initDevSourceCheckout creates a minimal, buildable Go source checkout
// (go.mod + main.go) as a git repo with one commit and an origin remote
// whose URL contains remoteSubstring. Returns the checkout dir.
func initDevSourceCheckout(t *testing.T, remoteSubstring string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module selfupgradetest\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "initial")
	runGit(t, dir, "remote", "add", "origin", "https://example.com/"+remoteSubstring+".git")
	return dir
}

// buildPlaceholderExe compiles a trivial Go program to path, so it exists as
// a valid, overwritable build artifact — `go build -o path .` refuses to
// clobber a file at path that isn't already a prior build's object file.
func buildPlaceholderExe(t *testing.T, path string) {
	t.Helper()
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte("module placeholder\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", path, ".")
	cmd.Dir = srcDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building placeholder exe: %s: %v", out, err)
	}
}

func testDevBuildConfig(t *testing.T, dir, version string) (DevBuildConfig, *bool, *bool, *[]string) {
	t.Helper()
	var logs []string
	postBuildCalled := false
	execCalled := false

	origExecutableFn := executableFn
	scratchExe := filepath.Join(t.TempDir(), "rebuilt-binary")
	// executableFn must resolve to an existing, valid executable — production
	// code always points it at the currently-running process's own binary
	// (which is both), and CheckAndRebuildDev resolves symlinks on it before
	// `go build -o` overwrites it in place; `go build -o` refuses to
	// overwrite a file that isn't a prior build's object file, so a plain
	// placeholder file doesn't work here.
	buildPlaceholderExe(t, scratchExe)
	executableFn = func() (string, error) { return scratchExe, nil }
	t.Cleanup(func() { executableFn = origExecutableFn })

	origExecFn := execFn
	execFn = func(argv0 string, argv []string, envv []string) error {
		execCalled = true
		return nil
	}
	t.Cleanup(func() { execFn = origExecFn })

	cfg := DevBuildConfig{
		Dir:            dir,
		Version:        version,
		BaseBranch:     "main",
		ExpectedRemote: "handarbeit/fabrik",
		Logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
		PostBuildHook: func(exe, hookDir string) error {
			postBuildCalled = true
			if exe != scratchExe {
				t.Errorf("PostBuildHook exe = %q, want %q", exe, scratchExe)
			}
			if hookDir != dir {
				t.Errorf("PostBuildHook dir = %q, want %q", hookDir, dir)
			}
			return nil
		},
	}
	return cfg, &postBuildCalled, &execCalled, &logs
}

// TestCheckAndRebuildDev_NotSourceCheckout verifies that a directory that
// isn't a git checkout of the expected remote is left alone entirely — no
// build, no hook, no exec.
func TestCheckAndRebuildDev_NotSourceCheckout(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir() // no git init at all

	cfg, postBuildCalled, execCalled, _ := testDevBuildConfig(t, dir, "dev(abc1234)")
	CheckAndRebuildDev(cfg)

	if *postBuildCalled {
		t.Error("PostBuildHook was called for a non-source-checkout directory")
	}
	if *execCalled {
		t.Error("execFn was called for a non-source-checkout directory")
	}
}

// TestCheckAndRebuildDev_SHAMismatchTriggersRebuildWithoutFetch verifies that
// when the running binary's embedded SHA doesn't match local HEAD, a rebuild
// is triggered directly — without ever calling `git fetch` against origin.
// origin is deliberately unreachable; if the implementation fetched anyway,
// the fetch would fail and the function would return early without
// rebuilding, which the assertions below would catch.
func TestCheckAndRebuildDev_SHAMismatchTriggersRebuildWithoutFetch(t *testing.T) {
	skipIfNoGit(t)
	dir := initDevSourceCheckout(t, "handarbeit/fabrik")

	// A SHA that cannot possibly prefix the real local HEAD.
	cfg, postBuildCalled, execCalled, logs := testDevBuildConfig(t, dir, "dev(0000000)")
	CheckAndRebuildDev(cfg)

	if !*postBuildCalled {
		t.Errorf("expected PostBuildHook to be called after SHA-mismatch rebuild, logs: %v", *logs)
	}
	if !*execCalled {
		t.Errorf("expected execFn to be called after SHA-mismatch rebuild, logs: %v", *logs)
	}
}

// TestCheckAndRebuildDev_PostBuildHookFailureIsNonFatal verifies that a
// PostBuildHook error is logged but does not prevent re-exec — the specific
// gap flagged as untested prior to this extraction.
func TestCheckAndRebuildDev_PostBuildHookFailureIsNonFatal(t *testing.T) {
	skipIfNoGit(t)
	dir := initDevSourceCheckout(t, "handarbeit/fabrik")

	cfg, postBuildCalled, execCalled, logs := testDevBuildConfig(t, dir, "dev(0000000)")
	cfg.PostBuildHook = func(exe, hookDir string) error {
		*postBuildCalled = true
		return fmt.Errorf("simulated plugin refresh failure")
	}

	CheckAndRebuildDev(cfg)

	if !*postBuildCalled {
		t.Fatal("PostBuildHook was not called")
	}
	if !*execCalled {
		t.Errorf("expected execFn to still be called despite PostBuildHook failure, logs: %v", *logs)
	}
	found := false
	for _, l := range *logs {
		if l == "post-build hook failed (non-fatal): simulated plugin refresh failure\n" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a 'post-build hook failed (non-fatal)' log line, got: %v", *logs)
	}
}

// TestCheckAndRebuildDev_RemoteAheadFetchesPullsAndRebuilds verifies the
// remote-ahead path: when the running version isn't a recognizable dev(...)
// SHA (so the SHA-mismatch shortcut doesn't fire) and origin has new commits,
// CheckAndRebuildDev fetches, pulls, rebuilds, and re-execs.
func TestCheckAndRebuildDev_RemoteAheadFetchesPullsAndRebuilds(t *testing.T) {
	skipIfNoGit(t)

	originDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(originDir, "go.mod"), []byte("module selfupgradetest\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(originDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, originDir, "init", "-b", "main")
	runGit(t, originDir, "config", "user.email", "test@test.com")
	runGit(t, originDir, "config", "user.name", "Test")
	runGit(t, originDir, "add", "-A")
	runGit(t, originDir, "commit", "-m", "initial")

	localDir := t.TempDir()
	runGit(t, localDir, "clone", originDir, ".")
	runGit(t, localDir, "config", "user.email", "test@test.com")
	runGit(t, localDir, "config", "user.name", "Test")

	// Advance origin past what local has cloned.
	runGit(t, originDir, "commit", "--allow-empty", "-m", "second")

	// Version is not a "dev(...)" string, so extractBinarySHA returns "" and
	// the SHA-mismatch shortcut never fires — forcing the remote-check path.
	cfg, postBuildCalled, execCalled, logs := testDevBuildConfig(t, localDir, "")
	CheckAndRebuildDev(cfg)

	if !*postBuildCalled {
		t.Errorf("expected PostBuildHook to be called after remote-ahead rebuild, logs: %v", *logs)
	}
	if !*execCalled {
		t.Errorf("expected execFn to be called after remote-ahead rebuild, logs: %v", *logs)
	}

	localHead, err := gitRevParse(localDir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	originHead, err := gitRevParse(originDir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if localHead != originHead {
		t.Errorf("expected local to have pulled origin's HEAD (%s), got %s", originHead, localHead)
	}
}

// TestCheckAndRebuildDev_LocalAheadDoesNotPull verifies that when local HEAD
// is ahead of (or diverged from) origin, no pull is attempted and no rebuild
// happens — an unpushed local commit must never be clobbered.
func TestCheckAndRebuildDev_LocalAheadDoesNotPull(t *testing.T) {
	skipIfNoGit(t)

	originDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(originDir, "go.mod"), []byte("module selfupgradetest\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(originDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, originDir, "init", "-b", "main")
	runGit(t, originDir, "config", "user.email", "test@test.com")
	runGit(t, originDir, "config", "user.name", "Test")
	runGit(t, originDir, "add", "-A")
	runGit(t, originDir, "commit", "-m", "initial")

	localDir := t.TempDir()
	runGit(t, localDir, "clone", originDir, ".")
	runGit(t, localDir, "config", "user.email", "test@test.com")
	runGit(t, localDir, "config", "user.name", "Test")
	// Local gets an unpushed commit; origin does not change.
	runGit(t, localDir, "commit", "--allow-empty", "-m", "local-only")

	cfg, postBuildCalled, execCalled, logs := testDevBuildConfig(t, localDir, "")
	CheckAndRebuildDev(cfg)

	if *postBuildCalled {
		t.Errorf("expected no rebuild when local is ahead of origin, logs: %v", *logs)
	}
	if *execCalled {
		t.Errorf("expected no re-exec when local is ahead of origin, logs: %v", *logs)
	}
}

// TestIsSourceCheckout verifies the git-remote substring match used to gate
// dev-build rebuilds to an actual source checkout.
func TestIsSourceCheckout(t *testing.T) {
	skipIfNoGit(t)

	t.Run("matching remote", func(t *testing.T) {
		dir := initDevSourceCheckout(t, "handarbeit/fabrik")
		if !IsSourceCheckout(dir, "handarbeit/fabrik") {
			t.Error("expected IsSourceCheckout to return true for a matching remote")
		}
	})

	t.Run("non-matching remote", func(t *testing.T) {
		dir := initDevSourceCheckout(t, "someoneelse/otherrepo")
		if IsSourceCheckout(dir, "handarbeit/fabrik") {
			t.Error("expected IsSourceCheckout to return false for a non-matching remote")
		}
	})

	t.Run("no git repo", func(t *testing.T) {
		if IsSourceCheckout(t.TempDir(), "handarbeit/fabrik") {
			t.Error("expected IsSourceCheckout to return false outside a git repo")
		}
	})
}

// TestExtractBinarySHA covers the dev(<sha>) version-string parsing used to
// decide whether a rebuild is needed without consulting the remote.
func TestExtractBinarySHA(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{"dev(abc1234)", "abc1234"},
		{"dev()", ""},
		{"v1.2.3", ""},
		{"", ""},
		{"dev(abc1234", ""},
	}
	for _, tc := range tests {
		if got := extractBinarySHA(tc.version); got != tc.want {
			t.Errorf("extractBinarySHA(%q) = %q, want %q", tc.version, got, tc.want)
		}
	}
}
