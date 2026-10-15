package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

func testStagesForSHAInvalidation() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Implement", Order: 3, Prompt: "implement"},
		{Name: "Validate", Order: 4, Prompt: "validate"},
	}
}

// shaInvalidationBoardFn builds a fetchProjectBoardFn that returns a single
// Validate-status item with the given labels.
func shaInvalidationBoardFn(itemNum int, labels []string) func(string, string, int, string) (*gh.ProjectBoard, error) {
	return func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
		return &gh.ProjectBoard{
			ProjectID: "PVT_1",
			Items: []gh.ProjectItem{
				{
					Number: itemNum,
					ItemID: fmt.Sprintf("PVTI_%d", itemNum),
					Status: "Validate",
					Repo:   "owner/repo",
					Labels: labels,
				},
			},
		}, nil
	}
}

// TestSHAInvalidation_ClearsLabelsOnSHAMismatch verifies SC-1: when an item
// carries stage:Validate:complete, a recorded completion SHA ("sha-N"), and the
// linked PR's current HEAD SHA is "sha-M" (different), the SHA-invalidation
// scan removes all five FR-3 labels from GitHub (including fabrik:rebase-needed,
// per #1225).
func TestSHAInvalidation_ClearsLabelsOnSHAMismatch(t *testing.T) {
	stgs := testStagesForSHAInvalidation()
	allLabels := []string{
		"stage:Validate:complete",
		"fabrik:auto-merge-enabled",
		"fabrik:awaiting-ci",
		"fabrik:awaiting-review",
		"fabrik:rebase-needed",
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: shaInvalidationBoardFn(42, allLabels),
	}
	eng := testEngineWithStages(t, client, stgs)

	// Record current HEAD SHA = "sha-M" and completion SHA = "sha-N" (mismatch).
	eng.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        "owner/repo",
		Number:      42,
		LinkedPRNum: 101,
		SHA:         "sha-M",
	})
	eng.store.Apply(itemstate.ValidateCompletedAtSHA{
		Repo:   "owner/repo",
		Number: 42,
		SHA:    "sha-N",
	})

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	removed := make(map[string]bool)
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 42 {
			removed[c.labelName] = true
		}
	}
	for _, lbl := range allLabels {
		if !removed[lbl] {
			t.Errorf("expected label %q to be removed on SHA mismatch, but it was not", lbl)
		}
	}
}

// TestSHAInvalidation_NoopOnSameSHA verifies SC-2 convergence: when the linked
// PR's HEAD SHA equals the recorded completion SHA, the scan is a no-op.
func TestSHAInvalidation_NoopOnSameSHA(t *testing.T) {
	stgs := testStagesForSHAInvalidation()
	labels := []string{
		"stage:Validate:complete",
		"fabrik:auto-merge-enabled",
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: shaInvalidationBoardFn(43, labels),
	}
	eng := testEngineWithStages(t, client, stgs)

	// Same SHA in both fields — no mismatch.
	eng.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        "owner/repo",
		Number:      43,
		LinkedPRNum: 102,
		SHA:         "sha-N",
	})
	eng.store.Apply(itemstate.ValidateCompletedAtSHA{
		Repo:   "owner/repo",
		Number: 43,
		SHA:    "sha-N",
	})

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 43 {
			t.Errorf("expected no label removals when SHA matches completion SHA, got removal of %q", c.labelName)
		}
	}
}

// TestSHAInvalidation_NoopOnEmptySHA verifies SC-3 (FR-5): when
// stage:Validate:complete is present but ValidateCompletedSHA is empty
// (pre-feature or worktree-HEAD-unavailable item), the scan leaves the item
// untouched even when the linked PR's HEAD SHA changes.
func TestSHAInvalidation_NoopOnEmptySHA(t *testing.T) {
	stgs := testStagesForSHAInvalidation()
	labels := []string{
		"stage:Validate:complete",
		"fabrik:auto-merge-enabled",
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: shaInvalidationBoardFn(44, labels),
	}
	eng := testEngineWithStages(t, client, stgs)

	// Set HeadSHA but apply no ValidateCompletedAtSHA mutation → completion SHA is "".
	eng.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        "owner/repo",
		Number:      44,
		LinkedPRNum: 103,
		SHA:         "sha-M",
	})

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 44 {
			t.Errorf("expected no label removals for legacy item with empty completion SHA, got removal of %q", c.labelName)
		}
	}
}

// TestSHAInvalidation_NoopWhenWorkerInFlight verifies SC-4 (FR-6): when a
// Validate worker is registered in the store, the scan defers and makes no
// label changes, even when a SHA mismatch is present.
func TestSHAInvalidation_NoopWhenWorkerInFlight(t *testing.T) {
	stgs := testStagesForSHAInvalidation()
	labels := []string{
		"stage:Validate:complete",
		"fabrik:auto-merge-enabled",
		"fabrik:awaiting-ci",
	}
	client := &mockGitHubClient{
		fetchProjectBoardFn: shaInvalidationBoardFn(45, labels),
	}
	eng := testEngineWithStages(t, client, stgs)

	// SHA mismatch would normally trigger invalidation...
	eng.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        "owner/repo",
		Number:      45,
		LinkedPRNum: 104,
		SHA:         "sha-M",
	})
	eng.store.Apply(itemstate.ValidateCompletedAtSHA{
		Repo:   "owner/repo",
		Number: 45,
		SHA:    "sha-N",
	})
	// ...but a Validate worker is in-flight (FR-6 guard).
	eng.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     45,
		User:       "testuser",
		AcquiredAt: time.Now(),
		Worker:     &itemstate.WorkerHandle{StageName: "Validate", StartedAt: time.Now()},
	})

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 45 {
			t.Errorf("expected no label removals while Validate worker in-flight, got removal of %q", c.labelName)
		}
	}
}

// --- #2158: landed/terminal exemption and pure catch-up recognition ---

const shaCUHead = "aaaaaaa1111111"

func shaGuardBoardFn(item gh.ProjectItem) func(string, string, int, string) (*gh.ProjectBoard, error) {
	return func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
		item.ItemID = fmt.Sprintf("PVTI_%d", item.Number)
		item.Repo = "owner/repo"
		return &gh.ProjectBoard{ProjectID: "PVT_1", Items: []gh.ProjectItem{item}}, nil
	}
}

func shaGuardMarkerComment(author, head string, pure bool) gh.Comment {
	return gh.Comment{Author: author, Body: "🏭 **Fabrik merge-train** — caught up\n" + formatCatchUpMarker(catchUpMarker{Head: head, Base: "bbbbbbb2222222", Pure: pure})}
}

// shaGuardRun polls once against an item that completed Validate at "sha-N" and whose
// PR head is head, and returns the labels removed from it.
func shaGuardRun(t *testing.T, item gh.ProjectItem, head string, mutate func(*Engine), comments func(string, string, int) ([]gh.Comment, error)) (*Engine, []string) {
	t.Helper()
	item.Number = 60
	item.Labels = append([]string{"stage:Validate:complete", "fabrik:auto-merge-enabled"}, item.Labels...)
	if item.Status == "" {
		item.Status = "Validate"
	}
	client := &mockGitHubClient{fetchProjectBoardFn: shaGuardBoardFn(item), fetchIssueCommentsFn: comments}
	eng := testEngineWithStages(t, client, testStagesForSHAInvalidation())
	eng.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 60, LinkedPRNum: 201, SHA: head})
	eng.store.Apply(itemstate.ValidateCompletedAtSHA{Repo: "owner/repo", Number: 60, SHA: "sha-N"})
	if mutate != nil {
		mutate(eng)
	}
	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	var removed []string
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 60 {
			removed = append(removed, c.labelName)
		}
	}
	return eng, removed
}

func TestSHAInvalidation_LandedItemsAreExempt(t *testing.T) {
	tests := map[string]struct {
		item   gh.ProjectItem
		mutate func(*Engine)
	}{
		"closed issue":               {item: gh.ProjectItem{IsClosed: true}},
		"landing verification label": {item: gh.ProjectItem{Labels: []string{awaitingLandingVerificationLabel}}},
		"credited pr label":          {item: gh.ProjectItem{Labels: []string{"fabrik:credited-pr:77"}}},
		"linked PR merged": {item: gh.ProjectItem{}, mutate: func(e *Engine) {
			e.store.Apply(itemstate.PRDetailsUpdated{Repo: "owner/repo", Number: 60, PRNumber: 201, State: "closed", Merged: true})
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			eng, removed := shaGuardRun(t, tc.item, "sha-M", tc.mutate, nil)
			// Other poll paths may drop transient labels from a closed issue; the
			// SHA-invalidation scan must leave stage:Validate:complete alone.
			for _, l := range removed {
				if l == "stage:Validate:complete" {
					t.Errorf("landed item must keep stage:Validate:complete, removed %v", removed)
				}
			}
			snap, _ := eng.store.Get("owner/repo", 60)
			if snap.ValidateCompletedSHA() != "sha-N" {
				t.Errorf("completion SHA must be untouched, got %q", snap.ValidateCompletedSHA())
			}
		})
	}
}

func TestSHAInvalidation_CleanupStageIsExempt(t *testing.T) {
	// Mirror the engine's Done stage: a CleanupWorktree stage.
	client := &mockGitHubClient{fetchProjectBoardFn: shaGuardBoardFn(gh.ProjectItem{Number: 60, Status: "Done", Labels: []string{"stage:Validate:complete"}})}
	stgs := append(testStagesForSHAInvalidation(), &stages.Stage{Name: "Done", Order: 9, CleanupWorktree: true})
	eng := testEngineWithStages(t, client, stgs)
	eng.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 60, LinkedPRNum: 201, SHA: "sha-M"})
	eng.store.Apply(itemstate.ValidateCompletedAtSHA{Repo: "owner/repo", Number: 60, SHA: "sha-N"})
	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == 60 {
			t.Errorf("Done item must keep completion labels, removed %q", c.labelName)
		}
	}
}

func TestSHAInvalidation_PureCatchUpHeadIsRecorded(t *testing.T) {
	comments := func(o, r string, n int) ([]gh.Comment, error) {
		return []gh.Comment{shaGuardMarkerComment("testuser", shaCUHead, true)}, nil
	}
	eng, removed := shaGuardRun(t, gh.ProjectItem{}, shaCUHead, nil, comments)
	if len(removed) != 0 {
		t.Fatalf("pure catch-up head must not invalidate, removed %v", removed)
	}
	snap, _ := eng.store.Get("owner/repo", 60)
	if snap.ValidateCompletedSHA() != shaCUHead {
		t.Errorf("completion SHA = %q, want the catch-up head %q", snap.ValidateCompletedSHA(), shaCUHead)
	}

	// A later external push moves the head again: it is invalidated (R3).
	eng.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 60, LinkedPRNum: 201, SHA: "ccccccc3333333"})
	eng.settleSHAInvalidationScan([]gh.ProjectItem{{Number: 60, Repo: "owner/repo", Status: "Validate", Labels: []string{"stage:Validate:complete"}}})
	snap, _ = eng.store.Get("owner/repo", 60)
	if snap.ValidateCompletedSHA() != "" {
		t.Errorf("later external push must clear the completion SHA, got %q", snap.ValidateCompletedSHA())
	}
}

func TestSHAInvalidation_NonEngineCatchUpStillInvalidates(t *testing.T) {
	tests := map[string]func(string, string, int) ([]gh.Comment, error){
		"marker from another author": func(o, r string, n int) ([]gh.Comment, error) {
			return []gh.Comment{shaGuardMarkerComment("mallory", shaCUHead, true)}, nil
		},
		"marker names a different head": func(o, r string, n int) ([]gh.Comment, error) {
			return []gh.Comment{shaGuardMarkerComment("testuser", "ddddddd4444444", true)}, nil
		},
		"pure=false": func(o, r string, n int) ([]gh.Comment, error) {
			return []gh.Comment{shaGuardMarkerComment("testuser", shaCUHead, false)}, nil
		},
		"trailer only, no marker": func(o, r string, n int) ([]gh.Comment, error) {
			return []gh.Comment{{Author: "testuser", Body: catchUpTrailerKey + ": " + shaCUHead}}, nil
		},
	}
	for name, comments := range tests {
		t.Run(name, func(t *testing.T) {
			_, removed := shaGuardRun(t, gh.ProjectItem{}, shaCUHead, nil, comments)
			if len(removed) == 0 {
				t.Error("expected invalidation, no labels removed")
			}
		})
	}
}

func TestSHAInvalidation_EmptySelfLoginStillInvalidates(t *testing.T) {
	comments := func(o, r string, n int) ([]gh.Comment, error) {
		return []gh.Comment{shaGuardMarkerComment("", shaCUHead, true)}, nil
	}
	_, removed := shaGuardRun(t, gh.ProjectItem{}, shaCUHead, func(e *Engine) { e.cfg.User = "" }, comments)
	if len(removed) == 0 {
		t.Error("empty self login must fail closed (invalidate)")
	}
}

func TestSHAInvalidation_MarkerReadErrorDefers(t *testing.T) {
	comments := func(o, r string, n int) ([]gh.Comment, error) { return nil, fmt.Errorf("boom") }
	eng, removed := shaGuardRun(t, gh.ProjectItem{}, shaCUHead, nil, comments)
	if len(removed) != 0 {
		t.Errorf("unreadable marker must defer, removed %v", removed)
	}
	snap, _ := eng.store.Get("owner/repo", 60)
	if snap.ValidateCompletedSHA() != "sha-N" {
		t.Errorf("completion SHA must be untouched on deferral, got %q", snap.ValidateCompletedSHA())
	}
}

// A catch-up pushes before it posts its marker; while it is in flight the unmarked moved
// head must not be read as external, and once the marker window closes the usual rules apply.
func TestSHAInvalidation_DefersWhileCatchUpPushInFlight(t *testing.T) {
	noMarker := func(o, r string, n int) ([]gh.Comment, error) { return nil, nil }
	var end func()
	eng, removed := shaGuardRun(t, gh.ProjectItem{}, shaCUHead, func(e *Engine) {
		end = e.beginCatchUpPush("owner", "repo", 201)
	}, noMarker)
	if len(removed) != 0 {
		t.Fatalf("catch-up in flight must defer invalidation, removed %v", removed)
	}
	snap, _ := eng.store.Get("owner/repo", 60)
	if snap.ValidateCompletedSHA() != "sha-N" {
		t.Errorf("completion SHA must be untouched while deferred, got %q", snap.ValidateCompletedSHA())
	}

	end()
	end() // idempotent
	if eng.catchUpPushInFlight("owner", "repo", 201) {
		t.Fatal("in-flight mark must clear after end")
	}
	eng.settleSHAInvalidationScan([]gh.ProjectItem{{Number: 60, Repo: "owner/repo", Status: "Validate", Labels: []string{"stage:Validate:complete", "fabrik:auto-merge-enabled"}}})
	snap, _ = eng.store.Get("owner/repo", 60)
	if snap.ValidateCompletedSHA() != "" {
		t.Errorf("without a marker the head is external once the catch-up is no longer in flight; completion SHA = %q", snap.ValidateCompletedSHA())
	}
}

func TestCatchUpPushInFlight_CountsOverlap(t *testing.T) {
	eng := testEngineWithStages(t, &mockGitHubClient{}, testStagesForSHAInvalidation())
	a := eng.beginCatchUpPush("o", "r", 5)
	b := eng.beginCatchUpPush("o", "r", 5)
	a()
	if !eng.catchUpPushInFlight("o", "r", 5) {
		t.Error("second overlapping catch-up must keep the PR in flight")
	}
	b()
	if eng.catchUpPushInFlight("o", "r", 5) {
		t.Error("PR must not be in flight after both ended")
	}
}

// TestSHAInvalidation_ResetsCommentBreaker pins #1575: SHA invalidation clears
// the #1089 comment breaker together with the cycle counters.
func TestSHAInvalidation_ResetsCommentBreaker(t *testing.T) {
	item := gh.ProjectItem{Number: 42, ItemID: "PVTI_42", Status: "Validate", Repo: "owner/repo",
		Labels: []string{"stage:Validate:complete"}}
	eng := testEngineWithStages(t, &mockGitHubClient{}, testStagesForSHAInvalidation())
	seedBreakerAndCycles(t, eng, item.Number)

	eng.handleValidateSHAInvalidation(item, "owner", "repo")

	assertBreakerAndCycles(t, eng, item, true)
}

// TestSHAInvalidation_DefersCommentBreakerResetWithCycles pins #1575 R3: a
// failed label removal applies neither reset; the retry applies both.
func TestSHAInvalidation_DefersCommentBreakerResetWithCycles(t *testing.T) {
	item := gh.ProjectItem{Number: 42, ItemID: "PVTI_42", Status: "Validate", Repo: "owner/repo",
		Labels: []string{"stage:Validate:complete"}}
	fail := true
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, n int, label string) error {
			if fail && label == "stage:Validate:complete" {
				return errors.New("boom")
			}
			return nil
		},
	}
	eng := testEngineWithStages(t, client, testStagesForSHAInvalidation())
	seedBreakerAndCycles(t, eng, item.Number)

	eng.handleValidateSHAInvalidation(item, "owner", "repo")
	assertBreakerAndCycles(t, eng, item, false)

	fail = false
	eng.handleValidateSHAInvalidation(item, "owner", "repo")
	assertBreakerAndCycles(t, eng, item, true)
}
