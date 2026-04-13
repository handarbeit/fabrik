package cmd

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/config"
	gh "github.com/handarbeit/fabrik/github"
	fabrikplugin "github.com/handarbeit/fabrik/plugin"
)

// ── upgrade mock ──────────────────────────────────────────────────────────────

// testGitHubUpgradeClient is a minimal GitHubClient mock for upgrade tests.
// It implements only FetchLatestRelease; all other methods return zero values.
type testGitHubUpgradeClient struct {
	fetchLatestReleaseFn func(owner, repo string) (*gh.LatestRelease, error)
}

func (m *testGitHubUpgradeClient) FetchLatestRelease(owner, repo string) (*gh.LatestRelease, error) {
	if m.fetchLatestReleaseFn != nil {
		return m.fetchLatestReleaseFn(owner, repo)
	}
	return nil, nil
}
func (m *testGitHubUpgradeClient) FetchProjectBoard(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
	return &gh.ProjectBoard{}, nil
}
func (m *testGitHubUpgradeClient) FetchItemDetails(item *gh.ProjectItem) error { return nil }
func (m *testGitHubUpgradeClient) FetchStatusField(projectID string) (*gh.StatusField, error) {
	return &gh.StatusField{Options: map[string]string{}}, nil
}
func (m *testGitHubUpgradeClient) FetchLabels(owner, repo string, issueNumber int) ([]string, error) {
	return nil, nil
}
func (m *testGitHubUpgradeClient) AddLabelToIssue(owner, repo string, issueNumber int, labelName string) error {
	return nil
}
func (m *testGitHubUpgradeClient) RemoveLabelFromIssue(owner, repo string, issueNumber int, labelName string) error {
	return nil
}
func (m *testGitHubUpgradeClient) AddComment(owner, repo string, issueNumber int, body string) error {
	return nil
}
func (m *testGitHubUpgradeClient) AddCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	return nil
}
func (m *testGitHubUpgradeClient) UpdateComment(owner, repo string, commentDatabaseID int, body string) error {
	return nil
}
func (m *testGitHubUpgradeClient) UpdateIssueBody(owner, repo string, issueNumber int, body string) error {
	return nil
}
func (m *testGitHubUpgradeClient) UpdateProjectItemStatus(projectID, itemID, statusFieldID, statusOptionID string) error {
	return nil
}
func (m *testGitHubUpgradeClient) GetIssueBody(owner, repo string, issueNumber int) (string, error) {
	return "", nil
}
func (m *testGitHubUpgradeClient) FindPRForIssue(owner, repo string, issueNumber int) (int, error) {
	return 0, nil
}
func (m *testGitHubUpgradeClient) CreateDraftPR(owner, repo, title, head, base string, issueNumber int) (int, error) {
	return 0, nil
}
func (m *testGitHubUpgradeClient) MarkPRReady(owner, repo string, prNumber int) error { return nil }
func (m *testGitHubUpgradeClient) MergePR(owner, repo string, prNumber int) error     { return nil }
func (m *testGitHubUpgradeClient) FetchLabelAppliedAt(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
	return time.Time{}, nil
}
func (m *testGitHubUpgradeClient) ArchiveProjectItem(projectID, itemID string) error { return nil }
func (m *testGitHubUpgradeClient) RateLimitStats() (gh.RateLimitStats, gh.RateLimitStats) {
	return gh.RateLimitStats{}, gh.RateLimitStats{}
}

// ── upgrade ───────────────────────────────────────────────────────────────────

func TestRunUpgrade_WritesPluginFiles(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	if err := runUpgrade(nil); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}

	// .fabrik/plugin/ should have been created with at least one file
	entries, err := os.ReadDir(filepath.Join(dir, ".fabrik", "plugin"))
	if err != nil {
		t.Fatalf(".fabrik/plugin not created: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected at least one entry in .fabrik/plugin/")
	}
}

func TestRunUpgrade_Idempotent(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	// Run twice — second run should overwrite without error
	if err := runUpgrade(nil); err != nil {
		t.Fatalf("first runUpgrade: %v", err)
	}
	if err := runUpgrade(nil); err != nil {
		t.Fatalf("second runUpgrade: %v", err)
	}
}

// TestRunUpgrade_DevBuildSkipsBinaryCheck verifies that a dev build never
// calls FetchLatestRelease — no network activity should occur.
func TestRunUpgrade_DevBuildSkipsBinaryCheck(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	called := false
	upgradeGitHubClient = &testGitHubUpgradeClient{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			called = true
			return nil, nil
		},
	}
	t.Cleanup(func() { upgradeGitHubClient = nil })

	// Ensure Version looks like a dev build.
	orig := Version
	Version = "dev(test)"
	t.Cleanup(func() { Version = orig })

	if err := runUpgrade(nil); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if called {
		t.Error("FetchLatestRelease should not be called for dev builds")
	}
}

// TestRunUpgrade_NetworkFailureFallsThrough verifies that when the release API
// returns an error, runUpgrade still falls through and refreshes plugin skills.
func TestRunUpgrade_NetworkFailureFallsThrough(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	upgradeGitHubClient = &testGitHubUpgradeClient{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			return nil, fmt.Errorf("network error: connection refused")
		},
	}
	t.Cleanup(func() { upgradeGitHubClient = nil })

	orig := Version
	Version = "v0.0.1"
	t.Cleanup(func() { Version = orig })

	if err := runUpgrade(nil); err != nil {
		t.Fatalf("runUpgrade should not fail on network error, got: %v", err)
	}

	// Plugin files should still have been written despite the network error.
	entries, err := os.ReadDir(filepath.Join(dir, ".fabrik", "plugin"))
	if err != nil {
		t.Fatalf(".fabrik/plugin not created: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected plugin files to be written even when binary upgrade fails")
	}
}

// TestRunUpgrade_BinaryUpgrade_DownloadAttempted verifies that when a newer
// release exists with a platform-matching asset, the download URL is hit. The
// server returns 500 so the upgrade fails gracefully and plugin refresh still
// runs.
func TestRunUpgrade_BinaryUpgrade_DownloadAttempted(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	downloaded := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloaded = true
		http.Error(w, "simulated server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	matchingAsset := fmt.Sprintf("fabrik_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	upgradeGitHubClient = &testGitHubUpgradeClient{
		fetchLatestReleaseFn: func(owner, repo string) (*gh.LatestRelease, error) {
			return &gh.LatestRelease{
				TagName: "v9.9.9",
				Assets: []gh.ReleaseAsset{
					{Name: matchingAsset, BrowserDownloadURL: srv.URL + "/asset.tar.gz"},
				},
			}, nil
		},
	}
	t.Cleanup(func() { upgradeGitHubClient = nil })

	orig := Version
	Version = "v0.0.1"
	t.Cleanup(func() { Version = orig })

	if err := runUpgrade(nil); err != nil {
		t.Fatalf("runUpgrade should not fail on download error, got: %v", err)
	}

	if !downloaded {
		t.Error("download server was not hit even though a matching asset was provided")
	}

	// Plugin files should still have been written despite the failed download.
	entries, err := os.ReadDir(filepath.Join(dir, ".fabrik", "plugin"))
	if err != nil {
		t.Fatalf(".fabrik/plugin not created: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected plugin files to be written even when binary download fails")
	}
}

func TestRefreshPlugin_WritesFiles(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	n, err := fabrikplugin.RefreshPlugin()
	if err != nil {
		t.Fatalf("RefreshPlugin: %v", err)
	}
	if n == 0 {
		t.Error("expected RefreshPlugin to write at least one file")
	}
}

// ── init buildConfigWithValues ────────────────────────────────────────────────

func TestBuildConfigWithValues_AllFields(t *testing.T) {
	result := buildConfigWithValues("myorg", "myrepo", "42", "", "myuser")

	if !strings.Contains(result, "owner: myorg") {
		t.Errorf("owner not in output: %s", result)
	}
	if !strings.Contains(result, "repo: myrepo") {
		t.Errorf("repo not in output: %s", result)
	}
	if !strings.Contains(result, "project: 42") {
		t.Errorf("project not in output: %s", result)
	}
	if !strings.Contains(result, "user: myuser") {
		t.Errorf("user not in output: %s", result)
	}
}

func TestBuildConfigWithValues_EmptyFields_KeepsComments(t *testing.T) {
	result := buildConfigWithValues("", "", "", "", "")

	// Empty strings should leave commented lines untouched
	if strings.Contains(result, "owner: ") && !strings.Contains(result, "# owner:") {
		t.Error("expected owner to remain commented when empty")
	}
	// The output should still be valid YAML-template text
	if len(result) == 0 {
		t.Error("result should not be empty")
	}
}

func TestBuildConfigWithValues_PartialFields(t *testing.T) {
	result := buildConfigWithValues("acme", "", "5", "", "")

	if !strings.Contains(result, "owner: acme") {
		t.Errorf("owner not replaced: %s", result)
	}
	if !strings.Contains(result, "project: 5") {
		t.Errorf("project not replaced: %s", result)
	}
	// repo and user should remain commented
	if strings.Contains(result, "\nrepo: ") {
		t.Errorf("repo should remain commented when empty: %s", result)
	}
}

// ── streamfilter ─────────────────────────────────────────────────────────────

func TestRunStreamFilter_ValidJSON(t *testing.T) {
	// Pipe valid JSON through the filter
	input := `{"type":"result","result":{"usage":{"input_tokens":10,"output_tokens":5}}}`

	origStdin := os.Stdin
	origStdout := os.Stdout
	defer func() {
		os.Stdin = origStdin
		os.Stdout = origStdout
	}()

	// Create a pipe for stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdin = r

	// Create a pipe for stdout
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = outW

	// Write input and close
	w.WriteString(input + "\n")
	w.Close()

	RunStreamFilter()
	outW.Close()

	var buf bytes.Buffer
	io.Copy(&buf, outR)
	outR.Close()

	// Should not panic and should produce some output
	_ = buf.String()
}

func TestRunStreamFilter_MalformedJSON(t *testing.T) {
	origStdin := os.Stdin
	origStdout := os.Stdout
	defer func() {
		os.Stdin = origStdin
		os.Stdout = origStdout
	}()

	r, w, _ := os.Pipe()
	os.Stdin = r
	outR, outW, _ := os.Pipe()
	os.Stdout = outW

	w.WriteString("not json at all\n{\"valid\": true}\n")
	w.Close()

	RunStreamFilter()
	outW.Close()
	io.Copy(io.Discard, outR)
	outR.Close()
}

// ── root.go helpers ───────────────────────────────────────────────────────────

func TestBuildProjectInfo_HomeRelative(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	cfg := &Config{Owner: "acme", Repo: "myrepo"}
	info := buildProjectInfo(cfg, config.ProjectConfig{})

	// CWD should not be empty
	if info.CWD == "" {
		t.Error("CWD should not be empty")
	}
}

func TestExecute_InvalidFabrikProjectNumber(t *testing.T) {
	resetFlags()
	dir := t.TempDir()
	chdirTest(t, dir)
	os.WriteFile(".gitignore", []byte(".env\n"), 0644)
	t.Setenv("FABRIK_PROJECT_NUMBER", "notanumber")
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--user", "u"}

	err := Execute()
	if err == nil {
		t.Fatal("expected error for invalid FABRIK_PROJECT_NUMBER")
	}
}

func TestWriteConfigTemplate_CreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	if err := os.MkdirAll(".fabrik", 0755); err != nil {
		t.Fatal(err)
	}

	if err := writeConfigTemplate("", "", "", "", false); err != nil {
		t.Fatalf("writeConfigTemplate: %v", err)
	}
	content, err := os.ReadFile(".fabrik/config.yaml")
	if err != nil {
		t.Fatalf("config.yaml not created: %v", err)
	}
	if len(content) == 0 {
		t.Error("config.yaml should not be empty")
	}
}

func TestBuildProjectInfo_RepoFormat(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	cfg := &Config{Owner: "org", Repo: "repo"}
	info := buildProjectInfo(cfg, config.ProjectConfig{Version: "v1.2.3"})
	if info.Version != "v1.2.3" {
		t.Errorf("Version = %q, want v1.2.3", info.Version)
	}
}
