package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Real-git tests of the catch-up's git half (#2044). Neutralisation: each fails with the
// step it names removed — the merge (pure/two-parents), stampCatchUpTrailer (trailer),
// restore() (every failure case's "worktree restored" assertion) or the busy handling
// (dirty). The push-rejected case is a hook-refused push; the lease itself is covered by
// TestPushCatchUp_RejectedWhenRemoteMovedAfterPrepare (worktree_catchup_test.go), where a
// concurrent push is refused rather than overwritten.

type catchUpGitWorld struct {
	wm      *WorktreeManager
	src     string
	head    string // the member branch head
	baseSHA string
	eng     *Engine
	p       trialParams
	m       trainMember
}

// newCatchUpGitWorld: origin has main and fabrik/issue-7 (one commit touching memberFile);
// then main advances by one commit touching mainFile, which the bare clone fetches. The
// member is therefore one base commit behind.
func newCatchUpGitWorld(t *testing.T, memberFile, memberContent, mainFile, mainContent string, claude *mockClaudeInvoker) *catchUpGitWorld {
	t.Helper()
	bareDir, wroot := setupBareRepoForTrain(t)
	src := filepath.Join(filepath.Dir(bareDir), "src")
	head := addMemberBranch(t, src, bareDir, "fabrik/issue-7", memberFile, memberContent)
	writeFile(t, filepath.Join(src, mainFile), mainContent)
	mustGit(t, src, "add", "-A")
	mustGit(t, src, "commit", "-m", "advance main")
	mustGit(t, bareDir, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
	baseSHA, err := gitRevParse(bareDir, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	wm := NewWorktreeManagerWithRoot(bareDir, wroot)
	client := &mockGitHubClient{fetchLabelsFn: func(o, r string, n int) ([]string, error) { return nil, nil }}
	eng := trainTestEngine(t, client, claude, wm)
	return &catchUpGitWorld{
		wm: wm, src: src, head: head, baseSHA: baseSHA, eng: eng,
		p: trialParams{owner: "owner", repo: "repo", baseBranch: "main", baseSHA: baseSHA, trainKey: "owner/repo", wm: wm, holdingStg: holdingStage(eng.cfg)},
		m: trainMember{item: gh.ProjectItem{Number: 7, Repo: "owner/repo", Status: "Queued"}, prNum: 70, headSHA: head},
	}
}

func (w *catchUpGitWorld) remoteHead(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(gitOutputDir(t, w.src, "rev-parse", "fabrik/issue-7"))
}

func (w *catchUpGitWorld) assertRestored(t *testing.T) {
	t.Helper()
	wtDir := w.wm.WorktreeDir(7)
	if got, _ := gitRevParse(wtDir, "HEAD"); got != w.head {
		t.Errorf("worktree HEAD = %s, want it restored to the member head %s", got, w.head)
	}
	if status, _ := gitOutputIn(wtDir, "status", "--porcelain"); status != "" {
		t.Errorf("worktree not clean after the failure: %q", status)
	}
	if got := w.remoteHead(t); got != w.head {
		t.Errorf("remote branch = %s, want it untouched at %s", got, w.head)
	}
}

func TestRunCatchUpGit_CleanMergeIsPureTwoParentMergeWithTrailerAndPushed(t *testing.T) {
	w := newCatchUpGitWorld(t, "member.txt", "member\n", "base.txt", "base\n", &mockClaudeInvoker{})

	out := w.eng.runCatchUpGit(context.Background(), w.p, w.m)
	if out.kind != catchUpPushed || !out.pure {
		t.Fatalf("outcome = %+v, want a pure push", out)
	}
	wtDir := w.wm.WorktreeDir(7)
	parents, _ := gitOutputIn(wtDir, "rev-list", "--parents", "-n", "1", out.newHead)
	if f := strings.Fields(parents); len(f) != 3 || f[1] != w.head || f[2] != w.baseSHA {
		t.Errorf("rev-list --parents = %q, want a merge of %s and %s", parents, w.head, w.baseSHA)
	}
	msg, _ := gitOutputIn(wtDir, "log", "-1", "--format=%B", out.newHead)
	if !strings.Contains(msg, catchUpTrailerKey+": "+w.baseSHA) {
		t.Errorf("commit message lacks the trailer:\n%s", msg)
	}
	if got := w.remoteHead(t); got != out.newHead {
		t.Errorf("remote branch = %s, want the catch-up commit %s", got, out.newHead)
	}
	for _, f := range []string{"member.txt", "base.txt"} {
		if _, err := os.Stat(filepath.Join(wtDir, f)); err != nil {
			t.Errorf("merged tree lacks %s: %v", f, err)
		}
	}
}

func resolveWithMarkerFreeFile(t *testing.T) *mockClaudeInvoker {
	t.Helper()
	return &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			if opts.CatchUpBaseSHA == "" {
				t.Error("the conflict-resolution invocation was not marked as a catch-up")
			}
			if len(comments) == 1 && !strings.Contains(comments[0].Body, "catch-up merge") {
				t.Errorf("the prompt still describes a trial merge:\n%s", comments[0].Body)
			}
			if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# resolved\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			mustGit(t, workDir, "add", "-A")
			mustGit(t, workDir, "commit", "-m", "chore(merge-train): resolve conflict for #7")
			return "resolved", false, TokenUsage{}, nil
		},
	}
}

func TestRunCatchUpGit_ConflictResolvedIsNotPureAndStillATwoParentMerge(t *testing.T) {
	w := newCatchUpGitWorld(t, "README.md", "# member\n", "README.md", "# main\n", resolveWithMarkerFreeFile(t))

	out := w.eng.runCatchUpGit(context.Background(), w.p, w.m)
	if out.kind != catchUpPushed {
		t.Fatalf("outcome = %+v, want a push", out)
	}
	if out.pure {
		t.Error("a catch-up that needed conflict resolution must not be pure — reviews of it are actionable")
	}
	parents, _ := gitOutputIn(w.wm.WorktreeDir(7), "rev-list", "--parents", "-n", "1", out.newHead)
	if f := strings.Fields(parents); len(f) != 3 || f[1] != w.head || f[2] != w.baseSHA {
		t.Errorf("rev-list --parents = %q", parents)
	}
	msg, _ := gitOutputIn(w.wm.WorktreeDir(7), "log", "-1", "--format=%B", out.newHead)
	if !strings.Contains(msg, catchUpTrailerKey+": "+w.baseSHA) {
		t.Errorf("commit message lacks the trailer:\n%s", msg)
	}
}

func TestRunCatchUpGit_UnresolvableConflictRestoresTheWorktreeAndEjects(t *testing.T) {
	// Claude "succeeds" but leaves the markers in place.
	w := newCatchUpGitWorld(t, "README.md", "# member\n", "README.md", "# main\n", &mockClaudeInvoker{})

	out := w.eng.runCatchUpGit(context.Background(), w.p, w.m)
	if out.kind != catchUpConflict {
		t.Fatalf("outcome = %+v, want catchUpConflict", out)
	}
	if !strings.Contains(out.reason, "could not be resolved") {
		t.Errorf("reason = %q", out.reason)
	}
	w.assertRestored(t)
}

func TestRunCatchUpGit_UsageLimitDefersAndRestores(t *testing.T) {
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, &claudeUsageLimitError{Message: "limit"}
		},
	}
	w := newCatchUpGitWorld(t, "README.md", "# member\n", "README.md", "# main\n", claude)

	out := w.eng.runCatchUpGit(context.Background(), w.p, w.m)
	if out.kind != catchUpDefer {
		t.Fatalf("outcome = %+v, want catchUpDefer — a usage limit is no verdict on the conflict", out)
	}
	w.assertRestored(t)
}

func TestRunCatchUpGit_RejectedPushFallsBackAndLeavesTheLocalBranchEqualToTheRemote(t *testing.T) {
	w := newCatchUpGitWorld(t, "member.txt", "member\n", "base.txt", "base\n", &mockClaudeInvoker{})
	hook := filepath.Join(w.src, ".git", "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'protected branch' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := w.eng.runCatchUpGit(context.Background(), w.p, w.m)
	if out.kind != catchUpFallback {
		t.Fatalf("outcome = %+v, want catchUpFallback", out)
	}
	if out.policyReason != "" {
		t.Errorf("bare prose without a GH marker must not be classified as policy, got %q (#2065)", out.policyReason)
	}
	w.assertRestored(t)

	// And the next attempt (hook removed) starts from a clean, equal branch.
	os.Remove(hook)
	if out := w.eng.runCatchUpGit(context.Background(), w.p, w.m); out.kind != catchUpPushed {
		t.Fatalf("retry outcome = %+v, want a push", out)
	}
}

// A push refused with GitHub's repository-rule marker is a policy rejection (#2065): the
// outcome carries the sanitised reason, and the worktree is restored like any other failure.
// git itself prefixes a hook's stderr with `remote: `, as it does GitHub's own output.
func TestRunCatchUpGit_RepoRuleRejectionIsClassifiedAsPolicy(t *testing.T) {
	w := newCatchUpGitWorld(t, "member.txt", "member\n", "base.txt", "base\n", &mockClaudeInvoker{})
	hook := filepath.Join(w.src, ".git", "hooks", "pre-receive")
	script := "#!/bin/sh\n" +
		"echo 'error: GH013: Repository rule violations found for refs/heads/fabrik/issue-7.' >&2\n" +
		"echo '- This branch must not contain merge commits.' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	out := w.eng.runCatchUpGit(context.Background(), w.p, w.m)
	if out.kind != catchUpFallback {
		t.Fatalf("outcome = %+v, want catchUpFallback", out)
	}
	if !strings.Contains(out.policyReason, "GH013") || !strings.Contains(out.policyReason, "must not contain merge commits") {
		t.Errorf("policyReason = %q", out.policyReason)
	}
	w.assertRestored(t)
}

func TestRunCatchUpGit_DirtyWorktreeDefersAndKeepsTheUncommittedWork(t *testing.T) {
	w := newCatchUpGitWorld(t, "member.txt", "member\n", "base.txt", "base\n", &mockClaudeInvoker{})
	wtDir, err := w.wm.PrepareCatchUp(7, "main", w.head)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wtDir, "wip.txt"), "uncommitted\n")

	out := w.eng.runCatchUpGit(context.Background(), w.p, w.m)
	if out.kind != catchUpDefer {
		t.Fatalf("outcome = %+v, want catchUpDefer", out)
	}
	if _, err := os.Stat(filepath.Join(wtDir, "wip.txt")); err != nil {
		t.Errorf("the uncommitted file was destroyed: %v", err)
	}
	if got := w.remoteHead(t); got != w.head {
		t.Errorf("remote moved to %s", got)
	}
}

func TestRunCatchUpGit_LocalBranchWithUnpushedWorkFallsBack(t *testing.T) {
	w := newCatchUpGitWorld(t, "member.txt", "member\n", "base.txt", "base\n", &mockClaudeInvoker{})
	wtDir, err := w.wm.PrepareCatchUp(7, "main", w.head)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wtDir, "unpushed.txt"), "x\n")
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "commit", "-m", "unpushed")
	local, _ := gitRevParse(wtDir, "HEAD")

	out := w.eng.runCatchUpGit(context.Background(), w.p, w.m)
	if out.kind != catchUpFallback {
		t.Fatalf("outcome = %+v, want catchUpFallback", out)
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != local {
		t.Errorf("unpushed work was touched: HEAD %s, was %s", got, local)
	}
}

func TestRewriteConflictCommentForCatchUp(t *testing.T) {
	for _, generated := range [][]string{nil, {"docs/llms-full.txt"}} {
		body := buildTrainConflictComment(gh.ProjectItem{Number: 7}, "abc123", generated).Body
		got := rewriteConflictCommentForCatchUp(body, 7, "abc123")
		if strings.Contains(got, "trial integration branch") {
			t.Errorf("generated=%v: still describes a trial:\n%s", generated, got)
		}
		if !strings.Contains(got, "catch-up merge of the merge-train's base commit `abc123`") {
			t.Errorf("generated=%v: not re-worded:\n%s", generated, got)
		}
	}
}
