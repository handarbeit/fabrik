package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// ciSettleWaitForCIStages returns a minimal Validate/wait_for_ci stage list used
// across settleAwaitingCIScan tests.
func ciSettleWaitForCIStages() []*stages.Stage {
	waitTrue := true
	return []*stages.Stage{
		{Name: "Validate", Order: 1, Prompt: "validate", WaitForCI: &waitTrue},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
}

// TestSettleAwaitingCIScan_BypassesBoardColumnAdmission is the core #1270
// regression: settleAwaitingCIScan must evaluate the CI gate for a
// fabrik:awaiting-ci item using only board.Items — it never calls
// itemMayNeedWork, selectDeepFetchCandidates, or the main catch-up loop's
// per-item admission gate. This test calls it directly (bypassing poll()
// entirely) and confirms the CI-fix reinvoke still dispatches, demonstrating
// the scan's independence from whatever silently excluded #3915 from that
// shared pipeline in the field.
func TestSettleAwaitingCIScan_BypassesBoardColumnAdmission(t *testing.T) {
	client := ciFailureSettleClient()
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxCiFixCycles = 5

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{
				Number: 20,
				Repo:   "owner/repo",
				Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}
	advancedItems := make(map[string]bool)

	eng.settleAwaitingCIScan(context.Background(), board, advancedItems)
	eng.wg.Wait()

	snap, _ := eng.store.Get("owner/repo", 20)
	if got := snap.CIFixCycles("Validate"); got != 1 {
		t.Errorf("CIFixCycles(Validate) = %d; want 1 — settleAwaitingCIScan must dispatch a CI-fix reinvoke directly from board.Items", got)
	}
	if !advancedItems["owner/repo#20"] {
		t.Error("expected advancedItems[owner/repo#20] set on successful CI-fix dispatch")
	}
}

// TestSettleAwaitingCIScan_NoDoubleDispatch runs a full eng.poll(ctx) for an
// item at a wait_for_ci Validate stage carrying only fabrik:awaiting-ci (no
// stage:Validate:complete) with a failing check run, and confirms the CI-fix
// cycle count is incremented exactly once — proving the main catch-up loop's
// narrowed (hasComplete-only) admission gate and the dedicated
// settleAwaitingCIScan never both act on the same item in the same poll.
func TestSettleAwaitingCIScan_NoDoubleDispatch(t *testing.T) {
	client := ciFailureSettleClient()
	client.fetchProjectBoardFn = func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
		return &gh.ProjectBoard{
			ProjectID: "PVT_1",
			Items: []gh.ProjectItem{
				{
					Number: 21,
					ItemID: "PVTI_21",
					Status: "Validate",
					Repo:   "owner/repo",
					Labels: []string{"fabrik:awaiting-ci"},
				},
			},
		}, nil
	}
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxCiFixCycles = 5

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait()

	snap, _ := eng.store.Get("owner/repo", 21)
	if got := snap.CIFixCycles("Validate"); got != 1 {
		t.Errorf("CIFixCycles(Validate) = %d; want exactly 1 — Phase 1's narrowed admission gate and settleAwaitingCIScan must never both dispatch for the same item in the same poll", got)
	}
}

// TestSettleAwaitingCIScan_LeftMergeQueue_ReEnqueues is a PR-review regression
// (pruefer): an item admitted into this scan can also carry
// fabrik:auto-merge-enabled in a merge-queue-enabled repo. handleAutoMergeConvergence's
// ejection-recovery ladder (merge_gate.go) needs pctx.priorInQueue — the item's
// PREVIOUS-poll merge-queue membership — to detect the poll-native "left the
// queue" edge. Before this fix, settleAwaitingCIScan built phase1Ctx without
// ever setting priorInQueue, so it always read false, silently losing the edge
// and leaving a cleanly-ejected PR stuck waiting for the queue forever instead
// of re-enqueuing — a narrower instance of the exact stranding-bug class #1270
// exists to close. This seeds the store with IsInMergeQueue=true (prior poll),
// simulates the PR having left the queue clean this poll (no LastEnqueuedSHA
// recorded, so only the leftQueue edge — not the SHA-change clause — can
// explain a re-enqueue), and asserts the re-enqueue fires.
func TestSettleAwaitingCIScan_LeftMergeQueue_ReEnqueues(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", HeadSHA: "abc12345", AutoMergeEnabled: false}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			// Fresh fetch: PR left the merge queue this poll (ejected); repo still
			// has the merge queue enabled.
			item.LinkedPRNumber = 10
			item.LinkedPRIsMergeQueueEnabled = true
			item.LinkedPRIsInMergeQueue = false
			return nil
		},
	}
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxEnqueueCycles = 5

	// Seed the store: the item WAS in the merge queue last poll. LinkedPRNumber
	// must be set — applyProjectItem's LinkedPR sync block (internal/itemstate/
	// project_apply.go) is gated on it being non-zero, otherwise IsInMergeQueue
	// never reaches the snapshot at all.
	eng.store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: 30,
		FreshState: gh.ProjectItem{
			Number: 30, Repo: "owner/repo",
			LinkedPRNumber:              10,
			LinkedPRIsMergeQueueEnabled: true,
			LinkedPRIsInMergeQueue:      true,
		},
	})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 30,
				Repo:   "owner/repo",
				Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci", "fabrik:auto-merge-enabled"},
			},
		},
	}

	eng.settleAwaitingCIScan(context.Background(), board, make(map[string]bool))

	if len(client.enqueuePullRequestCalls) != 1 {
		t.Fatalf("expected 1 re-enqueue on the poll-native left-queue edge, got %d — priorInQueue was not threaded into settleAwaitingCIScan's phase1Ctx", len(client.enqueuePullRequestCalls))
	}
	if client.enqueuePullRequestCalls[0].expectedHeadOID != "abc12345" {
		t.Errorf("re-enqueue used SHA %q, want abc12345", client.enqueuePullRequestCalls[0].expectedHeadOID)
	}
}

// TestSettleAwaitingCIScan_OrphanColumn_Escalates covers the "gate genuinely
// cannot be evaluated" case: an item carries fabrik:awaiting-ci but sits at a
// column with no wait_for_ci stage (here, a HoldingStage). Repeated settle
// passes must eventually pause the issue, remove the marker, and post an
// explanatory comment naming the stray column — never leave it silent forever.
func TestSettleAwaitingCIScan_OrphanColumn_Escalates(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
	}
	stgs := []*stages.Stage{
		{Name: "Queued", Order: 1, HoldingStage: true},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxRetries = 2

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 22,
				Repo:   "owner/repo",
				Status: "Queued",
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}

	for i := 0; i < eng.cfg.MaxRetries; i++ {
		eng.settleAwaitingCIScan(context.Background(), board, make(map[string]bool))
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	pausedAdded := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			markerRemoved = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused to be added after MaxRetries settle failures on an orphan column")
	}
	if !markerRemoved {
		t.Error("expected fabrik:awaiting-ci to be removed on escalation")
	}
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected an explanatory escalation comment to be posted")
	}
	found := false
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 22 && strings.Contains(c.body, "Queued") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected escalation comment to name the stray column, got: %v", client.addCommentCalls)
	}
}

// TestRecordAwaitingCIOrphanRetry_UnlimitedWhenMaxRetriesZero is a PR-review
// follow-up (pruefer): recordSettleRetry (engine/settle.go) silently no-ops
// when MaxRetries <= 0, so an orphan-column/deep-fetch-failure item would
// retry forever without ever escalating. This is not a gap specific to this
// scan — MaxRetries == 0 is the codebase-wide, documented, tested contract for
// "unlimited retries, never escalate" (cmd/root.go's `-max-retries` flag help
// text: "0 = unlimited"; mirrored by
// TestRecordNonDefaultBaseCloseRetry_UnlimitedWhenMaxRetriesZero and
// TestRecordMergeTrainMemberCloseRetry_UnlimitedWhenMaxRetriesZero for their
// own settle scans). This pins the same contract for
// recordAwaitingCIOrphanRetry, matching that precedent, rather than treating
// it as a defect to fix here — changing recordSettleRetry's shared semantics
// would affect four other production settle scans well outside #1270's scope.
func TestRecordAwaitingCIOrphanRetry_UnlimitedWhenMaxRetriesZero(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
	}
	stgs := []*stages.Stage{
		{Name: "Queued", Order: 1, HoldingStage: true},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxRetries = 0

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 29,
				Repo:   "owner/repo",
				Status: "Queued",
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}

	for i := 0; i < 10; i++ {
		eng.settleAwaitingCIScan(context.Background(), board, make(map[string]bool))
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("did not expect escalation (fabrik:paused) when MaxRetries == 0")
		}
	}
}

// TestSettleAwaitingCIScan_DeepFetchFailure_Escalates covers the other "gate
// genuinely cannot be evaluated" case: the item resolves to a real wait_for_ci
// stage, but FetchItemDetails fails on every settle pass (e.g. permissions,
// a deleted issue node, sustained API errors), so the scan can never reach
// checkCIGate for it. This must retry-then-escalate exactly like the
// orphan-column case, not retry silently forever — and the escalation comment
// must describe the fetch failure, not falsely claim a stray board column.
func TestSettleAwaitingCIScan_DeepFetchFailure_Escalates(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			return errors.New("simulated persistent GraphQL failure")
		},
	}
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxRetries = 2

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 25,
				Repo:   "owner/repo",
				Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}

	for i := 0; i < eng.cfg.MaxRetries; i++ {
		eng.settleAwaitingCIScan(context.Background(), board, make(map[string]bool))
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	pausedAdded := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			markerRemoved = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused to be added after MaxRetries settle failures on a persistent deep-fetch failure")
	}
	if !markerRemoved {
		t.Error("expected fabrik:awaiting-ci to be removed on escalation")
	}
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected an explanatory escalation comment to be posted")
	}
	found := false
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 25 && strings.Contains(c.body, "fetched from GitHub") && !strings.Contains(c.body, "wait_for_ci`") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected escalation comment to describe the fetch failure (not a stray-column claim), got: %v", client.addCommentCalls)
	}
}

// TestSettleAwaitingCIScan_RateLimitFailure_DoesNotEscalate is the #1313
// regression: a FetchItemDetails failure whose error text is a GraphQL/REST
// rate-limit or secondary-rate-limit exhaustion message must never consume the
// shared __awaiting_ci_orphan__ escalation counter, however many consecutive
// settle passes are affected — the item is simply deferred to a future poll.
// Runs well past MaxRetries and asserts no pause, no escalation comment, and
// fabrik:awaiting-ci remains present throughout.
func TestSettleAwaitingCIScan_RateLimitFailure_DoesNotEscalate(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			return errors.New("fetching details for item #25: GraphQL error: API rate limit already exceeded for user ID 282098327.")
		},
	}
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxRetries = 3

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 25,
				Repo:   "owner/repo",
				Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}

	for i := 0; i < eng.cfg.MaxRetries*3; i++ {
		eng.settleAwaitingCIScan(context.Background(), board, make(map[string]bool))
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("expected fabrik:paused to never be added for a rate-limited deep-fetch, however many consecutive polls fail")
		}
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			t.Error("expected fabrik:awaiting-ci to remain present — a rate-limited fetch must not clear it via escalation")
		}
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no escalation comment for a rate-limited deep-fetch, got: %v", client.addCommentCalls)
	}
}

// TestSettleAwaitingCIScan_MixedCauseRetries_EscalateAtCombinedMaxRetries
// covers the shared-counter design decision (pruefer review): orphan-column
// and deep-fetch-failure retries both key off the SAME
// __awaiting_ci_orphan__ counter, so they must accumulate toward one combined
// MaxRetries threshold rather than each cause getting its own independent
// budget. This alternates the item between an orphan column (no wait_for_ci
// stage) and a valid wait_for_ci stage with a persistently failing
// FetchItemDetails across settle passes, and asserts escalation fires at
// exactly MaxRetries combined attempts (not 2×MaxRetries) — with the
// escalation comment describing whichever cause is current on the final pass
// (here, the fetch failure), not the earlier orphan-column cause.
func TestSettleAwaitingCIScan_MixedCauseRetries_EscalateAtCombinedMaxRetries(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			return errors.New("simulated persistent GraphQL failure")
		},
	}
	stgs := []*stages.Stage{{Name: "Queued", Order: 1, HoldingStage: true}}
	stgs = append(stgs, ciSettleWaitForCIStages()...)
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxRetries = 4

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 26,
				Repo:   "owner/repo",
				Status: "Queued",
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}

	// Alternate cause each pass: orphan column, deep-fetch failure, orphan
	// column, deep-fetch failure — 4 combined attempts total.
	statuses := []string{"Queued", "Validate", "Queued", "Validate"}
	for _, status := range statuses {
		board.Items[0].Status = status
		eng.settleAwaitingCIScan(context.Background(), board, make(map[string]bool))
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	pausedAdded := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	if !pausedAdded {
		t.Fatal("expected fabrik:paused after 4 combined orphan-column + deep-fetch-failure attempts (MaxRetries=4) — the two causes must share one counter")
	}

	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			markerRemoved = true
		}
	}
	if !markerRemoved {
		t.Error("expected fabrik:awaiting-ci to be removed on escalation")
	}

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected exactly 1 escalation comment at the combined MaxRetries threshold, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	// The final pass's cause was the deep-fetch failure (item was on Validate,
	// a real wait_for_ci stage) — the comment must describe that, not the
	// earlier orphan-column cause, proving escalateAwaitingCIOrphanFailure
	// re-resolves the current stage rather than remembering the first cause
	// that incremented the shared counter.
	if !strings.Contains(body, "fetched from GitHub") {
		t.Errorf("expected escalation comment to describe the current (deep-fetch-failure) cause, got: %q", body)
	}
	if strings.Contains(body, "Queued") {
		t.Errorf("escalation comment must not claim the stray-column cause once the final pass resolved to a valid wait_for_ci stage, got: %q", body)
	}
}

// TestSettleAwaitingCIScan_SkipsPausedAndClosedItems mirrors the other settle
// scans' guards: paused items must not be fought by this scan, and closed
// items are the exclusive responsibility of runValidatePRTerminalAdvance
// (ADR-056 D2) — duplicating that recovery here would create a second owner.
func TestSettleAwaitingCIScan_SkipsPausedAndClosedItems(t *testing.T) {
	client := ciFailureSettleClient()
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 23, Repo: "owner/repo", Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci", "fabrik:paused"},
			},
			{
				Number: 24, Repo: "owner/repo", Status: "Validate", IsClosed: true,
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}

	eng.settleAwaitingCIScan(context.Background(), board, make(map[string]bool))
	eng.wg.Wait()

	for _, n := range []int{23, 24} {
		snap, _ := eng.store.Get("owner/repo", n)
		if snap.CIFixCycles("Validate") != 0 {
			t.Errorf("#%d: expected no CI-fix dispatch for a paused/closed item, got CIFixCycles=%d", n, snap.CIFixCycles("Validate"))
		}
	}
}

// TestSettleAwaitingCIScan_ClosedDuringDeepFetch_SkipsHandlerChain is a
// PR-review regression (pruefer): the shallow board.Items snapshot at the top
// of the loop can be stale by the time this item's own FetchItemDetails call
// runs — if the issue was closed in that window, the pre-fetch item.IsClosed
// check (already covered by TestSettleAwaitingCIScan_SkipsPausedAndClosedItems)
// cannot see it. This exercises the OTHER check: FetchItemDetails itself now
// queries GraphQL's `closed` field and populates item.IsClosed
// (github/project.go), so the post-fetch re-check added alongside the
// fabrik:awaiting-ci/fabrik:paused re-check must also catch a since-closed
// issue and skip the handler chain — closed-item recovery is
// runValidatePRTerminalAdvance's exclusive job (ADR-056 D2); this scan must
// not race it.
func TestSettleAwaitingCIScan_ClosedDuringDeepFetch_SkipsHandlerChain(t *testing.T) {
	client := ciFailureSettleClient()
	client.fetchItemDetailsFn = func(item *gh.ProjectItem) error {
		// Simulate the race: the issue was closed after the shallow board read
		// but before this deep-fetch ran.
		item.IsClosed = true
		return nil
	}
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				// Shallow snapshot still shows the item open — only the deep-fetch
				// reveals it closed.
				Number: 28, Repo: "owner/repo", Status: "Validate", IsClosed: false,
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}

	eng.settleAwaitingCIScan(context.Background(), board, make(map[string]bool))
	eng.wg.Wait()

	snap, _ := eng.store.Get("owner/repo", 28)
	if got := snap.CIFixCycles("Validate"); got != 0 {
		t.Errorf("CIFixCycles(Validate) = %d; want 0 — a since-closed item must not reach the CI-fix handler chain", got)
	}
}

// TestSettleAwaitingCIScan_RaceWithMainLoop_CycleLimitPause_NoDuplicateComment
// is a PR-review regression (pruefer): addCompleteLabelAndRemoveCI (ci.go) adds
// stage:X:complete and removes fabrik:awaiting-ci via two SEPARATE GitHub API
// calls. If the removal call fails transiently (rate limit, network blip)
// after the add succeeds, both labels persist simultaneously on GitHub for one
// poll — which, because hasComplete is computed independently in each
// admission path, routes the SAME item through both the main catch-up loop
// (admitted via hasComplete=true) and settleAwaitingCIScan (admitted via
// fabrik:awaiting-ci still present) in that one poll. Two direct
// settleAwaitingCIScan calls stand in for "main loop pass, then dedicated scan
// pass" here — both ultimately just run catchUpPhase1Handlers against the same
// hasComplete=true phase1Ctx, so this is a faithful proxy without needing to
// reproduce the exact deepFetchCandidates admission machinery.
//
// The CI-fix-reinvoke *dispatch* branch cannot double-fire here —
// dispatchWithCycleLimit's snap.Worker() != nil guard is applied synchronously
// before the reinvoke goroutine starts (reinvoke.go), which
// TestSettleAwaitingCIScan_NoDoubleDispatch already pins directly. This test
// covers the OTHER branch: with MaxCiFixCycles=0, cycleCount (0) >= maxCycles
// (0) is true from the first pass, so dispatchWithCycleLimit takes the
// pause() branch instead of dispatch() — and pauseForCIFixCycleLimit does not
// set Worker(). Before this fix, that branch had no equivalent guard and
// produced a duplicate pause comment in this exact race; pauseForCIFixCycleLimit
// (and pauseForCITimeout) now check hasCIGatePauseComment(item, stage) before
// posting, mirroring the hasSkippedComment precedent in
// no_work_needed_settle.go. fetchItemDetailsFn here simulates the real
// mechanism this relies on: each settleAwaitingCIScan pass calls
// FetchItemDetails, which does a genuinely fresh GraphQL fetch and repopulates
// item.Comments (github/project.go's applyComments) — so the second pass's
// fetch reflects the first pass's synchronous, already-completed AddComment
// call. A mock that left item.Comments empty across both calls (the prior,
// unrealistic version of this test) would validate nothing.
func TestSettleAwaitingCIScan_RaceWithMainLoop_CycleLimitPause_NoDuplicateComment(t *testing.T) {
	client := ciFailureSettleClient()
	var posted []gh.Comment
	client.fetchItemDetailsFn = func(item *gh.ProjectItem) error {
		item.Comments = append([]gh.Comment(nil), posted...)
		return nil
	}
	client.addCommentFn = func(_, _ string, _ int, body string) (int, error) {
		posted = append(posted, gh.Comment{ID: fmt.Sprintf("C_%d", len(posted)+1), Body: body})
		return len(posted), nil
	}
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxCiFixCycles = 0 // force the pause-at-limit branch on the very first attempt

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 27, Repo: "owner/repo", Status: "Validate",
				// Simulates the label-removal-failed race: both the completion
				// label (added) and the awaiting-ci marker (removal failed) are
				// present simultaneously.
				Labels: []string{"stage:Validate:complete", "fabrik:awaiting-ci"},
			},
		},
	}
	advancedItems := make(map[string]bool)

	eng.settleAwaitingCIScan(context.Background(), board, advancedItems)
	eng.settleAwaitingCIScan(context.Background(), board, advancedItems)
	eng.wg.Wait()

	client.mu.Lock()
	defer client.mu.Unlock()
	if got := len(client.addCommentCalls); got != 1 {
		t.Errorf("got %d pause comment(s) for the two-pass race window; want exactly 1 — hasCIGatePauseComment must suppress the second pass's duplicate", got)
	}
}

// TestSettleAwaitingCIScan_CIWaitTimeoutBackstop_PausesRegardlessOfGateClaim is
// a regression test for #1303's requested unconditional CIWaitTimeout
// backstop: checkCIGate's own timeout guard only fires once checkCIGate is
// actually reached, but any silent claim earlier in the Phase 1 handler chain
// (this test pins settle to PRMergeUnsettled on every poll via a permanently
// nil mergeable — mirroring the confirmed incident shape, where
// checkMergeabilityGate claims the item and checkCIGate never runs) makes
// that inner timeout dead. settleAwaitingCIScan must pause the issue on its
// own once fabrik:awaiting-ci exceeds CIWaitTimeout, independent of what any
// gate would otherwise classify or claim.
func TestSettleAwaitingCIScan_CIWaitTimeoutBackstop_PausesRegardlessOfGateClaim(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 43, HeadSHA: "cafebabe", State: "open", Merged: false}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			// mergeable=nil forever ("GitHub still computing") pins settle to
			// PRMergeUnsettled every poll, so checkMergeabilityGate claims the
			// item and checkCIGate is never reached — exactly the shape that
			// left the inner CIWaitTimeout guard dead in the field incident.
			return nil, "", nil
		},
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return time.Now().Add(-45 * time.Minute), nil // older than the default 30-minute CIWaitTimeout
		},
		addLabelToIssueFn:    func(_, _ string, _ int, _ string) error { return nil },
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 31, Repo: "owner/repo", Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}
	advancedItems := make(map[string]bool)

	eng.settleAwaitingCIScan(context.Background(), board, advancedItems)
	eng.wg.Wait()

	client.mu.Lock()
	defer client.mu.Unlock()
	pausedLabelAdded := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedLabelAdded = true
		}
	}
	if !pausedLabelAdded {
		t.Error("expected fabrik:paused to be applied by the CIWaitTimeout backstop even though the merge gate would claim the item every poll")
	}
	if len(client.addCommentCalls) != 1 {
		t.Errorf("expected exactly 1 pause comment, got %d", len(client.addCommentCalls))
	}

	// The handler chain must not have been reached: the CI-fix cycle counter
	// (only incremented by checkCIGate's ciFailure branch, which requires
	// checkCIGate to run at all) stays at zero.
	snap, _ := eng.store.Get("owner/repo", 31)
	if snap.CIFixCycles("Validate") != 0 {
		t.Errorf("expected the backstop to fire ahead of the handler chain (CIFixCycles=0), got %d", snap.CIFixCycles("Validate"))
	}
}

// TestSettleAwaitingCIScan_CIWaitTimeoutBackstop_NoOpWithinTimeout verifies the
// backstop added for #1303 does not fire — and does not disturb the normal
// gate-driven path — for an item whose fabrik:awaiting-ci label was applied
// recently (well within CIWaitTimeout).
func TestSettleAwaitingCIScan_CIWaitTimeoutBackstop_NoOpWithinTimeout(t *testing.T) {
	client := ciFailureSettleClient() // fetchLabelAppliedAtFn returns time.Now() — elapsed ≈ 0
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxCiFixCycles = 5

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 32, Repo: "owner/repo", Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}
	advancedItems := make(map[string]bool)

	eng.settleAwaitingCIScan(context.Background(), board, advancedItems)
	eng.wg.Wait()

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("backstop must not fire for an item well within CIWaitTimeout")
		}
	}
	// The normal path (checkCIGate reached, CI failure classified) must still
	// dispatch the CI-fix reinvoke exactly as before this change.
	snap, _ := eng.store.Get("owner/repo", 32)
	if snap.CIFixCycles("Validate") != 1 {
		t.Errorf("CIFixCycles(Validate) = %d; want 1 — the normal gate-driven path must be unaffected by the new backstop", snap.CIFixCycles("Validate"))
	}
}

// TestSettleAwaitingCIScan_ResumedAfterTimeout_GreenCI_Advances is the #1408
// regression for R1 and the Acceptance criterion's first bullet: an item that
// previously hit the CI wait timeout — carrying fabrik:awaiting-ci, a stale
// fabrik:awaiting-ci appliedAt (past CIWaitTimeout), and the pause comment the
// original timeout posted — but no fabrik:paused (a human resumed it by
// removing just that label, per the pause comment's own instructions) must be
// re-evaluated against LIVE CI, not re-escalated blind from the stale
// timestamp. Here CI has since gone green (mergeable_state=clean, the
// ADR-033 shortcut) — the item must advance: stage:Validate:complete added,
// fabrik:awaiting-ci removed, no new fabrik:paused, no new comment.
//
// This test fails against the pre-#1408 engine: the backstop unconditionally
// calls pauseForCITimeout and continues once appliedAt exceeds CIWaitTimeout;
// pauseForCITimeout finds the pre-existing pause comment via
// hasCIGatePauseComment and no-ops entirely (no labels touched, no return
// signal), and the continue skips the handler chain — so checkCIGate is never
// reached and stage:Validate:complete is never added, regardless of live CI
// state.
func TestSettleAwaitingCIScan_ResumedAfterTimeout_GreenCI_Advances(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 44, HeadSHA: "cafebabe", State: "open", Merged: false}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil // ADR-033 shortcut: PRMergeReady, CI gate clears
		},
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return time.Now().Add(-45 * time.Minute), nil // stale — past the default 30-minute CIWaitTimeout
		},
		addLabelToIssueFn:      func(_, _ string, _ int, _ string) error { return nil },
		removeLabelFromIssueFn: func(_, _ string, _ int, _ string) error { return nil },
		addCommentFn:           func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn:   func(_, _ string, _ int, _ string) error { return nil },
	}
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 45, Repo: "owner/repo", Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci"}, // no fabrik:paused — resumed by a human
				Comments: []gh.Comment{
					{ID: "C1", Body: "🏭 **Fabrik — CI wait timeout**\n\nThe CI gate for stage **Validate** timed out waiting for checks to pass.\n\n" +
						"Fabrik has paused this issue. Please check the PR's CI status, address any failures, and then remove the `fabrik:paused` label to resume."},
				},
			},
		},
	}
	advancedItems := make(map[string]bool)

	eng.settleAwaitingCIScan(context.Background(), board, advancedItems)
	eng.wg.Wait()

	client.mu.Lock()
	defer client.mu.Unlock()

	completeAdded, pausedReapplied := false, false
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "stage:Validate:complete":
			completeAdded = true
		case "fabrik:paused":
			pausedReapplied = true
		}
	}
	if !completeAdded {
		t.Error("expected stage:Validate:complete to be added — a resumed item with green CI must advance")
	}
	if pausedReapplied {
		t.Error("fabrik:paused must not be reapplied when live CI is green")
	}
	ciRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			ciRemoved = true
		}
	}
	if !ciRemoved {
		t.Error("expected fabrik:awaiting-ci to be removed on gate clear")
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no new comment posted, got %d", len(client.addCommentCalls))
	}
}

// TestSettleAwaitingCIScan_ResumedAfterTimeout_StillFailing_ReEscalates is the
// #1408 regression for R4 and the Acceptance criterion's second bullet: same
// fixture shape as the green-CI case above (stale fabrik:awaiting-ci
// appliedAt, existing timeout pause comment, no fabrik:paused), but CI is
// still failing. The item must be re-escalated — fabrik:paused +
// fabrik:awaiting-input reapplied — not silently stranded, and the existing
// pause comment must be reused rather than reposted.
func TestSettleAwaitingCIScan_ResumedAfterTimeout_StillFailing_ReEscalates(t *testing.T) {
	client := ciFailureSettleClient()
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return time.Now().Add(-45 * time.Minute), nil // stale — past the default 30-minute CIWaitTimeout
	}
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxCiFixCycles = 5

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 46, Repo: "owner/repo", Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci"}, // no fabrik:paused — resumed by a human
				Comments: []gh.Comment{
					{ID: "C1", Body: "🏭 **Fabrik — CI wait timeout**\n\nThe CI gate for stage **Validate** timed out waiting for checks to pass.\n\n" +
						"Fabrik has paused this issue. Please check the PR's CI status, address any failures, and then remove the `fabrik:paused` label to resume."},
				},
			},
		},
	}
	advancedItems := make(map[string]bool)

	eng.settleAwaitingCIScan(context.Background(), board, advancedItems)
	eng.wg.Wait()

	client.mu.Lock()
	defer client.mu.Unlock()

	pausedReapplied, awaitingInputReapplied := false, false
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:paused":
			pausedReapplied = true
		case "fabrik:awaiting-input":
			awaitingInputReapplied = true
		}
	}
	if !pausedReapplied {
		t.Error("expected fabrik:paused to be reapplied — a resumed item with still-failing CI must be re-escalated, not silently stranded")
	}
	if !awaitingInputReapplied {
		t.Error("expected fabrik:awaiting-input to be reapplied alongside fabrik:paused")
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected the existing pause comment to be reused, not reposted — got %d new comment(s)", len(client.addCommentCalls))
	}
}

// TestSettleAwaitingCIScan_ResumedAtCycleLimit_StillFailing_ReEscalates is the
// #1408 regression for R5: pauseForCIFixCycleLimit shares hasCIGatePauseComment
// and the same suppress-forever defect shape as pauseForCITimeout, but is
// reached only via the live-checked handler chain (never the backstop
// directly). A human resuming an item that was paused at the CI-fix cycle
// limit removes fabrik:paused but not CIFixCycles (never reset — see
// research), so on the next poll the item lands back at cycleCount >=
// maxCycles with CI still failing and the old cycle-limit comment already
// present. It must be re-escalated (labels reapplied), not silently
// stranded, and must not repost a duplicate comment.
func TestSettleAwaitingCIScan_ResumedAtCycleLimit_StillFailing_ReEscalates(t *testing.T) {
	client := ciFailureSettleClient() // fetchLabelAppliedAtFn returns time.Now() — elapsed ≈ 0, backstop does not fire
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxCiFixCycles = 2

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 47, Repo: "owner/repo", Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci"}, // no fabrik:paused — resumed by a human
				Comments: []gh.Comment{
					{ID: "C1", Body: "🏭 **Fabrik — CI fix cycle limit reached**\n\nThe stage **Validate** has been re-invoked to fix CI failures 2 time(s), " +
						"which has reached the maximum configured limit (`FABRIK_MAX_CI_FIX_CYCLES=2`)."},
				},
			},
		},
	}
	advancedItems := make(map[string]bool)

	// CIFixCycles already sits at the limit from before the pause — R5's
	// research confirmed pauseForCIFixCycleLimit never resets this counter, so
	// it survives a resume unchanged.
	for i := 0; i < eng.cfg.MaxCiFixCycles; i++ {
		eng.store.Apply(itemstate.CIFixCycleIncremented{
			Repo: "owner/repo", Number: 47, StageName: "Validate",
		})
	}

	eng.settleAwaitingCIScan(context.Background(), board, advancedItems)
	eng.wg.Wait()

	client.mu.Lock()
	defer client.mu.Unlock()

	pausedReapplied := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedReapplied = true
		}
	}
	if !pausedReapplied {
		t.Error("expected fabrik:paused to be reapplied — a resumed item still at the CI-fix cycle limit must be re-escalated, not silently stranded")
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected the existing cycle-limit comment to be reused, not reposted — got %d new comment(s)", len(client.addCommentCalls))
	}
}

// ---- #1460 R2/AC1/AC2: pauseForCIFixCycleLimit resumability ----

// TestCIFixCycleLimit_UnpauseResetsCounterAndAllowsMultipleCycles is the
// AC1/AC2 regression for #1460's confirmed site #2: PR #1445/ADR-1408 fixed
// only the comment-repost half (R4) of this site — it never applied
// itemstate.EnginePaused, so removing fabrik:paused was (and, without this
// fix, still would be) a no-op: CIFixCycles stayed pinned at the limit and
// the very next catch-up pass re-paused immediately.
//
// Uses ciFailureSettleClient(), the same fixture
// TestSettleAwaitingCIScan_ResumedAtCycleLimit_StillFailing_ReEscalates uses
// for the "still failing, re-escalate without reposting" (R4) case — that
// test's fixture never applies itemstate.EnginePaused itself (it seeds
// CIFixCycles directly), so it remains a valid, unaffected regression test
// for the case where the item was never actually paused through the real
// code path. This test instead drives the pause itself through the real
// handleMergeAndCIGates -> dispatchWithCycleLimit -> pauseForCIFixCycleLimit
// path via runPhase1Chain, mirroring the review/rebase/enqueue-cycle-limit
// tests.
//
// AC3: reverting pauseForCIFixCycleLimit's two itemstate.EnginePaused apply
// lines turns this test red at the Step 1 PausedByEngine assertion (verified
// by hand).
func TestCIFixCycleLimit_UnpauseResetsCounterAndAllowsMultipleCycles(t *testing.T) {
	client := ciFailureSettleClient() // classifies ciFailure on every call — CI never resolves
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxCiFixCycles = 2
	stage := eng.cfg.Stages[0] // "Validate", WaitForCI: true
	board := &gh.ProjectBoard{}
	const repo = "owner/repo"
	const number = 60

	// Step 1: drive a REAL pause through the actual code path. CIFixCycles
	// pre-set to exactly the limit; ciFailureSettleClient keeps CI classified
	// as failing on every call, so handleMergeAndCIGates's dispatchWithCycleLimit
	// reaches the pause branch and calls the real pauseForCIFixCycleLimit.
	for i := 0; i < eng.cfg.MaxCiFixCycles; i++ {
		eng.store.Apply(itemstate.CIFixCycleIncremented{Repo: repo, Number: number, StageName: "Validate"})
	}
	item := gh.ProjectItem{Number: number, Repo: repo, Labels: []string{"fabrik:awaiting-ci"}}
	pctx := &phase1Ctx{ctx: context.Background(), board: board, item: item, stage: stage, hasComplete: false, advancedItems: make(map[string]bool)}
	claimed := runPhase1Chain(eng, pctx)
	eng.wg.Wait()
	if !claimed {
		t.Fatal("expected the cycle-limit pause branch to claim the item")
	}

	client.mu.Lock()
	pausedApplied := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedApplied = true
		}
	}
	client.mu.Unlock()
	if !pausedApplied {
		t.Fatal("expected fabrik:paused to be applied by the real cycle-limit pause")
	}
	snap, _ := eng.store.Get(repo, number)
	if !snap.PausedByEngine("Validate") {
		t.Fatal("R2: pauseForCIFixCycleLimit must apply itemstate.EnginePaused so wasPaused becomes true on resume — PausedByEngine(Validate) is false after the real pause")
	}

	// Step 2 (AC1): simulate the operator removing fabrik:paused and run
	// another pass. CI is still (perpetually, per this fixture) failing, so
	// this same pass's own dispatch fires right after the reset — the
	// counter reading 1 (not the still-stuck 2) is itself proof the reset
	// landed.
	item.Labels = []string{"fabrik:awaiting-ci"}
	pctx = &phase1Ctx{ctx: context.Background(), board: board, item: item, stage: stage, hasComplete: false, advancedItems: make(map[string]bool)}
	runPhase1Chain(eng, pctx)
	eng.wg.Wait()

	snap, _ = eng.store.Get(repo, number)
	if snap.PausedByEngine("Validate") {
		t.Error("PausedByEngine(Validate) must be cleared after the reset pass")
	}
	if got := snap.CIFixCycles("Validate"); got != 1 {
		t.Fatalf("AC1: CIFixCycles(Validate) after unpause = %d; want 1 — the counter must have reset to 0 before this pass's own dispatch incremented it, not stayed pinned at the limit", got)
	}

	// Step 3 (AC2): confirm a further cycle proceeds without re-pausing
	// (baseline excludes Step 1's legitimate pause).
	client.mu.Lock()
	baseline := len(client.addLabelCalls)
	client.mu.Unlock()
	pctx = &phase1Ctx{ctx: context.Background(), board: board, item: item, stage: stage, hasComplete: false, advancedItems: make(map[string]bool)}
	claimed = runPhase1Chain(eng, pctx)
	eng.wg.Wait()
	if !claimed {
		t.Fatal("expected a second ci-fix reinvoke to claim the item")
	}
	snap, _ = eng.store.Get(repo, number)
	if got := snap.CIFixCycles("Validate"); got != 2 {
		t.Errorf("AC2: CIFixCycles(Validate) = %d; want 2 — a genuinely reset counter must permit more than one further cycle before re-hitting the limit", got)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.addLabelCalls[baseline:] {
		if c.labelName == "fabrik:paused" {
			t.Errorf("AC2: fabrik:paused must not be re-added — CIFixCycles(2) has not yet reached MaxCiFixCycles(%d) again", eng.cfg.MaxCiFixCycles)
		}
	}
}

// TestSettleAwaitingCIScan_StaleCachedPendingCheckRuns_EndToEnd is the full
// end-to-end regression test for #1303's confirmed root cause, reproducing the
// exact field incident shape through the REAL boardcache.CacheImpl (not a mock
// ReadClient) so FetchCheckRuns' cache-trust logic genuinely executes:
//
//   - The store already holds a check-run snapshot for the PR's head SHA
//     classifying as CheckRunsPending — mirroring what a dropped/absent
//     check_run webhook leaves behind on a webhook-less deployment.
//   - GitHub's live state (via the fallback client) has since resolved to a
//     definitive FAILURE on that same check name (a fresh, higher-ID run).
//   - The PR is otherwise MERGEABLE with mergeable_state=blocked (exactly the
//     PR #3932 shape from the issue's field evidence) and carries a stale
//     fabrik:rebase-needed label from an earlier, now-resolved conflict phase.
//
// Before RefreshCheckRunsLive existed, FetchCheckRuns' denylist cache-trust
// check would keep serving the cached Pending classification forever (only a
// would-be-FAILED classification forces a live refetch — the #958 leg 3
// guard), so settlePRMergeState → checkMergeabilityGate would classify
// PRMergeUnsettled and silently claim the item every poll, and checkCIGate
// (and the CI-fix reinvoke it drives) would never run. This test asserts the
// full chain now converges: RefreshCheckRunsLive primes the store with the
// live FAILURE before the handler chain runs, checkMergeabilityGate reaches
// PRMergeBlocked (clearing the stale fabrik:rebase-needed label), and
// checkCIGate classifies the failure and dispatches a CI-fix reinvoke.
func TestSettleAwaitingCIScan_StaleCachedPendingCheckRuns_EndToEnd(t *testing.T) {
	const sha = "5ad0a61cfeedface5ad0a61cfeedface5ad0a61c"
	const prNumber = 3932

	var fetchCheckRunsCalls int
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, HeadSHA: sha, State: "open", Merged: false}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, checkSHA string) ([]gh.CheckRun, error) {
			fetchCheckRunsCalls++
			// GitHub's genuinely live state: the same check name, resolved to a
			// definitive failure, at a higher check-run ID than the stale cached
			// pending run seeded into the store below.
			return []gh.CheckRun{
				{ID: 2, Name: "ci-fix-sentinel", Status: "completed", Conclusion: "failure"},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRHeadSHA = sha
			item.LinkedPRNumber = prNumber
			return nil
		},
		addLabelToIssueFn:      func(_, _ string, _ int, _ string) error { return nil },
		removeLabelFromIssueFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxCiFixCycles = 5
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	eng.readClient = cache

	// Seed the store with a stale cached PENDING classification for this exact
	// SHA — mirrors what a dropped/absent check_run webhook leaves behind: the
	// checks were pending when last observed, and nothing else supersedes that
	// snapshot on a webhook-less deployment.
	eng.store.Apply(itemstate.CheckRunCompleted{
		Repo: "owner/repo", SHA: sha,
		Run: gh.CheckRun{ID: 1, Name: "ci-fix-sentinel", Status: "in_progress"},
	})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 33, Repo: "owner/repo", Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci", "fabrik:rebase-needed"},
			},
		},
	}
	advancedItems := make(map[string]bool)

	eng.settleAwaitingCIScan(context.Background(), board, advancedItems)
	eng.wg.Wait()

	if fetchCheckRunsCalls == 0 {
		t.Fatal("expected at least one live FetchCheckRuns call (via RefreshCheckRunsLive) — the stale cached Pending snapshot must not be served indefinitely")
	}

	snap, _ := eng.store.Get("owner/repo", 33)
	if got := snap.CIFixCycles("Validate"); got != 1 {
		t.Errorf("CIFixCycles(Validate) = %d; want 1 — a CI-fix reinvoke must dispatch once the stale Pending cache is refreshed to the live Failure", got)
	}
	if !advancedItems["owner/repo#33"] {
		t.Error("expected advancedItems[owner/repo#33] set on successful CI-fix dispatch")
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	rebaseNeededRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			rebaseNeededRemoved = true
		}
	}
	if !rebaseNeededRemoved {
		t.Error("expected the stale fabrik:rebase-needed label to be cleared once checkMergeabilityGate reaches PRMergeBlocked")
	}
}

// TestSettleAwaitingCIScan_CacheHitPath_RefreshCheckRunsLiveReached is the
// #1325 regression test. Unlike
// TestSettleAwaitingCIScan_StaleCachedPendingCheckRuns_EndToEnd above — whose
// fetchItemDetailsFn sets item.LinkedPRHeadSHA/LinkedPRNumber directly, so its
// single FetchItemDetails call is always a cache MISS and never exercises
// copyDeepFieldsFromState — this test primes the real boardcache.CacheImpl
// with a genuine prior deep fetch, then calls settleAwaitingCIScan with a
// fresh/zeroed board item (no pre-populated LinkedPRHeadSHA/LinkedPRNumber,
// exactly like production's board.Items snapshot) and an unchanged UpdatedAt,
// so FetchItemDetails takes the cache-HIT branch. Before #1325's fix,
// copyDeepFieldsFromState never copied LinkedPRHeadSHA from the cached
// ItemState, so item.LinkedPRHeadSHA stayed "" on the hit path, the
// RefreshCheckRunsLive guard in settleAwaitingCIScan never fired, and the
// stale cached PENDING check-run classification would be served forever.
func TestSettleAwaitingCIScan_CacheHitPath_RefreshCheckRunsLiveReached(t *testing.T) {
	const sha = "cafefeed0000cafefeed0000cafefeed0000cafe"
	const prNumber = 4158
	t0 := time.Date(2026, 8, 1, 23, 0, 0, 0, time.UTC)

	var fetchItemDetailsCalls int
	var fetchCheckRunsCalls int
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			fetchItemDetailsCalls++
			item.LinkedPRHeadSHA = sha
			item.LinkedPRNumber = prNumber
			return nil
		},
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, HeadSHA: sha, State: "open", Merged: false}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, checkSHA string) ([]gh.CheckRun, error) {
			fetchCheckRunsCalls++
			// Live state: same check name as the stale cached run below, resolved
			// to a definitive failure at a higher check-run ID.
			return []gh.CheckRun{
				{ID: 2, Name: "ci-fix-sentinel", Status: "completed", Conclusion: "failure"},
			}, nil
		},
		addLabelToIssueFn:      func(_, _ string, _ int, _ string) error { return nil },
		removeLabelFromIssueFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxCiFixCycles = 5
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	eng.readClient = cache

	// Prime the cache with a genuine deep fetch (a miss, since the store is
	// still empty for this item) — mirrors a prior poll's deep fetch. This
	// seeds LastDeepFetchAt/LastSeenSourceUpdatedAt=t0 and, via
	// PRHeadSHAUpdated, ItemState.LinkedPR.HeadSHA/.Number.
	primeItem := gh.ProjectItem{Number: 34, Repo: "owner/repo", Status: "Validate", UpdatedAt: t0}
	if err := cache.FetchItemDetails(&primeItem); err != nil {
		t.Fatalf("priming FetchItemDetails: %v", err)
	}
	if fetchItemDetailsCalls != 1 {
		t.Fatalf("priming call: want 1 live fetch, got %d", fetchItemDetailsCalls)
	}

	// Seed a stale cached PENDING check-run classification for this SHA —
	// mirrors a dropped/absent check_run webhook, exactly as the sibling
	// end-to-end test above does.
	eng.store.Apply(itemstate.CheckRunCompleted{
		Repo: "owner/repo", SHA: sha,
		Run: gh.CheckRun{ID: 1, Name: "ci-fix-sentinel", Status: "in_progress"},
	})

	// The board item is fresh/zeroed at the deep-field layer — no pre-populated
	// LinkedPRHeadSHA/LinkedPRNumber — with the SAME UpdatedAt as the priming
	// call, so FetchItemDetails below takes the cache-HIT branch.
	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number:    34,
				Repo:      "owner/repo",
				Status:    "Validate",
				Labels:    []string{"fabrik:awaiting-ci"},
				UpdatedAt: t0,
			},
		},
	}
	advancedItems := make(map[string]bool)

	eng.settleAwaitingCIScan(context.Background(), board, advancedItems)
	eng.wg.Wait()

	if fetchItemDetailsCalls != 1 {
		t.Fatalf("settleAwaitingCIScan pass: fetchItemDetailsCalls = %d, want 1 (this pass must be a genuine cache hit, not a live re-fetch)", fetchItemDetailsCalls)
	}
	if fetchCheckRunsCalls == 0 {
		t.Fatal("expected RefreshCheckRunsLive to reach FetchCheckRuns on the cache-hit path — the call site under test was never reached")
	}

	snap, _ := eng.store.Get("owner/repo", 34)
	if got := snap.CIFixCycles("Validate"); got != 1 {
		t.Errorf("CIFixCycles(Validate) = %d; want 1 — the live FAILED classification observed via the cache-hit RefreshCheckRunsLive call must drive the CI-fix reinvoke, not the stale cached PENDING one", got)
	}
}
