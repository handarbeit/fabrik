package selfupgrade

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// whose URL is https://github.com/<remoteSubstring>.git. Returns the checkout dir.
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
	runGit(t, dir, "remote", "add", "origin", "https://github.com/"+remoteSubstring+".git")
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

// testSourceRemote is the owner/repo the clone-based tests expect.
const testSourceRemote = "handarbeit/fabrik"

// stubOriginURL replaces the origin-URL reader for the test's duration.
func stubOriginURL(t *testing.T, fn func(dir string) (string, error)) {
	t.Helper()
	t.Cleanup(SetOriginURLFnForTest(fn))
}

// testDevBuildConfig builds a DevBuildConfig wired with stubbed
// executableFn/execFn seams and an owner/repo ExpectedRemote. A real clone's
// local-path origin is presented as a github.com URL for that owner/repo;
// initDevSourceCheckout's github.com origin is used as-is and must line up
// with expectedRemote.
func testDevBuildConfig(t *testing.T, dir, version, expectedRemote string) (DevBuildConfig, *bool, *bool, *[]string) {
	t.Helper()
	var logs []string
	postBuildCalled := false
	execCalled := false

	// A real clone's origin is a local filesystem path, which can never satisfy
	// the github.com host check; present it as a github.com URL for
	// expectedRemote while every other git operation stays real. Remotes that
	// are already URLs (initDevSourceCheckout) pass through untouched.
	stubOriginURL(t, func(d string) (string, error) {
		u, err := gitOriginURL(d)
		if err == nil && strings.HasPrefix(u, "/") {
			return "https://github.com/" + expectedRemote + ".git", nil
		}
		return u, err
	})

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

// TestCheckAndRebuildDev_ExtraEnvReachesExec verifies cfg.ExtraEnv is appended
// to the re-exec environment, and that nil ExtraEnv leaves it equal to
// os.Environ().
func TestCheckAndRebuildDev_ExtraEnvReachesExec(t *testing.T) {
	skipIfNoGit(t)
	for _, tc := range []struct {
		name  string
		extra []string
	}{
		{"with extra env", []string{"FABRIK_DEV_REEXEC=1"}},
		{"nil extra env", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := initDevSourceCheckout(t, "handarbeit/fabrik")
			cfg, _, execCalled, logs := testDevBuildConfig(t, dir, "dev(0000000)", "handarbeit/fabrik")
			cfg.ExtraEnv = tc.extra
			var gotEnv []string
			execFn = func(argv0 string, argv []string, envv []string) error {
				gotEnv = envv
				return nil
			}
			CheckAndRebuildDev(cfg)
			if !*execCalled && gotEnv == nil {
				t.Fatalf("exec not reached, logs: %v", *logs)
			}
			want := append(os.Environ(), tc.extra...)
			if len(gotEnv) != len(want) {
				t.Fatalf("exec env has %d entries, want %d", len(gotEnv), len(want))
			}
			for i := range want {
				if gotEnv[i] != want[i] {
					t.Fatalf("exec env[%d] = %q, want %q", i, gotEnv[i], want[i])
				}
			}
		})
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
	// The clone's real origin is the local originDir path; testDevBuildConfig
	// presents it as a github.com URL for testSourceRemote.
	cfg, postBuildCalled, execCalled, logs := testDevBuildConfig(t, localDir, "", testSourceRemote)
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

	cfg, postBuildCalled, execCalled, logs := testDevBuildConfig(t, localDir, "", testSourceRemote)
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
	cfg, _, _, logs := testDevBuildConfig(t, localDir, "", testSourceRemote)
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
	_, localDir := initOriginAndClone(t)

	cfg, _, _, logs := testDevBuildConfig(t, localDir, "", testSourceRemote)
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
	_, localDir := initOriginAndClone(t)
	runGit(t, localDir, "commit", "--allow-empty", "-m", "local-only")

	cfg, _, _, logs := testDevBuildConfig(t, localDir, "", testSourceRemote)
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

	cfg, postBuildCalled, execCalled, logs := testDevBuildConfig(t, localDir, "", testSourceRemote)

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

// isolateGitConfig keeps the host's global/system git config (insteadOf rules
// in particular) out of `git remote get-url`.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// TestIsSourceCheckout drives IsSourceCheckout against real git checkouts.
func TestIsSourceCheckout(t *testing.T) {
	skipIfNoGit(t)
	isolateGitConfig(t)

	withOrigin := func(t *testing.T, url string) string {
		t.Helper()
		dir := initDevSourceCheckout(t, "placeholder/placeholder")
		runGit(t, dir, "remote", "set-url", "origin", url)
		return dir
	}

	tests := []struct {
		name string
		url  string
		want bool
	}{
		{"https", "https://github.com/handarbeit/fabrik", true},
		{"https .git", "https://github.com/handarbeit/fabrik.git", true},
		{"scp", "git@github.com:handarbeit/fabrik", true},
		{"scp .git", "git@github.com:handarbeit/fabrik.git", true},
		{"ssh url", "ssh://git@github.com/handarbeit/fabrik", true},
		{"ssh url .git", "ssh://git@github.com/handarbeit/fabrik.git", true},
		{"substring near-miss", "https://github.com/attacker/handarbeit-fabrik-fork", false},
		{"repo suffix near-miss", "https://github.com/handarbeit/fabrik-fork.git", false},
		{"same repo other host", "https://gitlab.com/handarbeit/fabrik.git", false},
		{"same repo self-hosted ssh", "git@git.example.com:handarbeit/fabrik.git", false},
		{"someoneelse", "https://github.com/someoneelse/otherrepo.git", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := withOrigin(t, tt.url)
			if got := IsSourceCheckout(dir, "handarbeit/fabrik"); got != tt.want {
				t.Errorf("IsSourceCheckout(origin=%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}

	t.Run("no origin remote", func(t *testing.T) {
		dir := initDevSourceCheckout(t, "handarbeit/fabrik")
		runGit(t, dir, "remote", "remove", "origin")
		if IsSourceCheckout(dir, "handarbeit/fabrik") {
			t.Error("expected false with no origin remote")
		}
	})

	t.Run("no git repo", func(t *testing.T) {
		if IsSourceCheckout(t.TempDir(), "handarbeit/fabrik") {
			t.Error("expected IsSourceCheckout to return false outside a git repo")
		}
	})

	t.Run("get-url failure", func(t *testing.T) {
		stubOriginURL(t, func(string) (string, error) { return "", fmt.Errorf("boom") })
		if IsSourceCheckout(t.TempDir(), "handarbeit/fabrik") {
			t.Error("expected false when the origin reader errors")
		}
	})
}

// TestMatchGitHubRemote is the exhaustive table for the pure remote parser.
func TestMatchGitHubRemote(t *testing.T) {
	const exp = "handarbeit/fabrik"
	accept := []string{
		"https://github.com/handarbeit/fabrik",
		"https://github.com/handarbeit/fabrik.git",
		"https://github.com/handarbeit/fabrik/",
		"https://github.com/handarbeit/fabrik.git/",
		"git@github.com:handarbeit/fabrik",
		"git@github.com:handarbeit/fabrik.git",
		"git@github.com:handarbeit/fabrik/",
		"git@github.com:handarbeit/fabrik.git/",
		"ssh://git@github.com/handarbeit/fabrik",
		"ssh://git@github.com/handarbeit/fabrik.git",
		"ssh://git@github.com/handarbeit/fabrik/",
		"ssh://git@github.com/handarbeit/fabrik.git/",
		"  https://github.com/handarbeit/fabrik.git\n",
		// Deliberate: GitHub names and hosts are case-insensitive.
		"https://GitHub.com/Handarbeit/Fabrik.git",
		"git@github.com:HANDARBEIT/FABRIK",
	}
	for _, u := range accept {
		if !matchGitHubRemote(u, exp) {
			t.Errorf("matchGitHubRemote(%q) = false, want true", u)
		}
	}

	reject := []string{
		"",
		"   ",
		// substring / look-alike
		"https://github.com/attacker/handarbeit-fabrik-fork",
		"https://github.com/handarbeit/fabrik-fork",
		"https://github.com/xhandarbeit/fabrik",
		"https://github.com/handarbeit/fabrik/extra",
		"https://github.com/handarbeit/fabrik/.git",
		"https://github.com/extra/handarbeit/fabrik",
		// hosts
		"https://github.com.evil.com/handarbeit/fabrik",
		"https://evil.com/github.com/handarbeit/fabrik",
		"https://gitlab.com/handarbeit/fabrik.git",
		"https://ssh.github.com/handarbeit/fabrik",
		"git@gitlab.com:handarbeit/fabrik.git",
		"git@github.com.evil.com:handarbeit/fabrik.git",
		"ssh://git@gitlab.com/handarbeit/fabrik",
		// empty / malformed segments
		"https://github.com/handarbeit/",
		"https://github.com/handarbeit",
		"https://github.com//fabrik",
		"https://github.com/handarbeit/.git",
		"https://github.com/handarbeit/..",
		"git@github.com:handarbeit/",
		"git@github.com:/fabrik",
		"git@github.com:/handarbeit/fabrik",
		"git@github.com:handarbeit",
		// userinfo, ports
		"https://user:tok@github.com/handarbeit/fabrik",
		"https://tok@github.com/handarbeit/fabrik",
		"https://github.com:443/handarbeit/fabrik",
		"ssh://git@github.com:22/handarbeit/fabrik",
		"ssh://other@github.com/handarbeit/fabrik",
		"ssh://git:pw@github.com/handarbeit/fabrik",
		"ssh://github.com/handarbeit/fabrik",
		"other@github.com:handarbeit/fabrik",
		"github.com:handarbeit/fabrik",
		// query, fragment, encoding
		"https://github.com/handarbeit/fabrik?x=1",
		"https://github.com/handarbeit/fabrik#frag",
		"https://github.com/handarbeit/fab%72ik",
		// schemes / local paths
		"http://github.com/handarbeit/fabrik",
		"git://github.com/handarbeit/fabrik",
		"git+ssh://git@github.com/handarbeit/fabrik",
		"file:///srv/handarbeit/fabrik",
		"/srv/handarbeit/fabrik",
		"handarbeit/fabrik",
		"github.com/handarbeit/fabrik",
		// whitespace / control characters inside
		"https://github.com/handarbeit/fab rik",
		"https://github.com/handarbeit/fabrik\n/x",
	}
	for _, u := range reject {
		if matchGitHubRemote(u, exp) {
			t.Errorf("matchGitHubRemote(%q) = true, want false", u)
		}
	}

	badExpectations := []string{"", "handarbeit", "handarbeit/", "/fabrik", "a/b/c", "a b/c", "a/..", "../b", "a/b%"}
	for _, e := range badExpectations {
		if matchGitHubRemote("https://github.com/handarbeit/fabrik", e) {
			t.Errorf("expectation %q accepted, want fail-closed", e)
		}
	}
}

// TestExtractBinarySHA covers the dev(<sha>) version-string parsing used to
// decide whether a rebuild is needed without consulting the remote.
func TestExtractBinarySHA(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{"dev(abc1234)", "abc1234"},
		{"dev(abc)+dirty", "abc"},
		{"dev(abc)+other", "abc"},
		{"dev()", ""},
		{"dev()+dirty", ""},
		{"v0.0.84", ""},
		{"garbage", ""},
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

func TestParseDevVersion_Dirty(t *testing.T) {
	for _, tc := range []struct {
		version string
		sha     string
		dirty   bool
	}{
		{"dev(abc)", "abc", false},
		{"dev(abc)+dirty", "abc", true},
		{"dev(abc)+other", "abc", false},
		{"dev()+dirty", "", false},
		{"v0.0.84+dirty", "", false},
	} {
		sha, dirty := parseDevVersion(tc.version)
		if sha != tc.sha || dirty != tc.dirty {
			t.Errorf("parseDevVersion(%q) = (%q, %v), want (%q, %v)", tc.version, sha, dirty, tc.sha, tc.dirty)
		}
	}
}

// headOf returns the full HEAD SHA of dir.
func headOf(t *testing.T, dir string) string {
	t.Helper()
	sha, err := gitRevParse(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// TestCompareDevBuild_DirtyBinaryOldSHARebuilds is the #2234 regression: a
// "+dirty" binary older than HEAD, with HEAD == origin/main, must be flagged
// for rebuild. With the old suffix-rejecting parser BinarySHA was "" and the
// check fell through to "up to date".
func TestCompareDevBuild_DirtyBinaryOldSHARebuilds(t *testing.T) {
	skipIfNoGit(t)
	_, localDir := initOriginAndClone(t)

	cfg, _, _, logs := testDevBuildConfig(t, localDir, "dev(0ld5ha1)+dirty", testSourceRemote)
	status, err := CompareDevBuild(cfg)
	if err != nil {
		t.Fatalf("CompareDevBuild returned error: %v, logs: %v", err, *logs)
	}
	if !status.ShaMismatch || !status.NeedsRebuild {
		t.Errorf("expected ShaMismatch and NeedsRebuild for dev(old)+dirty, got %+v", status)
	}
	if !status.BinaryDirty {
		t.Error("expected BinaryDirty=true")
	}
}

func TestCompareDevBuild_DirtyBinarySameSHANoRebuild(t *testing.T) {
	skipIfNoGit(t)
	_, localDir := initOriginAndClone(t)
	head := headOf(t, localDir)

	cfg, _, _, logs := testDevBuildConfig(t, localDir, "dev("+head[:7]+")+dirty", testSourceRemote)
	status, err := CompareDevBuild(cfg)
	if err != nil {
		t.Fatalf("CompareDevBuild returned error: %v, logs: %v", err, *logs)
	}
	if status.NeedsRebuild {
		t.Error("expected no rebuild for a dirty binary at the current commit (R2)")
	}
	if !status.BinaryDirty {
		t.Error("expected BinaryDirty=true")
	}
	if !strings.Contains(status.Reason, "dirty tree at the current commit") {
		t.Errorf("Reason should explain the dirty build at the current commit, got %q", status.Reason)
	}
}

func TestCompareDevBuild_DirtyBinaryRemoteAheadRebuilds(t *testing.T) {
	skipIfNoGit(t)
	originDir, localDir := initOriginAndClone(t)
	head := headOf(t, localDir)
	runGit(t, originDir, "commit", "--allow-empty", "-m", "second")

	cfg, _, _, logs := testDevBuildConfig(t, localDir, "dev("+head[:7]+")+dirty", testSourceRemote)
	status, err := CompareDevBuild(cfg)
	if err != nil {
		t.Fatalf("CompareDevBuild returned error: %v, logs: %v", err, *logs)
	}
	if !status.RemoteAhead || !status.NeedsRebuild {
		t.Errorf("expected RemoteAhead and NeedsRebuild, got %+v", status)
	}
	if status.Reason != "" {
		t.Errorf("Reason must be empty when a rebuild is needed, got %q", status.Reason)
	}
}

// TestCompareDevBuild_NoRebuildReasons checks that every silent-exit shape
// carries a Reason (R3).
func TestCompareDevBuild_NoRebuildReasons(t *testing.T) {
	skipIfNoGit(t)

	t.Run("up to date", func(t *testing.T) {
		_, localDir := initOriginAndClone(t)
		head := headOf(t, localDir)
		cfg, _, _, _ := testDevBuildConfig(t, localDir, "dev("+head[:7]+")", testSourceRemote)
		status, err := CompareDevBuild(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(status.Reason, "up to date") {
			t.Errorf("Reason = %q, want it to say up to date", status.Reason)
		}
	})
	t.Run("local ahead", func(t *testing.T) {
		_, localDir := initOriginAndClone(t)
		runGit(t, localDir, "commit", "--allow-empty", "-m", "local-only")
		head := headOf(t, localDir)
		cfg, _, _, _ := testDevBuildConfig(t, localDir, "dev("+head[:7]+")", testSourceRemote)
		status, err := CompareDevBuild(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(status.Reason, "ahead of or diverged") {
			t.Errorf("Reason = %q, want it to say ahead/diverged", status.Reason)
		}
	})
	t.Run("diverged", func(t *testing.T) {
		originDir, localDir := initOriginAndClone(t)
		runGit(t, originDir, "commit", "--allow-empty", "-m", "remote-only")
		runGit(t, localDir, "commit", "--allow-empty", "-m", "local-only")
		head := headOf(t, localDir)
		cfg, _, _, _ := testDevBuildConfig(t, localDir, "dev("+head[:7]+")", testSourceRemote)
		status, err := CompareDevBuild(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(status.Reason, "ahead of or diverged") {
			t.Errorf("Reason = %q, want it to say ahead/diverged", status.Reason)
		}
	})
	for _, version := range []string{"", "v0.0.84", "dev()", "garbage"} {
		t.Run("unparseable "+version, func(t *testing.T) {
			_, localDir := initOriginAndClone(t)
			cfg, _, _, _ := testDevBuildConfig(t, localDir, version, testSourceRemote)
			status, err := CompareDevBuild(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(status.Reason, "no parseable commit SHA") {
				t.Errorf("Reason = %q, want the unparseable-version note", status.Reason)
			}
		})
	}
	t.Run("not a source checkout", func(t *testing.T) {
		cfg, _, _, _ := testDevBuildConfig(t, t.TempDir(), "dev(abc)", "handarbeit/fabrik")
		status, err := CompareDevBuild(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if status.Applicable || !strings.Contains(status.Reason, "not a source checkout") {
			t.Errorf("got %+v, want Applicable=false with a not-a-source-checkout Reason", status)
		}
	})
}

// TestCheckAndRebuildDev_LogsOneOutcomeLine verifies R3: every no-rebuild
// decision produces exactly one log line saying why.
func TestCheckAndRebuildDev_LogsOneOutcomeLine(t *testing.T) {
	skipIfNoGit(t)
	resetDirtyReportedForTest()
	t.Cleanup(resetDirtyReportedForTest)

	t.Run("up to date", func(t *testing.T) {
		_, localDir := initOriginAndClone(t)
		head := headOf(t, localDir)
		cfg, _, execCalled, logs := testDevBuildConfig(t, localDir, "dev("+head[:7]+")", testSourceRemote)
		CheckAndRebuildDev(cfg)
		if *execCalled {
			t.Error("unexpected re-exec")
		}
		if len(*logs) != 1 || !strings.Contains((*logs)[0], "up to date") {
			t.Errorf("logs = %q, want exactly one up-to-date line", *logs)
		}
	})
	t.Run("not a source checkout", func(t *testing.T) {
		cfg, _, _, logs := testDevBuildConfig(t, t.TempDir(), "dev(abc)", "handarbeit/fabrik")
		CheckAndRebuildDev(cfg)
		if len(*logs) != 1 || !strings.Contains((*logs)[0], "not a source checkout") {
			t.Errorf("logs = %q, want exactly one not-applicable line", *logs)
		}
	})
	t.Run("dirty binary at current commit", func(t *testing.T) {
		_, localDir := initOriginAndClone(t)
		head := headOf(t, localDir)
		cfg, _, _, logs := testDevBuildConfig(t, localDir, "dev("+head[:7]+")+dirty", testSourceRemote)
		CheckAndRebuildDev(cfg)
		if len(*logs) != 1 || !strings.Contains((*logs)[0], "dirty tree at the current commit") {
			t.Errorf("logs = %q, want one line explaining the dirty build", *logs)
		}
	})
}

// TestCheckAndRebuildDev_DirtyBinaryOldSHAFromPulledCheckout is the incident:
// the checkout was pulled to origin/main by something else, so only the
// binary-vs-HEAD comparison can notice the stale "+dirty" binary.
func TestCheckAndRebuildDev_DirtyBinaryOldSHAFromPulledCheckout(t *testing.T) {
	skipIfNoGit(t)
	resetDirtyReportedForTest()
	t.Cleanup(resetDirtyReportedForTest)
	_, localDir := initOriginAndClone(t)

	cfg, postBuildCalled, execCalled, logs := testDevBuildConfig(t, localDir, "dev(0ld5ha1)+dirty", testSourceRemote)
	CheckAndRebuildDev(cfg)
	if !*postBuildCalled || !*execCalled {
		t.Errorf("expected rebuild and re-exec, logs: %q", *logs)
	}
}

func TestCheckAndRebuildDev_ReportsDirtyPathsOncePerProcess(t *testing.T) {
	skipIfNoGit(t)
	resetDirtyReportedForTest()
	t.Cleanup(resetDirtyReportedForTest)
	_, localDir := initOriginAndClone(t)
	head := headOf(t, localDir)
	if err := os.WriteFile(filepath.Join(localDir, "stray.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	count := func(logs []string) (n int) {
		for _, l := range logs {
			if strings.Contains(l, "uncommitted/untracked changes") {
				n++
				if !strings.Contains(l, "stray.txt") {
					t.Errorf("dirty line should name stray.txt: %q", l)
				}
			}
		}
		return n
	}

	cfg, _, _, logs := testDevBuildConfig(t, localDir, "dev("+head[:7]+")+dirty", testSourceRemote)
	CheckAndRebuildDev(cfg)
	if n := count(*logs); n != 1 {
		t.Fatalf("first check: %d dirty lines, want 1; logs: %q", n, *logs)
	}
	*logs = nil
	CheckAndRebuildDev(cfg)
	if n := count(*logs); n != 0 {
		t.Errorf("second check repeated the dirty line; logs: %q", *logs)
	}
	if len(*logs) != 1 {
		t.Errorf("second check should still log its outcome line, logs: %q", *logs)
	}
}

func TestCheckAndRebuildDev_CleanCheckoutLogsNoDirtyLine(t *testing.T) {
	skipIfNoGit(t)
	resetDirtyReportedForTest()
	t.Cleanup(resetDirtyReportedForTest)
	_, localDir := initOriginAndClone(t)
	head := headOf(t, localDir)

	cfg, _, _, logs := testDevBuildConfig(t, localDir, "dev("+head[:7]+")", testSourceRemote)
	CheckAndRebuildDev(cfg)
	for _, l := range *logs {
		if strings.Contains(l, "uncommitted") {
			t.Errorf("clean checkout logged a dirty line: %q", l)
		}
	}
	if dirtyReported.Load() {
		t.Error("a clean check must not consume the once-only report")
	}
}

func TestReportDirtyCheckout_CapsPaths(t *testing.T) {
	skipIfNoGit(t)
	resetDirtyReportedForTest()
	t.Cleanup(resetDirtyReportedForTest)
	_, localDir := initOriginAndClone(t)
	for i := 0; i < maxDirtyPathsListed+3; i++ {
		if err := os.WriteFile(filepath.Join(localDir, fmt.Sprintf("f%02d.txt", i)), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _, _, logs := testDevBuildConfig(t, localDir, "", testSourceRemote)
	reportDirtyCheckout(cfg)
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "and 3 more") || !strings.Contains((*logs)[0], "(13 paths)") {
		t.Errorf("logs = %q, want one capped line with 'and 3 more'", *logs)
	}
}

func TestReportDirtyCheckout_GitFailureIsSilent(t *testing.T) {
	resetDirtyReportedForTest()
	t.Cleanup(resetDirtyReportedForTest)
	var logs []string
	cfg := DevBuildConfig{Dir: filepath.Join(t.TempDir(), "missing"), Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	reportDirtyCheckout(cfg)
	if len(logs) != 0 {
		t.Errorf("git failure should log nothing, got %q", logs)
	}
}

// TestCheckAndRebuildDev_PreExecHookRunsImmediatelyBeforeExec verifies the hook
// fires after the rebuild and PostBuildHook but strictly before the exec, which
// is the last point a caller can flush state an exec would lose.
func TestCheckAndRebuildDev_PreExecHookRunsImmediatelyBeforeExec(t *testing.T) {
	skipIfNoGit(t)
	dir := initDevSourceCheckout(t, "handarbeit/fabrik")

	cfg, postBuildCalled, execCalled, logs := testDevBuildConfig(t, dir, "dev(0000000)", "handarbeit/fabrik")
	var calls int
	cfg.PreExecHook = func() {
		calls++
		if !*postBuildCalled {
			t.Error("PreExecHook ran before PostBuildHook")
		}
		if *execCalled {
			t.Error("PreExecHook ran after the exec")
		}
	}
	CheckAndRebuildDev(cfg)

	if calls != 1 || !*execCalled {
		t.Errorf("PreExecHook calls = %d, exec called = %v, logs: %v", calls, *execCalled, *logs)
	}
}

// TestCheckAndRebuildDev_ExecFailedHookRunsOnlyWhenExecReturns verifies that a
// caller which paused work in PreExecHook is told to resume when the exec fails
// (an exec that succeeds never returns, so the hook cannot run then).
func TestCheckAndRebuildDev_ExecFailedHookRunsOnlyWhenExecReturns(t *testing.T) {
	skipIfNoGit(t)
	for _, tc := range []struct {
		name     string
		execErr  error
		wantHook int
	}{
		{"exec succeeded path returns nil", nil, 0},
		{"exec failed", fmt.Errorf("exec format error"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := initDevSourceCheckout(t, "handarbeit/fabrik")
			cfg, _, _, _ := testDevBuildConfig(t, dir, "dev(0000000)", "handarbeit/fabrik")
			origExecFn := execFn
			execFn = func(string, []string, []string) error { return tc.execErr }
			t.Cleanup(func() { execFn = origExecFn })
			var hook int
			cfg.ExecFailedHook = func() { hook++ }
			CheckAndRebuildDev(cfg)
			if hook != tc.wantHook {
				t.Errorf("ExecFailedHook calls = %d, want %d", hook, tc.wantHook)
			}
		})
	}
}
