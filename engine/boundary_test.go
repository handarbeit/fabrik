package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// --- applyWorktreeBoundary tests ---

func TestApplyWorktreeBoundary_ScopesEditAndWrite(t *testing.T) {
	tools := []string{"Read", "Edit", "Write", "Glob", "Bash(git:*)"}
	got := applyWorktreeBoundary(tools, "/work/issue-1")
	want := []string{"Read", "Edit(/work/issue-1/**)", "Write(/work/issue-1/**)", "Glob", "Bash(git:*)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("applyWorktreeBoundary = %v, want %v", got, want)
	}
}

func TestApplyWorktreeBoundary_CustomAllowedTools(t *testing.T) {
	tools := []string{"Edit", "Write", "Bash(go:*)"}
	got := applyWorktreeBoundary(tools, "/worktrees/issue-42")
	want := []string{"Edit(/worktrees/issue-42/**)", "Write(/worktrees/issue-42/**)", "Bash(go:*)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("applyWorktreeBoundary = %v, want %v", got, want)
	}
}

func TestApplyWorktreeBoundary_BashUnchanged(t *testing.T) {
	tools := []string{"Bash(git:*)", "Bash(gh:*)", "Read"}
	got := applyWorktreeBoundary(tools, "/some/path")
	if !reflect.DeepEqual(got, tools) {
		t.Errorf("applyWorktreeBoundary changed Bash entries: got %v, want %v", got, tools)
	}
}

func TestApplyWorktreeBoundary_EmptyWorkDir_NoOp(t *testing.T) {
	tools := []string{"Edit", "Write", "Read"}
	got := applyWorktreeBoundary(tools, "")
	if !reflect.DeepEqual(got, tools) {
		t.Errorf("applyWorktreeBoundary with empty workDir = %v, want %v", got, tools)
	}
}

func TestApplyWorktreeBoundary_DoesNotMutateInput(t *testing.T) {
	tools := []string{"Edit", "Write"}
	orig := make([]string, len(tools))
	copy(orig, tools)
	_ = applyWorktreeBoundary(tools, "/path")
	if !reflect.DeepEqual(tools, orig) {
		t.Error("applyWorktreeBoundary mutated input slice")
	}
}

func TestApplyWorktreeBoundary_EmptyTools(t *testing.T) {
	got := applyWorktreeBoundary([]string{}, "/path")
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

func TestApplyWorktreeBoundary_AlreadyScopedPassesThrough(t *testing.T) {
	// Pre-scoped entries are not "Edit" or "Write" bare — they should pass through unchanged.
	tools := []string{"Edit(/other/**)", "Write(/other/**)"}
	got := applyWorktreeBoundary(tools, "/new")
	if !reflect.DeepEqual(got, tools) {
		t.Errorf("pre-scoped entries changed: got %v, want %v", got, tools)
	}
}

// --- snapshotRepoRefs tests ---

func initRepoWithCommit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Create a commit so HEAD resolves.
	f := filepath.Join(dir, "README.md")
	if err := os.WriteFile(f, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestSnapshotRepoRefs_BareRepo(t *testing.T) {
	skipIfNoGit(t)
	bareDir := initBareRepo(t)
	refs, err := snapshotRepoRefs(bareDir)
	if err != nil {
		t.Fatalf("snapshotRepoRefs: %v", err)
	}
	// A freshly initialized bare repo returns a map (possibly empty).
	// Every value in the map must be a non-empty SHA.
	for ref, sha := range refs {
		if sha == "" {
			t.Errorf("ref %s has empty SHA", ref)
		}
		_ = ref
	}
}

func TestSnapshotRepoRefs_EmptyDir(t *testing.T) {
	refs, err := snapshotRepoRefs("")
	if err != nil {
		t.Fatalf("snapshotRepoRefs with empty dir: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected empty map, got %v", refs)
	}
}

func TestSnapshotRepoRefs_WithRefs(t *testing.T) {
	skipIfNoGit(t)
	// Create a regular repo with a commit, then clone it as bare.
	srcDir := initRepoWithCommit(t)
	bareDir := t.TempDir()
	cloneCmd := exec.Command("git", "clone", "--bare", srcDir, bareDir)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v\n%s", err, out)
	}
	refs, err := snapshotRepoRefs(bareDir)
	if err != nil {
		t.Fatalf("snapshotRepoRefs: %v", err)
	}
	if len(refs) == 0 {
		t.Error("expected refs after clone, got empty map")
	}
	// Every value should be a non-empty SHA.
	for ref, sha := range refs {
		if sha == "" {
			t.Errorf("ref %s has empty SHA", ref)
		}
	}
}

// --- crossRepoViolations tests ---

func TestCrossRepoViolations_NoChange(t *testing.T) {
	before := map[string]map[string]string{
		"owner/repo-a": {"refs/heads/main": "aaa"},
		"owner/repo-b": {"refs/heads/main": "bbb"},
	}
	after := map[string]map[string]string{
		"owner/repo-a": {"refs/heads/main": "aaa"},
		"owner/repo-b": {"refs/heads/main": "bbb"},
	}
	got := crossRepoViolations(before, after, "owner/repo-a")
	if len(got) != 0 {
		t.Errorf("expected no violations, got %v", got)
	}
}

func TestCrossRepoViolations_DetectsNewRef(t *testing.T) {
	before := map[string]map[string]string{
		"owner/repo-b": {},
	}
	after := map[string]map[string]string{
		"owner/repo-b": {"refs/heads/evil": "deadbeef"},
	}
	got := crossRepoViolations(before, after, "owner/repo-a")
	if len(got) != 1 {
		t.Fatalf("expected 1 violation, got %v", got)
	}
	if !strings.Contains(got[0], "repo-b") || !strings.Contains(got[0], "evil") {
		t.Errorf("unexpected violation string: %q", got[0])
	}
}

func TestCrossRepoViolations_DetectsChangedRef(t *testing.T) {
	before := map[string]map[string]string{
		"owner/repo-b": {"refs/heads/main": "aaa"},
	}
	after := map[string]map[string]string{
		"owner/repo-b": {"refs/heads/main": "bbb"},
	}
	got := crossRepoViolations(before, after, "owner/repo-a")
	if len(got) != 1 {
		t.Fatalf("expected 1 violation, got %v", got)
	}
	if !strings.Contains(got[0], "aaa") || !strings.Contains(got[0], "bbb") {
		t.Errorf("unexpected violation string: %q", got[0])
	}
}

func TestCrossRepoViolations_IgnoresActiveRepo(t *testing.T) {
	before := map[string]map[string]string{
		"owner/repo-a": {"refs/heads/main": "aaa"},
	}
	after := map[string]map[string]string{
		"owner/repo-a": {"refs/heads/main": "bbb", "refs/heads/feature": "ccc"},
	}
	got := crossRepoViolations(before, after, "owner/repo-a")
	if len(got) != 0 {
		t.Errorf("expected no violations for active repo, got %v", got)
	}
}

func TestCrossRepoViolations_SortedOutput(t *testing.T) {
	before := map[string]map[string]string{
		"owner/repo-b": {},
		"owner/repo-c": {},
	}
	after := map[string]map[string]string{
		"owner/repo-b": {"refs/heads/z": "111"},
		"owner/repo-c": {"refs/heads/a": "222"},
	}
	got := crossRepoViolations(before, after, "owner/repo-a")
	if len(got) != 2 {
		t.Fatalf("expected 2 violations, got %v", got)
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("violations not sorted: %v", got)
	}
}

func TestCrossRepoViolations_DetectsDeletedRef(t *testing.T) {
	before := map[string]map[string]string{
		"owner/repo-b": {"refs/heads/main": "aaa", "refs/heads/old-branch": "bbb"},
	}
	after := map[string]map[string]string{
		"owner/repo-b": {"refs/heads/main": "aaa"},
	}
	got := crossRepoViolations(before, after, "owner/repo-a")
	if len(got) != 1 {
		t.Fatalf("expected 1 violation for deleted ref, got %v", got)
	}
	if !strings.Contains(got[0], "old-branch") || !strings.Contains(got[0], "deleted") {
		t.Errorf("deletion violation should mention branch name and 'deleted': %q", got[0])
	}
}

func TestCrossRepoViolations_SkipsRepoAbsentFromBefore(t *testing.T) {
	// A repo that appears in after but not in before (lazy registration or
	// snapshot failure during pre-audit) must not generate false positives.
	before := map[string]map[string]string{
		"owner/repo-a": {"refs/heads/main": "aaa"},
	}
	after := map[string]map[string]string{
		"owner/repo-a": {"refs/heads/main": "aaa"},
		"owner/repo-b": {"refs/heads/main": "bbb"}, // new repo, not in before
	}
	got := crossRepoViolations(before, after, "owner/repo-a")
	if len(got) != 0 {
		t.Errorf("repos absent from before snapshot should not generate violations, got %v", got)
	}
}

func TestCrossRepoViolations_RemoteTrackingRefNotFlagged(t *testing.T) {
	// Reproduces the real-world false-positive: a refs/remotes/origin/* ref appearing
	// in a sibling bare clone (updated by a concurrent git fetch) must not be reported
	// as a boundary violation. Remote-tracking refs are passively-observed upstream
	// state, not locally-authored mutations.
	before := map[string]map[string]string{
		"owner/repo-b": {},
	}
	after := map[string]map[string]string{
		"owner/repo-b": {"refs/remotes/origin/fabrik/issue-64": "dbe2a1c917e2e9f1b7d784724c60717045e7e4dc"},
	}
	got := crossRepoViolations(before, after, "owner/repo-a")
	if len(got) != 0 {
		t.Errorf("remote-tracking ref change must not be flagged as a violation, got %v", got)
	}
}

func TestCrossRepoViolations_LocalBranchInSiblingFlagged(t *testing.T) {
	// A new refs/heads/ ref in a sibling repo IS a genuine boundary violation and
	// must be detected. This confirms the refs/remotes/ filter does not suppress
	// legitimate violations.
	before := map[string]map[string]string{
		"owner/repo-b": {},
	}
	after := map[string]map[string]string{
		"owner/repo-b": {"refs/heads/feature": "deadbeef"},
	}
	got := crossRepoViolations(before, after, "owner/repo-a")
	if len(got) != 1 {
		t.Fatalf("expected 1 violation for new local branch in sibling repo, got %v", got)
	}
	if !strings.Contains(got[0], "repo-b") || !strings.Contains(got[0], "feature") {
		t.Errorf("violation should name sibling repo and branch: %q", got[0])
	}
}
