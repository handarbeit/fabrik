package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// TestCatchUpPhase1HandlersOrder asserts the handler precedence order by name.
// A future reorder of catchUpPhase1Handlers is a test failure, not a silent
// behavioral change — this directly implements the ADR-056 D3 "ordering as data"
// invariant. engineUnpause (position 0, #1460 R2) must run first and
// unconditionally so a just-reset cycle counter is what every other handler in
// the same pass evaluates. ADR-028 requires merge gate before CI gate; both are
// inside handleMergeAndCIGates (position 4), which follows
// handleAutoMergeConvergence (position 3) so that auto-merge items bypass
// settlePRMergeState entirely.
func TestCatchUpPhase1HandlersOrder(t *testing.T) {
	want := []string{
		"engineUnpause",
		"dependencies",
		"reviewGate",
		"autoMergeConvergence",
		"mergeAndCIGates",
	}
	if len(catchUpPhase1Handlers) != len(want) {
		t.Fatalf("catchUpPhase1Handlers has %d entries; want %d", len(catchUpPhase1Handlers), len(want))
	}
	for i, h := range catchUpPhase1Handlers {
		if h.name != want[i] {
			t.Errorf("catchUpPhase1Handlers[%d].name = %q; want %q", i, h.name, want[i])
		}
	}
}

// ---- handleReviewGate dispatch tests ----

// makeReviewGatePctx returns a phase1Ctx configured for review gate handler tests.
// The item has stage:Implement:complete and a single unresolved review thread comment.
func makeReviewGatePctx(board *gh.ProjectBoard, advancedItems map[string]bool) *phase1Ctx {
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"stage:Implement:complete"},
		LinkedPRReviewThreadComments: []gh.Comment{
			{
				ID:             "PRRC_handler_1",
				DatabaseID:     100,
				Author:         "copilot",
				Body:           "Please fix this.",
				ReviewThreadID: "RT_handler_1",
			},
		},
	}
	// Stage without WaitForReviews so checkReviewGate returns (false, false)
	// immediately — the handler then proceeds to buildReviewThreadComments which
	// finds the unresolved thread comment and dispatches a review reinvoke.
	stage := &stages.Stage{Name: "Implement", Order: 1, Prompt: "implement"}
	return &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stage,
		hasComplete:   true,
		advancedItems: advancedItems,
	}
}

// TestHandleReviewGate_WorkerInFlight_SkipsDispatch verifies that when a goroutine
// from a previous poll cycle is still running for an item, handleReviewGate
// claims the item (returns true) but does NOT increment ReviewCycles or dispatch
// a new reinvoke — preventing double-dispatch and spurious cycle limit advances.
func TestHandleReviewGate_WorkerInFlight_SkipsDispatch(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeReviewGatePctx(board, advancedItems)

	// Simulate an in-flight worker from a previous poll cycle.
	eng.store.Apply(itemstate.WorkerEntered{
		Repo: "owner/repo", Number: 10, StageName: "Implement", StartedAt: time.Now(),
	})

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	snap, _ := eng.store.Get("owner/repo", 10)
	if snap.ReviewCycles("Implement") != 0 {
		t.Errorf("ReviewCycles(Implement) = %d; want 0 (guard must prevent dispatch)", snap.ReviewCycles("Implement"))
	}
	if advancedItems["owner/repo#10"] {
		t.Error("advancedItems must not be set when worker-in-flight guard fires")
	}
}

// TestHandleReviewGate_CycleLimit_Pauses verifies that when ReviewCycles reaches
// MaxReviewCycles, handleReviewGate pauses the issue (adds fabrik:paused +
// fabrik:awaiting-input) instead of dispatching another reinvoke.
func TestHandleReviewGate_CycleLimit_Pauses(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 1, nil
		},
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeReviewGatePctx(board, advancedItems)

	// Pre-fill ReviewCycles to the limit.
	for i := 0; i < eng.cfg.MaxReviewCycles; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{
			Repo: "owner/repo", Number: 10, StageName: "Implement",
		})
	}

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	// Pause labels must have been added.
	client.mu.Lock()
	labelNames := make([]string, len(client.addLabelCalls))
	for i, c := range client.addLabelCalls {
		labelNames[i] = c.labelName
	}
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labelNames {
		if l == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Errorf("expected fabrik:paused to be added on cycle limit; labels added: %v", labelNames)
	}
	if advancedItems["owner/repo#10"] {
		t.Error("advancedItems must not be set on cycle limit pause")
	}
}

// TestHandleReviewGate_HappyPath_Dispatches verifies the happy path: no worker
// in-flight, cycle count below limit, unresolved review thread comments present →
// ReviewCycles incremented, advancedItems set, handler returns true.
func TestHandleReviewGate_HappyPath_Dispatches(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeReviewGatePctx(board, advancedItems)

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	// advancedItems is set synchronously before the goroutine runs.
	if !advancedItems["owner/repo#10"] {
		t.Error("advancedItems[owner/repo#10] must be set on successful dispatch")
	}
	// ReviewCycles incremented synchronously.
	snap, _ := eng.store.Get("owner/repo", 10)
	if snap.ReviewCycles("Implement") != 1 {
		t.Errorf("ReviewCycles(Implement) = %d; want 1", snap.ReviewCycles("Implement"))
	}
	eng.wg.Wait()
}

// ---- handleReviewGate #1207 guard-2 disable tests ----

// makeAutoMergeReviewGatePctx returns a phase1Ctx for an item already in the
// GitHub auto-merge convergence flow (fabrik:auto-merge-enabled present) with
// a single unresolved review thread comment on the current head — the shape
// of the #1183/PR#1202 incident this guard closes.
func makeAutoMergeReviewGatePctx(board *gh.ProjectBoard, advancedItems map[string]bool, extraLabels []string, mergeQueueEnabled bool, outdated bool) *phase1Ctx {
	item := gh.ProjectItem{
		Number:                      10,
		Repo:                        "owner/repo",
		Labels:                      append([]string{"stage:Implement:complete", "fabrik:auto-merge-enabled"}, extraLabels...),
		LinkedPRNumber:              55,
		LinkedPRHeadSHA:             "sha1",
		LinkedPRIsMergeQueueEnabled: mergeQueueEnabled,
		LinkedPRReviewThreadComments: []gh.Comment{
			{
				ID:             "PRRC_handler_1",
				DatabaseID:     100,
				Author:         "pruefer",
				Body:           "late finding",
				ReviewThreadID: "RT_handler_1",
				IsOutdated:     outdated,
			},
		},
	}
	stage := &stages.Stage{Name: "Implement", Order: 1, Prompt: "implement"}
	return &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stage,
		hasComplete:   true,
		advancedItems: advancedItems,
	}
}

// TestHandleReviewGate_DisablesNativeAutoMerge_OnUnresolvedCurrentHeadThread
// verifies #1207 guard 2: when an item already carrying
// fabrik:auto-merge-enabled has a fresh unresolved thread on its current
// head, DisablePullRequestAutoMerge fires (not DequeuePullRequest) and
// fabrik:auto-merge-enabled is removed, on the SAME poll that dispatches the
// review reinvoke — not deferred to a separate checkAutoMergeConvergence pass.
func TestHandleReviewGate_DisablesNativeAutoMerge_OnUnresolvedCurrentHeadThread(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeAutoMergeReviewGatePctx(board, advancedItems, nil, false, false)

	got := eng.handleReviewGate(pctx)
	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()

	if len(client.disablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("expected DisablePullRequestAutoMerge called once, got %d", len(client.disablePullRequestAutoMergeCalls))
	}
	if client.disablePullRequestAutoMergeCalls[0].prNumber != 55 {
		t.Errorf("DisablePullRequestAutoMerge called with PR %d, want 55", client.disablePullRequestAutoMergeCalls[0].prNumber)
	}
	if len(client.dequeuePullRequestCalls) != 0 {
		t.Errorf("DequeuePullRequest must not be called on a non-queue repo, got %d call(s)", len(client.dequeuePullRequestCalls))
	}
	foundRemoval := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			foundRemoval = true
		}
	}
	if !foundRemoval {
		t.Error("expected fabrik:auto-merge-enabled to be removed after successful disable")
	}
}

// TestHandleReviewGate_DequeuesOnMergeQueueEnabledRepo verifies the
// queue-aware branch of guard 2 (#1207): when the linked PR is merge-queue
// enabled, DequeuePullRequest fires instead of DisablePullRequestAutoMerge —
// mirroring reenableAutoMergeAfterRebase's existing queue-aware branch.
func TestHandleReviewGate_DequeuesOnMergeQueueEnabledRepo(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeAutoMergeReviewGatePctx(board, advancedItems, nil, true, false)

	got := eng.handleReviewGate(pctx)
	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()

	if len(client.dequeuePullRequestCalls) != 1 {
		t.Fatalf("expected DequeuePullRequest called once, got %d", len(client.dequeuePullRequestCalls))
	}
	if client.dequeuePullRequestCalls[0].prNumber != 55 {
		t.Errorf("DequeuePullRequest called with PR %d, want 55", client.dequeuePullRequestCalls[0].prNumber)
	}
	if len(client.disablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("DisablePullRequestAutoMerge must not be called on a queue-enabled repo, got %d call(s)", len(client.disablePullRequestAutoMergeCalls))
	}
	foundRemoval := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			foundRemoval = true
		}
	}
	if !foundRemoval {
		t.Error("expected fabrik:auto-merge-enabled to be removed after successful dequeue")
	}
}

// TestHandleReviewGate_OutdatedThread_DoesNotDisableAutoMerge verifies the
// stale-SHA scoping half of guard 2 (#1207): a thread GitHub has marked
// isOutdated (superseded by a later push) must not trigger a disable — the
// review-reinvoke dispatch still fires (buildReviewThreadComments is
// unscoped), but no GitHub mutation to disable merge machinery happens.
func TestHandleReviewGate_OutdatedThread_DoesNotDisableAutoMerge(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeAutoMergeReviewGatePctx(board, advancedItems, nil, false, true)

	got := eng.handleReviewGate(pctx)
	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()

	if len(client.disablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("DisablePullRequestAutoMerge must not be called for an outdated-only thread, got %d call(s)", len(client.disablePullRequestAutoMergeCalls))
	}
	if len(client.dequeuePullRequestCalls) != 0 {
		t.Errorf("DequeuePullRequest must not be called for an outdated-only thread, got %d call(s)", len(client.dequeuePullRequestCalls))
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			t.Error("fabrik:auto-merge-enabled must not be removed for an outdated-only thread")
		}
	}
}

// TestHandleReviewGate_DisablesAutoMerge_EvenWhenCycleLimitReached verifies
// that guard 2's disable fires even on the same poll that trips
// MaxReviewCycles and pauses the issue — the disable must not be
// conditioned on the reinvoke actually dispatching, since a paused item is
// exactly the case where GitHub must not be left free to merge while nobody
// is addressing the thread. Also confirms MaxReviewCycles still bounds and
// escalates (#1207's "no new never-advancing state" requirement).
func TestHandleReviewGate_DisablesAutoMerge_EvenWhenCycleLimitReached(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 1, nil
		},
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeAutoMergeReviewGatePctx(board, advancedItems, nil, false, false)

	for i := 0; i < eng.cfg.MaxReviewCycles; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{
			Repo: "owner/repo", Number: 10, StageName: "Implement",
		})
	}

	got := eng.handleReviewGate(pctx)
	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()

	if len(client.disablePullRequestAutoMergeCalls) != 1 {
		t.Errorf("expected DisablePullRequestAutoMerge called once even at cycle limit, got %d", len(client.disablePullRequestAutoMergeCalls))
	}
	client.mu.Lock()
	labelNames := make([]string, len(client.addLabelCalls))
	for i, c := range client.addLabelCalls {
		labelNames[i] = c.labelName
	}
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labelNames {
		if l == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Errorf("expected fabrik:paused to be added on cycle limit (MaxReviewCycles must still escalate); labels added: %v", labelNames)
	}
}

// TestGuard2RoundTrip_DisableThenResolveReenablesAutoMerge verifies the full
// #1207 round trip: guard 2 disables auto-merge and removes
// fabrik:auto-merge-enabled when a fresh unresolved thread appears, and once
// that thread is resolved (ROCKET reaction, the existing review-reinvoke
// dedup signal), the next poll's attemptMergeOnValidate call (poll.go's
// Phase 2 Validate branch) re-enables auto-merge and re-applies a fresh
// fabrik:auto-merge-enabled label — no manual intervention, no stuck state.
func TestGuard2RoundTrip_DisableThenResolveReenablesAutoMerge(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 55, HeadSHA: "sha1"}, nil
		},
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
		{Name: "Validate", Order: 3, Prompt: "validate"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeAutoMergeReviewGatePctx(board, advancedItems, nil, false, false)

	// Step 1: guard 2 fires — disable + label removal.
	if got := eng.handleReviewGate(pctx); !got {
		t.Fatal("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	if len(client.disablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("expected DisablePullRequestAutoMerge called once, got %d", len(client.disablePullRequestAutoMergeCalls))
	}
	foundRemoval := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			foundRemoval = true
		}
	}
	if !foundRemoval {
		t.Fatal("expected fabrik:auto-merge-enabled to be removed after disable")
	}

	// Step 2: the review-reinvoke loop (§2.9) addresses the finding and reacts
	// ROCKET — the existing buildReviewThreadComments dedup signal. The item no
	// longer carries fabrik:auto-merge-enabled (removed in step 1).
	resolvedItem := gh.ProjectItem{
		Number:          10,
		Repo:            "owner/repo",
		Labels:          []string{"stage:Validate:complete"},
		LinkedPRHeadSHA: "sha1",
		LinkedPRReviewThreadComments: []gh.Comment{
			{
				ID: "PRRC_handler_1", DatabaseID: 100, Author: "pruefer", Body: "late finding", ReviewThreadID: "RT_handler_1",
				IsOutdated: false,
				Reactions:  []gh.ReactionGroup{{Content: "ROCKET", Count: 1}},
			},
		},
	}

	// Step 3: poll.go's Phase 2 Validate branch retries attemptMergeOnValidate
	// on every poll while the label is absent. It must now proceed and
	// re-apply a fresh label.
	enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), board, resolvedItem, &stages.Stage{Name: "Validate"})
	if err != nil {
		t.Fatalf("unexpected error on re-enable: %v", err)
	}
	if deferred {
		t.Error("expected deferred=false once the thread is resolved (ROCKET reaction)")
	}
	if !enabled {
		t.Error("expected enabled=true — auto-merge must be re-enabled once the thread resolves")
	}
	if len(client.enablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("expected EnablePullRequestAutoMerge called once on re-enable, got %d", len(client.enablePullRequestAutoMergeCalls))
	}
	if client.enablePullRequestAutoMergeCalls[0].prNumber != 55 {
		t.Errorf("EnablePullRequestAutoMerge called with PR %d, want 55", client.enablePullRequestAutoMergeCalls[0].prNumber)
	}
	foundReapplied := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			foundReapplied = true
		}
	}
	if !foundReapplied {
		t.Error("expected fabrik:auto-merge-enabled to be re-applied after re-enable")
	}
}

// TestHandleReviewGate_Terminated_Claims is the #1223 sibling regression: when
// checkReviewGate's handleBrokenReviewLinkage path pauses the item directly
// (terminated=true), handleReviewGate must claim the item (return true) rather
// than reading the all-false blocked/timedOut tuple as "gate cleared naturally"
// and falling through to buildReviewThreadComments — which would leave the item
// unclaimed and let Phase 2 advance an item that was just paused.
func TestHandleReviewGate_Terminated_Claims(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: boolPtr(true)},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 0, // no linkage via closingIssuesReferences — broken linkage
	}
	advancedItems := make(map[string]bool)
	pctx := &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stgs[0],
		hasComplete:   true,
		advancedItems: advancedItems,
	}

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed) on broken-linkage terminated pause, got false")
	}
	if advancedItems["owner/repo#10"] {
		t.Error("advancedItems must not be set for a terminated (paused) item")
	}
	client.mu.Lock()
	labelNames := make([]string, len(client.addLabelCalls))
	for i, c := range client.addLabelCalls {
		labelNames[i] = c.labelName
	}
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labelNames {
		if l == "fabrik:paused" {
			hasPaused = true
		}
		if l == "fabrik:awaiting-review" {
			t.Error("fabrik:awaiting-review must not be applied for a terminated broken-linkage pause")
		}
	}
	if !hasPaused {
		t.Errorf("expected fabrik:paused to be added for broken linkage; labels added: %v", labelNames)
	}
}

// ---- handleReviewGate Finding 1 (#1375) reorder tests ----
//
// These pin the crux of Finding 1: a review-reinvoke dispatches whenever
// buildReviewFeedbackComments has something actionable, regardless of
// checkReviewGate's blocked/timedOut return — not only when the gate has
// cleared naturally. Before the fix, an authoritative CHANGES_REQUESTED verdict
// kept checkReviewGate returning blocked=true indefinitely, which short-circuited
// handleReviewGate before the reinvoke path was ever reached.

// TestHandleReviewGate_AuthoritativeBlocked_ReinvokesOnActionableBody verifies
// that a CHANGES_REQUESTED review's body dispatches a reinvoke even though
// checkReviewGate reports blocked=true (review_authority: authoritative, no
// approving verdict). This is the exact e2e harness shape (AC1/AC6): a review
// body with zero inline thread comments.
func TestHandleReviewGate_AuthoritativeBlocked_ReinvokesOnActionableBody(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "CHANGES_REQUESTED", nil
		},
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"stage:Implement:complete"},
		LinkedPRNumber: 55,
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED", Body: "please fix the error handling", DatabaseID: 900},
		},
	}
	advancedItems := make(map[string]bool)
	pctx := &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stgs[0],
		hasComplete:   true,
		advancedItems: advancedItems,
	}

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	if !advancedItems["owner/repo#10"] {
		t.Error("expected advancedItems[owner/repo#10] set — the reinvoke must dispatch despite blocked=true")
	}
	snap, _ := eng.store.Get("owner/repo", 10)
	if snap.ReviewCycles("Implement") != 1 {
		t.Errorf("ReviewCycles(Implement) = %d; want 1 (reinvoke must have dispatched)", snap.ReviewCycles("Implement"))
	}
	if !snap.CooldownAt("review-blocked").IsZero() {
		t.Error("expected NO review-blocked cooldown recorded — the reinvoke path must take priority over the blocked-cooldown tail")
	}
	eng.wg.Wait()
}

// TestHandleReviewGate_TimedOutButActionable_DispatchesReinvokeNotPause pins a
// Pruefer review finding (#1375, PR #1376): checkReviewGate can return
// timedOut=true — not just blocked=true — while syntheticComments is
// simultaneously non-empty. This happens whenever fabrik:awaiting-review has
// already been applied for longer than ReviewWaitTimeout by the time an
// authoritative-blocking review is first seen unprocessed (e.g. after an
// engine restart that lost the in-memory label-applied-at/CommentProcessed
// caches, forcing a live re-fetch of the real, already-old GitHub label
// timestamp). checkAwaitingReviewTimeout's mixed/pure-human branch — which
// authorityReason != "" always routes into, since Finding 2's allBots fix
// forces allBots=false whenever authorityReason is set — has, by the time
// checkReviewGate returns, already removed fabrik:awaiting-review as a side
// effect of the (false, true, true) it's about to return.
//
// handleReviewGate must still dispatch the reinvoke rather than pausing:
// syntheticComments is computed and consumed before blocked/timedOut are ever
// read, so the already-applied label removal is not compounded by also
// pausing the issue. fabrik:paused/fabrik:awaiting-input must NOT be applied
// — pauseForReviewTimeout must never run on this path. The removed
// fabrik:awaiting-review label self-heals on the next poll: with the label
// gone, checkAwaitingReviewTimeout's guard loop finds no match and returns
// done=false immediately, so checkReviewGate re-applies the label fresh
// (resetting the timeout clock) if the gate is still blocked once the
// reinvoke's dedup has caught up — verified separately by
// TestHandleReviewGate_AuthoritativeBlocked_AlreadyProcessed_FallsThroughToCooldown.
func TestHandleReviewGate_TimedOutButActionable_DispatchesReinvokeNotPause(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "CHANGES_REQUESTED", nil
		},
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	// fabrik:awaiting-review was applied well over ReviewWaitTimeout ago —
	// checkAwaitingReviewTimeout's 1x-elapsed branch fires on this call.
	awaitingApplied := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"stage:Implement:complete", "fabrik:awaiting-review"},
		LinkedPRNumber: 55,
		// alice already reviewed — GitHub's reviewRequests only ever lists
		// *pending* requests, so a submitted review means alice no longer
		// appears in LinkedPRReviewRequests. This is the len(outstanding)==0
		// && hasReviews branch, which is what produces a non-empty
		// authorityReason (and, in turn, allBots=false) in the first place.
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED", Body: "please fix the error handling", DatabaseID: 900},
		},
	}
	advancedItems := make(map[string]bool)
	pctx := &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stgs[0],
		hasComplete:   true,
		advancedItems: advancedItems,
	}

	// Sanity check: checkReviewGate itself must actually report timedOut=true
	// for this item/config — otherwise this test would only be re-proving
	// TestHandleReviewGate_AuthoritativeBlocked_ReinvokesOnActionableBody's
	// blocked=true case under a different name.
	blocked, timedOut, terminated, _ := eng.checkReviewGate(board, item, stgs[0])
	if blocked || !timedOut || terminated {
		t.Fatalf("checkReviewGate(...) = (blocked=%v, timedOut=%v, terminated=%v); want (false, true, false) for this test to exercise the intended interaction", blocked, timedOut, terminated)
	}

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	if !advancedItems["owner/repo#10"] {
		t.Error("expected advancedItems[owner/repo#10] set — the reinvoke must dispatch despite timedOut=true")
	}
	snap, _ := eng.store.Get("owner/repo", 10)
	if snap.ReviewCycles("Implement") != 1 {
		t.Errorf("ReviewCycles(Implement) = %d; want 1 (reinvoke must have dispatched)", snap.ReviewCycles("Implement"))
	}
	// Wait for the async reinvoke goroutine to finish before reading
	// client.addLabelCalls — it's mutated by the mock's AddLabelToIssue from
	// that goroutine, so reading it beforehand races with that write (as the
	// sibling TestHandleReviewGate_NonDefaultBase_AuthoritativeBlocked_SingleFetchPRReviewsCall's
	// wg.Wait()-before-read pattern already establishes for its own counter).
	eng.wg.Wait()
	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:paused" || call.labelName == "fabrik:awaiting-input" {
			t.Errorf("pauseForReviewTimeout must not have run on this path; unexpected label added: %q", call.labelName)
		}
	}
}

// TestHandleReviewGate_NonDefaultBase_AuthoritativeBlocked_SingleFetchPRReviewsCall
// is a regression test for a Pruefer review finding (#1375, PR #1376): on a
// base:<branch> item, checkReviewGate already issues a live FetchPRReviews
// REST call internally to compute blocked/timedOut, and handleReviewGate must
// reuse that same result (via checkReviewGate's resolvedReviews return value)
// rather than calling resolveReviewsForFeedback, which would silently repeat
// the identical call in the same synchronous chain — for an answer that
// cannot have changed, since nothing async runs in between.
//
// The expected total is 2, not 1: dispatchReviewReinvoke's build() runs
// inside the async reinvoke goroutine (after an unbounded semaphore wait) and
// deliberately re-resolves reviews fresh rather than reusing the synchronous
// chain's result, since real time may have passed by then — a second,
// intentional call, documented on buildReviewBodyComments/build() above. This
// test pins the fix's actual effect: cutting the synchronous chain's own
// redundant call (checkReviewGate's internal fetch duplicated by
// resolveReviewsForFeedback), from 3 total calls per dispatch down to 2.
func TestHandleReviewGate_NonDefaultBase_AuthoritativeBlocked_SingleFetchPRReviewsCall(t *testing.T) {
	var mu sync.Mutex
	var fetchPRReviewsCalls int
	client := &mockGitHubClient{
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			mu.Lock()
			fetchPRReviewsCalls++
			mu.Unlock()
			if prNumber != 55 {
				t.Errorf("expected FetchPRReviews called with resolved PR #55, got #%d", prNumber)
			}
			return []gh.PRReview{
				{Author: "alice", State: "CHANGES_REQUESTED", Body: "please fix the error handling", DatabaseID: 900},
			}, nil
		},
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "CHANGES_REQUESTED", nil
		},
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"stage:Implement:complete", "base:develop"},
		LinkedPRNumber: 55,
		// LinkedPRReviews deliberately empty — closedByPullRequestsReferences
		// (and everything nested inside it) is structurally empty for a
		// base:<branch> item; the REST fallback inside checkReviewGate is what
		// must populate reviews instead. See buildReviewBodyComments' doc
		// comment for the same base:<branch> gap.
	}
	advancedItems := make(map[string]bool)
	pctx := &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stgs[0],
		hasComplete:   true,
		advancedItems: advancedItems,
	}

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	if !advancedItems["owner/repo#10"] {
		t.Error("expected advancedItems[owner/repo#10] set — the reinvoke must dispatch despite blocked=true")
	}
	snap, _ := eng.store.Get("owner/repo", 10)
	if snap.ReviewCycles("Implement") != 1 {
		t.Errorf("ReviewCycles(Implement) = %d; want 1 (reinvoke must have dispatched)", snap.ReviewCycles("Implement"))
	}
	// Wait for the async reinvoke goroutine's own build()-time fetch to
	// complete (establishes happens-before via sync.WaitGroup) before reading
	// the counter — reading beforehand raced with that goroutine's write.
	eng.wg.Wait()
	mu.Lock()
	got2 := fetchPRReviewsCalls
	mu.Unlock()
	if got2 != 2 {
		t.Errorf("FetchPRReviews called %d times; want 2 (1 synchronous in handleReviewGate's own chain + 1 from build()'s intentional fresh re-resolve) — checkReviewGate's resolved reviews must be reused by the synchronous chain, not re-fetched a second time there", got2)
	}
}

// TestHandleReviewGate_AuthoritativeBlocked_AlreadyProcessed_FallsThroughToCooldown
// verifies R7's dedup bound: once the same review body has already been
// recorded via CommentProcessed (a prior poll's reinvoke already addressed
// it) and no new thread comments exist, handleReviewGate must fall through to
// the pre-existing blocked-cooldown tail unchanged — not dispatch again for
// the same review every poll (the #1083 runaway-reinvoke shape).
func TestHandleReviewGate_AuthoritativeBlocked_AlreadyProcessed_FallsThroughToCooldown(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "CHANGES_REQUESTED", nil
		},
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"stage:Implement:complete"},
		LinkedPRNumber: 55,
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED", Body: "please fix the error handling", DatabaseID: 900},
		},
	}
	// Simulate the review body already having been addressed by a prior reinvoke.
	eng.store.Apply(itemstate.CommentProcessed{Repo: "owner/repo", Number: 10, CommentID: "review-body:900", At: time.Now()})

	advancedItems := make(map[string]bool)
	pctx := &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stgs[0],
		hasComplete:   true,
		advancedItems: advancedItems,
	}

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed via blocked-cooldown fallback), got false")
	}
	if advancedItems["owner/repo#10"] {
		t.Error("expected NO reinvoke dispatch — the review body was already processed")
	}
	snap, _ := eng.store.Get("owner/repo", 10)
	if snap.ReviewCycles("Implement") != 0 {
		t.Errorf("ReviewCycles(Implement) = %d; want 0 (no dispatch should have occurred)", snap.ReviewCycles("Implement"))
	}
	if snap.CooldownAt("review-blocked").IsZero() {
		t.Error("expected review-blocked cooldown recorded — falls through to the pre-existing blocked tail")
	}
}

// TestHandleReviewGate_AuthoritativeBlocked_CycleLimitReached_Pauses is AC4's
// unit-level analog: a fresh, distinct, still-actionable authoritative
// CHANGES_REQUESTED review at MaxReviewCycles routes to
// pauseForReviewCycleLimit (not an unbounded reinvoke loop) even though the
// gate is still blocked, not cleared.
func TestHandleReviewGate_AuthoritativeBlocked_CycleLimitReached_Pauses(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "CHANGES_REQUESTED", nil
		},
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"stage:Implement:complete"},
		LinkedPRNumber: 55,
		LinkedPRReviews: []gh.PRReview{
			// A fresh, distinct review (new DatabaseID) — not yet processed —
			// so buildReviewFeedbackComments is non-empty even at the cycle limit.
			{Author: "alice", State: "CHANGES_REQUESTED", Body: "please fix this too", DatabaseID: 901},
		},
	}
	for i := 0; i < eng.cfg.MaxReviewCycles; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{
			Repo: "owner/repo", Number: 10, StageName: "Implement",
		})
	}

	advancedItems := make(map[string]bool)
	pctx := &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stgs[0],
		hasComplete:   true,
		advancedItems: advancedItems,
	}

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	if advancedItems["owner/repo#10"] {
		t.Error("advancedItems must not be set on cycle limit pause")
	}
	client.mu.Lock()
	labelNames := make([]string, len(client.addLabelCalls))
	for i, c := range client.addLabelCalls {
		labelNames[i] = c.labelName
	}
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labelNames {
		if l == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Errorf("expected fabrik:paused (cycle limit reached) even though the gate is still authoritative-blocked; labels added: %v", labelNames)
	}
}

// TestHandleReviewGate_AuthoritativeBlocked_AlreadyProcessed_AtCycleLimit_PausesForCycleLimit
// is A3 (issue #1518): the exact shape that was unreachable before this fix —
// the currently-outstanding review body has already been deduped as processed
// (syntheticComments is empty on this poll, exactly as in the sibling
// ..._FallsThroughToCooldown test above) but the underlying verdict is still
// blocking and ReviewCycles is already at MaxReviewCycles. This must
// terminate in pauseForReviewCycleLimit, not fall through to the
// cooldown/timeout tail the cycleCount==0 sibling correctly uses. Reverting
// the new terminal check added to handleReviewGate's fallback tail (R1/R2)
// turns this test red — it regresses to asserting the old
// CooldownAt("review-blocked") behavior instead, identically to the sibling
// test at cycleCount==0.
func TestHandleReviewGate_AuthoritativeBlocked_AlreadyProcessed_AtCycleLimit_PausesForCycleLimit(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "CHANGES_REQUESTED", nil
		},
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"stage:Implement:complete"},
		LinkedPRNumber: 55,
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED", Body: "please fix the error handling", DatabaseID: 900},
		},
	}
	// Simulate the review body already having been addressed by a prior reinvoke.
	eng.store.Apply(itemstate.CommentProcessed{Repo: "owner/repo", Number: 10, CommentID: "review-body:900", At: time.Now()})
	// ...and that the loop already spent every cycle trying to converge.
	for i := 0; i < eng.cfg.MaxReviewCycles; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: 10, StageName: "Implement"})
	}

	advancedItems := make(map[string]bool)
	pctx := &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stgs[0],
		hasComplete:   true,
		advancedItems: advancedItems,
	}

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	if advancedItems["owner/repo#10"] {
		t.Error("expected NO reinvoke dispatch — the review body was already processed")
	}
	eng.wg.Wait()
	client.mu.Lock()
	labelNames := make([]string, len(client.addLabelCalls))
	for i, c := range client.addLabelCalls {
		labelNames[i] = c.labelName
	}
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labelNames {
		if l == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Errorf("expected fabrik:paused via pauseForReviewCycleLimit even though syntheticComments was empty this poll; labels added: %v", labelNames)
	}
}

// TestHandleReviewGate_TimedOut_NoReviewsEver_PausesForTimeout is A4 (issue
// #1518): a genuine "nobody has ever responded" block — a human reviewer was
// requested, ReviewWaitTimeout has elapsed, and nothing has ever been
// submitted (so ReviewCycles/ReviewBlockedCycles are both structurally 0 — a
// reinvoke can only ever fire in response to actual review content) — must
// still terminate in pauseForReviewTimeout, not the new cycle-limit check
// added for R1/R2. Explicit non-regression pin for R4: the new terminal
// check in handleReviewGate's fallback tail is gated on
// max(ReviewCycles, ReviewBlockedCycles) >= MaxReviewCycles, which a
// structurally-zero pair can never satisfy.
func TestHandleReviewGate_TimedOut_NoReviewsEver_PausesForTimeout(t *testing.T) {
	client := &mockGitHubClient{}
	awaitingApplied := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: boolPtr(true)},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 3
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"stage:Implement:complete", "fabrik:awaiting-review"},
		LinkedPRNumber: 55,
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "bob", IsBot: false},
		},
		// No LinkedPRReviews at all — nobody has ever reviewed.
	}

	// Sanity check, mirroring TestHandleReviewGate_TimedOutButActionable_DispatchesReinvokeNotPause:
	// confirm checkReviewGate reports the exact (blocked, timedOut) shape this
	// test means to exercise before asserting on handleReviewGate's outcome.
	blocked, timedOut, terminated, _ := eng.checkReviewGate(board, item, stgs[0])
	if blocked || !timedOut || terminated {
		t.Fatalf("checkReviewGate(...) = (blocked=%v, timedOut=%v, terminated=%v); want (false, true, false) for this test to exercise the intended interaction", blocked, timedOut, terminated)
	}

	advancedItems := make(map[string]bool)
	pctx := &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stgs[0],
		hasComplete:   true,
		advancedItems: advancedItems,
	}

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	if advancedItems["owner/repo#10"] {
		t.Error("expected NO reinvoke dispatch — nothing actionable has ever been submitted")
	}
	snap, _ := eng.store.Get("owner/repo", 10)
	if got := snap.ReviewCycles("Implement"); got != 0 {
		t.Errorf("ReviewCycles(Implement) = %d; want 0", got)
	}
	if got := snap.ReviewBlockedCycles("Implement"); got != 0 {
		t.Errorf("ReviewBlockedCycles(Implement) = %d; want 0", got)
	}
	client.mu.Lock()
	labelNames := make([]string, len(client.addLabelCalls))
	for i, c := range client.addLabelCalls {
		labelNames[i] = c.labelName
	}
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labelNames {
		if l == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Errorf("expected fabrik:paused via pauseForReviewTimeout; labels added: %v", labelNames)
	}
}

// ---- handleMergeAndCIGates rebase reinvoke dispatch tests ----

// makeMergeGatePctx returns a phase1Ctx for merge-gate/CI-gate handler tests.
// The stage has WaitForCI enabled.
func makeMergeGatePctx(board *gh.ProjectBoard, advancedItems map[string]bool) *phase1Ctx {
	waitTrue := true
	item := gh.ProjectItem{
		Number: 20,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-ci"},
	}
	stage := &stages.Stage{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue}
	return &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stage,
		hasComplete:   false,
		advancedItems: advancedItems,
	}
}

// conflictingSettleClient returns a mockGitHubClient whose FetchLinkedPR +
// FetchPRMergeableFields produce a PRMergeConflicting result from settlePRMergeState.
func conflictingSettleClient() *mockGitHubClient {
	return &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "deadbeef", State: "open", Merged: false}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			f := false
			return &f, "dirty", nil
		},
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
	}
}

// TestHandleRebaseReinvoke_WorkerInFlight_SkipsDispatch verifies the worker-in-flight
// guard for the rebase reinvoke path inside handleMergeAndCIGates.
func TestHandleRebaseReinvoke_WorkerInFlight_SkipsDispatch(t *testing.T) {
	client := conflictingSettleClient()
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxRebaseCycles = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	// Simulate an in-flight worker from a previous poll cycle.
	eng.store.Apply(itemstate.WorkerEntered{
		Repo: "owner/repo", Number: 20, StageName: "Implement", StartedAt: time.Now(),
	})

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	snap, _ := eng.store.Get("owner/repo", 20)
	if snap.RebaseCycles("Implement") != 0 {
		t.Errorf("RebaseCycles(Implement) = %d; want 0 (guard must prevent dispatch)", snap.RebaseCycles("Implement"))
	}
	if advancedItems["owner/repo#20"] {
		t.Error("advancedItems must not be set when worker-in-flight guard fires")
	}
}

// TestHandleRebaseReinvoke_CycleLimit_Pauses verifies that when RebaseCycles
// reaches MaxRebaseCycles, handleMergeAndCIGates pauses the issue.
func TestHandleRebaseReinvoke_CycleLimit_Pauses(t *testing.T) {
	client := conflictingSettleClient()
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxRebaseCycles = 2

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	// Pre-fill RebaseCycles to the limit.
	for i := 0; i < eng.cfg.MaxRebaseCycles; i++ {
		eng.store.Apply(itemstate.RebaseCycleIncremented{
			Repo: "owner/repo", Number: 20, StageName: "Implement",
		})
	}

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	client.mu.Lock()
	labelNames := make([]string, len(client.addLabelCalls))
	for i, c := range client.addLabelCalls {
		labelNames[i] = c.labelName
	}
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labelNames {
		if l == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Errorf("expected fabrik:paused on rebase cycle limit; labels added: %v", labelNames)
	}
	if advancedItems["owner/repo#20"] {
		t.Error("advancedItems must not be set on cycle limit pause")
	}
}

// TestHandleRebaseReinvoke_HappyPath_Dispatches verifies the happy path for rebase
// reinvoke: PRMergeConflicting, no worker in-flight, cycle below limit →
// RebaseCycles incremented, advancedItems set, handler returns true.
func TestHandleRebaseReinvoke_HappyPath_Dispatches(t *testing.T) {
	client := conflictingSettleClient()
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxRebaseCycles = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed), got false")
	}
	if !advancedItems["owner/repo#20"] {
		t.Error("advancedItems[owner/repo#20] must be set on successful rebase dispatch")
	}
	snap, _ := eng.store.Get("owner/repo", 20)
	if snap.RebaseCycles("Implement") != 1 {
		t.Errorf("RebaseCycles(Implement) = %d; want 1", snap.RebaseCycles("Implement"))
	}
	eng.wg.Wait()
}

// ---- handleMergeAndCIGates CI-fix reinvoke dispatch tests ----

// ciFailureSettleClient returns a mockGitHubClient whose settle sequence produces
// PRMergeBlocked (a completed CI failure check run, mergeable PR).
func ciFailureSettleClient() *mockGitHubClient {
	return &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 43, HeadSHA: "cafebabe", State: "open", Merged: false}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "test", Status: "completed", Conclusion: "failure"},
			}, nil
		},
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			// Return the current time so elapsed ≈ 0 — well within the default
			// 30-minute CIWaitTimeout, preventing the timeout path from firing.
			return time.Now(), nil
		},
		addLabelToIssueFn:    func(_, _ string, _ int, _ string) error { return nil },
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
}

// TestHandleCIFixReinvoke_WorkerInFlight_SkipsDispatch verifies the worker-in-flight
// guard for the CI-fix reinvoke path inside handleMergeAndCIGates.
func TestHandleCIFixReinvoke_WorkerInFlight_SkipsDispatch(t *testing.T) {
	client := ciFailureSettleClient()
	waitTrue := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxCiFixCycles = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	// Simulate an in-flight worker.
	eng.store.Apply(itemstate.WorkerEntered{
		Repo: "owner/repo", Number: 20, StageName: "Implement", StartedAt: time.Now(),
	})

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	snap, _ := eng.store.Get("owner/repo", 20)
	if snap.CIFixCycles("Implement") != 0 {
		t.Errorf("CIFixCycles(Implement) = %d; want 0 (guard must prevent dispatch)", snap.CIFixCycles("Implement"))
	}
	if advancedItems["owner/repo#20"] {
		t.Error("advancedItems must not be set when worker-in-flight guard fires")
	}
}

// TestHandleCIFixReinvoke_CycleLimit_Pauses verifies that when CIFixCycles
// reaches MaxCiFixCycles, handleMergeAndCIGates pauses the issue.
func TestHandleCIFixReinvoke_CycleLimit_Pauses(t *testing.T) {
	client := ciFailureSettleClient()
	waitTrue := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxCiFixCycles = 2

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	// Pre-fill CIFixCycles to the limit.
	for i := 0; i < eng.cfg.MaxCiFixCycles; i++ {
		eng.store.Apply(itemstate.CIFixCycleIncremented{
			Repo: "owner/repo", Number: 20, StageName: "Implement",
		})
	}

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	client.mu.Lock()
	labelNames := make([]string, len(client.addLabelCalls))
	for i, c := range client.addLabelCalls {
		labelNames[i] = c.labelName
	}
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labelNames {
		if l == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Errorf("expected fabrik:paused on CI-fix cycle limit; labels added: %v", labelNames)
	}
	if advancedItems["owner/repo#20"] {
		t.Error("advancedItems must not be set on cycle limit pause")
	}
}

// TestHandleCIFixReinvoke_NoOpDebounce_SkipsDispatch is the #958 leg 2
// regression: when a prior CI-fix reinvoke already recorded a no-op (no new
// commit pushed) for the exact current head SHA, handleMergeAndCIGates must
// not dispatch another reinvoke or increment CIFixCycles — repeating the
// same no-op burns cycle budget for nothing.
func TestHandleCIFixReinvoke_NoOpDebounce_SkipsDispatch(t *testing.T) {
	client := ciFailureSettleClient()
	waitTrue := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxCiFixCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	// A prior reinvoke already observed no new commit for this exact head SHA
	// (ciFailureSettleClient's PR HeadSHA is "cafebabe").
	eng.store.Apply(itemstate.CIFixNoOpRecorded{Repo: "owner/repo", Number: 20, SHA: "cafebabe"})

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	snap, _ := eng.store.Get("owner/repo", 20)
	if snap.CIFixCycles("Implement") != 0 {
		t.Errorf("CIFixCycles(Implement) = %d; want 0 (no-op debounce must prevent dispatch)", snap.CIFixCycles("Implement"))
	}
	if advancedItems["owner/repo#20"] {
		t.Error("advancedItems must not be set when the no-op debounce guard fires")
	}
}

// TestHandleCIFixReinvoke_HappyPath_Dispatches verifies the happy path for CI-fix
// reinvoke: PRMergeBlocked (CI failed), no worker in-flight, cycle below limit →
// CIFixCycles incremented, advancedItems set, handler returns true.
func TestHandleCIFixReinvoke_HappyPath_Dispatches(t *testing.T) {
	client := ciFailureSettleClient()
	waitTrue := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxCiFixCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed), got false")
	}
	if !advancedItems["owner/repo#20"] {
		t.Error("advancedItems[owner/repo#20] must be set on successful CI-fix dispatch")
	}
	snap, _ := eng.store.Get("owner/repo", 20)
	if snap.CIFixCycles("Implement") != 1 {
		t.Errorf("CIFixCycles(Implement) = %d; want 1", snap.CIFixCycles("Implement"))
	}
	eng.wg.Wait()
}

// TestHandleMergeAndCIGates_CITerminated_Claims is the #1223 regression: when
// checkCIGate's PRMergeTerminal/closed-without-merge branch pauses the item
// directly (terminated=true), handleMergeAndCIGates must claim the item (return
// true) rather than reading the all-false blocked/ciFailure/ciTimedOut tuple as
// "gate cleared" — a false read would let Phase 2 advance an item that was just
// paused in this same poll pass.
func TestHandleMergeAndCIGates_CITerminated_Claims(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "deadbeef", State: "closed", Merged: false}, nil
		},
		fetchPRMergedFn: func(owner, repo string, prNumber int) (bool, error) { return false, nil },
		addCommentFn:    func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
	}
	waitTrue := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed) on CI-gate terminated pause, got false")
	}
	if advancedItems["owner/repo#20"] {
		t.Error("advancedItems must not be set for a terminated (paused) item")
	}
	client.mu.Lock()
	labelNames := make([]string, len(client.addLabelCalls))
	for i, c := range client.addLabelCalls {
		labelNames[i] = c.labelName
	}
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labelNames {
		if l == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Errorf("expected fabrik:paused to be added for closed-not-merged PR; labels added: %v", labelNames)
	}
}

// TestHandleMergeAndCIGates_MergeBlocked_LogsClaim is a #1303 regression:
// handleMergeAndCIGates's `if mergeBlocked { return true }` branch used to
// claim the item with no log line at all. Combined with
// checkMergeabilityGate's own previously-silent PRMergeUnsettled/PRMergeQueued
// branches, a stuck classification could claim an item every poll with zero
// trace in the logs — exactly the shape that left this issue's field incident
// undiagnosable for over an hour. This asserts the claim now logs under the
// "ci-gate" tag, naming checkCIGate as unreached.
func TestHandleMergeAndCIGates_MergeBlocked_LogsClaim(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "deadbeef", State: "open", Merged: false}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			// mergeable=nil ("GitHub still computing") pins settle to PRMergeUnsettled,
			// so checkMergeabilityGate claims (mergeBlocked=true) and checkCIGate is
			// never reached.
			return nil, "", nil
		},
	}
	waitTrue := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	events := make(chan tui.Event, 16)
	eng.events = events

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	pctx := makeMergeGatePctx(board, make(map[string]bool))

	if got := eng.handleMergeAndCIGates(pctx); !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed) on merge-gate block, got false")
	}
	if pctx.reachedCIGate {
		t.Error("expected reachedCIGate to stay false — checkCIGate must not run when the merge gate claims first")
	}

	close(events)
	var found bool
	for ev := range events {
		if le, ok := ev.(tui.LogEvent); ok && le.Tag == "ci-gate" && strings.Contains(le.Message, "merge gate blocked") && strings.Contains(le.Message, "checkCIGate not reached") {
			found = true
		}
	}
	if !found {
		t.Error("expected a ci-gate log line naming the merge-gate-blocked claim and that checkCIGate was not reached")
	}
}

// TestDispatchCIFixReinvoke_BotNoticeInReviewThread_ExcludedFromInvocation
// verifies the #1221 chokepoint end to end via dispatchCIFixReinvoke: a bot
// service notice merged in from item.LinkedPRReviewThreadComments is excluded
// by processComments's chokepoint filter, while the synthetic
// "ci-fix-synthetic" comment (author "fabrik", never bot-classified) survives
// and reaches Claude.
func TestDispatchCIFixReinvoke_BotNoticeInReviewThread_ExcludedFromInvocation(t *testing.T) {
	skipIfNoGit(t)

	var seenComments []gh.Comment
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			seenComments = comments
			return "fixed the CI failure", false, TokenUsage{}, nil
		},
	}
	waitTrue := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 60,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-ci"},
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "RT_notice", DatabaseID: 950, ReviewThreadID: "thread1", Author: "gemini-code-assist[bot]", Body: "You have reached your daily quota limit."},
		},
	}
	stage := stgs[0]
	settle := PRSettleResult{
		Status:    PRMergeBlocked,
		CheckRuns: []gh.CheckRun{{Name: "test", Status: "completed", Conclusion: "failure"}},
	}

	eng.dispatchCIFixReinvoke(context.Background(), board, item, stage, settle)
	eng.wg.Wait()

	if len(claude.forCommentsCalls) != 1 {
		t.Fatalf("expected exactly 1 InvokeForComments call, got %d", len(claude.forCommentsCalls))
	}
	if len(seenComments) != 1 {
		t.Fatalf("expected exactly 1 comment reaching InvokeForComments (synthetic only), got %d: %v", len(seenComments), seenComments)
	}
	if seenComments[0].ID != "ci-fix-synthetic" {
		t.Errorf("expected synthetic ci-fix comment to survive, got %q", seenComments[0].ID)
	}
}

// TestDispatchRebaseReinvoke_BotNoticeInReviewThread_ExcludedFromInvocation
// mirrors the CI-fix test above for dispatchRebaseReinvoke: a bot service
// notice merged in from item.LinkedPRReviewThreadComments is excluded, while
// the synthetic "rebase-synthetic" comment survives and reaches Claude.
func TestDispatchRebaseReinvoke_BotNoticeInReviewThread_ExcludedFromInvocation(t *testing.T) {
	skipIfNoGit(t)

	var seenComments []gh.Comment
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			seenComments = comments
			return "rebased", false, TokenUsage{}, nil
		},
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 61,
		Repo:   "owner/repo",
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "RT_notice", DatabaseID: 951, ReviewThreadID: "thread1", Author: "coderabbitai[bot]", Body: "auto-generated reply by CodeRabbit"},
		},
	}
	stage := stgs[0]

	eng.dispatchRebaseReinvoke(context.Background(), board, item, stage)
	eng.wg.Wait()

	if len(claude.forCommentsCalls) != 1 {
		t.Fatalf("expected exactly 1 InvokeForComments call, got %d", len(claude.forCommentsCalls))
	}
	if len(seenComments) != 1 {
		t.Fatalf("expected exactly 1 comment reaching InvokeForComments (synthetic only), got %d: %v", len(seenComments), seenComments)
	}
	if seenComments[0].ID != "rebase-synthetic" {
		t.Errorf("expected synthetic rebase comment to survive, got %q", seenComments[0].ID)
	}
}

// ---- handleEngineUnpause tests (#1460 R2) ----

// runPhase1Chain drives pctx through the full ordered catchUpPhase1Handlers
// list, mirroring the exact loop poll.go's main catch-up loop and
// settleAwaitingCIScan both run (ci_settle.go). AC1/AC2 tests for the four
// confirmed cycle-limit sites must go through this — not call
// clearFailedStage directly — since the defect (and the fix) lives in
// whether the real chain reaches and resets the stuck counter, not in
// clearFailedStage's own correctness (already covered by
// TestClearFailedStage_ReviewCycleCount_ResetsOnlyCurrentStage).
func runPhase1Chain(eng *Engine, pctx *phase1Ctx) (claimed bool) {
	for _, h := range catchUpPhase1Handlers {
		if h.run(eng, pctx) {
			return true
		}
	}
	return false
}

// TestHandleEngineUnpause_PausedByEngine_ResetsAndReturnsFalse verifies the
// core reset behavior: when the store carries a stale EnginePaused record for
// the stage (the item reached this handler chain — meaning fabrik:paused is
// currently absent per poll.go's/ci_settle.go's admission filters — while
// still marked paused-by-engine from before), handleEngineUnpause fires
// clearFailedStage (zeroing ReviewCycles/CIFixCycles/RebaseCycles/
// EnqueueCycles and clearing PausedByEngine) and always returns false so the
// rest of the chain still runs this same pass.
func TestHandleEngineUnpause_PausedByEngine_ResetsAndReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := []*stages.Stage{{Name: "Implement", Order: 1, Prompt: "implement"}}
	eng := testEngineWithStages(t, client, stgs)

	item := gh.ProjectItem{Number: 20, Repo: "owner/repo", Labels: []string{"stage:Implement:complete"}}
	stage := stgs[0]
	pctx := &phase1Ctx{ctx: context.Background(), board: &gh.ProjectBoard{}, item: item, stage: stage, hasComplete: true, advancedItems: make(map[string]bool)}

	eng.store.Apply(itemstate.EnginePaused{Repo: "owner/repo", Number: 20, StageName: "Implement"})
	for i := 0; i < 3; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: 20, StageName: "Implement"})
		eng.store.Apply(itemstate.CIFixCycleIncremented{Repo: "owner/repo", Number: 20, StageName: "Implement"})
		eng.store.Apply(itemstate.RebaseCycleIncremented{Repo: "owner/repo", Number: 20, StageName: "Implement"})
		eng.store.Apply(itemstate.EnqueueCycleIncremented{Repo: "owner/repo", Number: 20, StageName: "Implement"})
	}

	got := eng.handleEngineUnpause(pctx)

	if got {
		t.Error("handleEngineUnpause must always return false — it resets state but never claims the item")
	}
	snap, _ := eng.store.Get("owner/repo", 20)
	if snap.PausedByEngine("Implement") {
		t.Error("PausedByEngine(Implement) must be cleared after handleEngineUnpause")
	}
	if got := snap.ReviewCycles("Implement"); got != 0 {
		t.Errorf("ReviewCycles(Implement) = %d; want 0", got)
	}
	if got := snap.CIFixCycles("Implement"); got != 0 {
		t.Errorf("CIFixCycles(Implement) = %d; want 0", got)
	}
	if got := snap.RebaseCycles("Implement"); got != 0 {
		t.Errorf("RebaseCycles(Implement) = %d; want 0", got)
	}
	if got := snap.EnqueueCycles("Implement"); got != 0 {
		t.Errorf("EnqueueCycles(Implement) = %d; want 0", got)
	}
}

// TestHandleEngineUnpause_NotPaused_NoOp verifies the common steady-state
// case (no store entry, or PausedByEngine false) is a true no-op: no store
// mutation, no clearFailedStage side effects, always returns false.
func TestHandleEngineUnpause_NotPaused_NoOp(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := []*stages.Stage{{Name: "Implement", Order: 1, Prompt: "implement"}}
	eng := testEngineWithStages(t, client, stgs)

	item := gh.ProjectItem{Number: 21, Repo: "owner/repo", Labels: []string{"stage:Implement:complete"}}
	stage := stgs[0]
	pctx := &phase1Ctx{ctx: context.Background(), board: &gh.ProjectBoard{}, item: item, stage: stage, hasComplete: true, advancedItems: make(map[string]bool)}

	// No store entry at all for issue 21 — snap lookup errors, PausedByEngine
	// defaults false either way.
	got := eng.handleEngineUnpause(pctx)
	if got {
		t.Error("handleEngineUnpause must return false when the item was never engine-paused")
	}

	client.mu.Lock()
	labelCalls := len(client.addLabelCalls) + len(client.removeLabelCalls)
	client.mu.Unlock()
	if labelCalls != 0 {
		t.Errorf("expected no label mutations for a never-paused item, got %d", labelCalls)
	}
}
