package engine

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/githubauth"
)

// writeEngineTestAppKey generates a small (test-only) RSA key and writes it
// as a PEM file under dir, mirroring internal/githubauth/tokenauth_test.go's
// writeTestPrivateKey (duplicated rather than exported cross-package for one
// helper).
func writeEngineTestAppKey(t *testing.T, dir string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generating test RSA key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	path := filepath.Join(dir, "app-private-key.pem")
	if err := os.WriteFile(path, pemBytes, 0600); err != nil {
		t.Fatalf("writing private key file: %v", err)
	}
	return path
}

// newFakeGitHubAppServer serves just enough of the GitHub App + GraphQL
// surface for setUpGitHubAppAuth/resolveGitHubAppAuth to run end-to-end
// against an httptest server: /app (identity), /app/installations (list —
// #1763, needed only by the non-pinned discovery path; AppInstallationID
// pinned callers never hit it), /app/installations/{id} (granted
// permissions), /app/installations/{id}/access_tokens (token mint),
// /installation/repositories (#1763, called unconditionally by the
// non-pinned discovery path for every installation regardless of
// repository_selection — see internal/githubauth/derive.go), and /graphql
// (ResolveOwner's repositoryOwner query, answered from ownerType —
// "organization" or "user").
func newFakeGitHubAppServer(t *testing.T, installationID int64, account, ownerType string, permissions map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"slug": "fabrik", "id": 1})
	})
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"id":                   installationID,
				"account":              map[string]string{"login": account},
				"repository_selection": "all",
				"permissions":          permissions,
			},
		})
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "ghs_test_token",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":                   installationID,
			"account":              map[string]string{"login": account},
			"repository_selection": "all",
			"permissions":          permissions,
		})
	})
	mux.HandleFunc("/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"repositories": []map[string]interface{}{
				{"full_name": account + "/some-repo"},
			},
		})
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var typename string
		switch ownerType {
		case "organization":
			typename = "Organization"
		case "user":
			typename = "User"
		default:
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"data":{"repositoryOwner":{"__typename":%q,"id":"node-id-123"}}}`, typename)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestValidateGitHubAppConfig_UnconfiguredIsPATMode(t *testing.T) {
	if err := validateGitHubAppConfig(Config{}); err != nil {
		t.Errorf("validateGitHubAppConfig(unconfigured) = %v, want nil (PAT mode)", err)
	}
}

func TestValidateGitHubAppConfig_FullyConfiguredIsValid(t *testing.T) {
	cfg := Config{GitHubAppID: 1, GitHubAppPrivateKeyPath: filepath.Join(t.TempDir(), "key.pem"), GitHubAppInstallationID: 2}
	if err := validateGitHubAppConfig(cfg); err != nil {
		t.Errorf("validateGitHubAppConfig(fully configured) = %v, want nil", err)
	}
}

func TestValidateGitHubAppConfig_PartialConfig_NamesEachMissingField(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantMsg []string
	}{
		{"onlyAppID", Config{GitHubAppID: 1}, []string{"github_app_private_key_path", "github_app_installation_id"}},
		{"onlyKeyPath", Config{GitHubAppPrivateKeyPath: filepath.Join(t.TempDir(), "key.pem")}, []string{"github_app_id", "github_app_installation_id"}},
		{"onlyInstallationID", Config{GitHubAppInstallationID: 2}, []string{"github_app_id", "github_app_private_key_path"}},
		{"missingOnlyKeyPath", Config{GitHubAppID: 1, GitHubAppInstallationID: 2}, []string{"github_app_private_key_path"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGitHubAppConfig(tc.cfg)
			if err == nil {
				t.Fatal("expected an error for a partially-configured GitHub App setup")
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name missing field %q", err.Error(), want)
				}
			}
		})
	}
}

func TestRefuseGHESWithGitHubApp(t *testing.T) {
	if err := RefuseGHESWithGitHubApp(""); err != nil {
		t.Errorf("RefuseGHESWithGitHubApp(no GHES) = %v, want nil", err)
	}
	err := RefuseGHESWithGitHubApp("github.example.com")
	if err == nil {
		t.Fatal("expected an error refusing GHES + GitHub App auth combination")
	}
	if !strings.Contains(err.Error(), "github.example.com") {
		t.Errorf("error %q does not name the configured GHES host", err.Error())
	}
}

func TestRefuseHTTPSWorkerGitUnderAppAuth(t *testing.T) {
	tests := []struct {
		name          string
		gitSSH        bool
		hasSSHRewrite bool
		wantErr       bool
	}{
		{name: "https, no rewrite: refused", gitSSH: false, hasSSHRewrite: false, wantErr: true},
		{name: "git_ssh true: allowed", gitSSH: true, hasSSHRewrite: false, wantErr: false},
		{name: "SSH rewrite active: allowed", gitSSH: false, hasSSHRewrite: true, wantErr: false},
		{name: "both git_ssh and rewrite: allowed", gitSSH: true, hasSSHRewrite: true, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RefuseHTTPSWorkerGitUnderAppAuth(tt.gitSSH, tt.hasSSHRewrite)
			if tt.wantErr && err == nil {
				t.Fatal("expected a refusal error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
			if tt.wantErr {
				for _, want := range []string{"git_ssh", "insteadOf", "contents"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q missing expected substring %q", err.Error(), want)
					}
				}
			}
		})
	}
}

func TestRefuseWebhooksWithGitHubApp(t *testing.T) {
	if err := RefuseWebhooksWithGitHubApp(false); err != nil {
		t.Errorf("RefuseWebhooksWithGitHubApp(no webhooks) = %v, want nil", err)
	}
	err := RefuseWebhooksWithGitHubApp(true)
	if err == nil {
		t.Fatal("expected an error refusing --webhooks + GitHub App auth combination")
	}
	if !strings.Contains(err.Error(), "webhooks") {
		t.Errorf("error %q does not name --webhooks", err.Error())
	}
}

func TestFormatPermissionShortfalls_NamesEachOne(t *testing.T) {
	shortfalls := []githubauth.RequiredPermissionShortfall{
		{Permission: "issues", Required: "write", Granted: "read"},
		{Permission: "organization_projects", Required: "write", Granted: ""},
	}
	msg := FormatPermissionShortfalls(shortfalls)
	if !strings.Contains(msg, "issues") || !strings.Contains(msg, `required "write"`) || !strings.Contains(msg, `granted "read"`) {
		t.Errorf("message %q missing issues shortfall detail", msg)
	}
	if !strings.Contains(msg, "organization_projects") || !strings.Contains(msg, `granted "none"`) {
		t.Errorf("message %q missing organization_projects shortfall detail (want granted \"none\" for an absent permission)", msg)
	}
}

func TestResolveGitHubAppAuth_Unconfigured_ReturnsNilNilNilForPATMode(t *testing.T) {
	client, reconciler, err := resolveGitHubAppAuth(context.Background(), Config{}, t.TempDir(), "")
	if err != nil {
		t.Fatalf("resolveGitHubAppAuth(unconfigured): %v", err)
	}
	if client != nil || reconciler != nil {
		t.Errorf("resolveGitHubAppAuth(unconfigured) = (%v, %v), want (nil, nil) — PAT mode must be a no-op", client, reconciler)
	}
}

func TestResolveGitHubAppAuth_PartialConfig_FailsBeforeAnyNetworkCall(t *testing.T) {
	// No httptest server at all — a network attempt here would fail with a
	// connection error, not the config-validation error this test expects.
	cfg := Config{GitHubAppID: 1}
	_, _, err := resolveGitHubAppAuth(context.Background(), cfg, t.TempDir(), "http://127.0.0.1:1")
	if err == nil {
		t.Fatal("expected a config-validation error for a partial GitHub App config")
	}
	if !strings.Contains(err.Error(), "github_app_private_key_path") {
		t.Errorf("error %q does not name the missing field", err.Error())
	}
}

func TestResolveGitHubAppAuth_GHESCombination_Refused(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	cfg := Config{
		Owner: "handarbeit", GHESHost: "github.example.com",
		GitHubAppID: 1, GitHubAppPrivateKeyPath: keyPath, GitHubAppInstallationID: 2,
	}
	_, _, err := resolveGitHubAppAuth(context.Background(), cfg, dir, "http://127.0.0.1:1")
	if err == nil {
		t.Fatal("expected GHES + GitHub App auth combination to be refused")
	}
	if !strings.Contains(err.Error(), "GHES") && !strings.Contains(err.Error(), "Enterprise Server") {
		t.Errorf("error %q does not explain the GHES refusal", err.Error())
	}
}

func TestResolveGitHubAppAuth_WebhooksCombination_Refused(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	cfg := Config{
		Owner: "handarbeit", Webhooks: true,
		GitHubAppID: 1, GitHubAppPrivateKeyPath: keyPath, GitHubAppInstallationID: 2,
	}
	_, _, err := resolveGitHubAppAuth(context.Background(), cfg, dir, "http://127.0.0.1:1")
	if err == nil {
		t.Fatal("expected --webhooks + GitHub App auth combination to be refused")
	}
	if !strings.Contains(err.Error(), "webhooks") {
		t.Errorf("error %q does not explain the webhooks refusal", err.Error())
	}
}

func TestSetUpGitHubAppAuth_Success_WiresClientAndReconciler(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	srv := newFakeGitHubAppServer(t, 999, "handarbeit", "organization", RequiredGitHubAppPermissions(false))

	cfg := Config{
		Owner: "handarbeit", Repo: "fabrik",
		GitHubAppID: 42, GitHubAppPrivateKeyPath: keyPath, GitHubAppInstallationID: 999,
	}
	client, reconciler, err := setUpGitHubAppAuth(context.Background(), cfg, dir, srv.URL)
	if err != nil {
		t.Fatalf("setUpGitHubAppAuth: %v", err)
	}
	if client == nil {
		t.Fatal("expected a non-nil client")
	}
	if reconciler == nil {
		t.Fatal("expected a non-nil reconciler")
	}
	if got := reconciler.BotLogin(); got != "fabrik[bot]" {
		t.Errorf("BotLogin() = %q, want %q", got, "fabrik[bot]")
	}
	if got := client.Token(); got == "" {
		t.Error("expected the minted client to carry a non-empty token")
	}
}

// TestResolveGitHubAppAuth_BothConfigured_AppAuthWinsAndLogsPrecedence covers
// handarbeit-pruefer's PR review finding: when both a PAT (cfg.Token) and a
// full GitHub App config are present, App auth silently won with no trace
// that the PAT was ignored. This asserts both halves of the fix — App auth
// still wins (unchanged behavior) — and a precedence line is now logged to
// stdout, so an operator migrating between the two isn't left wondering why
// a still-valid FABRIK_TOKEN has no effect.
func TestResolveGitHubAppAuth_BothConfigured_AppAuthWinsAndLogsPrecedence(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	srv := newFakeGitHubAppServer(t, 999, "handarbeit", "organization", RequiredGitHubAppPermissions(false))

	cfg := Config{
		Owner: "handarbeit", Repo: "fabrik", Token: "ghp_still_valid_pat",
		GitHubAppID: 42, GitHubAppPrivateKeyPath: keyPath, GitHubAppInstallationID: 999,
	}

	var client *gh.Client
	var reconciler *githubauth.Reconciler
	var err error
	output := captureStdout(func() {
		client, reconciler, err = resolveGitHubAppAuth(context.Background(), cfg, dir, srv.URL)
	})
	if err != nil {
		t.Fatalf("resolveGitHubAppAuth: %v", err)
	}
	if client == nil || reconciler == nil {
		t.Fatal("expected App auth to win (non-nil client and reconciler) even with a PAT also configured")
	}
	if !strings.Contains(output, "takes precedence over the configured personal access token") {
		t.Errorf("expected a precedence-logging line naming the ignored PAT, got output: %q", output)
	}
}

// TestResolveGitHubAppAuth_NoPAT_NoPrecedenceLogLine confirms the log line
// added above is conditional on cfg.Token being set — the common case (App
// auth configured, no PAT at all) must not gain a spurious log line about a
// credential that was never present.
func TestResolveGitHubAppAuth_NoPAT_NoPrecedenceLogLine(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	srv := newFakeGitHubAppServer(t, 999, "handarbeit", "organization", RequiredGitHubAppPermissions(false))

	cfg := Config{
		Owner: "handarbeit", Repo: "fabrik",
		GitHubAppID: 42, GitHubAppPrivateKeyPath: keyPath, GitHubAppInstallationID: 999,
	}

	var err error
	output := captureStdout(func() {
		_, _, err = resolveGitHubAppAuth(context.Background(), cfg, dir, srv.URL)
	})
	if err != nil {
		t.Fatalf("resolveGitHubAppAuth: %v", err)
	}
	if strings.Contains(output, "takes precedence over the configured personal access token") {
		t.Errorf("did not expect a PAT-precedence log line when no PAT was configured, got output: %q", output)
	}
}

func TestSetUpGitHubAppAuth_UserOwnedBoard_RefusedExplicitly(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	srv := newFakeGitHubAppServer(t, 999, "someuser", "user", RequiredGitHubAppPermissions(false))

	cfg := Config{
		Owner: "someuser", Repo: "fabrik",
		GitHubAppID: 42, GitHubAppPrivateKeyPath: keyPath, GitHubAppInstallationID: 999,
	}
	_, _, err := setUpGitHubAppAuth(context.Background(), cfg, dir, srv.URL)
	if err == nil {
		t.Fatal("expected an explicit refusal for a user-owned board under App auth")
	}
	if !strings.Contains(err.Error(), "organization") {
		t.Errorf("error %q does not explain the organization-only requirement", err.Error())
	}
}

func TestSetUpGitHubAppAuth_GrantShortfall_NamesEachMissingPermission(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	// Grant only "metadata" and "issues", withholding organization_projects
	// and the other required scopes.
	srv := newFakeGitHubAppServer(t, 999, "handarbeit", "organization", map[string]string{
		"metadata": "read", "issues": "write",
	})

	cfg := Config{
		Owner: "handarbeit", Repo: "fabrik",
		GitHubAppID: 42, GitHubAppPrivateKeyPath: keyPath, GitHubAppInstallationID: 999,
	}
	_, _, err := setUpGitHubAppAuth(context.Background(), cfg, dir, srv.URL)
	if err == nil {
		t.Fatal("expected a grant-shortfall error")
	}
	if !strings.Contains(err.Error(), "organization_projects") {
		t.Errorf("error %q does not name the missing organization_projects permission", err.Error())
	}
}

// TestReconcile_NonPinnedDiscovery_NoBrowserOpensViaOptionsSeam is the
// R6/AC3 regression test for #1763: a test in the engine package must be
// able to exercise githubauth.Reconcile's non-pinned discovery path (a
// watched-but-uninstalled owner) with zero side effects on the developer's
// desktop, without needing to know that Options.NoBrowser's zero value
// (false) is otherwise unsafe. It deliberately leaves NoBrowser unset and
// relies solely on Options.OpenBrowser (R3's seam) to prove the seam itself
// is what makes this test safe — not a NoBrowser: true a test author would
// have to remember to set. The fake server's only installation is under
// "handarbeit"; watching "notinstalled/otherrepo" reaches
// guideMissingInstallations for the "notinstalled" owner.
func TestReconcile_NonPinnedDiscovery_NoBrowserOpensViaOptionsSeam(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	srv := newFakeGitHubAppServer(t, 111, "handarbeit", "organization", RequiredGitHubAppPermissions(false))

	var openedURLs []string
	stubOpenBrowser := func(url string) error {
		openedURLs = append(openedURLs, url)
		return nil
	}

	_, err := githubauth.Reconcile(context.Background(), githubauth.Options{
		AppID: 42, AppPrivateKeyPath: keyPath,
		AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"notinstalled/otherrepo"},
		BaseURL:      srv.URL,
		OpenBrowser:  stubOpenBrowser,
		// NoBrowser deliberately left unset (its unsafe zero value) — the
		// point of this test is that OpenBrowser alone is enough.
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(openedURLs) != 1 {
		t.Fatalf("stub OpenBrowser called %d times, want exactly 1 (proves the non-pinned discovery path reached guideMissingInstallations, safely, via the Options seam rather than a real browser)", len(openedURLs))
	}
	if !strings.Contains(openedURLs[0], "/apps/fabrik/installations/new") {
		t.Errorf("opened URL = %q, want it to name the App's guided-install page", openedURLs[0])
	}
}

// TestRun_ShutdownOnSignal_WithGitHubAppAuth_WaitsForRefreshLoop confirms
// Run()'s refresh-loop wiring (poll.go) doesn't deadlock: with e.ghAppAuth
// set, Run() must still start its refresh-loop goroutine under Run()'s own
// ctx, cancel it on shutdown, and return promptly — proving
// waitGitHubAppRefreshLoops' defer is ordered so cancel() (which stops the
// goroutine) runs before it blocks, not after. If the ordering in poll.go
// were reversed, this test would hang until its own timeout fires.
func TestRun_ShutdownOnSignal_WithGitHubAppAuth_WaitsForRefreshLoop(t *testing.T) {
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
	// GitSSH avoids tripping the #1756 App-auth+HTTPS-worker-git startup
	// refusal added to Run() — this test is about refresh-loop shutdown
	// wiring, not the git story, and must not depend on the host's own git
	// config for an insteadOf rewrite.
	eng.cfg.GitSSH = true
	eng.ghAppAuth = reconciler
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	eng.fabrikDir = dir

	readyCh := make(chan struct{})
	eng.cfg.ReadyCh = readyCh

	done := make(chan error, 1)
	go func() { done <- eng.Run() }()

	<-readyCh
	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGINT)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not shut down in time — the GitHub App refresh-loop wait may be deadlocked")
	}
}

func TestEngineRequiredGitHubAppPermissions_WebhooksAddsScope(t *testing.T) {
	without := RequiredGitHubAppPermissions(false)
	if _, ok := without["repository_hooks"]; ok {
		t.Error("repository_hooks permission should not be required when cfg.Webhooks is false")
	}
	with := RequiredGitHubAppPermissions(true)
	if with["repository_hooks"] != "write" {
		t.Errorf("repository_hooks permission = %q, want %q when cfg.Webhooks is true", with["repository_hooks"], "write")
	}
}
