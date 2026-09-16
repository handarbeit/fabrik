package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/githubauth"
)

// isolateGitConfig points the git-config lookups checkURLRewrite (and hence
// RefuseHTTPSWorkerGitUnderAppAuth's hasSSHRewrite input) makes at a
// test-local global config file, so these tests are deterministic regardless
// of the host's own ~/.gitconfig — mirroring tests/sim/simgh/git.go's
// GIT_CONFIG_GLOBAL/GIT_CONFIG_NOSYSTEM precedent (#1756, same class of
// problem R5 solves for the live e2e bed). rewrite, when non-empty, is
// written verbatim into the global config file so callers can opt a subtest
// into an active url.git@github.com:.insteadOf rewrite.
func isolateGitConfig(t *testing.T, rewrite string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfgPath, []byte(rewrite), 0644); err != nil {
		t.Fatalf("writing isolated git config: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfgPath)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

const sshRewriteGitConfig = "[url \"git@github.com:\"]\n\tinsteadOf = https://github.com/\n"

// runEngineUntilShutdown starts eng.Run() in the background, waits for
// ReadyCh (proving Run() got past every preflight, including the #1756
// refusal under test), sends SIGINT, and asserts a clean, prompt shutdown.
// Mirrors TestRun_ShutdownOnSignal_WithGitHubAppAuth_WaitsForRefreshLoop's
// shape.
func runEngineUntilShutdown(t *testing.T, eng *Engine) {
	t.Helper()
	readyCh := make(chan struct{})
	eng.cfg.ReadyCh = readyCh

	done := make(chan error, 1)
	go func() { done <- eng.Run() }()

	select {
	case <-readyCh:
	case err := <-done:
		t.Fatalf("Run() exited before signaling ready (preflight refused startup unexpectedly): %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not become ready in time")
	}

	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGINT)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not shut down in time")
	}
}

// newAppAuthTestEngine builds an Engine wired with a real, working
// githubauth.Reconciler (against a fake GitHub App server) as e.ghAppAuth,
// exactly as App-auth mode leaves it after New() — so Run()'s
// e.ghAppAuth != nil gate for the #1756 refusal is exercised the same way
// production code sets it, not via a bare struct-literal assignment.
func newAppAuthTestEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	srv := newFakeGitHubAppServer(t, 999, "handarbeit", "organization", RequiredGitHubAppPermissions(false))

	reconciler, err := githubauth.Reconcile(context.Background(), githubauth.Options{
		AppID: 42, AppInstallationID: 999, AppPrivateKeyPath: keyPath,
		AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"handarbeit/*"},
		BaseURL:      srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 300
	eng.ghAppAuth = reconciler
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	eng.fabrikDir = dir
	return eng
}

// TestRun_GitHubAppAuth_HTTPSNoRewrite_RefusedAtStartup covers #1756's AC2:
// App auth + default HTTPS git config, with no SSH rewrite masking it, must
// be refused at startup with a clear message — never left to fail later as a
// confusing mid-stage 403.
func TestRun_GitHubAppAuth_HTTPSNoRewrite_RefusedAtStartup(t *testing.T) {
	isolateGitConfig(t, "")
	eng := newAppAuthTestEngine(t)
	// eng.cfg.GitSSH is false by default (testEngine's Config zero value).

	err := eng.Run()
	if err == nil {
		t.Fatal("expected Run() to refuse App auth + default HTTPS worker git, got nil error")
	}
	for _, want := range []string{"HTTPS", "git_ssh", "insteadOf"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal error %q missing expected substring %q", err.Error(), want)
		}
	}
}

// TestRun_GitHubAppAuth_GitSSH_StartsCleanly confirms setting git_ssh: true
// avoids the #1756 refusal and Run() proceeds to a normal, signal-driven
// shutdown.
func TestRun_GitHubAppAuth_GitSSH_StartsCleanly(t *testing.T) {
	isolateGitConfig(t, "")
	eng := newAppAuthTestEngine(t)
	eng.cfg.GitSSH = true
	runEngineUntilShutdown(t, eng)
}

// TestRun_GitHubAppAuth_SSHRewrite_StartsCleanly confirms an active
// url.git@github.com:.insteadOf rewrite also avoids the #1756 refusal, since
// it transparently redirects worker HTTPS git to SSH before any credential
// helper is ever consulted.
func TestRun_GitHubAppAuth_SSHRewrite_StartsCleanly(t *testing.T) {
	isolateGitConfig(t, sshRewriteGitConfig)
	eng := newAppAuthTestEngine(t)
	// eng.cfg.GitSSH stays false — the rewrite alone must be sufficient.
	runEngineUntilShutdown(t, eng)
}

// TestRun_PATMode_HTTPSNoRewrite_Unaffected pins AC5: PAT mode (no App auth
// configured, e.ghAppAuth == nil) must never see the #1756 refusal, even
// under the exact HTTPS+no-rewrite configuration that triggers it under App
// auth.
func TestRun_PATMode_HTTPSNoRewrite_Unaffected(t *testing.T) {
	isolateGitConfig(t, "")
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 300
	// eng.ghAppAuth stays nil (PAT mode); eng.cfg.GitSSH stays false.
	runEngineUntilShutdown(t, eng)
}
