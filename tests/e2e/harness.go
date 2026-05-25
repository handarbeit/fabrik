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
	"syscall"
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
//
// Detection strategy: check the lock file. Fabrik atomically writes its PID
// to .fabrik/fabrik.lock on startup and unlinks it on shutdown. If the file
// exists and the named PID is still alive, Fabrik is running.
func AssertFabrikRunning(t *testing.T, env *Env) {
	t.Helper()
	lockPath := filepath.Join(env.FabrikTestDir, ".fabrik", "fabrik.lock")
	contents, err := os.ReadFile(lockPath)
	if err != nil {
		t.Skipf("Fabrik instance not running at %s — no lock file (start it: cd %s && ./fabrik -notui --auto-upgrade &)",
			env.FabrikTestDir, env.FabrikTestDir)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(contents)), "%d", &pid); err != nil || pid <= 0 {
		t.Skipf("Fabrik lock file at %s is malformed (%q)", lockPath, contents)
	}
	if err := syscallSignalZero(pid); err != nil {
		t.Skipf("Fabrik lock file claims pid %d but process is dead (%v) — stale lock at %s; remove and restart",
			pid, err, lockPath)
	}
}

// syscallSignalZero sends signal 0 to a pid — a no-op signal that returns an
// error if the process doesn't exist. POSIX-portable liveness check.
func syscallSignalZero(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.Signal(0))
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

// AddLabel adds a label to the issue. Fails the test if gh refuses (e.g. label not seeded).
func AddLabel(t *testing.T, env *Env, repo string, issueNumber int, label string) {
	t.Helper()
	if _, err := ghOutput(env, "issue", "edit", fmt.Sprint(issueNumber), "-R", repo, "--add-label", label); err != nil {
		t.Fatalf("add label %q on %s#%d: %v", label, repo, issueNumber, err)
	}
}

// RemoveLabel removes a label from the issue.
func RemoveLabel(t *testing.T, env *Env, repo string, issueNumber int, label string) {
	t.Helper()
	if _, err := ghOutput(env, "issue", "edit", fmt.Sprint(issueNumber), "-R", repo, "--remove-label", label); err != nil {
		t.Fatalf("remove label %q on %s#%d: %v", label, repo, issueNumber, err)
	}
}

// CommentOnIssue posts a comment as the test bed's PAT identity. Used to simulate
// user input (e.g. resuming a FABRIK_BLOCKED_ON_INPUT pause).
func CommentOnIssue(t *testing.T, env *Env, repo string, issueNumber int, body string) {
	t.Helper()
	if _, err := ghOutput(env, "issue", "comment", fmt.Sprint(issueNumber), "-R", repo, "--body", body); err != nil {
		t.Fatalf("post comment on %s#%d: %v", repo, issueNumber, err)
	}
}

// WaitForIssueClosed polls until the issue is CLOSED or timeout expires.
func WaitForIssueClosed(t *testing.T, env *Env, repo string, issueNumber int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if IssueState(t, env, repo, issueNumber) == "CLOSED" {
			return
		}
		time.Sleep(15 * time.Second)
	}
	t.Fatalf("timed out waiting for %s#%d to close (still %s)", repo, issueNumber, IssueState(t, env, repo, issueNumber))
}

// WaitForLabelAbsent polls until the named label is no longer on the issue.
func WaitForLabelAbsent(t *testing.T, env *Env, repo string, issueNumber int, label string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		labels := IssueLabels(t, env, repo, issueNumber)
		present := false
		for _, l := range labels {
			if l == label {
				present = true
				break
			}
		}
		if !present {
			return
		}
		time.Sleep(15 * time.Second)
	}
	t.Fatalf("timed out waiting for label %q to disappear from %s#%d", label, repo, issueNumber)
}

// IssueState returns "OPEN" or "CLOSED".
func IssueState(t *testing.T, env *Env, repo string, issueNumber int) string {
	t.Helper()
	out, err := ghOutput(env, "issue", "view", fmt.Sprint(issueNumber), "-R", repo, "--json", "state", "--jq", ".state")
	if err != nil {
		t.Fatalf("read state for %s#%d: %v", repo, issueNumber, err)
	}
	return strings.TrimSpace(out)
}

// WaitForChildIssueInRepo polls the named child repo for the first issue
// authored by the bot during this test run. Returns the child issue number.
// Used to detect cross-repo spawn outcomes.
//
// Filter: state=OPEN, labels include "fabrik:sub-issue", createdAt >= since.
func WaitForChildIssueInRepo(t *testing.T, env *Env, childRepo string, since time.Time, timeout time.Duration) int {
	t.Helper()
	sinceStr := since.UTC().Format(time.RFC3339)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := ghOutput(env, "issue", "list", "-R", childRepo,
			"--label", "fabrik:sub-issue", "--state", "open",
			"--search", "created:>="+sinceStr,
			"--json", "number,createdAt", "--jq", "[.[] | .number]")
		if err == nil {
			var nums []int
			if json.Unmarshal([]byte(strings.TrimSpace(out)), &nums) == nil && len(nums) > 0 {
				return nums[0]
			}
		}
		time.Sleep(15 * time.Second)
	}
	t.Fatalf("timed out waiting for a child sub-issue in %s (since %s)", childRepo, sinceStr)
	return 0
}

// AssertBlockedBy verifies that `issueRepo#issueNumber` is recorded as
// blocked-by `blockerRepo#blockerNumber` via the Issue Dependencies API.
func AssertBlockedBy(t *testing.T, env *Env, issueRepo string, issueNumber int, blockerRepo string, blockerNumber int) {
	t.Helper()
	owner, name, ok := splitRepo(issueRepo)
	if !ok {
		t.Fatalf("bad repo: %q", issueRepo)
	}
	q := fmt.Sprintf(`
query {
  repository(owner: "%s", name: "%s") {
    issue(number: %d) {
      blockedBy(first: 10) { nodes { number repository { nameWithOwner } } }
    }
  }
}`, owner, name, issueNumber)
	out, err := ghOutput(env, "api", "graphql", "-f", "query="+q,
		"--jq", ".data.repository.issue.blockedBy.nodes")
	if err != nil {
		t.Fatalf("query blockedBy for %s#%d: %v\n%s", issueRepo, issueNumber, err, out)
	}
	type node struct {
		Number     int `json:"number"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	}
	var nodes []node
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &nodes); err != nil {
		t.Fatalf("parse blockedBy response: %v\n%s", err, out)
	}
	for _, n := range nodes {
		if n.Number == blockerNumber && n.Repository.NameWithOwner == blockerRepo {
			return
		}
	}
	t.Fatalf("%s#%d not blocked by %s#%d (blockedBy was: %+v)", issueRepo, issueNumber, blockerRepo, blockerNumber, nodes)
}

// CloseIssue best-effort closes the named issue (used in t.Cleanup).
func CloseIssue(env *Env, repo string, issueNumber int) {
	_, _ = ghOutput(env, "issue", "close", fmt.Sprint(issueNumber), "-R", repo,
		"--reason", "completed", "--comment", "Closing: e2e test teardown")
}

// ResetOpenIssues closes every OPEN issue on the test repos. Useful at session
// start to wipe state from prior runs.
func ResetOpenIssues(t *testing.T, env *Env) {
	t.Helper()
	for _, repo := range []string{env.RepoAlpha, env.RepoBeta} {
		out, err := ghOutput(env, "issue", "list", "-R", repo, "--state", "open",
			"--json", "number", "--jq", "[.[] | .number]")
		if err != nil {
			t.Logf("ResetOpenIssues: could not list issues in %s: %v", repo, err)
			continue
		}
		var nums []int
		_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &nums)
		for _, n := range nums {
			CloseIssue(env, repo, n)
			t.Logf("ResetOpenIssues: closed %s#%d", repo, n)
		}
	}
}

func splitRepo(repo string) (owner, name string, ok bool) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// LogOffset captures the size of the test bed's fabrik.log at this moment.
// Pass it to WaitForLogLine to scan starting from this offset — call this
// BEFORE the triggering action (filing an issue, posting a comment, etc.) so
// the scan doesn't miss lines emitted between the trigger and the wait.
//
// Returns 0 if the log file does not yet exist (Fabrik will create it).
func LogOffset(t *testing.T, env *Env) int64 {
	t.Helper()
	info, err := os.Stat(env.LogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("stat %s: %v", env.LogPath, err)
	}
	return info.Size()
}

// WaitForLogLine scans the test bed's fabrik.log starting at startOffset
// (typically the value returned by LogOffset before the trigger action) and
// returns the first line matching the substring, or fails after timeout.
//
// Avoids the seek-to-EOF race that would otherwise miss lines emitted
// between the trigger and the start of the wait.
//
// Prefer observable GitHub state over log scraping where possible — log
// formats are less stable than label/state transitions.
func WaitForLogLine(t *testing.T, env *Env, substring string, startOffset int64, timeout time.Duration) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	f, err := os.Open(env.LogPath)
	if err != nil {
		t.Fatalf("open %s: %v", env.LogPath, err)
	}
	defer f.Close()

	if _, err := f.Seek(startOffset, 0); err != nil {
		t.Fatalf("seek %s to %d: %v", env.LogPath, startOffset, err)
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
	t.Fatalf("timed out waiting for log line containing %q (scanned from offset %d)", substring, startOffset)
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

// readEnvFileValue returns the value of `key` from a .env-style file.
// Tolerates leading/trailing whitespace, surrounding quotes on the value,
// and blank/comment lines. Does not (yet) handle escapes or line
// continuations — keep secrets on single lines.
func readEnvFileValue(path, key string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) != key {
			continue
		}
		val := strings.TrimSpace(parts[1])
		// Strip a single matched pair of surrounding double or single quotes.
		if len(val) >= 2 {
			first, last := val[0], val[len(val)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		return val, nil
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

// parseIssueNumberFromURL extracts the trailing integer from an issue URL,
// tolerating optional trailing slash and trailing whitespace.
// URL form: https://github.com/owner/repo/issues/NUM[/]
func parseIssueNumberFromURL(url string) int {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
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
