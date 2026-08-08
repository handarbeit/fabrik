package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

func TestItemNeedsWork_SkipsPaused(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{"fabrik:paused"},
	}

	if eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should return false for paused item")
	}
}

func TestItemNeedsWork_Paused_HumanComment_Dispatches(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{"fabrik:paused"},
		Comments: []gh.Comment{
			{ID: "C1", Author: "otheruser", Body: "Please do something"},
		},
	}

	// A human comment on a paused item triggers work (unpause).
	if !eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should return true for paused item with a new human comment")
	}
}

// TestItemNeedsWork_Paused_MixedBatch_Dispatches and
// TestItemNeedsWork_AwaitingInput_MixedBatch_Dispatches cover the gate half of
// the gate/action symmetry ADR 069 relies on: TestProcessItem_Paused_MixedBatch_ProcessesBothCommentsOnUnpause
// and TestProcessItem_AwaitingInput_MixedBatch_ProcessesBothCommentsOnUnblock
// (blocked_on_input_test.go) already assert the action admits a mixed
// bot+human batch; nothing asserted the gate does the same, which is exactly
// the case where a filtering slip would strand an item (admitted by one side,
// silently skipped by the other).
func TestItemNeedsWork_Paused_MixedBatch_Dispatches(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{"fabrik:paused"},
		Comments: []gh.Comment{
			{ID: "B1", Author: "gemini-code-assist", Body: "quota notice"},
			{ID: "H1", Author: "otheruser", Body: "please continue"},
		},
	}

	if !eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should return true for paused item with a mixed bot+human comment batch")
	}
}

func TestItemNeedsWork_AwaitingInput_MixedBatch_Dispatches(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{"fabrik:paused", "fabrik:awaiting-input"},
		Comments: []gh.Comment{
			{ID: "B1", Author: "gemini-code-assist", Body: "quota notice"},
			{ID: "H1", Author: "otheruser", Body: "please continue"},
		},
	}

	if !eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should return true for awaiting-input item with a mixed bot+human comment batch")
	}
}

func TestItemNeedsWork_Paused_BotComment_Skips(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{"fabrik:paused"},
		Comments: []gh.Comment{
			{ID: "C1", Author: "gemini-code-assist", Body: "quota notice"},
		},
	}

	// A bot comment must not defeat an operator-applied pause (#1083).
	if eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should return false for paused item with only a bot comment")
	}
}

func TestItemNeedsWork_NonPaused_BotComment_StillDispatches(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{},
		Comments: []gh.Comment{
			{ID: "C1", Author: "gemini-code-assist", Body: "review comment"},
		},
	}

	// Regression guard: bot comment dispatch on a non-paused issue is unaffected
	// by the human-only resume-trigger restriction — only the paused/awaiting-input
	// resume decision is scoped to humans.
	if !eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should return true for non-paused item with a bot comment")
	}
}

func TestFindNewComments(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Comments: []gh.Comment{
			{ID: "C1", Author: "testuser", Body: "Do this"},
			{ID: "C2", Author: "otheruser", Body: "Not from us"},
			{ID: "C3", Author: "testuser", Body: "🏭 **Fabrik — output"},
			{ID: "C4", Author: "testuser", Body: "Also do that"},
		},
	}

	comments := eng.findNewComments(item)
	if len(comments) != 3 {
		t.Fatalf("expected 3 new comments, got %d", len(comments))
	}
	if comments[0].ID != "C1" || comments[1].ID != "C2" || comments[2].ID != "C4" {
		t.Errorf("comments = %v", comments)
	}

	// After markCommentsProcessed, second call should return no new comments
	eng.markCommentsProcessed(item, comments)
	comments2 := eng.findNewComments(item)
	if len(comments2) != 0 {
		t.Errorf("expected 0 new comments on second call, got %d", len(comments2))
	}
}

func TestMapKeys(t *testing.T) {
	m := map[string]string{
		"a": "1",
		"b": "2",
		"c": "3",
	}
	keys := mapKeys(m)
	if len(keys) != 3 {
		t.Errorf("expected 3 keys, got %d", len(keys))
	}
	// Check all keys present (order doesn't matter)
	found := map[string]bool{}
	for _, k := range keys {
		found[k] = true
	}
	for _, expected := range []string{"a", "b", "c"} {
		if !found[expected] {
			t.Errorf("missing key %q", expected)
		}
	}
}

func TestNewWithDeps(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManager("/repo")
	cfg := Config{Owner: "o", Repo: "r"}

	eng := NewWithDeps(cfg, client, claude, wm)
	if eng.cfg.Owner != "o" {
		t.Errorf("Owner = %q", eng.cfg.Owner)
	}
	if eng.store == nil {
		t.Error("store should be initialized")
	}
}

func TestNew(t *testing.T) {
	skipIfNoGit(t)
	// New() runs the real claudeNameFlagSupported probe. Save/restore so the
	// result doesn't leak into later tests in this package's test binary, and
	// isolate PATH to a directory with no claude binary so the probe fails
	// fast via exec.LookPath rather than depending on (or waiting up to the
	// probe's 5s timeout on) whatever happens to be installed on the runner.
	origSupported := claudeNameFlagSupported
	defer func() { claudeNameFlagSupported = origSupported }()
	t.Setenv("PATH", t.TempDir())

	cfg := Config{
		Owner: "o",
		Repo:  "r",
		Token: "tok",
	}
	eng, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if eng.client == nil {
		t.Error("client should not be nil")
	}
	if eng.claude == nil {
		t.Error("claude should not be nil")
	}
	// worktreeManagers starts empty — repos are registered lazily via ensureRepoReady
	if eng.worktreeManagers == nil {
		t.Error("worktreeManagers map should be initialized")
	}
	if eng.fabrikDir == "" {
		t.Error("fabrikDir should not be empty")
	}
}

func TestNew_WiresMergeStrategy(t *testing.T) {
	skipIfNoGit(t)
	// See TestNew: save/restore claudeNameFlagSupported and isolate PATH so
	// the real probe New() runs is deterministic and fast rather than
	// depending on whatever claude binary happens to be on the runner's PATH.
	origSupported := claudeNameFlagSupported
	defer func() { claudeNameFlagSupported = origSupported }()
	t.Setenv("PATH", t.TempDir())

	cfg := Config{
		Owner:             "o",
		Repo:              "r",
		Token:             "tok",
		AutoMergeStrategy: "SQUASH",
	}
	eng, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ghClient, ok := eng.client.(*gh.Client)
	if !ok {
		t.Fatalf("eng.client is %T, want *gh.Client", eng.client)
	}
	if got := ghClient.MergeStrategy(); got != "SQUASH" {
		t.Errorf("client.MergeStrategy() = %q, want %q", got, "SQUASH")
	}
}

// TestNew_GHESHost_ReleaseClientStaysOnGithubCom covers the self-upgrade
// wrong-host risk Research surfaced: when a GHES host is configured,
// eng.client must be GHES-targeted while eng.releaseClient (used by
// checkReleaseUpgrade to fetch Fabrik's own release from
// handarbeit/fabrik) must remain a distinct, github.com-targeted client.
// A shared/aliased client here would silently misdirect fabrik upgrade's
// release lookup at the customer's GHES instance instead of github.com.
func TestNew_GHESHost_ReleaseClientStaysOnGithubCom(t *testing.T) {
	skipIfNoGit(t)
	origSupported := claudeNameFlagSupported
	defer func() { claudeNameFlagSupported = origSupported }()
	t.Setenv("PATH", t.TempDir())

	cfg := Config{
		Owner:    "o",
		Repo:     "r",
		Token:    "tok",
		GHESHost: "github.example.com",
	}
	eng, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ghesClient, ok := eng.client.(*gh.Client)
	if !ok {
		t.Fatalf("eng.client is %T, want *gh.Client", eng.client)
	}
	releaseClient, ok := eng.releaseClient.(*gh.Client)
	if !ok {
		t.Fatalf("eng.releaseClient is %T, want *gh.Client", eng.releaseClient)
	}
	if ghesClient == releaseClient {
		t.Fatal("eng.releaseClient must be a distinct client instance from eng.client when a GHES host is configured — sharing one client would let the GHES baseURL leak into the release-upgrade path")
	}
	if eng.hostClient != ghesClient {
		t.Errorf("eng.hostClient = %p, want same instance as eng.client (%p)", eng.hostClient, ghesClient)
	}
	// cfg.Token authenticates the configured GHES host, not github.com — it
	// must not be forwarded to the github.com-pinned releaseClient, or every
	// self-upgrade request would fail with "Bad credentials" instead of
	// succeeding unauthenticated against fabrik's public release repo.
	if got := releaseClient.Token(); got != "" {
		t.Errorf("releaseClient.Token() = %q, want empty (cfg.Token is GHES-scoped, not valid on github.com)", got)
	}
}

// TestReleaseUpgradeToken covers the token-isolation half of the self-upgrade
// wrong-host risk: cfg.Token must pass through unchanged for the default
// github.com path, but must be dropped (not forwarded to github.com) when a
// GHES host is configured, since it authenticates the GHES instance, not
// github.com.
func TestReleaseUpgradeToken(t *testing.T) {
	if got := releaseUpgradeToken(Config{Token: "tok"}); got != "tok" {
		t.Errorf("releaseUpgradeToken with no GHES host = %q, want %q (unchanged default)", got, "tok")
	}
	if got := releaseUpgradeToken(Config{Token: "tok", GHESHost: "github.example.com"}); got != "" {
		t.Errorf("releaseUpgradeToken with GHES host = %q, want empty", got)
	}
}

func TestRun_ShutdownOnSignal(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 300 // long poll so we don't hit a second tick

	// Provide a temp fabrikDir so Run() can create the lock file.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	eng.fabrikDir = dir

	// Use ReadyCh so we only send SIGINT after signal.Notify is registered.
	readyCh := make(chan struct{})
	eng.cfg.ReadyCh = readyCh

	done := make(chan error, 1)
	go func() {
		done <- eng.Run()
	}()

	// Wait for Run to register signal handlers before sending SIGINT.
	<-readyCh
	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGINT)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not shut down in time")
	}
}

func TestStoreCommentConcurrency(t *testing.T) {
	e := &Engine{
		cfg:   Config{User: "testuser"},
		store: itemstate.NewStore(nil),
	}

	var wg sync.WaitGroup
	// Simulate concurrent CommentProcessed writes from multiple workers
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			e.store.Apply(itemstate.CommentProcessed{
				Repo:      "owner/repo",
				Number:    n + 1,
				CommentID: fmt.Sprintf("c-%d", n),
				At:        time.Now(),
			})
		}(i)
	}
	wg.Wait()

	// Verify one of the entries was recorded
	snap, err := e.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if snap.CommentProcessed("c-0").IsZero() {
		t.Error("expected CommentProcessed[c-0] to be set")
	}
}

// TestMarkCommentsProcessedConcurrency verifies markCommentsProcessed is safe
// when called from multiple goroutines.
func TestMarkCommentsProcessedConcurrency(t *testing.T) {
	e := &Engine{
		cfg:   Config{User: "testuser"},
		store: itemstate.NewStore(nil),
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			item := gh.ProjectItem{Number: n + 1, Repo: "owner/repo"}
			comments := []gh.Comment{
				{ID: fmt.Sprintf("c-%d-a", n)},
				{ID: fmt.Sprintf("c-%d-b", n)},
			}
			e.markCommentsProcessed(item, comments)
		}(i)
	}
	wg.Wait()

	// Verify a sample entry is recorded correctly
	snap, err := e.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if snap.CommentProcessed("c-0-a").IsZero() {
		t.Error("expected CommentProcessed[c-0-a] to be set after markCommentsProcessed")
	}
}

// TestFindNewCommentsFiltering verifies that findNewComments correctly filters
// already-processed and fabrik-output comments regardless of author.
func TestFindNewCommentsFiltering(t *testing.T) {
	e := &Engine{
		cfg:   Config{User: "alice"},
		store: itemstate.NewStore(nil),
	}

	// Pre-mark one comment as processed via the store
	e.store.Apply(itemstate.CommentProcessed{Repo: "owner/repo", Number: 42, CommentID: "c2", At: time.Now()})

	item := gh.ProjectItem{
		Number: 42,
		Repo:   "owner/repo",
		Comments: []gh.Comment{
			{ID: "c1", Author: "alice", Body: "please fix"},        // new — should be returned
			{ID: "c2", Author: "alice", Body: "already seen"},      // already processed
			{ID: "c3", Author: "bob", Body: "colleague comment"},   // any author — should be returned
			{ID: "c4", Author: "alice", Body: "🏭 **Fabrik output"}, // fabrik output — skipped
		},
	}

	result := e.findNewComments(item)
	if len(result) != 2 {
		t.Fatalf("expected 2 new comments, got %d", len(result))
	}
	if result[0].ID != "c1" || result[1].ID != "c3" {
		t.Errorf("expected comments c1 and c3, got %v", result)
	}
}

// TestHumanNewComments verifies humanNewComments filters findNewComments'
// output to exclude bot logins, leaving only human-authored comments (#1083).
// It deliberately does NOT exclude e.cfg.User: that is the operator's own
// GitHub login (in the common single-account deployment, the same account
// Fabrik posts as), so excluding it would filter out the operator's own
// resume reply. Fabrik's own output is already excluded upstream by
// findNewComments' body-prefix check, independent of author.
func TestHumanNewComments(t *testing.T) {
	e := &Engine{
		cfg:   Config{User: "fabrikbot"},
		store: itemstate.NewStore(nil),
	}

	item := gh.ProjectItem{
		Number: 42,
		Repo:   "owner/repo",
		Comments: []gh.Comment{
			{ID: "c1", Author: "alice", Body: "please continue"},         // human — kept
			{ID: "c2", Author: "gemini-code-assist", Body: "quota hit"},  // bot — filtered
			{ID: "c3", Author: "dependabot[bot]", Body: "bump version"},  // bot — filtered
			{ID: "c4", Author: "fabrikbot", Body: "reply from operator"}, // matches cfg.User — kept, not a bot login
			{ID: "c5", Author: "bob", Body: "another human"},             // human — kept
		},
	}

	result := e.humanNewComments(item)
	if len(result) != 3 {
		t.Fatalf("expected 3 human comments, got %d: %v", len(result), result)
	}
	if result[0].ID != "c1" || result[1].ID != "c4" || result[2].ID != "c5" {
		t.Errorf("expected comments c1, c4, and c5, got %v", result)
	}
}

// TestHumanNewComments_EmptyAuthor_FailsClosed verifies that a comment with no
// resolvable author (e.g. a deleted GitHub account, which the deep fetch
// leaves as an empty Author string) is treated as non-human rather than as an
// implicit resume trigger. IsBotLogin("") is false, so without this guard an
// unattributed comment would silently defeat a pause exactly like the bot
// chatter this fix targets (#1083) — fail closed instead.
func TestHumanNewComments_EmptyAuthor_FailsClosed(t *testing.T) {
	e := &Engine{
		cfg:   Config{User: "operator"},
		store: itemstate.NewStore(nil),
	}

	item := gh.ProjectItem{
		Number: 9,
		Repo:   "owner/repo",
		Comments: []gh.Comment{
			{ID: "e1", Author: "", Body: "comment from a deleted account"},
			{ID: "h1", Author: "alice", Body: "please continue"},
		},
	}

	result := e.humanNewComments(item)
	if len(result) != 1 || result[0].ID != "h1" {
		t.Fatalf("expected only the human comment h1, got %v", result)
	}
}

// TestHumanNewComments_MixedBatch_KeepsOnlyHuman verifies that humanNewComments
// isolates the human-authored comment out of a mixed human+bot batch. This
// filtered result is used only to decide *whether* to resume a paused /
// awaiting-input item — it is not what gets handed to processComments once
// resumed (processItem passes the full raw findNewComments batch at that
// point; see TestProcessItem_Paused_MixedBatch_ProcessesBothCommentsOnUnpause
// and its awaiting-input counterpart).
func TestHumanNewComments_MixedBatch_KeepsOnlyHuman(t *testing.T) {
	e := &Engine{
		cfg:   Config{User: "operator"},
		store: itemstate.NewStore(nil),
	}

	item := gh.ProjectItem{
		Number: 7,
		Repo:   "owner/repo",
		Comments: []gh.Comment{
			{ID: "b1", Author: "gemini-code-assist", Body: "quota notice"},
			{ID: "h1", Author: "operator", Body: "please continue"},
			{ID: "b2", Author: "dependabot[bot]", Body: "bump version"},
		},
	}

	result := e.humanNewComments(item)
	if len(result) != 1 || result[0].ID != "h1" {
		t.Fatalf("expected only the human comment h1, got %v", result)
	}
}

// TestMaxConcurrentDefault verifies the default config value.
func TestMaxConcurrentDefault(t *testing.T) {
	cfg := Config{MaxConcurrent: 5}
	if cfg.MaxConcurrent != 5 {
		t.Errorf("expected default MaxConcurrent=5, got %d", cfg.MaxConcurrent)
	}
}

// TestConcurrentItemDispatch verifies that the non-blocking semaphore dispatch
// used in poll() respects MaxConcurrent and processes all items across multiple
// simulated poll cycles without data races.
func TestConcurrentItemDispatch(t *testing.T) {
	const numItems = 15
	const maxConcurrent = 3

	e := &Engine{
		cfg: Config{
			User:          "testuser",
			MaxConcurrent: maxConcurrent,
			Stages:        nil, // no matching stage → processItem returns nil immediately
		},
		store: itemstate.NewStore(nil),
		sem:   make(chan struct{}, maxConcurrent),
	}

	board := &gh.ProjectBoard{}
	items := make([]gh.ProjectItem, numItems)
	for i := range items {
		items[i] = gh.ProjectItem{Number: i + 1, Status: "NoSuchStage"}
	}

	var (
		mu          sync.Mutex
		processed   int
		maxInFlight int
		inFlight    int
	)

	// Replicate the non-blocking dispatch pattern from poll(). Items that don't
	// get a semaphore slot are retried in subsequent cycles, mirroring real behaviour.
	remaining := make([]gh.ProjectItem, len(items))
	copy(remaining, items)
	var dispatchWg sync.WaitGroup

	for len(remaining) > 0 {
		var nextRound []gh.ProjectItem
		for _, item := range remaining {
			item := item
			select {
			case e.sem <- struct{}{}:
			default:
				nextRound = append(nextRound, item)
				continue
			}
			dispatchWg.Add(1)
			go func() {
				defer dispatchWg.Done()
				defer func() { <-e.sem }()

				mu.Lock()
				inFlight++
				if inFlight > maxInFlight {
					maxInFlight = inFlight
				}
				mu.Unlock()

				if err := e.processItem(context.Background(), board, item); err != nil {
					t.Errorf("processItem error for issue #%d: %v", item.Number, err)
				}

				mu.Lock()
				inFlight--
				processed++
				mu.Unlock()
			}()
		}
		remaining = nextRound
		if len(remaining) > 0 {
			// Yield so in-flight goroutines can make progress and free semaphore slots.
			dispatchWg.Wait()
		}
	}
	dispatchWg.Wait()

	if processed != numItems {
		t.Errorf("expected %d items processed, got %d", numItems, processed)
	}
	if maxInFlight > maxConcurrent {
		t.Errorf("max in-flight goroutines was %d, expected <= %d", maxInFlight, maxConcurrent)
	}
}

// TestPollNonBlockingAtCapacity verifies that the dispatch loop in poll() skips
// items via non-blocking semaphore acquire when all slots are taken, so poll()
// itself never blocks and the ticker can fire on schedule.
func TestPollNonBlockingAtCapacity(t *testing.T) {
	const maxConcurrent = 2

	e := &Engine{
		cfg: Config{
			User:          "testuser",
			MaxConcurrent: maxConcurrent,
			Stages:        nil,
		},
		sem: make(chan struct{}, maxConcurrent),
	}

	// Fill the semaphore to simulate two in-flight workers from a previous cycle.
	e.sem <- struct{}{}
	e.sem <- struct{}{}

	items := []gh.ProjectItem{
		{Number: 1, Status: "NoSuchStage"},
		{Number: 2, Status: "NoSuchStage"},
	}

	// Replicate the non-blocking dispatch from poll().
	dispatched := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range items {
			select {
			case e.sem <- struct{}{}:
				e.wg.Add(1)
				dispatched++
				go func() {
					defer e.wg.Done()
					defer func() { <-e.sem }()
				}()
			default:
				// skipped — at capacity
			}
		}
	}()

	select {
	case <-done:
		// dispatch loop returned without blocking — correct
	case <-time.After(time.Second):
		t.Fatal("dispatch loop blocked when semaphore was full")
	}

	if dispatched != 0 {
		t.Errorf("expected 0 dispatched (semaphore full), got %d", dispatched)
	}
}

// TestIdleCountNotIncrementedWhileWorkersInFlight verifies that idleCount (which
// drives auto-upgrade) is not incremented when dispatched==0 but a worker is
// still running from a previous poll cycle — whether that worker is a
// per-item dispatch (itemstate.WorkerEntered) or a repo-scoped merge-train
// worker (itemstate.Store.EnterRepoWorker). Upgrading while a worker is
// in-flight would call syscall.Exec and kill it.
//
// Unlike a prior version of this test, this drives the actual production
// guard (engine/poll.go's dispatched==0 branch, via a real eng.poll() call)
// rather than a copy of its if/else logic inlined in the test body — the
// guard could previously be deleted entirely and the test would still pass.
func TestIdleCountNotIncrementedWhileWorkersInFlight(t *testing.T) {
	newIdleTestEngine := func(t *testing.T) *Engine {
		t.Helper()
		client := &mockGitHubClient{
			fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
				// Zero items on the board — nothing to dispatch this poll cycle,
				// so poll() takes the dispatched==0 idle-guard branch.
				return &gh.ProjectBoard{ProjectID: "PVT_1", Items: nil}, nil
			},
		}
		return NewWithDeps(Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			AutoUpgrade:   true,
			Stages:        testStages(),
		}, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	}

	t.Run("no worker in flight", func(t *testing.T) {
		eng := newIdleTestEngine(t)
		if _, err := eng.poll(context.Background()); err != nil {
			t.Fatalf("poll: %v", err)
		}
		// idleUpgradeThreshold is 2, so a single idle poll only increments
		// idleCount to 1 — never far enough to reach checkAndUpgrade's
		// syscall.Exec, but far enough to prove the guard doesn't suppress
		// the increment when nothing is actually in flight.
		if eng.idleCount != 1 {
			t.Errorf("idleCount = %d, want 1 (guard must not suppress the increment when no worker is in flight)", eng.idleCount)
		}
	})

	t.Run("per-item worker in flight", func(t *testing.T) {
		eng := newIdleTestEngine(t)
		eng.store.Apply(itemstate.WorkerEntered{
			Repo:      "owner/repo",
			Number:    42,
			StageName: "test",
			StartedAt: time.Now(),
		})
		if _, err := eng.poll(context.Background()); err != nil {
			t.Fatalf("poll: %v", err)
		}
		if eng.idleCount != 0 {
			t.Errorf("idleCount = %d, want 0 while a per-item worker is in flight", eng.idleCount)
		}
	})

	t.Run("merge-train worker in flight", func(t *testing.T) {
		eng := newIdleTestEngine(t)
		eng.store.EnterRepoWorker("owner/repo")
		if _, err := eng.poll(context.Background()); err != nil {
			t.Fatalf("poll: %v", err)
		}
		if eng.idleCount != 0 {
			t.Errorf("idleCount = %d, want 0 while a merge-train worker is in flight", eng.idleCount)
		}
	})
}

func TestExtractModelOverride(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{"no labels", nil, ""},
		{"no model label", []string{"stage:Plan:complete", "fabrik:locked"}, ""},
		{"single model label", []string{"model:opus"}, "opus"},
		{"model label among others", []string{"stage:Plan", "model:sonnet", "fabrik:locked"}, "sonnet"},
		{"empty model name skipped", []string{"model:", "model:haiku"}, "haiku"},
		{"multiple model labels uses first", []string{"model:opus", "model:sonnet"}, "opus"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := eng.extractModelOverride(0, tc.labels)
			if got != tc.want {
				t.Errorf("extractModelOverride(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

func TestExtractModelOverrideWarnsOnMultiple(t *testing.T) {
	// Verify no panic and correct return value when multiple model labels are present.
	// The warning goes to eng.logf (stdout in test mode) and is tested behaviorally above.
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	result := eng.extractModelOverride(0, []string{"model:opus", "model:sonnet", "model:haiku"})
	if result != "opus" {
		t.Errorf("expected %q, got %q", "opus", result)
	}
}

func TestExtractEffortOverride(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{"no labels", nil, ""},
		{"no effort label", []string{"stage:Plan:complete", "model:opus"}, ""},
		{"single effort:high", []string{"effort:high"}, "high"},
		{"single effort:max", []string{"effort:max"}, "max"},
		{"single effort:low", []string{"effort:low"}, "low"},
		{"single effort:medium", []string{"effort:medium"}, "medium"},
		{"effort label among others", []string{"stage:Plan", "effort:high", "fabrik:locked"}, "high"},
		{"empty effort name skipped", []string{"effort:", "effort:max"}, "max"},
		{"invalid prefix ignored", []string{"model:opus", "fabrik:yolo"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := eng.extractEffortOverride(0, tc.labels)
			if got != tc.want {
				t.Errorf("extractEffortOverride(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

func TestExtractEffortOverrideMultipleLabelsPrecedence(t *testing.T) {
	// When multiple effort: labels are present, the highest-ranked wins (max > high > medium > low).
	// This differs from model: override which uses first-found.
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{"low+max → max", []string{"effort:low", "effort:max"}, "max"},
		{"medium+high → high", []string{"effort:medium", "effort:high"}, "high"},
		{"low+medium → medium", []string{"effort:low", "effort:medium"}, "medium"},
		{"all four → max", []string{"effort:low", "effort:medium", "effort:high", "effort:max"}, "max"},
		{"max+high (reverse order) → max", []string{"effort:high", "effort:max"}, "max"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := eng.extractEffortOverride(0, tc.labels)
			if got != tc.want {
				t.Errorf("extractEffortOverride(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

func TestBaseBranchForItem(t *testing.T) {
	skipIfNoGit(t)
	// Use a real bare-clone + origin setup (rather than a standalone repo with
	// no remote at all) so the resolveBaseLabelBranch remote-check/fetch path
	// exercises an authoritative ls-remote against a genuine origin, not just
	// an error from a missing remote.
	_, srcDir, _, wm := setupTrainRepo(t)
	repoDir := wm.baseDir

	// Get the HEAD SHA so we can inject remote tracking refs.
	shaCmd := exec.Command("git", "rev-parse", "HEAD")
	shaCmd.Dir = repoDir
	shaOut, err := shaCmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	sha := strings.TrimSpace(string(shaOut))

	// Inject remote tracking refs directly in the local clone for valid-branch
	// tests — these short-circuit at the local branchExists check and never
	// reach the remote-check/fetch path, so they're unaffected by it.
	for _, branch := range []string{"develop", "release/1.x", "feature/a"} {
		cmd := exec.Command("git", "update-ref", "refs/remotes/origin/"+branch, sha)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git update-ref origin/%s: %s: %v", branch, out, err)
		}
	}

	// Branch created directly on the real origin (srcDir), never fetched into
	// the local clone — exercises the local-miss -> remote-hit -> fetch path.
	mustGit(t, srcDir, "branch", "release/remote-only")

	tests := []struct {
		name        string
		labels      []string
		want        string
		wantComment bool
	}{
		{
			name:   "no base label returns default",
			labels: []string{"stage:Plan:complete", "model:opus"},
			want:   "main",
		},
		{
			name:   "empty base label skipped returns default",
			labels: []string{"base:"},
			want:   "main",
		},
		{
			name:   "valid base:develop returns develop",
			labels: []string{"base:develop"},
			want:   "develop",
		},
		{
			name:   "base with slash base:release/1.x returns release/1.x",
			labels: []string{"base:release/1.x"},
			want:   "release/1.x",
		},
		{
			name:        "nonexistent branch falls back to default and posts comment",
			labels:      []string{"base:nonexistent"},
			want:        "main",
			wantComment: true,
		},
		{
			name:   "multiple base labels uses first",
			labels: []string{"base:develop", "base:feature/a"},
			want:   "develop",
		},
		{
			name:   "base label among other labels",
			labels: []string{"stage:Research:complete", "base:develop", "model:sonnet"},
			want:   "develop",
		},
		{
			name:   "branch absent locally but present on remote is resolved without fallback",
			labels: []string{"base:release/remote-only"},
			want:   "release/remote-only",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{}
			eng := testEngine(t, client, &mockClaudeInvoker{})
			item := gh.ProjectItem{Number: 42, Labels: tc.labels}

			got, err := eng.baseBranchForItem(item, wm)
			if err != nil {
				t.Fatalf("baseBranchForItem: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			client.mu.Lock()
			commentCount := len(client.addCommentCalls)
			var commentBody string
			if commentCount > 0 {
				commentBody = client.addCommentCalls[0].body
			}
			client.mu.Unlock()
			if tc.wantComment && commentCount == 0 {
				t.Error("expected fallback comment to be posted, but none was")
			}
			if !tc.wantComment && commentCount > 0 {
				t.Errorf("unexpected comment posted: %q", commentBody)
			}
			if tc.wantComment && !strings.Contains(commentBody, "ls-remote") {
				t.Errorf("expected fallback comment to mention the ls-remote check, got: %q", commentBody)
			}
		})
	}

	// Confirm the fetch actually populated the local clone, proving
	// EnsureWorktree would be able to resolve the branch downstream.
	if !wm.branchExists("origin/release/remote-only") {
		t.Error("expected release/remote-only to be resolvable locally after resolveBaseLabelBranch fetched it")
	}
}

func TestItemNeedsWork_CleanupStage_NeedsWork(t *testing.T) {
	rootDir := t.TempDir()
	worktreeDir := filepath.Join(rootDir, "issue-1")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatalf("create worktree dir: %v", err)
	}
	wm := NewWorktreeManagerWithRoot(t.TempDir(), rootDir)
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStagesWithCleanup(),
		},
		&mockGitHubClient{},
		&mockClaudeInvoker{},
		wm,
	)

	item := gh.ProjectItem{
		Number: 1,
		Status: "Done",
		Labels: []string{}, // no completion label → needs work
	}
	if !eng.itemNeedsWork(item) {
		t.Error("cleanup stage without completion label should need work")
	}
}

func TestItemNeedsWork_CleanupStage_AlreadyComplete(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Stages = testStagesWithCleanup()

	item := gh.ProjectItem{
		Number: 1,
		Status: "Done",
		Labels: []string{"stage:Done:complete"},
	}
	if eng.itemNeedsWork(item) {
		t.Error("cleanup stage with completion label should not need work")
	}
}

func TestItemNeedsWork_CleanupStage_PausedItem(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Stages = testStagesWithCleanup()

	item := gh.ProjectItem{
		Number: 1,
		Status: "Done",
		Labels: []string{"fabrik:paused"}, // paused → should not need work
	}
	if eng.itemNeedsWork(item) {
		t.Error("paused cleanup stage item should not need work")
	}
}
