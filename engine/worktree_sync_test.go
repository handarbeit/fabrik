package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// Tests for the issue-branch sync and the verified-lease push (#2295, ADR 2295). They use
// real git, via the #2089 fixture: a bare origin, a source repo that doubles as the
// WorktreeManager's base dir, and a mock GitHub client.

const syncBranch = "fabrik/issue-7"

// humanCommit simulates a person pushing a real (non-empty) commit to the remote issue
// branch from their own clone, on top of the current remote tip of that branch (or of main
// when the branch is absent). The bare clone's tracking refs are then refreshed, as any full
// fetch (FetchOrigin) does — which is exactly what defeats a bare --force-with-lease.
func (g *pushGuardEnv) humanCommit(file string) string {
	g.t.Helper()
	remoteURL := g.git(g.sourceDir, "remote", "get-url", "origin")
	clone := filepath.Join(g.t.TempDir(), "human")
	g.git(g.t.TempDir(), "clone", "-q", remoteURL, clone)
	g.git(clone, "config", "user.email", "human@test.com")
	g.git(clone, "config", "user.name", "Human")
	if g.remoteRef(syncBranch) != "" {
		g.git(clone, "checkout", "-q", "-b", "work", "origin/"+syncBranch)
	} else {
		g.git(clone, "checkout", "-q", "-b", "work", "origin/main")
	}
	writeFile(g.t, filepath.Join(clone, file), file+"\n")
	g.git(clone, "add", "-A")
	g.git(clone, "commit", "-q", "-m", "human "+file)
	sha := g.git(clone, "rev-parse", "HEAD")
	g.git(clone, "push", "-q", "origin", "work:refs/heads/"+syncBranch)
	g.git(g.wm.baseDir, "fetch", "origin")
	return sha
}

// commitFile commits a real file in the worktree.
func (g *pushGuardEnv) commitFile(file string) string {
	g.t.Helper()
	writeFile(g.t, filepath.Join(g.wtDir, file), file+"\n")
	g.git(g.wtDir, "add", "-A")
	g.git(g.wtDir, "commit", "-q", "-m", "local "+file)
	return g.git(g.wtDir, "rev-parse", "HEAD")
}

func (g *pushGuardEnv) head() string { return g.git(g.wtDir, "rev-parse", "HEAD") }

// advanceMain pushes a new commit to origin/main from the source repo.
func (g *pushGuardEnv) advanceMain(file string) {
	g.t.Helper()
	writeFile(g.t, filepath.Join(g.sourceDir, file), file+"\n")
	g.git(g.sourceDir, "add", "-A")
	g.git(g.sourceDir, "commit", "-q", "-m", "main "+file)
	g.git(g.sourceDir, "push", "-q", "origin", "main")
	g.git(g.wm.baseDir, "fetch", "origin")
}

func (g *pushGuardEnv) ensure(skipUpdate bool, opts ...EnsureWorktreeOption) (SyncReport, error) {
	g.t.Helper()
	var rep SyncReport
	_, err := g.wm.EnsureWorktree(7, "main", skipUpdate, append(opts, WithSyncReport(&rep))...)
	return rep, err
}

func (g *pushGuardEnv) push() error {
	return g.e.pushBranchUnlessQueued(gh.ProjectItem{Number: 7}, g.wm)
}

// Out-of-band push, local at base: the stale local branch is fast-forwarded before the
// rebase and the human's commit survives the next push.
func TestBranchSync_FastForwardsBehindLocalAndHumanCommitSurvives(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	human := g.humanCommit("human.txt")

	rep, err := g.ensure(false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if rep.Outcome != SyncFastForwarded {
		t.Fatalf("outcome = %s (%s), want fast-forwarded", rep.Outcome, rep.Reason)
	}
	if got := g.head(); got != human {
		t.Fatalf("HEAD = %s, want the human commit %s", got, human)
	}
	g.commitFile("work.txt")
	if err := g.push(); err != nil {
		t.Fatalf("push: %v", err)
	}
	if out := g.git(g.wtDir, "merge-base", "--is-ancestor", human, g.remoteRef(syncBranch)); out != "" {
		t.Fatalf("unexpected output %q", out)
	}
}

// Non-vacuity: with the sync neutralised the local branch stays stale.
func TestBranchSync_NonVacuous_DisabledSyncLeavesLocalStale(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	human := g.humanCommit("human.txt")
	g.wm.SetBranchSyncDisabledForTest(true)

	if _, err := g.ensure(false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if got := g.head(); got == human {
		t.Fatalf("local branch reached the human commit with the sync disabled")
	}
}

// The #2058 report shape with local work: local carries its own commit, a person pushed a
// different one, and the shared tracking ref was refreshed. The push is refused, the remote
// is unchanged, and the item is paused with one comment. #2089 cannot catch this.
func TestBranchSync_ReportShapeWithLocalWorkIsRefusedAndPaused(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	g.commitFile("work.txt")
	human := g.humanCommit("human.txt")
	item := gh.ProjectItem{Number: 7}

	err := g.e.pushBranchUnlessQueued(item, g.wm)
	if !errors.Is(err, ErrPushRefusedDiverged) || !errors.Is(err, ErrPushRefused) {
		t.Fatalf("want ErrPushRefusedDiverged, got %v", err)
	}
	if got := g.remoteRef(syncBranch); got != human {
		t.Fatalf("remote changed: %s, want %s", got, human)
	}
	labels := g.addedLabels()
	if !hasStr(labels, "fabrik:paused") || !hasStr(labels, "fabrik:awaiting-input") {
		t.Errorf("want paused+awaiting-input, got %v", labels)
	}
	comments := g.comments()
	if len(comments) != 1 {
		t.Fatalf("want 1 comment, got %d: %v", len(comments), comments)
	}
	for _, want := range []string{"🏭 **Fabrik — ", divergedPauseFragment, "left untouched", g.head(), human} {
		if !strings.Contains(comments[0], want) {
			t.Errorf("comment missing %q: %s", want, comments[0])
		}
	}
}

// Non-vacuity: with the bare lease restored the same setup overwrites the person's commit.
func TestBranchSync_NonVacuous_BareLeaseOverwritesRemote(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	g.commitFile("work.txt")
	human := g.humanCommit("human.txt")
	g.e.SetBareLeasePushForTest(true)

	if err := g.push(); err != nil {
		t.Fatalf("bare push: %v", err)
	}
	if got := g.remoteRef(syncBranch); got == human {
		t.Fatalf("expected the bare lease to overwrite the remote")
	}
}

// The same divergence is found before work: EnsureWorktree returns the typed error, rebases
// nothing and leaves both heads alone.
func TestBranchSync_EnsureWorktreeReportsDivergence(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	local := g.commitFile("work.txt")
	human := g.humanCommit("human.txt")
	g.advanceMain("base.txt")

	rep, err := g.ensure(false)
	var d *BranchDivergedError
	if !errors.As(err, &d) || !errors.Is(err, ErrBranchDiverged) {
		t.Fatalf("want *BranchDivergedError, got %v", err)
	}
	if rep.Outcome != SyncDiverged || d.LocalHead != local || d.RemoteHead != human {
		t.Fatalf("report %+v / error %+v", rep, d)
	}
	if got := g.head(); got != local {
		t.Errorf("local head moved to %s (rebased?), want %s", got, local)
	}
	if got := g.remoteRef(syncBranch); got != human {
		t.Errorf("remote moved")
	}
}

// An unpushed pure rebase of the remote is reset to the remote head.
func TestBranchSync_ResetsUnpushedPureRebase(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	g.commitFile("work.txt")
	if err := g.push(); err != nil {
		t.Fatalf("seed push: %v", err)
	}
	pushed := g.head()
	g.advanceMain("base.txt")
	if _, err := g.ensure(false, WithoutIssueBranchSync()); err != nil { // engine rebase, never pushed
		t.Fatal(err)
	}
	if g.head() == pushed {
		t.Fatal("setup: expected the engine rebase to move HEAD")
	}

	rep, err := g.ensure(true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != SyncReset || g.head() != pushed {
		t.Fatalf("outcome %s, HEAD %s, want reset to %s", rep.Outcome, g.head(), pushed)
	}
}

// Unpushed local work (local ahead of an unchanged remote) is kept and pushes normally.
func TestBranchSync_KeepsUnpushedLocalWorkAndPushes(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	g.commitFile("one.txt")
	if err := g.push(); err != nil {
		t.Fatal(err)
	}
	local := g.commitFile("two.txt")
	// A full fetch elsewhere must not matter.
	g.git(g.wm.baseDir, "fetch", "origin")

	rep, err := g.ensure(true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != SyncEqual && rep.Outcome != SyncKeptLocal {
		t.Fatalf("outcome %s", rep.Outcome)
	}
	if g.head() != local {
		t.Fatalf("local work moved")
	}
	if err := g.push(); err != nil {
		t.Fatalf("push: %v", err)
	}
	if g.remoteRef(syncBranch) != local {
		t.Fatalf("remote = %s, want %s", g.remoteRef(syncBranch), local)
	}
}

// Ordinary stage: rebase plus a new commit with the remote unchanged pushes on the recorded head.
func TestBranchSync_OrdinaryStagePushesOnRecordedHead(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	g.commitFile("one.txt")
	if err := g.push(); err != nil {
		t.Fatal(err)
	}
	if got := g.wm.ReconciledHead(7); got != g.head() {
		t.Fatalf("reconciled head = %s, want the pushed %s", got, g.head())
	}
	g.advanceMain("base.txt")
	if _, err := g.ensure(false); err != nil { // sync (equal) then rebase
		t.Fatal(err)
	}
	g.commitFile("two.txt")
	if err := g.push(); err != nil {
		t.Fatalf("push: %v", err)
	}
	if g.remoteRef(syncBranch) != g.head() {
		t.Fatalf("remote not updated")
	}
	if g.wm.ReconciledHead(7) != g.head() {
		t.Fatalf("pushed head not recorded")
	}
}

// Lease race: the remote moves between verification and push. The push is rejected and
// nothing is overwritten (the re-verification then sees the divergence).
func TestBranchSync_LeaseRaceDoesNotOverwrite(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	g.commitFile("one.txt")
	if err := g.push(); err != nil {
		t.Fatal(err)
	}
	g.commitFile("two.txt")
	var human string
	g.wm.SetPrePushHookForTest(func() {
		if human == "" {
			human = g.humanCommit("race.txt")
		}
	})

	err := g.push()
	if !errors.Is(err, ErrPushRefusedDiverged) {
		t.Fatalf("want a divergence refusal after the race, got %v", err)
	}
	if human == "" || g.remoteRef(syncBranch) != human {
		t.Fatalf("remote = %s, want the racing commit %s", g.remoteRef(syncBranch), human)
	}
}

// A second rejection is ErrPushLeaseRejected, non-pausing, and still overwrites nothing.
func TestBranchSync_SecondLeaseRejectionIsReportedNotForced(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	c1 := g.commitFile("one.txt")
	c2 := g.commitFile("two.txt")
	g.commitFile("three.txt")
	// Remote starts at base-only (absent): the first attempt's lease requires absence.
	step := 0
	g.wm.SetPrePushHookForTest(func() {
		step++
		sha := c1
		if step == 2 {
			sha = c2
		}
		g.git(g.wtDir, "push", "-q", "origin", sha+":refs/heads/"+syncBranch)
	})

	err := g.push()
	if !errors.Is(err, ErrPushLeaseRejected) {
		t.Fatalf("want ErrPushLeaseRejected, got %v", err)
	}
	if got := g.remoteRef(syncBranch); got != c2 {
		t.Fatalf("remote = %s, want the racing push %s", got, c2)
	}
	if len(g.comments()) != 0 {
		t.Errorf("a lease rejection must not pause")
	}
}

// Worker-pushed branch: local equals remote even though the engine's record is stale; no pause.
func TestBranchSync_WorkerPushedBranchDoesNotPause(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	g.commitFile("one.txt")
	if err := g.push(); err != nil {
		t.Fatal(err)
	}
	g.commitFile("two.txt")
	g.git(g.wtDir, "push", "-q", "--force-with-lease", "origin", syncBranch) // the skill's own push
	if err := g.push(); err != nil {
		t.Fatalf("push after worker push: %v", err)
	}
	if len(g.comments()) != 0 || len(g.addedLabels()) != 0 {
		t.Errorf("worker-pushed branch paused the item")
	}
	if g.wm.ReconciledHead(7) != g.head() {
		t.Errorf("head not recorded")
	}
}

// After a restart nothing is recorded: the push takes the fetch-and-classify path and still
// protects out-of-band work.
func TestBranchSync_RestartWithoutRecordClassifies(t *testing.T) {
	g := newPushGuardEnv(t, 7, openPR(55), nil)
	g.commitFile("one.txt")
	if err := g.push(); err != nil {
		t.Fatal(err)
	}
	g.commitFile("two.txt")
	g.wm.mu.Lock()
	g.wm.reconciled = nil
	g.wm.mu.Unlock()
	if err := g.push(); err != nil {
		t.Fatalf("push without a record: %v", err)
	}
	if g.remoteRef(syncBranch) != g.head() {
		t.Fatalf("remote not updated")
	}

	g.commitFile("three.txt")
	g.humanCommit("human.txt")
	g.wm.mu.Lock()
	g.wm.reconciled = nil
	g.wm.mu.Unlock()
	if err := g.push(); !errors.Is(err, ErrPushRefusedDiverged) {
		t.Fatalf("want divergence refusal, got %v", err)
	}
}

// A push of a branch that is only behind a moved remote publishes nothing.
func TestBranchSync_PushSkipsWhenLocalHoldsNothingRemoteLacks(t *testing.T) {
	g := newPushGuardEnv(t, 7, nil, nil)
	human := g.humanCommit("human.txt")
	if err := g.push(); err != nil {
		t.Fatalf("push: %v", err)
	}
	if g.remoteRef(syncBranch) != human {
		t.Fatalf("a stale behind branch rewound the remote")
	}
}

// An absent remote is a positive absence: the branch is pushed with a must-not-exist lease.
func TestBranchSync_AbsentRemotePushesNewBranch(t *testing.T) {
	g := newPushGuardEnv(t, 7, nil, nil)
	g.commitFile("one.txt")
	if g.remoteRef(syncBranch) != "" {
		t.Fatal("setup: remote branch exists")
	}
	if err := g.wm.PushVerified(7, "main"); err != nil {
		t.Fatalf("PushVerified: %v", err)
	}
	if g.remoteRef(syncBranch) != g.head() {
		t.Fatalf("branch not created")
	}
}

// R2: a dirty tree, an unreadable remote and an absent remote leave the branch alone, and an
// unreadable remote is never read as absence.
func TestBranchSync_SkipsWithoutMutation(t *testing.T) {
	t.Run("dirty tree", func(t *testing.T) {
		g := newPushGuardEnv(t, 7, nil, nil)
		g.humanCommit("human.txt")
		before := g.head()
		writeFile(t, filepath.Join(g.wtDir, "wip.txt"), "wip\n")
		rep, err := g.ensure(true)
		if err != nil || rep.Outcome != SyncSkipped || g.head() != before {
			t.Fatalf("outcome %s err %v head moved=%v", rep.Outcome, err, g.head() != before)
		}
	})
	t.Run("unreadable remote", func(t *testing.T) {
		g := newPushGuardEnv(t, 7, nil, nil)
		g.humanCommit("human.txt")
		before := g.head()
		g.git(g.sourceDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
		rep, err := g.ensure(true)
		if err != nil || rep.Outcome != SyncSkipped || g.head() != before {
			t.Fatalf("outcome %s err %v", rep.Outcome, err)
		}
		if err := g.wm.PushVerified(7, "main"); !errors.Is(err, ErrPushUnverified) {
			t.Fatalf("want ErrPushUnverified, got %v", err)
		}
	})
	t.Run("remote absent", func(t *testing.T) {
		g := newPushGuardEnv(t, 7, nil, nil)
		rep, err := g.ensure(true)
		if err != nil || rep.Outcome != SyncSkipped || rep.Reason != "remote branch absent" {
			t.Fatalf("outcome %s (%s) err %v", rep.Outcome, rep.Reason, err)
		}
	})
}

// A reused stale local branch with no worktree directory is synced too.
func TestBranchSync_ReusedLocalBranchIsSynced(t *testing.T) {
	g := newPushGuardEnv(t, 7, nil, nil)
	human := g.humanCommit("human.txt")
	if err := g.wm.CleanupWorktree(7, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(g.wtDir); err == nil {
		t.Fatal("setup: worktree still exists")
	}
	if _, err := g.ensure(false); err != nil {
		t.Fatal(err)
	}
	if g.head() != human {
		t.Fatalf("HEAD = %s, want %s", g.head(), human)
	}
}

// R6 / #2150: the head EnsureWorktree reports is the synced one, so the comment path's
// restore goes back to it and never undoes the sync.
func TestBranchSync_PreRebaseHeadIsTheSyncedHead(t *testing.T) {
	g := newPushGuardEnv(t, 7, nil, nil)
	stale := g.head()
	human := g.humanCommit("human.txt")
	g.advanceMain("base.txt")

	rep, err := g.ensure(false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.PreRebaseHead != human || rep.PreRebaseHead == stale {
		t.Fatalf("PreRebaseHead = %s, want the synced head %s (stale %s)", rep.PreRebaseHead, human, stale)
	}
	post := g.head()
	if post == human {
		t.Fatal("setup: expected the engine rebase to move HEAD")
	}
	g.git(g.wm.baseDir, "fetch", "origin") // RestoreUnpushedRebase checks the rebased head is not on the remote
	restored, reason := g.wm.RestoreUnpushedRebase(7, "main", rep.PreRebaseHead, post)
	if !restored || g.head() != human {
		t.Fatalf("restore = %v (%s), HEAD %s, want the synced head %s", restored, reason, g.head(), human)
	}
}

// R7: PrepareCatchUp opts out of the sync, so a diverged local keeps its own error.
func TestBranchSync_PrepareCatchUpKeepsItsOwnDivergedError(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wtDir, "local.txt"), "x\n")
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "commit", "-m", "local only")
	newHead := advanceRemoteBranch(t, srcDir, "remote.txt")
	mustGit(t, wm.baseDir, "fetch", "origin")

	_, err = wm.PrepareCatchUp(7, "main", newHead)
	if !errors.Is(err, ErrCatchUpLocalDiverged) || errors.Is(err, ErrBranchDiverged) {
		t.Fatalf("want ErrCatchUpLocalDiverged only, got %v", err)
	}
}

func TestPushRefusedSentinels(t *testing.T) {
	if !errors.Is(ErrPushRefusedZeroAhead, ErrPushRefused) || !errors.Is(ErrPushRefusedDiverged, ErrPushRefused) {
		t.Fatal("both refusals must wrap ErrPushRefused")
	}
	if got := ErrPushRefusedZeroAhead.Error(); got != "push refused: issue branch has no commits ahead of base while its PR is open" {
		t.Errorf("zero-ahead message changed: %q", got)
	}
	if errors.Is(ErrPushRefusedZeroAhead, ErrPushRefusedDiverged) {
		t.Error("the two refusals must stay distinct")
	}
}
