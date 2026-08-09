package selfupgrade

import (
	"bytes"
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

// testDevBuildConfig builds a DevBuildConfig wired with stubbed
// executableFn/execFn seams and an ExpectedRemote matching whatever remote
// substring dir's origin was actually configured with (initDevSourceCheckout
// or a real clone both need this to line up with IsSourceCheckout's check).
func testDevBuildConfig(t *testing.T, dir, version, expectedRemote string) (DevBuildConfig, *bool, *bool, *[]string) {
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
	// Resolve once up front so the PostBuildHook assertion below compares
	// against the same resolved form CheckAndRebuildDev itself produces
	// (EvalSymlinks) — on macOS, t.TempDir() paths live under /var, which is
	// itself a symlink to /private/var.
	resolvedScratchExe, err := filepath.EvalSymlinks(scratchExe)
	if err != nil {
		t.Fatal(err)
	}
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
		ExpectedRemote: expectedRemote,
		Logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
		PostBuildHook: func(exe, hookDir string) error {
			postBuildCalled = true
			if exe != resolvedScratchExe {
				t.Errorf("PostBuildHook exe = %q, want %q", exe, resolvedScratchExe)
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

	cfg, postBuildCalled, execCalled, _ := testDevBuildConfig(t, dir, "dev(abc1234)", "handarbeit/fabrik")
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
	cfg, postBuildCalled, execCalled, logs := testDevBuildConfig(t, dir, "dev(0000000)", "handarbeit/fabrik")
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

	cfg, postBuildCalled, execCalled, logs := testDevBuildConfig(t, dir, "dev(0000000)", "handarbeit/fabrik")
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
	// ExpectedRemote is originDir itself: `git clone <originDir> .` sets
	// origin's URL to that literal path.
	cfg, postBuildCalled, execCalled, logs := testDevBuildConfig(t, localDir, "", originDir)
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

	cfg, postBuildCalled, execCalled, logs := testDevBuildConfig(t, localDir, "", originDir)
	CheckAndRebuildDev(cfg)

	if *postBuildCalled {
		t.Errorf("expected no rebuild when local is ahead of origin, logs: %v", *logs)
	}
	if *execCalled {
		t.Errorf("expected no re-exec when local is ahead of origin, logs: %v", *logs)
	}
}

// initOriginAndClone creates a bare-ish origin source checkout and a clone of
// it, both wired the way CompareDevBuild/CheckAndRebuildDev expect (git user
// configured, ExpectedRemote resolvable to originDir). Returns both dirs.
func initOriginAndClone(t *testing.T) (originDir, localDir string) {
	t.Helper()
	originDir = t.TempDir()
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

	localDir = t.TempDir()
	runGit(t, localDir, "clone", originDir, ".")
	runGit(t, localDir, "config", "user.email", "test@test.com")
	runGit(t, localDir, "config", "user.name", "Test")
	return originDir, localDir
}

// TestCompareDevBuild_NotSourceCheckout verifies that a directory that isn't
// a git checkout of the expected remote reports Applicable=false and takes no
// action.
func TestCompareDevBuild_NotSourceCheckout(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir() // no git init at all

	cfg, _, _, _ := testDevBuildConfig(t, dir, "dev(abc1234)", "handarbeit/fabrik")
	status, err := CompareDevBuild(cfg)
	if err != nil {
		t.Fatalf("CompareDevBuild returned error: %v", err)
	}
	if status.Applicable {
		t.Error("expected Applicable=false for a non-source-checkout directory")
	}
	if status.NeedsRebuild {
		t.Error("expected NeedsRebuild=false for a non-source-checkout directory")
	}
}

// TestCompareDevBuild_ShaMismatchSkipsFetch verifies that when the running
// binary's embedded SHA doesn't match local HEAD, CompareDevBuild reports
// NeedsRebuild without ever calling `git fetch` against origin — origin is
// deliberately unreachable, so RemoteRef staying empty is proof no fetch ran
// (a fetch attempt would otherwise surface as an error here).
func TestCompareDevBuild_ShaMismatchSkipsFetch(t *testing.T) {
	skipIfNoGit(t)
	dir := initDevSourceCheckout(t, "handarbeit/fabrik")

	cfg, _, _, logs := testDevBuildConfig(t, dir, "dev(0000000)", "handarbeit/fabrik")
	status, err := CompareDevBuild(cfg)
	if err != nil {
		t.Fatalf("CompareDevBuild returned error: %v, logs: %v", err, *logs)
	}
	if !status.Applicable {
		t.Fatal("expected Applicable=true")
	}
	if !status.ShaMismatch {
		t.Error("expected ShaMismatch=true")
	}
	if !status.NeedsRebuild {
		t.Error("expected NeedsRebuild=true")
	}
	if status.RemoteRef != "" {
		t.Errorf("expected no fetch to have run (RemoteRef empty), got %q", status.RemoteRef)
	}
	if status.CommitsBehind != 0 {
		t.Errorf("expected CommitsBehind=0 when the SHA-mismatch shortcut fires, got %d", status.CommitsBehind)
	}
}

// TestCompareDevBuild_RemoteAhead verifies the remote-ahead field values:
// CommitsBehind, RemoteRef, and LocalCommitTime are all populated correctly
// when origin has new commits.
func TestCompareDevBuild_RemoteAhead(t *testing.T) {
	skipIfNoGit(t)
	originDir, localDir := initOriginAndClone(t)

	runGit(t, originDir, "commit", "--allow-empty", "-m", "second")
	runGit(t, originDir, "commit", "--allow-empty", "-m", "third")

	// Version is not a "dev(...)" string, so extractBinarySHA returns "" and
	// the SHA-mismatch shortcut never fires — forcing the remote-check path.
	cfg, _, _, logs := testDevBuildConfig(t, localDir, "", originDir)
	status, err := CompareDevBuild(cfg)
	if err != nil {
		t.Fatalf("CompareDevBuild returned error: %v, logs: %v", err, *logs)
	}
	if status.ShaMismatch {
		t.Error("expected ShaMismatch=false")
	}
	if !status.RemoteAhead {
		t.Error("expected RemoteAhead=true")
	}
	if !status.NeedsRebuild {
		t.Error("expected NeedsRebuild=true")
	}
	if status.CommitsBehind != 2 {
		t.Errorf("CommitsBehind = %d, want 2", status.CommitsBehind)
	}
	remoteHead, err := gitRevParse(originDir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if status.RemoteRef != remoteHead {
		t.Errorf("RemoteRef = %q, want %q", status.RemoteRef, remoteHead)
	}
	if status.LocalCommitTime.IsZero() {
		t.Error("expected LocalCommitTime to be populated")
	}
}

// TestCompareDevBuild_UpToDate verifies that a checkout whose local HEAD
// matches origin/BaseBranch reports NeedsRebuild=false.
func TestCompareDevBuild_UpToDate(t *testing.T) {
	skipIfNoGit(t)
	originDir, localDir := initOriginAndClone(t)

	cfg, _, _, logs := testDevBuildConfig(t, localDir, "", originDir)
	status, err := CompareDevBuild(cfg)
	if err != nil {
		t.Fatalf("CompareDevBuild returned error: %v, logs: %v", err, *logs)
	}
	if !status.Applicable {
		t.Error("expected Applicable=true")
	}
	if status.NeedsRebuild {
		t.Error("expected NeedsRebuild=false when up to date")
	}
	if status.RemoteRef == "" {
		t.Error("expected RemoteRef to be resolved by the fetch")
	}
}

// TestCompareDevBuild_LocalAheadNoRebuild verifies that a checkout with
// unpushed local commits (ahead of origin) reports NeedsRebuild=false — an
// unpushed local commit must never be flagged for rebuild.
func TestCompareDevBuild_LocalAheadNoRebuild(t *testing.T) {
	skipIfNoGit(t)
	originDir, localDir := initOriginAndClone(t)
	runGit(t, localDir, "commit", "--allow-empty", "-m", "local-only")

	cfg, _, _, logs := testDevBuildConfig(t, localDir, "", originDir)
	status, err := CompareDevBuild(cfg)
	if err != nil {
		t.Fatalf("CompareDevBuild returned error: %v, logs: %v", err, *logs)
	}
	if status.RemoteAhead {
		t.Error("expected RemoteAhead=false when local is ahead of origin")
	}
	if status.NeedsRebuild {
		t.Error("expected NeedsRebuild=false when local is ahead of origin")
	}
}

// TestCompareDevBuild_NeverBuildsOrExecs is the AC6 assertion: run
// CompareDevBuild against a fixture that is unambiguously rebuild-eligible
// (remote ahead, NeedsRebuild=true — the exact condition that would make
// CheckAndRebuildDev pull/build/exec) and confirm none of PostBuildHook,
// execFn, or an on-disk binary rewrite happened. The executable's bytes are
// compared before/after since `go build -o` would rewrite them; unchanged
// bytes plus untriggered hooks together prove no build or exec occurred, not
// just that this particular run happened not to need one.
func TestCompareDevBuild_NeverBuildsOrExecs(t *testing.T) {
	skipIfNoGit(t)
	originDir, localDir := initOriginAndClone(t)
	runGit(t, originDir, "commit", "--allow-empty", "-m", "second")

	cfg, postBuildCalled, execCalled, logs := testDevBuildConfig(t, localDir, "", originDir)

	exePath, err := executableFn()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatal(err)
	}

	status, err := CompareDevBuild(cfg)
	if err != nil {
		t.Fatalf("CompareDevBuild returned error: %v, logs: %v", err, *logs)
	}
	if !status.NeedsRebuild {
		t.Fatal("fixture must be rebuild-eligible for this test to prove anything")
	}

	after, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("CompareDevBuild modified the executable on disk — it must never build")
	}
	if *postBuildCalled {
		t.Error("CompareDevBuild called PostBuildHook — it must never build/exec")
	}
	if *execCalled {
		t.Error("CompareDevBuild called execFn — it must never exec")
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
