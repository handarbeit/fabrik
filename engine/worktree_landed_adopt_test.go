package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// landedTestLog captures WorktreeManager log lines.
type landedTestLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *landedTestLog) attach(wm *WorktreeManager) {
	wm.logfFn = func(_ int, _ string, format string, args ...any) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, fmt.Sprintf(format, args...))
	}
}

func (l *landedTestLog) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ln := range l.lines {
		if strings.Contains(ln, sub) {
			return true
		}
	}
	return false
}

// commitIssueBranch creates fabrik/issue-<n> on origin with one commit, returns
// the tip, and leaves origin on main.
func commitIssueBranch(t *testing.T, origin string, n int, file string) string {
	t.Helper()
	gitOut(t, origin, "checkout", "-q", "-b", fmt.Sprintf("fabrik/issue-%d", n))
	if err := os.WriteFile(filepath.Join(origin, file), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, origin, "add", file)
	gitOut(t, origin, "commit", "-q", "-m", "work on "+file)
	tip := gitOut(t, origin, "rev-parse", "HEAD")
	gitOut(t, origin, "checkout", "-q", "main")
	return tip
}

// refreshOriginMain brings the bare clone's origin/main up to date. The clone
// is taken before the issue branch exists so that the local branch is missing
// (a bare clone copies every head, which would skip the adoption path).
func refreshOriginMain(t *testing.T, bare string) {
	t.Helper()
	gitOut(t, bare, "fetch", "origin", "+refs/heads/main:refs/remotes/origin/main")
}

func TestEnsureWorktree_DoesNotAdoptBranchMergedByMergeCommit(t *testing.T) {
	skipIfNoGit(t)
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkDisabled=%v", disabled), func(t *testing.T) {
			origin := initBareRepo(t)
			bare := cloneBareWithRefspec(t, origin)
			tip := commitIssueBranch(t, origin, 7, "a.txt")
			gitOut(t, origin, "merge", "-q", "--no-ff", "-m", "merge issue 7", "fabrik/issue-7")
			mainTip := gitOut(t, origin, "rev-parse", "main")
			refreshOriginMain(t, bare)

			wm := NewWorktreeManager(bare)
			var log landedTestLog
			log.attach(wm)
			wm.SetLandedBranchCheckDisabledForTest(disabled)
			wtDir, err := wm.EnsureWorktree(7, "main", true)
			if err != nil {
				t.Fatalf("EnsureWorktree: %v", err)
			}
			got := gitOut(t, wtDir, "rev-parse", "HEAD")
			if disabled {
				// Non-vacuity: without the check the landed branch is adopted.
				if got != tip {
					t.Fatalf("with the check neutralised HEAD = %s, want adopted tip %s", got, tip)
				}
				return
			}
			if got != mainTip {
				t.Errorf("HEAD = %s, want origin/main %s", got, mainTip)
			}
			if !log.has("already contained in main") {
				t.Errorf("missing the not-adopting log line: %v", log.lines)
			}
		})
	}
}

func TestEnsureWorktree_DoesNotAdoptBranchEqualToBase(t *testing.T) {
	skipIfNoGit(t)
	origin := initBareRepo(t)
	bare := cloneBareWithRefspec(t, origin)
	gitOut(t, origin, "branch", "fabrik/issue-9", "main")
	mainTip := gitOut(t, origin, "rev-parse", "main")

	wm := NewWorktreeManager(bare)
	var log landedTestLog
	log.attach(wm)
	wtDir, err := wm.EnsureWorktree(9, "main", true)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if got := gitOut(t, wtDir, "rev-parse", "HEAD"); got != mainTip {
		t.Errorf("HEAD = %s, want %s", got, mainTip)
	}
	if !log.has("already contained in main") || log.has("landed") {
		t.Errorf("log should say 'contained', not 'landed': %v", log.lines)
	}
}

func TestEnsureWorktree_DoesNotAdoptSquashMergedPRHead(t *testing.T) {
	skipIfNoGit(t)
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkDisabled=%v", disabled), func(t *testing.T) {
			origin := initBareRepo(t)
			bare := cloneBareWithRefspec(t, origin)
			tip := commitIssueBranch(t, origin, 7, "a.txt")
			gitOut(t, origin, "merge", "-q", "--squash", "fabrik/issue-7")
			gitOut(t, origin, "commit", "-q", "-m", "squash of issue 7")
			mainTip := gitOut(t, origin, "rev-parse", "main")
			refreshOriginMain(t, bare)

			wm := NewWorktreeManager(bare)
			var log landedTestLog
			log.attach(wm)
			wm.SetLandedBranchCheckDisabledForTest(disabled)
			wtDir, err := wm.EnsureWorktree(7, "main", true, WithLandedPRHeads(func() []string { return []string{"deadbeef", tip} }))
			if err != nil {
				t.Fatalf("EnsureWorktree: %v", err)
			}
			got := gitOut(t, wtDir, "rev-parse", "HEAD")
			if disabled {
				if got != tip {
					t.Fatalf("with the check neutralised HEAD = %s, want adopted tip %s", got, tip)
				}
				return
			}
			if got != mainTip {
				t.Errorf("HEAD = %s, want origin/main %s", got, mainTip)
			}
			if !log.has("head (") || !log.has("merged PR") {
				t.Errorf("missing the merged-PR log line: %v", log.lines)
			}
		})
	}
}

// A squash-merged branch with no (or a different, or an unknown) merged-PR head
// is adopted: R2 falls back to R1 only, and R1 cannot see a squash merge.
func TestEnsureWorktree_SquashMergedBranchWithoutMatchingPRHeadIsAdopted(t *testing.T) {
	skipIfNoGit(t)
	for name, opts := range map[string][]EnsureWorktreeOption{
		"no option":      nil,
		"other head":     {WithLandedPRHeads(func() []string { return []string{"0123456789abcdef"} })},
		"empty result":   {WithLandedPRHeads(func() []string { return nil })},
		"empty-sha only": {WithLandedPRHeads(func() []string { return []string{""} })},
	} {
		t.Run(name, func(t *testing.T) {
			origin := initBareRepo(t)
			bare := cloneBareWithRefspec(t, origin)
			tip := commitIssueBranch(t, origin, 7, "a.txt")
			gitOut(t, origin, "merge", "-q", "--squash", "fabrik/issue-7")
			gitOut(t, origin, "commit", "-q", "-m", "squash of issue 7")

			wtDir, err := NewWorktreeManager(bare).EnsureWorktree(7, "main", true, opts...)
			if err != nil {
				t.Fatalf("EnsureWorktree: %v", err)
			}
			if got := gitOut(t, wtDir, "rev-parse", "HEAD"); got != tip {
				t.Errorf("HEAD = %s, want adopted tip %s", got, tip)
			}
		})
	}
}

// #1916 regression: unmerged work on origin is still adopted, even when the
// caller supplies merged-PR heads that do not match the tip.
func TestEnsureWorktree_StillAdoptsUnmergedBranch(t *testing.T) {
	skipIfNoGit(t)
	origin := initBareRepo(t)
	bare := cloneBareWithRefspec(t, origin)
	tip := commitIssueBranch(t, origin, 7, "a.txt")

	wm := NewWorktreeManager(bare)
	var log landedTestLog
	log.attach(wm)
	wtDir, err := wm.EnsureWorktree(7, "main", true, WithLandedPRHeads(func() []string { return []string{"0123456789abcdef"} }))
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if got := gitOut(t, wtDir, "rev-parse", "HEAD"); got != tip {
		t.Errorf("HEAD = %s, want adopted tip %s", got, tip)
	}
	if !log.has("starting the local branch from it") {
		t.Errorf("missing the adoption log line: %v", log.lines)
	}
}

// R5: a git error in the ancestry check adopts rather than forking from base.
func TestEnsureWorktree_AncestryCheckErrorStillAdopts(t *testing.T) {
	skipIfNoGit(t)
	origin := initBareRepo(t)
	bare := cloneBareWithRefspec(t, origin)
	tip := commitIssueBranch(t, origin, 7, "a.txt")
	// Landed by merge commit, so only the (failing) check could decline it.
	gitOut(t, origin, "merge", "-q", "--no-ff", "-m", "merge issue 7", "fabrik/issue-7")

	wm := NewWorktreeManager(bare)
	var log landedTestLog
	log.attach(wm)
	wm.SetIsAncestorFnForTest(func(dir, ancestor, descendant string) (bool, error) {
		return false, fmt.Errorf("simulated git failure")
	})
	wtDir, err := wm.EnsureWorktree(7, "main", true)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if got := gitOut(t, wtDir, "rev-parse", "HEAD"); got != tip {
		t.Errorf("HEAD = %s, want adopted tip %s", got, tip)
	}
	if !log.has("could not tell") {
		t.Errorf("missing the doubt log line: %v", log.lines)
	}
}

func TestGitIsAncestor_ExitCodes(t *testing.T) {
	skipIfNoGit(t)
	origin := initBareRepo(t)
	tip := commitIssueBranch(t, origin, 7, "a.txt")
	base := gitOut(t, origin, "rev-parse", "main")

	if ok, err := gitIsAncestor(origin, base, tip); err != nil || !ok {
		t.Errorf("base is an ancestor of tip: got (%v, %v)", ok, err)
	}
	if ok, err := gitIsAncestor(origin, tip, base); err != nil || ok {
		t.Errorf("tip is not an ancestor of base: got (%v, %v)", ok, err)
	}
	if _, err := gitIsAncestor(origin, "refs/heads/nope", base); err == nil {
		t.Error("an unresolvable ref must be an error, not 'not an ancestor'")
	}
}

func TestEnsureWorktree_BaseMismatchWarnsAndStillAdopts(t *testing.T) {
	skipIfNoGit(t)
	origin := initBareRepo(t)
	bare := cloneBareWithRefspec(t, origin)
	tip := commitIssueBranch(t, origin, 7, "a.txt")

	wm := NewWorktreeManager(bare)
	var log landedTestLog
	log.attach(wm)
	wtDir, err := wm.EnsureWorktree(7, "main", true, WithPRBaseProbe(func() (string, bool) { return "release/1.x", true }))
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if got := gitOut(t, wtDir, "rev-parse", "HEAD"); got != tip {
		t.Errorf("HEAD = %s, want adopted tip %s", got, tip)
	}
	if !log.has("WARNING") || !log.has(`"release/1.x"`) || !log.has(`"main"`) {
		t.Errorf("warning must name both bases: %v", log.lines)
	}
}

func TestEnsureWorktree_MatchingOrUnknownPRBaseDoesNotWarn(t *testing.T) {
	skipIfNoGit(t)
	for name, probe := range map[string]func() (string, bool){
		"same base": func() (string, bool) { return "main", true },
		"unknown":   func() (string, bool) { return "release/1.x", false },
	} {
		t.Run(name, func(t *testing.T) {
			origin := initBareRepo(t)
			bare := cloneBareWithRefspec(t, origin)
			commitIssueBranch(t, origin, 7, "a.txt")
			wm := NewWorktreeManager(bare)
			var log landedTestLog
			log.attach(wm)
			if _, err := wm.EnsureWorktree(7, "main", true, WithPRBaseProbe(probe)); err != nil {
				t.Fatalf("EnsureWorktree: %v", err)
			}
			if log.has("WARNING") {
				t.Errorf("unexpected warning: %v", log.lines)
			}
		})
	}
}

// The lazy callbacks must not run when nothing is adopted.
func TestEnsureWorktree_OptionsAreLazy(t *testing.T) {
	skipIfNoGit(t)
	origin := initBareRepo(t)
	bare := cloneBareWithRefspec(t, origin)
	called := false
	wm := NewWorktreeManager(bare)
	_, err := wm.EnsureWorktree(11, "main", true,
		WithLandedPRHeads(func() []string { called = true; return nil }),
		WithPRBaseProbe(func() (string, bool) { called = true; return "", false }))
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if called {
		t.Error("option callbacks ran although no origin branch exists")
	}
}
