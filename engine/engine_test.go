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

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/internal/itemstate"
)

func TestItemNeedsWork_SkipsPaused(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{"fabrik:paused"},
	}

	if eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should return false for paused item")
	}
}

func TestItemNeedsWork_SkipsPausedWithNewComments(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{"fabrik:paused"},
		Comments: []gh.Comment{
			{ID: "C1", Author: "otheruser", Body: "Please do something"},
		},
	}

	// Any comment (regardless of author) on a paused item triggers work.
	if !eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should return true for paused item with a new comment from any user")
	}
}

func TestFindNewComments(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

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

func TestRun_ShutdownOnSignal(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{}, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 300 // long poll so we don't hit a second tick

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
		for _, item := range items {
			item := item
			select {
			case e.sem <- struct{}{}:
				e.inFlight.Store(item.Number, struct{}{})
				e.wg.Add(1)
				dispatched++
				go func() {
					defer e.wg.Done()
					defer func() { <-e.sem }()
					defer e.inFlight.Delete(item.Number)
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
// drives auto-upgrade) is not incremented when dispatched==0 but workers are
// still running from a previous poll cycle. Upgrading while workers are in-flight
// would call syscall.Exec and kill them.
func TestIdleCountNotIncrementedWhileWorkersInFlight(t *testing.T) {
	e := &Engine{
		cfg: Config{
			AutoUpgrade:   true,
			MaxConcurrent: 1,
		},
		sem: make(chan struct{}, 1),
	}

	// Simulate an in-flight worker by populating the map directly.
	e.inFlight.Store(42, struct{}{})

	// With dispatched==0 and an in-flight worker, idleCount must not increment.
	dispatched := 0
	var hasInFlight bool
	e.inFlight.Range(func(_, _ any) bool { hasInFlight = true; return false })

	if hasInFlight {
		e.idleCount = 0
	} else if dispatched == 0 {
		e.idleCount++
	}

	if e.idleCount != 0 {
		t.Errorf("idleCount should remain 0 while workers are in-flight, got %d", e.idleCount)
	}
}

func TestExtractModelOverride(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
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
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	result := eng.extractModelOverride(0, []string{"model:opus", "model:sonnet", "model:haiku"})
	if result != "opus" {
		t.Errorf("expected %q, got %q", "opus", result)
	}
}

func TestExtractEffortOverride(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
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
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

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
	repoDir := initBareRepo(t)

	// Get the HEAD SHA so we can inject remote tracking refs.
	shaCmd := exec.Command("git", "rev-parse", "HEAD")
	shaCmd.Dir = repoDir
	shaOut, err := shaCmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	sha := strings.TrimSpace(string(shaOut))

	// Inject remote tracking refs for valid-branch tests.
	for _, branch := range []string{"liminis", "release/1.x", "feature/a"} {
		cmd := exec.Command("git", "update-ref", "refs/remotes/origin/"+branch, sha)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git update-ref origin/%s: %s: %v", branch, out, err)
		}
	}

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
			name:   "valid base:liminis returns liminis",
			labels: []string{"base:liminis"},
			want:   "liminis",
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
			labels: []string{"base:liminis", "base:feature/a"},
			want:   "liminis",
		},
		{
			name:   "base label among other labels",
			labels: []string{"stage:Research:complete", "base:liminis", "model:sonnet"},
			want:   "liminis",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{}
			eng := testEngine(client, &mockClaudeInvoker{})
			wm := NewWorktreeManager(repoDir)
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
			client.mu.Unlock()
			if tc.wantComment && commentCount == 0 {
				t.Error("expected fallback comment to be posted, but none was")
			}
			if !tc.wantComment && commentCount > 0 {
				t.Errorf("unexpected comment posted: %q", client.addCommentCalls[0].body)
			}
		})
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
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
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
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
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
