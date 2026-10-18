package engine

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/warnings"
)

type forgeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *forgeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *forgeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var errTimeout = fmt.Errorf("executing request: %w", &url.Error{Op: "Post", URL: "u", Err: context.DeadlineExceeded})

func newForgeTestEngine(t *testing.T) (*Engine, *forgeClock) {
	t.Helper()
	dir := t.TempDir()
	prev := warnings.WarningsPathOverride
	warnings.WarningsPathOverride = dir + "/warnings.json"
	t.Cleanup(func() { warnings.WarningsPathOverride = prev })
	e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	clk := &forgeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	e.SetClock(clk)
	return e, clk
}

func TestForgeHold_EngagesOnSustainedTransportFailures(t *testing.T) {
	e, clk := newForgeTestEngine(t)
	// A burst of many failures inside the window does not engage.
	for i := 0; i < 20; i++ {
		e.ObserveForgeResult(errTimeout)
	}
	if e.forgeHeld() {
		t.Fatal("burst shorter than the window engaged the hold")
	}
	clk.Advance(forgeHoldWindow)
	e.ObserveForgeResult(errTimeout)
	if !e.forgeHeld() {
		t.Fatal("sustained failures did not engage the hold")
	}
	snap := e.health.snapshot()
	if !snap.ForgeHeld || snap.ForgeFailures < forgeHoldMinFailures || snap.ForgeLastError == "" {
		t.Errorf("health mirror not published: %+v", snap)
	}
	entries, _ := warnings.Load()
	found := false
	for _, en := range entries {
		if en.Key == "forge_unreachable:github" {
			found = true
		}
	}
	if !found {
		t.Error("operator warning was not recorded")
	}

	e.ObserveForgeResult(nil)
	if e.forgeHeld() {
		t.Fatal("a success did not lift the hold")
	}
	if e.health.snapshot().ForgeHeld {
		t.Error("health mirror still shows the hold")
	}
	entries, _ = warnings.Load()
	for _, en := range entries {
		if en.Key == "forge_unreachable:github" {
			t.Error("warning not resolved on lift")
		}
	}
}

func TestForgeHold_SingleErrorAndFewFailuresNeverEngage(t *testing.T) {
	e, clk := newForgeTestEngine(t)
	e.ObserveForgeResult(errTimeout)
	clk.Advance(time.Hour) // span satisfied, count is not
	e.ObserveForgeResult(errTimeout)
	if e.forgeHeld() {
		t.Fatal("two failures engaged the hold")
	}
}

func TestForgeHold_ClientErrorLiftsAndResetsRun(t *testing.T) {
	e, clk := newForgeTestEngine(t)
	for i := 0; i < 4; i++ {
		e.ObserveForgeResult(errTimeout)
	}
	e.ObserveForgeResult(errors.New("GitHub API returned 404: nope")) // answered: resets the run
	clk.Advance(forgeHoldWindow)
	e.ObserveForgeResult(errTimeout)
	if e.forgeHeld() {
		t.Fatal("run was not reset by a 4xx")
	}
	for i := 0; i < 5; i++ {
		e.ObserveForgeResult(errTimeout)
	}
	clk.Advance(forgeHoldWindow)
	e.ObserveForgeResult(errTimeout)
	if !e.forgeHeld() {
		t.Fatal("hold should engage")
	}
	e.ObserveForgeResult(errors.New("GitHub API returned 403: forbidden"))
	if e.forgeHeld() {
		t.Fatal("a 4xx did not lift the hold")
	}
}

func TestForgeHold_StaleRunLapses(t *testing.T) {
	e, clk := newForgeTestEngine(t)
	for i := 0; i < 4; i++ {
		e.ObserveForgeResult(errTimeout)
	}
	clk.Advance(forgeHoldRunLapse + time.Minute)
	e.ObserveForgeResult(errTimeout)
	if e.forgeHeld() {
		t.Fatal("old failures completed by one new one engaged the hold")
	}
}

func TestForgeHold_DisabledAndCanceled(t *testing.T) {
	e, clk := newForgeTestEngine(t)
	e.SetForgeHoldDisabledForTest(true)
	for i := 0; i < 10; i++ {
		e.ObserveForgeResult(errTimeout)
	}
	clk.Advance(time.Hour)
	e.ObserveForgeResult(errTimeout)
	if e.forgeHeld() {
		t.Fatal("disabled seam still engaged")
	}
	e.SetForgeHoldDisabledForTest(false)
	for i := 0; i < 10; i++ {
		e.ObserveForgeResult(context.Canceled)
	}
	clk.Advance(time.Hour)
	e.ObserveForgeResult(context.Canceled)
	if e.forgeHeld() {
		t.Fatal("context.Canceled counted as transport")
	}
}

// A call that never reached the forge (cancelled context at shutdown, a local
// request-build failure) says nothing about reachability: it must not lift an
// engaged hold nor reset the failure run (#2393 review).
func TestForgeHold_UnrelatedErrorsNeitherLiftNorReset(t *testing.T) {
	unrelated := map[string]error{
		"canceled":       fmt.Errorf("executing request: %w", &url.Error{Op: "Get", URL: "u", Err: context.Canceled}),
		"marshal":        errors.New("marshaling request: json: unsupported type"),
		"create-request": errors.New("creating request: invalid method"),
	}
	for name, uerr := range unrelated {
		t.Run(name, func(t *testing.T) {
			e, clk := newForgeTestEngine(t)
			for i := 0; i < forgeHoldMinFailures; i++ {
				e.ObserveForgeResult(errTimeout)
			}
			clk.Advance(forgeHoldWindow)
			e.ObserveForgeResult(errTimeout)
			if !e.forgeHeld() {
				t.Fatal("hold did not engage")
			}
			e.ObserveForgeResult(uerr)
			if !e.forgeHeld() {
				t.Fatalf("%s lifted an engaged hold", name)
			}
			if !e.health.snapshot().ForgeHeld {
				t.Error("health mirror cleared by an unrelated error")
			}
			// And an unrelated error mid-run does not reset it either.
			e2, clk2 := newForgeTestEngine(t)
			for i := 0; i < forgeHoldMinFailures-1; i++ {
				e2.ObserveForgeResult(errTimeout)
			}
			e2.ObserveForgeResult(uerr)
			clk2.Advance(forgeHoldWindow)
			e2.ObserveForgeResult(errTimeout)
			if !e2.forgeHeld() {
				t.Fatalf("%s reset the failure run", name)
			}
		})
	}
}

func TestForgeHold_ConcurrentObserve(t *testing.T) {
	e, _ := newForgeTestEngine(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if j%7 == 0 && i%2 == 0 {
					e.ObserveForgeResult(nil)
				} else {
					e.ObserveForgeResult(errTimeout)
				}
				_ = e.forgeHeld()
			}
		}(i)
	}
	wg.Wait()
}

// engageForgeHold drives the tracker into the held state on the test clock.
func engageForgeHold(e *Engine, clk *forgeClock) {
	for i := 0; i < forgeHoldMinFailures; i++ {
		e.ObserveForgeResult(errTimeout)
	}
	clk.Advance(forgeHoldWindow)
	e.ObserveForgeResult(errTimeout)
}

func TestForgeHold_DispatchCandidatesStartsNothing(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	e := testEngine(t, client, claude)
	clk := &forgeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	e.SetClock(clk)
	prev := warnings.WarningsPathOverride
	warnings.WarningsPathOverride = t.TempDir() + "/warnings.json"
	t.Cleanup(func() { warnings.WarningsPathOverride = prev })
	engageForgeHold(e, clk)
	if !e.forgeHeld() {
		t.Fatal("hold not engaged")
	}

	item := gh.ProjectItem{Number: 7, Title: "t", Status: "Research"}
	dispatched, deferred := e.dispatchCandidates(context.Background(), &gh.ProjectBoard{}, []gh.ProjectItem{item})
	e.wg.Wait()
	if dispatched != 0 {
		t.Errorf("dispatched = %d, want 0 while held", dispatched)
	}
	if len(deferred) != 1 {
		t.Errorf("deferred = %v, want the candidate handed back for re-admission", deferred)
	}
	if len(claude.calls) != 0 || len(client.addLabelCalls) != 0 {
		t.Errorf("held dispatch touched claude/labels: %d calls, %v", len(claude.calls), client.addLabelCalls)
	}
	snap, err := e.store.Get("owner/repo", 7)
	if err == nil && snap.Worker() != nil {
		t.Error("a worker handle was recorded while held")
	}
}

func TestForgeHold_CommentReviewIsNoOp(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	e := testEngine(t, client, claude)
	clk := &forgeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	e.SetClock(clk)
	prev := warnings.WarningsPathOverride
	warnings.WarningsPathOverride = t.TempDir() + "/warnings.json"
	t.Cleanup(func() { warnings.WarningsPathOverride = prev })
	engageForgeHold(e, clk)

	item := gh.ProjectItem{Number: 8}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "someone", Body: "hi"}}
	kind, err := e.processCommentsClassified(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Review", Order: 1}, comments)
	if err != nil || kind != didNotRunForgeHeld {
		t.Fatalf("kind=%q err=%v, want didNotRunForgeHeld", kind, err)
	}
	if len(client.addLabelCalls) != 0 || len(client.addCommentReactionCalls) != 0 || len(claude.forCommentsCalls) != 0 {
		t.Error("held comment review had side effects")
	}
	if got := e.commentBreakerCount(item); got != 0 {
		t.Errorf("breaker charged: %d", got)
	}
}

func TestForgeHold_ConflictResolutionNotAttemptedAndUncharged(t *testing.T) {
	claude := &mockClaudeInvoker{}
	e := testEngine(t, &mockGitHubClient{}, claude)
	clk := &forgeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	e.SetClock(clk)
	prev := warnings.WarningsPathOverride
	warnings.WarningsPathOverride = t.TempDir() + "/warnings.json"
	t.Cleanup(func() { warnings.WarningsPathOverride = prev })
	engageForgeHold(e, clk)

	_, _, err := e.resolveConflictWithClaude(context.Background(), "owner/repo", gh.ProjectItem{Number: 9}, t.TempDir(), &stages.Stage{Name: "Queued"}, "sha", nil, "", nil, InvokeOptions{})
	if !errors.Is(err, errForgeHeldNotAttempted) {
		t.Fatalf("err = %v, want errForgeHeldNotAttempted", err)
	}
	if !trainCancelled(context.Background(), err) {
		t.Error("trainCancelled must read the forge hold as an uncharged not-attempted outcome")
	}
	if len(claude.forCommentsCalls) != 0 {
		t.Error("claude invoked while held")
	}
}

func TestForgeHold_ReinvokeRefundsAndSpawnsNothing(t *testing.T) {
	claude := &mockClaudeInvoker{}
	e := testEngine(t, &mockGitHubClient{}, claude)
	clk := &forgeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	e.SetClock(clk)
	prev := warnings.WarningsPathOverride
	warnings.WarningsPathOverride = t.TempDir() + "/warnings.json"
	t.Cleanup(func() { warnings.WarningsPathOverride = prev })
	engageForgeHold(e, clk)

	refunded := false
	built := false
	e.dispatchReinvoke(context.Background(), &gh.ProjectBoard{}, gh.ProjectItem{Number: 10}, &stages.Stage{Name: "Review"}, reinvokeOpts{
		tag:   "test-reinvoke",
		build: func(string) []gh.Comment { built = true; return nil },
		cycle: cycleCharge{label: "test", refund: func(repo string, n int, stage string) []itemstate.Mutation {
			refunded = true
			return nil
		}},
	})
	e.wg.Wait()
	if !refunded {
		t.Error("the pre-charged cycle was not refunded")
	}
	if built || len(claude.forCommentsCalls) != 0 {
		t.Error("held reinvoke built comments or invoked claude")
	}
}
