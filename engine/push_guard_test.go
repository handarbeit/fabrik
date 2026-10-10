package engine

import (
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// pushGuardEnv is a real-git fixture for the #2089 zero-ahead guard: a bare
// origin, a worktree manager, an engine over a mock client, and helpers that
// reproduce the #2058 shape.
type pushGuardEnv struct {
	t         *testing.T
	sourceDir string
	wm        *WorktreeManager
	e         *Engine
	client    *mockGitHubClient
	wtDir     string
}

func newPushGuardEnv(t *testing.T, issue int, pr *gh.PRDetails, prErr error) *pushGuardEnv {
	t.Helper()
	skipIfNoGit(t)
	sourceDir := initRepoWithRemote(t)
	wm := NewWorktreeManager(sourceDir)
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) { return pr, prErr },
	}
	e := NewWithDeps(Config{Owner: "owner", Repo: "repo", MaxConcurrent: 1, Stages: testStages()},
		client, &mockClaudeInvoker{}, wm)
	wtDir, err := wm.EnsureWorktree(issue, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	return &pushGuardEnv{t: t, sourceDir: sourceDir, wm: wm, e: e, client: client, wtDir: wtDir}
}

func (g *pushGuardEnv) git(dir string, args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

// remoteRef returns the origin SHA of the branch ("" when absent).
func (g *pushGuardEnv) remoteRef(branch string) string {
	g.t.Helper()
	out := g.git(g.wtDir, "ls-remote", "origin", branch)
	if out == "" {
		return ""
	}
	return strings.Fields(out)[0]
}

// humanPush simulates an out-of-band push: a commit on top of main lands on the
// remote issue branch without the local worktree knowing, and the bare clone's
// tracking refs are then refreshed (any full fetch does this) so the bare
// --force-with-lease would have passed.
func (g *pushGuardEnv) humanPush(branch string) string {
	g.t.Helper()
	sha := g.git(g.sourceDir, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "human fix")
	g.git(g.sourceDir, "push", "origin", sha+":refs/heads/"+branch)
	g.git(g.wm.baseDir, "fetch", "origin")
	return sha
}

func (g *pushGuardEnv) commitLocal(msg string) {
	g.t.Helper()
	g.git(g.wtDir, "commit", "--allow-empty", "-m", msg)
}

func (g *pushGuardEnv) addedLabels() []string {
	g.client.mu.Lock()
	defer g.client.mu.Unlock()
	var out []string
	for _, c := range g.client.addLabelCalls {
		out = append(out, c.labelName)
	}
	return out
}

func (g *pushGuardEnv) comments() []string {
	g.client.mu.Lock()
	defer g.client.mu.Unlock()
	var out []string
	for _, c := range g.client.addCommentCalls {
		out = append(out, c.body)
	}
	return out
}

func openPR(n int) *gh.PRDetails { return &gh.PRDetails{Number: n, State: "open"} }

func hasStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestPushGuard_RefusesZeroAheadWithOpenPR(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	human := g.humanPush("fabrik/issue-7")
	item := gh.ProjectItem{Number: 7}

	err := g.e.pushBranchUnlessQueued(item, g.wm)
	if !errors.Is(err, ErrPushRefusedZeroAhead) {
		t.Fatalf("want ErrPushRefusedZeroAhead, got %v", err)
	}
	if got := g.remoteRef("fabrik/issue-7"); got != human {
		t.Errorf("remote ref changed: got %s want %s", got, human)
	}
	labels := g.addedLabels()
	if !hasStr(labels, "fabrik:paused") || !hasStr(labels, "fabrik:awaiting-input") {
		t.Errorf("want paused+awaiting-input labels, got %v", labels)
	}
	comments := g.comments()
	if len(comments) != 1 {
		t.Fatalf("want exactly 1 comment, got %d: %v", len(comments), comments)
	}
	for _, want := range []string{"🏭 **Fabrik — ", "carries no commits", "left untouched", "PR #55"} {
		if !strings.Contains(comments[0], want) {
			t.Errorf("comment missing %q: %s", want, comments[0])
		}
	}
}

// Non-vacuity: with the guard neutralised the same setup overwrites the remote.
func TestPushGuard_NonVacuous_DisabledGuardOverwritesRemote(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	human := g.humanPush("fabrik/issue-7")
	g.e.SetPushZeroAheadGuardDisabledForTest(true)

	if err := g.e.pushBranchUnlessQueued(gh.ProjectItem{Number: 7}, g.wm); err != nil {
		t.Fatalf("push with guard disabled: %v", err)
	}
	if got := g.remoteRef("fabrik/issue-7"); got == human {
		t.Fatalf("expected the stale zero-ahead branch to overwrite the remote when the guard is off")
	}
	if len(g.comments()) != 0 {
		t.Errorf("no pause expected with guard disabled")
	}
}

func TestPushGuard_AheadOfBasePushes(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	g.commitLocal("real work")
	if err := g.e.pushBranchUnlessQueued(gh.ProjectItem{Number: 7}, g.wm); err != nil {
		t.Fatalf("push: %v", err)
	}
	if g.remoteRef("fabrik/issue-7") == "" {
		t.Error("expected branch on origin")
	}
	if len(g.comments()) != 0 {
		t.Errorf("unexpected comments: %v", g.comments())
	}
}

func TestPushGuard_BehindBaseWithOwnCommitsPushes(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	g.commitLocal("real work")
	// Move base ahead on origin and refresh the bare clone's tracking ref.
	g.git(g.sourceDir, "commit", "--allow-empty", "-m", "base moved")
	g.git(g.sourceDir, "push", "origin", "main")
	g.git(g.wm.baseDir, "fetch", "origin")
	if err := g.e.pushBranchUnlessQueued(gh.ProjectItem{Number: 7}, g.wm); err != nil {
		t.Fatalf("push: %v", err)
	}
}

func TestPushGuard_NoOpenPRPushes(t *testing.T) {
	cases := map[string]struct {
		pr  *gh.PRDetails
		err error
	}{
		"no PR":     {nil, nil},
		"closed PR": {&gh.PRDetails{Number: 5, State: "closed"}, nil},
		"merged PR": {&gh.PRDetails{Number: 5, State: "closed", Merged: true}, nil},
		"read err":  {nil, errors.New("boom")},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			g := newPushGuardEnv(t, 7, c.pr, c.err)
			if err := g.e.pushBranchUnlessQueued(gh.ProjectItem{Number: 7}, g.wm); err != nil {
				t.Fatalf("push: %v", err)
			}
			if g.remoteRef("fabrik/issue-7") == "" {
				t.Error("expected branch on origin")
			}
			if len(g.comments()) != 0 {
				t.Errorf("unexpected comments: %v", g.comments())
			}
		})
	}
}

func TestPushGuard_QueuedItemSkippedWithoutPause(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	err := g.e.pushBranchUnlessQueued(gh.ProjectItem{Number: 7, LinkedPRIsInMergeQueue: true}, g.wm)
	if err != nil {
		t.Fatalf("queued push should be a nil no-op, got %v", err)
	}
	if len(g.comments()) != 0 || len(g.addedLabels()) != 0 {
		t.Errorf("queued item must not be paused")
	}
}

func TestPushGuard_SpecOnlyCommitCountsAsAhead(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	g.git(g.wtDir, "commit", "--allow-empty", "-m", "spec: persist")
	// A spec-only commit (specs/7-x/spec.md) is still a commit ahead.
	n, err := g.wm.CommitsAheadOfRef(7, "main")
	if err != nil || n != 1 {
		t.Fatalf("CommitsAheadOfRef = %d, %v; want 1", n, err)
	}
	if err := g.e.pushBranchUnlessQueued(gh.ProjectItem{Number: 7}, g.wm); err != nil {
		t.Fatalf("push: %v", err)
	}
}

func TestPushGuard_BaseLabelMeasuresAgainstLabelBranch(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	// Create origin/feature one commit ahead of main, and a local branch == feature.
	feature := g.git(g.sourceDir, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "feature tip")
	g.git(g.sourceDir, "push", "origin", feature+":refs/heads/feature")
	g.git(g.wm.baseDir, "fetch", "origin")
	g.git(g.wtDir, "reset", "--hard", feature)
	g.humanPush("fabrik/issue-7")

	item := gh.ProjectItem{Number: 7, Labels: []string{"base:feature"}}
	err := g.e.pushBranchUnlessQueued(item, g.wm)
	if !errors.Is(err, ErrPushRefusedZeroAhead) {
		t.Fatalf("want refusal measured against origin/feature, got %v", err)
	}
	if n, _ := g.wm.CommitsAheadOfRef(7, "main"); n != 1 {
		t.Errorf("sanity: branch should be 1 ahead of main, got %d", n)
	}
}

func TestPushGuard_DedupesCommentAcrossEpisode(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	g.humanPush("fabrik/issue-7")
	item := gh.ProjectItem{Number: 7, Comments: []gh.Comment{{
		Body: buildZeroAheadPauseComment("fabrik/issue-7", "main", 55),
	}}}
	if err := g.e.pushBranchUnlessQueued(item, g.wm); !errors.Is(err, ErrPushRefusedZeroAhead) {
		t.Fatalf("want refusal, got %v", err)
	}
	if len(g.comments()) != 0 {
		t.Errorf("no second comment expected, got %v", g.comments())
	}
	if !hasStr(g.addedLabels(), "fabrik:paused") || !hasStr(g.addedLabels(), "fabrik:awaiting-input") {
		t.Errorf("pause labels should be re-applied, got %v", g.addedLabels())
	}
}

func TestPushGuard_MarkPRReadySkipsMarkReadyOnRefusal(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	g.humanPush("fabrik/issue-7")
	var mu sync.Mutex
	marked := 0
	g.client.markPRReadyFn = func(owner, repo string, prNumber int) error {
		mu.Lock()
		defer mu.Unlock()
		marked++
		return nil
	}
	g.e.markPRReady(gh.ProjectItem{Number: 7}, 55)
	mu.Lock()
	defer mu.Unlock()
	if marked != 0 {
		t.Errorf("MarkPRReady called %d times after a refused push", marked)
	}
}

func TestPushGuard_PRCreationPathsPushZeroAhead(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil) // even an "open" read must not matter
	if err := g.e.pushBranchForNewPR(gh.ProjectItem{Number: 7}, g.wm); err != nil {
		t.Fatalf("pushBranchForNewPR: %v", err)
	}
	if g.remoteRef("fabrik/issue-7") == "" {
		t.Error("zero-ahead branch should push on the PR-creation path")
	}
}

func TestCommitsAheadOfRef(t *testing.T) {
	g := newPushGuardEnv(t, 7, nil, nil)
	if n, err := g.wm.CommitsAheadOfRef(7, "main"); err != nil || n != 0 {
		t.Fatalf("zero case: %d, %v", n, err)
	}
	g.commitLocal("a")
	g.commitLocal("b")
	if n, err := g.wm.CommitsAheadOfRef(7, "main"); err != nil || n != 2 {
		t.Fatalf("two ahead: %d, %v", n, err)
	}
	if _, err := g.wm.CommitsAheadOfRef(7, "no-such-branch"); err == nil {
		t.Fatal("missing base ref must be an error, not zero")
	}
}
