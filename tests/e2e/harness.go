//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Defaults wire to the canonical test bed. Override via env vars in CI or
// when the bed lives elsewhere.
const (
	defaultFabrikTestDir = "~/dev/fabrik-test"
	defaultRepoAlpha     = "handarbeit/fabrik-test-alpha"
	defaultRepoBeta      = "handarbeit/fabrik-test-beta"
	defaultProjectNumber = 2
	defaultProjectOwner  = "handarbeit"
)

// Env carries the resolved test-bed paths and identifiers. Constructed by
// LoadEnv from environment variables (with sensible defaults).
type Env struct {
	FabrikTestDir string // absolute path to ~/dev/fabrik-test
	LogPath       string // absolute path to the test instance's fabrik.log
	RepoAlpha     string // "owner/repo" form
	RepoBeta      string // "owner/repo" form
	ProjectOwner  string // org or user owning the test project
	ProjectNumber int    // GitHub Project v2 number
	GHToken       string // FABRIK_TOKEN from the test bed's .env (passed to gh as GH_TOKEN)
}

// LoadEnv resolves the test-bed environment. Fails the test cleanly if the
// bed is not set up.
func LoadEnv(t *testing.T) *Env {
	t.Helper()

	dir := getenvOr("FABRIK_TEST_DIR", expandHome(defaultFabrikTestDir))
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("test bed not present at %s — see tests/e2e/README.md for setup", dir)
	}

	// Read FABRIK_TOKEN from the test bed's own .env so we don't depend on
	// the operator having the right token in their current shell.
	envFile := filepath.Join(dir, ".env")
	token, err := readEnvFileValue(envFile, "FABRIK_TOKEN")
	if err != nil {
		t.Fatalf("could not read FABRIK_TOKEN from %s: %v", envFile, err)
	}

	return &Env{
		FabrikTestDir: dir,
		LogPath:       filepath.Join(dir, ".fabrik", "fabrik.log"),
		RepoAlpha:     getenvOr("FABRIK_TEST_REPO_ALPHA", defaultRepoAlpha),
		RepoBeta:      getenvOr("FABRIK_TEST_REPO_BETA", defaultRepoBeta),
		ProjectOwner:  getenvOr("FABRIK_TEST_PROJECT_OWNER", defaultProjectOwner),
		ProjectNumber: defaultProjectNumber,
		GHToken:       token,
	}
}

// AssertFabrikRunning verifies the test-bed Fabrik instance is alive.
// Skips the test if not — we don't auto-start it (yet).
func AssertFabrikRunning(t *testing.T, env *Env) {
	t.Helper()

	// Look for a process whose command line includes the fabrik-test directory
	// and ends in "/fabrik" (the binary, not a subprocess).
	out, _ := exec.Command("ps", "ax", "-o", "pid,command").CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, env.FabrikTestDir+"/fabrik") &&
			!strings.Contains(line, "grep") &&
			!strings.Contains(line, "claude") {
			return
		}
	}
	t.Skipf("Fabrik instance not running at %s — start it (cd %s && ./fabrik -notui &)", env.FabrikTestDir, env.FabrikTestDir)
}

// FileIssue creates an issue in the named repo with the given title, body, and
// labels. Returns the issue number. Registers a t.Cleanup to close the issue
// at test end.
func FileIssue(t *testing.T, env *Env, repo, title, body string, labels ...string) int {
	t.Helper()

	args := []string{"issue", "create", "-R", repo, "--title", title, "--body", body}
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	out, err := ghOutput(env, args...)
	if err != nil {
		t.Fatalf("file issue %q in %s: %v\n%s", title, repo, err, out)
	}
	// gh prints the URL on the last non-empty line.
	url := lastNonEmpty(out)
	num := parseIssueNumberFromURL(url)
	if num == 0 {
		t.Fatalf("could not parse issue number from gh output: %q", out)
	}

	t.Cleanup(func() {
		// Best-effort close; don't fail teardown.
		_, _ = ghOutput(env, "issue", "close", fmt.Sprint(num), "-R", repo, "--reason", "completed", "--comment", "Closing: e2e test teardown")
	})

	return num
}

// AddIssueToProject adds the issue to the test bed's project, returning the
// project-item ID for subsequent field edits.
func AddIssueToProject(t *testing.T, env *Env, repo string, issueNumber int) string {
	t.Helper()
	url := fmt.Sprintf("https://github.com/%s/issues/%d", repo, issueNumber)
	out, err := ghOutput(env, "project", "item-add", fmt.Sprint(env.ProjectNumber),
		"--owner", env.ProjectOwner, "--url", url, "--format", "json")
	if err != nil {
		t.Fatalf("add #%d to project: %v\n%s", issueNumber, err, out)
	}
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("parse project item-add output: %v\n%s", err, out)
	}
	return parsed.ID
}

// SetIssueStatus moves the project item to the named Status column (e.g. "Specify").
func SetIssueStatus(t *testing.T, env *Env, itemID, columnName string) {
	t.Helper()
	projectID, statusFieldID, optionID := resolveStatusOption(t, env, columnName)
	_, err := ghOutput(env, "project", "item-edit",
		"--id", itemID, "--project-id", projectID,
		"--field-id", statusFieldID, "--single-select-option-id", optionID)
	if err != nil {
		t.Fatalf("set issue status to %s: %v", columnName, err)
	}
}

// WaitForIssueLabel polls the issue until it has all of the named labels, or
// timeout expires.
func WaitForIssueLabel(t *testing.T, env *Env, repo string, issueNumber int, label string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		labels := IssueLabels(t, env, repo, issueNumber)
		for _, l := range labels {
			if l == label {
				return
			}
		}
		time.Sleep(15 * time.Second)
	}
	t.Fatalf("timed out waiting for label %q on %s#%d (had: %v)",
		label, repo, issueNumber, IssueLabels(t, env, repo, issueNumber))
}

// IssueLabels returns the current labels on the issue.
func IssueLabels(t *testing.T, env *Env, repo string, issueNumber int) []string {
	t.Helper()
	out, err := ghOutput(env, "issue", "view", fmt.Sprint(issueNumber), "-R", repo,
		"--json", "labels", "--jq", "[.labels[].name]")
	if err != nil {
		t.Fatalf("read labels for %s#%d: %v", repo, issueNumber, err)
	}
	var labels []string
	_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &labels)
	return labels
}

// WaitForLogLine tails the test bed's fabrik.log and returns the first line
// matching the substring (or all of the substrings). Use sparingly — prefer
// observable GitHub state over log scraping where possible.
func WaitForLogLine(t *testing.T, env *Env, substring string, timeout time.Duration) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	f, err := os.Open(env.LogPath)
	if err != nil {
		t.Fatalf("open %s: %v", env.LogPath, err)
	}
	defer f.Close()

	// Start from end of file; we only care about lines appearing after this call.
	if _, err := f.Seek(0, 2); err != nil {
		t.Fatalf("seek %s: %v", env.LogPath, err)
	}
	r := bufio.NewReader(f)

	for ctx.Err() == nil {
		line, err := r.ReadString('\n')
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if strings.Contains(line, substring) {
			return line
		}
	}
	t.Fatalf("timed out waiting for log line containing %q", substring)
	return ""
}

// ── small internals ────────────────────────────────────────────────────────

func getenvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func readEnvFileValue(path, key string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	prefix := key + "="
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix)), nil
		}
	}
	return "", fmt.Errorf("%s not found in %s", key, path)
}

func ghOutput(env *Env, args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	cmd.Env = append(os.Environ(), "GH_TOKEN="+env.GHToken)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func lastNonEmpty(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" {
			return l
		}
	}
	return ""
}

func parseIssueNumberFromURL(url string) int {
	// URL form: https://github.com/owner/repo/issues/NUM
	parts := strings.Split(url, "/")
	if len(parts) < 2 {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(parts[len(parts)-1], "%d", &n); err != nil {
		return 0
	}
	return n
}

func resolveStatusOption(t *testing.T, env *Env, columnName string) (projectID, statusFieldID, optionID string) {
	t.Helper()
	type field struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Options []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"options"`
	}
	type projectView struct {
		ID string `json:"id"`
	}

	pv, err := ghOutput(env, "project", "view", fmt.Sprint(env.ProjectNumber),
		"--owner", env.ProjectOwner, "--format", "json")
	if err != nil {
		t.Fatalf("project view: %v", err)
	}
	var pj projectView
	if err := json.Unmarshal([]byte(pv), &pj); err != nil {
		t.Fatalf("parse project view: %v", err)
	}

	fl, err := ghOutput(env, "project", "field-list", fmt.Sprint(env.ProjectNumber),
		"--owner", env.ProjectOwner, "--format", "json")
	if err != nil {
		t.Fatalf("project field-list: %v", err)
	}
	var wrapped struct {
		Fields []field `json:"fields"`
	}
	if err := json.Unmarshal([]byte(fl), &wrapped); err != nil {
		t.Fatalf("parse field-list: %v", err)
	}
	for _, f := range wrapped.Fields {
		if f.Name != "Status" {
			continue
		}
		for _, o := range f.Options {
			if o.Name == columnName {
				return pj.ID, f.ID, o.ID
			}
		}
	}
	t.Fatalf("could not resolve Status option %q", columnName)
	return "", "", ""
}
