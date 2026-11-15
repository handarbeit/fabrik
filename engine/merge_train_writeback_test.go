package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Tests for the multi-member trial conflict write-back (#2572, ADR 2572).
//
// Neutralisation (each fails with the step it names removed):
//   - Replayed*           : the base merge / noClaude resolve / PushCatchUp in runWriteBackGit.
//   - NeedsClaude*        : the noClaude guard in resolveConflictWithClaude.
//   - CleanBaseMerge*     : the clean-merge skip (it would push a pure catch-up).
//   - Busy*               : catchUpBusyReason / PrepareCatchUp.
//   - Rejected*           : restore() after a refused push.
//   - Gate*               : the single-member / seam / attempted-once guards in writeBackGateOpen.
//   - Continue*           : the tree-equality check in continueTrialFromHead.

// writeBackWorld is the catch-up git world plus a rerere-enabled bare clone.
type writeBackWorld struct {
	*catchUpGitWorld
	claudeCalls int
	mu          sync.Mutex
}

func newWriteBackWorld(t *testing.T) *writeBackWorld {
	t.Helper()
	ww := &writeBackWorld{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			ww.mu.Lock()
			ww.claudeCalls++
			ww.mu.Unlock()
			return "", false, TokenUsage{}, nil
		},
	}
	ww.catchUpGitWorld = newCatchUpGitWorld(t, "README.md", "# member\n", "README.md", "# main\n", claude)
	mustGitDir(t, ww.wm.baseDir, "config", "rerere.enabled", "true")
	mustGitDir(t, ww.wm.baseDir, "config", "rerere.autoupdate", "true")
	return ww
}

// recordResolution resolves the member-vs-base conflict once in a scratch worktree so
// rerere holds it, mirroring the trial that resolved it first.
func (w *writeBackWorld) recordResolution(t *testing.T) {
	t.Helper()
	scratch := filepath.Join(t.TempDir(), "scratch")
	mustGitDir(t, w.wm.baseDir, "worktree", "add", "--detach", scratch, w.head)
	t.Cleanup(func() { gitOutputIn(w.wm.baseDir, "worktree", "remove", "--force", scratch) })
	mustGit(t, scratch, "config", "user.email", "t@t.com")
	mustGit(t, scratch, "config", "user.name", "T")
	if _, err := gitCombined(scratch, "merge", "--no-ff", "-m", "scratch", w.baseSHA); err == nil {
		t.Fatal("scratch merge should conflict")
	}
	writeFile(t, filepath.Join(scratch, "README.md"), "# resolved\n")
	mustGit(t, scratch, "add", "-A")
	mustGit(t, scratch, "commit", "-m", "resolve")
}

func TestRunWriteBackGit_ReplayedResolutionIsPushedAsTwoParentMergeWithoutClaude(t *testing.T) {
	w := newWriteBackWorld(t)
	w.recordResolution(t)

	out := w.eng.runWriteBackGit(context.Background(), w.p, w.m)
	if out.kind != wbWritten {
		t.Fatalf("outcome = %+v, want wbWritten", out)
	}
	if w.claudeCalls != 0 {
		t.Errorf("Claude invoked %d time(s); the write-back replays, never derives", w.claudeCalls)
	}
	wtDir := w.wm.WorktreeDir(7)
	parents, _ := gitOutputIn(wtDir, "rev-list", "--parents", "-n", "1", out.newHead)
	if f := strings.Fields(parents); len(f) != 3 || f[1] != w.head || f[2] != w.baseSHA {
		t.Errorf("rev-list --parents = %q, want a merge of %s and %s", parents, w.head, w.baseSHA)
	}
	if got := w.remoteHead(t); got != out.newHead {
		t.Errorf("remote branch = %s, want %s", got, out.newHead)
	}
	if b, _ := os.ReadFile(filepath.Join(wtDir, "README.md")); string(b) != "# resolved\n" {
		t.Errorf("README.md = %q, want the replayed resolution", b)
	}
	msg, _ := gitOutputIn(wtDir, "log", "-1", "--format=%B", out.newHead)
	if !strings.Contains(msg, catchUpTrailerKey+": "+w.baseSHA) {
		t.Errorf("commit lacks the audit trailer:\n%s", msg)
	}
}

func TestRunWriteBackGit_NeedsClaudeIsSkippedAndRestored(t *testing.T) {
	w := newWriteBackWorld(t) // no recorded resolution: rerere cannot replay
	out := w.eng.runWriteBackGit(context.Background(), w.p, w.m)
	if out.kind != wbSkipped || !strings.Contains(out.reason, "Claude") {
		t.Fatalf("outcome = %+v, want a skip naming Claude", out)
	}
	if w.claudeCalls != 0 {
		t.Errorf("Claude invoked %d time(s) by a write-back", w.claudeCalls)
	}
	w.assertRestored(t)
}

func TestRunWriteBackGit_CleanBaseMergeIsSkippedAndRestored(t *testing.T) {
	w := &writeBackWorld{}
	w.catchUpGitWorld = newCatchUpGitWorld(t, "member.txt", "member\n", "base.txt", "base\n", &mockClaudeInvoker{})
	out := w.eng.runWriteBackGit(context.Background(), w.p, w.m)
	if out.kind != wbSkipped || !strings.Contains(out.reason, "merges cleanly") {
		t.Fatalf("outcome = %+v, want a clean-merge skip", out)
	}
	w.assertRestored(t)
}

func TestRunWriteBackGit_OwnershipChecksSkipWithoutTouchingTheBranch(t *testing.T) {
	t.Run("fabrik:editing", func(t *testing.T) {
		w := newWriteBackWorld(t)
		w.recordResolution(t)
		w.eng.client.(*mockGitHubClient).fetchLabelsFn = func(o, r string, n int) ([]string, error) {
			return []string{catchUpEditingLabel}, nil
		}
		out := w.eng.runWriteBackGit(context.Background(), w.p, w.m)
		if out.kind != wbSkipped || !strings.Contains(out.reason, catchUpEditingLabel) {
			t.Fatalf("outcome = %+v", out)
		}
		if got := w.remoteHead(t); got != w.head {
			t.Errorf("remote moved to %s", got)
		}
	})
	t.Run("unreadable labels", func(t *testing.T) {
		w := newWriteBackWorld(t)
		w.recordResolution(t)
		w.eng.client.(*mockGitHubClient).fetchLabelsFn = func(o, r string, n int) ([]string, error) {
			return nil, os.ErrDeadlineExceeded
		}
		if out := w.eng.runWriteBackGit(context.Background(), w.p, w.m); out.kind != wbSkipped {
			t.Fatalf("outcome = %+v", out)
		}
	})
	t.Run("dirty worktree", func(t *testing.T) {
		w := newWriteBackWorld(t)
		w.recordResolution(t)
		wtDir, err := w.wm.PrepareCatchUp(7, "main", w.head)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(wtDir, "wip.txt"), "uncommitted\n")
		out := w.eng.runWriteBackGit(context.Background(), w.p, w.m)
		if out.kind != wbSkipped {
			t.Fatalf("outcome = %+v", out)
		}
		if _, err := os.Stat(filepath.Join(wtDir, "wip.txt")); err != nil {
			t.Errorf("uncommitted file destroyed: %v", err)
		}
	})
	t.Run("remote moved", func(t *testing.T) {
		w := newWriteBackWorld(t)
		w.recordResolution(t)
		m := w.m
		m.headSHA = "0000000000000000000000000000000000000000"
		if out := w.eng.runWriteBackGit(context.Background(), w.p, m); out.kind != wbSkipped {
			t.Fatalf("outcome = %+v", out)
		}
		if got := w.remoteHead(t); got != w.head {
			t.Errorf("remote moved to %s", got)
		}
	})
}

func TestRunWriteBackGit_RejectedPushIsUnchargedAndRestored(t *testing.T) {
	w := newWriteBackWorld(t)
	w.recordResolution(t)
	hook := filepath.Join(w.src, ".git", "hooks", "pre-receive")
	script := "#!/bin/sh\necho 'error: GH013: Repository rule violations found' >&2\necho '- must not contain merge commits' >&2\nexit 1\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out := w.eng.runWriteBackGit(context.Background(), w.p, w.m)
	if out.kind != wbRejected {
		t.Fatalf("outcome = %+v, want wbRejected", out)
	}
	if !strings.Contains(out.policyReason, "GH013") {
		t.Errorf("policyReason = %q", out.policyReason)
	}
	w.assertRestored(t)
}

// continueTrialFromHead: adopts the new member head only when the trial tree is unchanged.
func TestContinueTrialFromHead_AdoptsOnlyAnIdenticalTree(t *testing.T) {
	w := newWriteBackWorld(t)
	w.recordResolution(t)
	out := w.eng.runWriteBackGit(context.Background(), w.p, w.m)
	if out.kind != wbWritten {
		t.Fatalf("outcome = %+v", out)
	}

	trialDir := filepath.Join(t.TempDir(), "trial")
	mustGitDir(t, w.wm.baseDir, "worktree", "add", "--detach", trialDir, w.baseSHA)
	t.Cleanup(func() { gitOutputIn(w.wm.baseDir, "worktree", "remove", "--force", trialDir) })
	mustGit(t, trialDir, "config", "user.email", "t@t.com")
	mustGit(t, trialDir, "config", "user.name", "T")
	// The trial's own resolved merge of the member.
	if _, err := gitCombined(trialDir, "merge", "--no-ff", "--no-edit", w.head); err == nil {
		t.Fatal("trial merge should conflict")
	}
	writeFile(t, filepath.Join(trialDir, "README.md"), "# resolved\n")
	mustGit(t, trialDir, "add", "-A")
	mustGit(t, trialDir, "commit", "-m", "resolved in the trial")
	resolvedSHA, _ := gitRevParse(trialDir, "HEAD")

	// A head whose tree differs is refused and the resolved commit restored.
	other := strings.TrimSpace(gitOutputDir(t, w.src, "rev-parse", "main"))
	if ok, _ := w.eng.continueTrialFromHead(trialDir, w.baseSHA, other); ok {
		t.Fatal("a head with a different tree must not be adopted")
	}
	if got, _ := gitRevParse(trialDir, "HEAD"); got != resolvedSHA {
		t.Errorf("trial HEAD = %s, want the resolved commit %s restored", got, resolvedSHA)
	}

	ok, why := w.eng.continueTrialFromHead(trialDir, w.baseSHA, out.newHead)
	if !ok {
		t.Fatalf("identical tree must be adopted: %s", why)
	}
	if got, _ := gitRevParse(trialDir, "HEAD"); got == resolvedSHA {
		t.Error("trial HEAD did not move to the rebuilt merge")
	}
	parents, _ := gitOutputIn(trialDir, "rev-list", "--parents", "-n", "1", "HEAD")
	if f := strings.Fields(parents); len(f) != 3 || f[2] != out.newHead {
		t.Errorf("rebuilt merge parents = %q, want the new member head %s second", parents, out.newHead)
	}
}

// --- gate, marker, logging and uncharged skips (git half injected) ---

type writeBackLogs struct {
	mu    sync.Mutex
	lines []string
}

func (l *writeBackLogs) sink(_ int, _, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, msg)
}

func (l *writeBackLogs) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

func newWriteBackGateWorld(t *testing.T, out writeBackOutcome) (*Engine, *mockGitHubClient, trialParams, trainMember, *writeBackLogs, *int) {
	t.Helper()
	client := &mockGitHubClient{}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, nil)
	logs := &writeBackLogs{}
	eng.SetLogSinkForTest(logs.sink)
	calls := 0
	eng.trainWriteBackGitFn = func(ctx context.Context, p trialParams, m trainMember) writeBackOutcome {
		calls++
		return out
	}
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", baseSHA: cuBaseSHAForWB, trainKey: "owner/repo"}
	m := trainMember{item: gh.ProjectItem{Number: 7, Repo: "owner/repo"}, prNum: 70, headSHA: "memberhead1"}
	return eng, client, p, m, logs, &calls
}

const cuBaseSHAForWB = "ba5e000000000000000000000000000000000002"

func TestTryWriteBack_SkippedAndRejectedLogOneLineAndContinueUnchanged(t *testing.T) {
	for name, tc := range map[string]struct {
		out  writeBackOutcome
		want string
	}{
		"skipped":  {writeBackOutcome{kind: wbSkipped, reason: "a stage session is in flight on the member"}, "skipped: a stage session"},
		"rejected": {writeBackOutcome{kind: wbRejected, reason: "lease"}, "push rejected: lease"},
	} {
		t.Run(name, func(t *testing.T) {
			eng, client, p, m, logs, calls := newWriteBackGateWorld(t, tc.out)
			got, adopted := eng.tryWriteBackAndContinue(context.Background(), p, 2, m, "", "")
			if adopted || got.headSHA != m.headSHA {
				t.Errorf("member = %+v adopted=%v, want it unchanged", got, adopted)
			}
			if *calls != 1 || logs.count("write-back: #7 base "+cuBaseSHAForWB) != 1 || logs.count(tc.want) != 1 {
				t.Errorf("calls=%d, lines=%v", *calls, logs.lines)
			}
			if len(client.addCommentCalls) != 0 {
				t.Errorf("no marker may be posted for %s, got %d comment(s)", name, len(client.addCommentCalls))
			}
			if n := eng.catchUpAttemptCount(p.trainKey, 7); n != 0 {
				t.Errorf("a %s write-back charged the catch-up attempt counter (%d)", name, n)
			}
		})
	}
}

func TestTryWriteBack_GateSkipsAreSilentAndOnceOnly(t *testing.T) {
	out := writeBackOutcome{kind: wbSkipped, reason: "x"}
	t.Run("single member batch", func(t *testing.T) {
		eng, _, p, m, logs, calls := newWriteBackGateWorld(t, out)
		eng.tryWriteBackAndContinue(context.Background(), p, 1, m, "", "")
		if *calls != 0 || len(logs.lines) != 0 {
			t.Errorf("calls=%d lines=%v", *calls, logs.lines)
		}
	})
	t.Run("seam", func(t *testing.T) {
		eng, _, p, m, _, calls := newWriteBackGateWorld(t, out)
		eng.SetTrainWriteBackDisabledForTest(true)
		eng.tryWriteBackAndContinue(context.Background(), p, 2, m, "", "")
		if *calls != 0 {
			t.Errorf("write-back ran with the seam set")
		}
	})
	t.Run("singleton_catch_up off", func(t *testing.T) {
		eng, _, p, m, _, calls := newWriteBackGateWorld(t, out)
		eng.cfg.SingletonCatchUp = "off"
		eng.tryWriteBackAndContinue(context.Background(), p, 2, m, "", "")
		if *calls != 0 {
			t.Errorf("write-back ran with singleton_catch_up: off")
		}
	})
	t.Run("no pinned base", func(t *testing.T) {
		eng, _, p, m, _, calls := newWriteBackGateWorld(t, out)
		p.baseSHA = ""
		eng.tryWriteBackAndContinue(context.Background(), p, 2, m, "", "")
		if *calls != 0 {
			t.Errorf("write-back ran without a pinned base")
		}
	})
	t.Run("policy memo", func(t *testing.T) {
		eng, _, p, m, _, calls := newWriteBackGateWorld(t, out)
		eng.recordCatchUpPolicyRejection(p.repoKey(), "GH013")
		eng.tryWriteBackAndContinue(context.Background(), p, 2, m, "", "")
		if *calls != 0 {
			t.Errorf("write-back ran under the policy memo")
		}
	})
	t.Run("once per member and base", func(t *testing.T) {
		eng, _, p, m, _, calls := newWriteBackGateWorld(t, out)
		eng.tryWriteBackAndContinue(context.Background(), p, 2, m, "", "")
		eng.tryWriteBackAndContinue(context.Background(), p, 2, m, "", "")
		if *calls != 1 {
			t.Errorf("calls = %d, want 1 for the same member and base", *calls)
		}
		p.baseSHA = "ba5e000000000000000000000000000000000009"
		eng.tryWriteBackAndContinue(context.Background(), p, 2, m, "", "")
		if *calls != 2 {
			t.Errorf("a new base must allow a new attempt, calls = %d", *calls)
		}
	})
}

func TestTryWriteBack_PolicyRejectionFeedsTheSharedMemo(t *testing.T) {
	eng, _, p, m, _, _ := newWriteBackGateWorld(t, writeBackOutcome{kind: wbRejected, reason: "rejected", policyReason: "GH013 rule"})
	eng.tryWriteBackAndContinue(context.Background(), p, 2, m, "", "")
	if !eng.catchUpPolicyBackoff(p.repoKey()) {
		t.Error("a policy-classified rejection must set the per-repo memo (ADR 2065)")
	}
}

func TestTryWriteBack_WrittenPostsMarkerAndKeepsOriginalHeadWhenTrialCannotContinue(t *testing.T) {
	eng, client, p, m, logs, _ := newWriteBackGateWorld(t, writeBackOutcome{kind: wbWritten, newHead: "newhead22222"})
	// An unreadable trial worktree: continueTrialFromHead declines.
	got, adopted := eng.tryWriteBackAndContinue(context.Background(), p, 2, m, filepath.Join(t.TempDir(), "missing"), "pre")
	if adopted || got.headSHA != m.headSHA {
		t.Errorf("member = %+v adopted=%v: a trial that was not rebuilt keeps the original head", got, adopted)
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("comments = %d, want the one write-back marker", len(client.addCommentCalls))
	}
	c := client.addCommentCalls[0]
	want := formatCatchUpMarker(catchUpMarker{Head: "newhead22222", Base: cuBaseSHAForWB, Kind: catchUpKindWriteBack})
	if c.issueNumber != 70 || !strings.Contains(c.body, want) {
		t.Errorf("marker comment on #%d = %q, want %q on PR 70", c.issueNumber, c.body, want)
	}
	if strings.Contains(c.body, "pure=true") {
		t.Error("a write-back marker is never pure")
	}
	if logs.count("write-back: #7 base "+cuBaseSHAForWB+" — written back") != 1 {
		t.Errorf("want exactly one written-back line, got %v", logs.lines)
	}
	if eng.catchUpPushInFlight("owner", "repo", 70) {
		t.Error("the push-in-flight window must be closed after the marker")
	}
}

// resolveConflictWithClaude under noClaude never dispatches and returns the sentinel;
// without it the default behaviour is unchanged (the dispatch happens).
func TestResolveConflictWithClaude_NoClaudeNeverDispatches(t *testing.T) {
	w := newWriteBackWorld(t)
	opts := InvokeOptions{noClaude: true}
	resolved, _, err := w.eng.resolveConflictWithClaude(context.Background(), "owner/repo", w.m.item, t.TempDir(), w.p.holdingStg, w.head, nil, w.baseSHA, nil, opts)
	if resolved || err != errWriteBackNeedsClaude {
		t.Fatalf("resolved=%v err=%v, want the sentinel", resolved, err)
	}
	if w.claudeCalls != 0 {
		t.Errorf("Claude dispatched %d time(s) under noClaude", w.claudeCalls)
	}
}
