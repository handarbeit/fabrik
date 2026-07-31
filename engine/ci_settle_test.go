package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

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
