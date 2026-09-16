package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/githubauth"
	"github.com/handarbeit/fabrik/warnings"
)

// This file covers #1750's regression requirements (R6): App auth must
// never gate out dispatch wholesale due to a permissions.push false
// negative, and the fix's own failure modes (ambiguous fetch, truncated
// list, confirmed zero-repo installation) must behave exactly as designed
// rather than silently reintroducing the same "nothing to do" failure
// shape the issue describes.

// --- resolveAppAccessibleRepos (New()'s startup fetch + R4 hard refusal) ---

func TestResolveAppAccessibleRepos_Success(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	srv := newFakeGitHubAppServer(t, 999, "handarbeit", "organization", RequiredGitHubAppPermissions(false),
		withAccessibleRepos("handarbeit/fabrik", "handarbeit/fabrik-test-alpha"))

	reconciler, err := githubauth.Reconcile(context.Background(), githubauth.Options{
		AppID: 42, AppInstallationID: 999, AppPrivateKeyPath: keyPath,
		AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"handarbeit/*"},
		BaseURL:      srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	result, err := resolveAppAccessibleRepos(reconciler, 999)
	if err != nil {
		t.Fatalf("resolveAppAccessibleRepos: %v", err)
	}
	if result.truncated {
		t.Error("expected truncated=false for a full single-page result")
	}
	if !result.repos["handarbeit/fabrik"] || !result.repos["handarbeit/fabrik-test-alpha"] {
		t.Errorf("repos = %v, want both handarbeit/fabrik and handarbeit/fabrik-test-alpha", result.repos)
	}
}

// TestResolveAppAccessibleRepos_ZeroRepos_HardRefuses is AC-critical (R4):
// a confirmed-empty installation must produce a distinguishable
// *noAccessibleReposError so New() can refuse startup outright, naming the
// installation — this is the "nothing to do, ever" misconfiguration case,
// not an ambiguous one.
func TestResolveAppAccessibleRepos_ZeroRepos_HardRefuses(t *testing.T) {
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

	_, err = resolveAppAccessibleRepos(reconciler, 999)
	if err == nil {
		t.Fatal("expected an error for a zero-repo installation")
	}
	var noRepos *noAccessibleReposError
	if !errors.As(err, &noRepos) {
		t.Fatalf("expected a *noAccessibleReposError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "999") {
		t.Errorf("error should name the installation ID: %v", err)
	}
	if !strings.Contains(err.Error(), "https://github.com/settings/installations/999") {
		t.Errorf("error should link to the installation's settings page: %v", err)
	}
}

// TestResolveAppAccessibleRepos_Truncated confirms the pagination-ceiling
// signal is plumbed through — a truncated result must not be conflated with
// either the zero-repos hard refusal or a fetch error.
func TestResolveAppAccessibleRepos_Truncated(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	repos := make([]string, 100)
	for i := range repos {
		repos[i] = fmt.Sprintf("handarbeit/repo-%03d", i)
	}
	srv := newFakeGitHubAppServer(t, 999, "handarbeit", "organization", RequiredGitHubAppPermissions(false),
		withAccessibleRepos(repos...), withReposTruncated())

	reconciler, err := githubauth.Reconcile(context.Background(), githubauth.Options{
		AppID: 42, AppInstallationID: 999, AppPrivateKeyPath: keyPath,
		AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"handarbeit/*"},
		BaseURL:      srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	result, err := resolveAppAccessibleRepos(reconciler, 999)
	if err != nil {
		t.Fatalf("resolveAppAccessibleRepos: %v", err)
	}
	if !result.truncated {
		t.Error("expected truncated=true when the fake server never returns a short page")
	}
}

// TestResolveAppAccessibleRepos_FetchError_IsNotHardRefusal confirms a
// transient listing failure is a plain error, never mistaken for the
// zero-repos hard-refusal case — New() must fail open (R3) on this, not
// refuse startup.
func TestResolveAppAccessibleRepos_FetchError_IsNotHardRefusal(t *testing.T) {
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	srv := newFakeGitHubAppServer(t, 999, "handarbeit", "organization", RequiredGitHubAppPermissions(false),
		withFailRepoList())

	reconciler, err := githubauth.Reconcile(context.Background(), githubauth.Options{
		AppID: 42, AppInstallationID: 999, AppPrivateKeyPath: keyPath,
		AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"handarbeit/*"},
		BaseURL:      srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	_, err = resolveAppAccessibleRepos(reconciler, 999)
	if err == nil {
		t.Fatal("expected an error for a failed repo listing")
	}
	var noRepos *noAccessibleReposError
	if errors.As(err, &noRepos) {
		t.Error("a fetch error must not be classified as the zero-repos hard refusal")
	}
}

// --- resolveRepoAccess's App-auth branch (resolveAppRepoAccess) ---

// TestResolveRepoAccess_AppMode_ListedRepoGrantsCanPushWithoutRESTCall is
// AC1/AC3: under App auth, a repo present in the installation's accessible
// list gets CanPush: true, and the PAT-only FetchRepoAccess REST endpoint
// (whose permissions.push is meaningless for an installation token) must
// never be called at all.
func TestResolveRepoAccess_AppMode_ListedRepoGrantsCanPushWithoutRESTCall(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	client := &mockGitHubClient{
		fetchRepoAccessFn: func(owner, repo string) (gh.RepoAccess, error) {
			t.Error("resolveRepoAccess must not call the PAT-only FetchRepoAccess REST endpoint under App auth (AC3)")
			return gh.RepoAccess{}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.ghAppAuth = &githubauth.Reconciler{}
	eng.appAccessibleRepos = map[string]bool{"owner/repo": true}
	eng.appAccessibleReposReady = true

	access := eng.resolveRepoAccess("owner", "repo")
	if !access.CanPush {
		t.Errorf("resolveRepoAccess() = %+v, want CanPush=true for a listed repo", access)
	}
}

// TestResolveRepoAccess_AppMode_ExcludedRepoRecordsWarning is AC4: a repo
// confirmed absent from a complete accessible-repo list gets CanPush: false
// and a persistent, TUI-visible warnings.Record entry — not merely a
// single startup log line an operator could miss.
func TestResolveRepoAccess_AppMode_ExcludedRepoRecordsWarning(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.ghAppAuth = &githubauth.Reconciler{}
	eng.appAccessibleRepos = map[string]bool{"owner/other-repo": true}
	eng.appAccessibleReposReady = true
	eng.cfg.GitHubAppInstallationID = 999

	access := eng.resolveRepoAccess("owner", "repo")
	if access.CanPush {
		t.Error("expected CanPush=false for a repo confirmed excluded from a complete accessible-repo list")
	}

	entries, err := warnings.Load()
	if err != nil {
		t.Fatalf("loading warnings: %v", err)
	}
	var found *warnings.Entry
	for i := range entries {
		if entries[i].Key == "repo_access:owner/repo" {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatal("expected a repo_access warnings.Record entry for the excluded repo")
	}
	if found.Type != "repo_access" {
		t.Errorf("Type = %q, want %q", found.Type, "repo_access")
	}
	if !strings.Contains(found.Detail, "999") {
		t.Errorf("Detail should name the installation ID: %q", found.Detail)
	}
}

// TestResolveRepoAccess_AppMode_TruncatedListFailsOpen is R3: a repo missing
// from a truncated list is ambiguous (it may simply be beyond the
// pagination ceiling), so it must never produce CanPush: false.
func TestResolveRepoAccess_AppMode_TruncatedListFailsOpen(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.ghAppAuth = &githubauth.Reconciler{}
	eng.appAccessibleRepos = map[string]bool{"owner/other-repo": true}
	eng.appAccessibleReposReady = true
	eng.appAccessibleReposTrunc = true

	access := eng.resolveRepoAccess("owner", "repo")
	if !access.CanPush {
		t.Errorf("resolveRepoAccess() = %+v, want CanPush=true (fail-open) for a repo missing from a truncated list", access)
	}
}

// TestResolveRepoAccess_AppMode_FetchNeverSucceededFailsOpen is R3's other
// ambiguous case: the startup fetch itself never completed successfully
// (appAccessibleReposReady stays false), which must also fail open rather
// than treat every repo as excluded.
func TestResolveRepoAccess_AppMode_FetchNeverSucceededFailsOpen(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.ghAppAuth = &githubauth.Reconciler{}
	// appAccessibleReposReady left at its zero value (false).

	access := eng.resolveRepoAccess("owner", "repo")
	if !access.CanPush {
		t.Errorf("resolveRepoAccess() = %+v, want CanPush=true (fail-open) when the startup fetch never succeeded", access)
	}
}

// --- checkAllowAutoMerge's App-auth skip (R5) ---

// TestCheckAllowAutoMerge_AppMode_NoOp confirms checkAllowAutoMerge doesn't
// even attempt the check under App auth: the real allow_auto_merge value is
// unreadable without administration scope, which the App deliberately
// doesn't request, so misreporting it (rather than skipping) would be worse
// than no check at all.
func TestCheckAllowAutoMerge_AppMode_NoOp(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	client := &mockGitHubClient{
		fetchRepoAccessFn: func(owner, repo string) (gh.RepoAccess, error) {
			t.Error("checkAllowAutoMerge must not probe repo access at all under App auth (R5)")
			return gh.RepoAccess{}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.ghAppAuth = &githubauth.Reconciler{}

	out := captureStdout(func() {
		eng.checkAllowAutoMerge("owner", "repo")
	})
	if out != "" {
		t.Errorf("expected no output from checkAllowAutoMerge under App auth; got: %q", out)
	}
}
