package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// Tests for #2046: the merge train holds an e.sem slot only around each Claude
// invocation it makes (resolveConflictWithClaude), never for its lifecycle.
// Neutralisation: the saturated-slots lifecycle test fails against the old
// lifecycle-long hold (prepareTrainWorker blocked on e.sem until its context
// expired); the conflict tests fail if the acquire/release is removed from
// resolveConflictWithClaude (held-during / free-after assertions) or widened past
// the InvokeForComments line.

const slotWaitLogFragment = "waiting for a free worker slot for conflict resolution on #"

func saturateSem(e *Engine) {
	for i := 0; i < cap(e.sem); i++ {
		e.sem <- struct{}{}
	}
}

func drainSem(e *Engine) {
	for len(e.sem) > 0 {
		<-e.sem
	}
}

// cancelOnSlotWait cancels ctx as soon as the engine announces a real slot wait for
// conflict resolution, so a test can cancel deterministically *while waiting*.
// The returned channel closes when it has fired; stop ends the watcher.
func cancelOnSlotWait(ch chan tui.Event, cancel context.CancelFunc) (fired <-chan struct{}, stop func()) {
	done := make(chan struct{})
	quit := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case raw := <-ch:
				if ev, ok := raw.(tui.LogEvent); ok && strings.Contains(ev.Message, slotWaitLogFragment) {
					cancel()
					return
				}
			case <-quit:
				return
			}
		}
	}()
	var once sync.Once
	return done, func() { once.Do(func() { close(quit) }) }
}

func countSlotWaitLogs(events []tui.Event) int {
	n := 0
	for _, raw := range events {
		if ev, ok := raw.(tui.LogEvent); ok && ev.Tag == "merge-train" && strings.Contains(ev.Message, slotWaitLogFragment) {
			n++
		}
	}
	return n
}

func TestAcquireTrainSlot_FreeSlotLogsNothingAndReleaseIsIdempotent(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	ch := make(chan tui.Event, 64)
	eng.events = ch

	release, err := eng.acquireTrainSlot(context.Background(), "owner/repo", makeTrainItem(3, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(eng.sem); got != 1 {
		t.Fatalf("occupancy after acquire = %d, want 1", got)
	}
	// Another worker's slot must survive a defensive double release.
	eng.sem <- struct{}{}
	release()
	release()
	if got := len(eng.sem); got != 1 {
		t.Errorf("occupancy after double release = %d, want 1 (the other worker's slot)", got)
	}
	<-eng.sem
	if n := countSlotWaitLogs(collectEvents(ch, 20*time.Millisecond)); n != 0 {
		t.Errorf("a free slot logged %d wait message(s), want none", n)
	}
}

func TestAcquireTrainSlot_SaturatedLogsOnceThenAcquiresWhenFreed(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	ch := make(chan tui.Event, 64)
	eng.events = ch
	saturateSem(eng)

	got := make(chan error, 1)
	var release func()
	go func() {
		var err error
		// Repo deliberately empty (ProjectItem.Repo can be empty on some board
		// paths): the wait line must still reach the train's row via repoKey.
		item := makeTrainItem(3, "x")
		item.Repo = ""
		release, err = eng.acquireTrainSlot(context.Background(), "owner/repo", item)
		got <- err
	}()

	// Wait for the announcement, then free one slot.
	deadline := time.After(5 * time.Second)
	var events []tui.Event
	for countSlotWaitLogs(events) == 0 {
		select {
		case ev := <-ch:
			events = append(events, ev)
		case <-deadline:
			t.Fatal("no wait message while the slots were saturated")
		}
	}
	for _, raw := range events {
		if ev, ok := raw.(tui.LogEvent); ok && strings.Contains(ev.Message, slotWaitLogFragment) && ev.Repo != "owner/repo" {
			t.Errorf("wait message routed to repo %q, want owner/repo (the job row)", ev.Repo)
		}
	}
	<-eng.sem
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquire did not complete after a slot was freed")
	}
	release()
	if n := countSlotWaitLogs(append(events, collectEvents(ch, 20*time.Millisecond)...)); n != 1 {
		t.Errorf("wait message logged %d times, want exactly 1", n)
	}
	drainSem(eng)
}

func TestAcquireTrainSlot_CancelWhileWaitingTakesNoSlot(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	saturateSem(eng)
	defer drainSem(eng)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release, err := eng.acquireTrainSlot(ctx, "owner/repo", makeTrainItem(3, "x"))
	if err == nil {
		release()
		t.Fatal("expected an error from a cancelled wait")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if !trainCancelled(ctx, err) {
		t.Error("trainCancelled must recognise the wait cancellation")
	}
	if got := len(eng.sem); got != cap(eng.sem) {
		t.Errorf("occupancy = %d, want %d unchanged", got, cap(eng.sem))
	}
}

func TestTrainCancelled(t *testing.T) {
	live := context.Background()
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	generic := fmt.Errorf("boom")
	if trainCancelled(live, nil) {
		t.Error("nil error is never a cancellation")
	}
	if trainCancelled(live, generic) {
		t.Error("a generic error on a live context is a verdict, not a cancellation")
	}
	if !trainCancelled(live, fmt.Errorf("x: %w", context.Canceled)) || !trainCancelled(live, context.DeadlineExceeded) {
		t.Error("context errors are cancellations")
	}
	if !trainCancelled(dead, generic) {
		t.Error("any error on a dead context is a cancellation (killed in-flight invocation)")
	}
	if trainCancelled(live, &claudeUsageLimitError{Message: "limit"}) {
		t.Error("a usage-limit error is not a cancellation — its behaviour is unchanged")
	}
}

// With every slot held by stage workers, a conflict-free train assembles, opens its
// trial PR and polls CI without ever acquiring a slot. Under the old lifecycle-long
// hold prepareTrainWorker blocked on e.sem until the context expired and no trial PR
// was ever opened.
func TestRunMergeTrainWorker_SaturatedSlotsConflictFreeTrainNeverTakesASlot(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)
	sha1 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-1", "one.txt", "one\n")
	sha2 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-2", "two.txt", "two\n")

	var mu sync.Mutex
	var created int
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			switch issueNumber {
			case 1:
				return &gh.PRDetails{Number: 10, HeadSHA: sha1, State: "open"}, nil
			case 2:
				return &gh.PRDetails{Number: 11, HeadSHA: sha2, State: "open"}, nil
			}
			return nil, fmt.Errorf("not found")
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) { return 1, nil },
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			mu.Lock()
			created++
			mu.Unlock()
			return 99, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, wm)
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()

	saturateSem(eng)
	defer drainSem(eng)

	batch := []gh.ProjectItem{makeTrainItem(1, "Issue 1"), makeTrainItem(2, "Issue 2")}
	state := &mergeTrainWorkerState{assembling: true, trialName: fmt.Sprintf("merge-train-repo-%d", time.Now().Unix())}
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", "main"), state)
	eng.store.EnterRepoWorker(mergeTrainKey("owner/repo", "main"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		eng.runMergeTrainWorker(ctx, state, "owner", "repo", "main", batch)
		close(done)
	}()
	// Poll for the slot count: a train that took (or released) a slot would be
	// visible as a change from the saturated occupancy.
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the train did not finish with every slot held by stage workers")
	}

	mu.Lock()
	n := created
	mu.Unlock()
	if n != 1 {
		t.Errorf("trial PRs opened = %d, want 1 — the train must proceed without a slot", n)
	}
	if len(claude.forCommentsCalls) != 0 {
		t.Errorf("Claude invoked %d time(s) for a conflict-free train", len(claude.forCommentsCalls))
	}
	if got := len(eng.sem); got != cap(eng.sem) {
		t.Errorf("occupancy = %d, want %d — the train must neither take nor release a slot", got, cap(eng.sem))
	}
	if _, found := eng.mergeTrainInFlight.Load(mergeTrainKey("owner/repo", "main")); found {
		t.Error("in-flight marker not cleared on return")
	}
}

// A conflict needing Claude holds a slot for exactly the invocation: held while the
// mock runs, free again as soon as it returns.
func TestRunCatchUpGit_ConflictHoldsASlotOnlyAroundTheInvocation(t *testing.T) {
	var occupancyDuring int
	var eng *Engine
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			occupancyDuring = len(eng.sem)
			if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# resolved\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			mustGit(t, workDir, "add", "-A")
			mustGit(t, workDir, "commit", "-m", "chore(merge-train): resolve conflict for #7")
			return "resolved", false, TokenUsage{}, nil
		},
	}
	w := newCatchUpGitWorld(t, "README.md", "# member\n", "README.md", "# main\n", claude)
	eng = w.eng

	out := eng.runCatchUpGit(context.Background(), w.p, w.m)
	if out.kind != catchUpPushed {
		t.Fatalf("outcome = %+v, want a push", out)
	}
	if occupancyDuring != 1 {
		t.Errorf("occupancy during the invocation = %d, want 1", occupancyDuring)
	}
	if got := len(eng.sem); got != 0 {
		t.Errorf("occupancy after the catch-up = %d, want 0 — the slot must be given back", got)
	}
	if n := len(claude.forCommentsCalls); n != 1 {
		t.Errorf("Claude invoked %d times, want 1", n)
	}
}

// Singleton catch-up with every slot held: cancelling while waiting defers the
// catch-up uncharged — worktree restored, remote untouched, Claude never invoked,
// occupancy unchanged.
func TestRunCatchUpGit_CancelWhileWaitingForASlotDefersUncharged(t *testing.T) {
	claude := &mockClaudeInvoker{}
	w := newCatchUpGitWorld(t, "README.md", "# member\n", "README.md", "# main\n", claude)
	ch := make(chan tui.Event, 256)
	w.eng.events = ch
	saturateSem(w.eng)
	defer drainSem(w.eng)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fired, stop := cancelOnSlotWait(ch, cancel)
	defer stop()

	out := w.eng.runCatchUpGit(ctx, w.p, w.m)
	<-fired
	if out.kind != catchUpDefer {
		t.Fatalf("outcome = %+v, want catchUpDefer — a cancelled slot wait is no verdict on the conflict", out)
	}
	if len(claude.forCommentsCalls) != 0 {
		t.Error("Claude must not be invoked when the slot was never acquired")
	}
	w.assertRestored(t)
	if got := len(w.eng.sem); got != cap(w.eng.sem) {
		t.Errorf("occupancy = %d, want %d unchanged", got, cap(w.eng.sem))
	}
}

// A cancellation while the trial assembly waits for a conflict-resolution slot ends
// cleanly: no member is ejected or commented on, nothing is paused or charged to the
// runaway counter, no trial PR is opened, and no slot leaks.
func TestRunMergeTrainWorker_CancelWhileWaitingForConflictSlotChargesNothing(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)
	sha1 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-1", "counter.txt", "from-branch-1\n")
	sha2 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-2", "counter.txt", "from-branch-2\n")

	var mu sync.Mutex
	var comments, labels, created int
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			switch issueNumber {
			case 1:
				return &gh.PRDetails{Number: 10, HeadSHA: sha1, State: "open"}, nil
			case 2:
				return &gh.PRDetails{Number: 11, HeadSHA: sha2, State: "open"}, nil
			}
			return nil, fmt.Errorf("not found")
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			mu.Lock()
			comments++
			mu.Unlock()
			return 1, nil
		},
		addLabelToIssueFn: func(owner, repo string, issueNumber int, label string) error {
			mu.Lock()
			labels++
			mu.Unlock()
			return nil
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			mu.Lock()
			created++
			mu.Unlock()
			return 99, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, wm)
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()
	ch := make(chan tui.Event, 256)
	eng.events = ch
	saturateSem(eng)
	defer drainSem(eng)

	batch := []gh.ProjectItem{makeTrainItem(1, "Issue 1"), makeTrainItem(2, "Issue 2")}
	state := &mergeTrainWorkerState{assembling: true, trialName: fmt.Sprintf("merge-train-repo-%d", time.Now().Unix())}
	trainKey := mergeTrainKey("owner/repo", "main")
	eng.mergeTrainInFlight.Store(trainKey, state)
	eng.store.EnterRepoWorker(trainKey)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fired, stop := cancelOnSlotWait(ch, cancel)
	defer stop()

	eng.runMergeTrainWorker(ctx, state, "owner", "repo", "main", batch)
	select {
	case <-fired:
	default:
		t.Fatal("the train never reached a slot wait — the conflict path was not exercised")
	}

	mu.Lock()
	defer mu.Unlock()
	if comments != 0 || labels != 0 {
		t.Errorf("a cancelled slot wait posted %d comment(s) and %d label(s) — nobody may be ejected or paused", comments, labels)
	}
	if created != 0 {
		t.Errorf("trial PRs opened = %d, want 0", created)
	}
	if len(claude.forCommentsCalls) != 0 {
		t.Error("Claude must not be invoked when the slot was never acquired")
	}
	eng.mergeTrainTrialsMu.Lock()
	trials := len(eng.mergeTrainTrials[trainKey])
	eng.mergeTrainTrialsMu.Unlock()
	if trials != 0 {
		t.Errorf("runaway trial counter = %d, want 0 — a cancellation is not a trial", trials)
	}
	if got := len(eng.sem); got != cap(eng.sem) {
		t.Errorf("occupancy = %d, want %d unchanged — no leak", got, cap(eng.sem))
	}
	if _, found := eng.mergeTrainInFlight.Load(trainKey); found {
		t.Error("in-flight marker not cleared on return")
	}
}

// A conflict that is resolved with a free slot releases it, and a second conflict-free
// worker run leaves occupancy at zero (no leak and no double release across runs).
func TestRunMergeTrainWorker_ConflictResolutionReleasesItsSlot(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)
	sha1 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-1", "counter.txt", "from-branch-1\n")
	sha2 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-2", "counter.txt", "from-branch-2\n")

	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			switch issueNumber {
			case 1:
				return &gh.PRDetails{Number: 10, HeadSHA: sha1, State: "open"}, nil
			case 2:
				return &gh.PRDetails{Number: 11, HeadSHA: sha2, State: "open"}, nil
			}
			return nil, fmt.Errorf("not found")
		},
		addCommentFn:    func(owner, repo string, issueNumber int, body string) (int, error) { return 1, nil },
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) { return 99, nil },
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
	}
	var eng *Engine
	var during int
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			during = len(eng.sem)
			if err := os.WriteFile(filepath.Join(workDir, "counter.txt"), []byte("from-branch-1\nfrom-branch-2\n"), 0o644); err != nil {
				return "", false, TokenUsage{}, err
			}
			mustGit(t, workDir, "add", "-A")
			mustGit(t, workDir, "commit", "--no-edit", "-m", fmt.Sprintf("chore(merge-train): resolve conflict for #%d", issue.Number))
			return "resolved", true, TokenUsage{}, nil
		},
	}
	eng = trainTestEngine(t, client, claude, wm)
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()

	batch := []gh.ProjectItem{makeTrainItem(1, "Issue 1"), makeTrainItem(2, "Issue 2")}
	state := &mergeTrainWorkerState{assembling: true, trialName: fmt.Sprintf("merge-train-repo-%d", time.Now().Unix())}
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", "main"), state)
	eng.store.EnterRepoWorker(mergeTrainKey("owner/repo", "main"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", "main", batch)

	if len(claude.forCommentsCalls) == 0 {
		t.Fatal("conflict resolution was not exercised")
	}
	if during != 1 {
		t.Errorf("occupancy during the invocation = %d, want 1", during)
	}
	if got := len(eng.sem); got != 0 {
		t.Errorf("occupancy after the worker returned = %d, want 0 — leaked or double-released slot", got)
	}
}
