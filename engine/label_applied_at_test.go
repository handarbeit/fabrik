package engine

import (
	"context"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// TestSettleAwaitingCIScan_LabelAppliedAt_DedupesWithinSinglePass is the direct
// regression test for the duplication #1314 was filed against: within a single
// settleAwaitingCIScan pass, the CIWaitTimeout backstop (ci_settle.go) and
// checkCIGate's classifyCIFromCheckRuns (ci.go) both need "when was
// fabrik:awaiting-ci applied" for the same item. Before the record-at-write
// cache, each queried FetchLabelAppliedAt independently — two live, fully
// paginated REST fetches per item per poll. Now the first call (whichever
// runs first) populates the store's LabelAppliedAt cache and the second is a
// cache hit, so exactly one live call reaches the client.
//
// Paired with a gate-correctness assertion (CIFixCycles advances to 1, mirroring
// TestSettleAwaitingCIScan_CIWaitTimeoutBackstop_NoOpWithinTimeout) so this is
// non-vacuous: a stubbed-out or accidentally-skipped labelAppliedAt call would
// fail this assertion too, not just the call-count one.
func TestSettleAwaitingCIScan_LabelAppliedAt_DedupesWithinSinglePass(t *testing.T) {
	client := ciFailureSettleClient() // fetchLabelAppliedAtFn returns time.Now() — elapsed ≈ 0
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxCiFixCycles = 5

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 60, Repo: "owner/repo", Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}
	advancedItems := make(map[string]bool)

	eng.settleAwaitingCIScan(context.Background(), board, advancedItems)
	eng.wg.Wait()

	client.mu.Lock()
	var awaitingCICalls int
	for _, c := range client.fetchLabelAppliedAtCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			awaitingCICalls++
		}
	}
	client.mu.Unlock()
	if awaitingCICalls != 1 {
		t.Errorf("live FetchLabelAppliedAt(fabrik:awaiting-ci) calls = %d; want exactly 1 — the CIWaitTimeout backstop and checkCIGate both consult the same timestamp in this pass and must share one cache-populating fetch", awaitingCICalls)
	}

	// The normal gate-driven path must still behave correctly: a genuine CI
	// failure dispatches a CI-fix reinvoke exactly as before this change.
	snap, _ := eng.store.Get("owner/repo", 60)
	if got := snap.CIFixCycles("Validate"); got != 1 {
		t.Errorf("CIFixCycles(Validate) = %d; want 1 — gate behavior must be unaffected by the dedup", got)
	}
}

// TestLabelAppliedAt_CrossPollCaching verifies the steady-state benefit this
// issue exists for: once the cache is warm for an item/label, a subsequent
// read — modeling a later poll cycle — makes zero further live REST calls
// while still returning the correct (cached) value.
func TestLabelAppliedAt_CrossPollCaching(t *testing.T) {
	fixedTime := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	client := &mockGitHubClient{
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return fixedTime, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 61, Repo: "owner/repo"}

	// "Poll 1": cold cache, live fetch.
	got1, err := eng.labelAppliedAt(item, "owner", "repo", "fabrik:awaiting-review")
	if err != nil {
		t.Fatalf("labelAppliedAt (poll 1): %v", err)
	}
	if !got1.Equal(fixedTime) {
		t.Errorf("poll 1: labelAppliedAt = %v; want %v", got1, fixedTime)
	}

	// "Poll 2": warm cache — must not call FetchLabelAppliedAt again.
	got2, err := eng.labelAppliedAt(item, "owner", "repo", "fabrik:awaiting-review")
	if err != nil {
		t.Fatalf("labelAppliedAt (poll 2): %v", err)
	}
	if !got2.Equal(fixedTime) {
		t.Errorf("poll 2: labelAppliedAt = %v; want the cached %v", got2, fixedTime)
	}

	client.mu.Lock()
	calls := len(client.fetchLabelAppliedAtCalls)
	client.mu.Unlock()
	if calls != 1 {
		t.Errorf("live FetchLabelAppliedAt calls across two polls = %d; want exactly 1 (only the cold-cache first read)", calls)
	}
}

// TestLabelAppliedAt_ColdStartFallback_SelfHeals verifies the restart-recovery
// path required by the issue: a fresh store (simulating an engine restart,
// where the in-memory LabelAppliedAt cache is empty) still falls through to
// FetchLabelAppliedAt for the first read, and self-heals the cache so later
// reads in the same process are free.
func TestLabelAppliedAt_ColdStartFallback_SelfHeals(t *testing.T) {
	fixedTime := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	client := &mockGitHubClient{
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			return fixedTime, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{}) // fresh store — nothing recorded yet
	item := gh.ProjectItem{Number: 62, Repo: "owner/repo"}

	got, err := eng.labelAppliedAt(item, "owner", "repo", "fabrik:auto-merge-enabled")
	if err != nil {
		t.Fatalf("labelAppliedAt: %v", err)
	}
	if !got.Equal(fixedTime) {
		t.Errorf("labelAppliedAt = %v; want %v (from the cold-start fallback fetch)", got, fixedTime)
	}

	snap, err := eng.store.Get("owner/repo", 62)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if cached := snap.LabelAppliedAt("fabrik:auto-merge-enabled"); !cached.Equal(fixedTime) {
		t.Errorf("cold-start fetch should self-heal the cache; LabelAppliedAt = %v, want %v", cached, fixedTime)
	}

	// A second read must now be a pure cache hit — no additional live call.
	if _, err := eng.labelAppliedAt(item, "owner", "repo", "fabrik:auto-merge-enabled"); err != nil {
		t.Fatalf("labelAppliedAt (second read): %v", err)
	}
	client.mu.Lock()
	calls := len(client.fetchLabelAppliedAtCalls)
	client.mu.Unlock()
	if calls != 1 {
		t.Errorf("live FetchLabelAppliedAt calls = %d; want exactly 1 (the cold-start miss); the self-healed cache should serve the second read", calls)
	}
}

// TestLabelAppliedAt_AppliedRemovedReapplied_ReadsLatest is the correctness
// regression the issue explicitly required: "most recent application," not
// "any application." It exercises the actual guarded write path —
// classifyCIFromCheckRuns's applyLabelAdd on a confirmed CI failure, followed
// by removeAwaitingCILabel (which now clears the cache entry via
// clearLabelAppliedAtNow, see the PR #1369 review fix below), followed by a
// later genuine re-application — and asserts the cache-backed read returns
// the *second* application's timestamp, not the first. fetchLabelAppliedAtFn
// fails the test if ever called, proving this all happens without falling
// back to a live fetch (the cache never needs the live fallback across the
// sequence, even though it does go briefly cold between removal and re-apply).
func TestLabelAppliedAt_AppliedRemovedReapplied_ReadsLatest(t *testing.T) {
	failingCheckRuns := []gh.CheckRun{{Name: "test", Status: "completed", Conclusion: "failure"}}
	client := &mockGitHubClient{
		addLabelToIssueFn:      func(_, _ string, _ int, _ string) error { return nil },
		removeLabelFromIssueFn: func(_, _ string, _ int, _ string) error { return nil },
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			t.Fatal("FetchLabelAppliedAt must not be called: the guarded write path never needs the live fallback")
			return time.Time{}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 63, Repo: "owner/repo"}

	// First application: no fabrik:awaiting-ci label yet, CI check failed —
	// classifyCIFromCheckRuns applies it via the guarded (!hasLabel) write path.
	eng.classifyCIFromCheckRuns("owner", "repo", item, failingCheckRuns)
	snap, _ := eng.store.Get("owner/repo", 63)
	first := snap.LabelAppliedAt("fabrik:awaiting-ci")
	if first.IsZero() {
		t.Fatal("expected fabrik:awaiting-ci LabelAppliedAt to be recorded after the first application")
	}
	item.Labels = append(item.Labels, "fabrik:awaiting-ci")

	time.Sleep(2 * time.Millisecond) // ensure the re-application's timestamp is distinguishable from the first

	// Removal: a genuine removal now clears the cache entry (syncLabelRemoval
	// -> clearLabelAppliedAtNow) rather than leaving it stale. This is what
	// makes recordLabelAppliedAtNow's "don't overwrite an existing entry"
	// guard (the PR #1369 review fix) safe: the entry is only ever non-empty
	// when the label is genuinely still applied, so the guard never blocks a
	// legitimate re-application.
	eng.removeAwaitingCILabel("owner", "repo", item)
	snapAfterRemove, _ := eng.store.Get("owner/repo", 63)
	if got := snapAfterRemove.LabelAppliedAt("fabrik:awaiting-ci"); !got.IsZero() {
		t.Errorf("removal must clear the LabelAppliedAt cache entry; got %v, want zero", got)
	}
	item.Labels = nil // reflect the removal locally, as a fresh re-fetch would

	// Re-application: label absent again (both in item.Labels and in the
	// now-cleared cache), CI still failing — a genuine re-apply, not a
	// defensive idempotent no-op.
	eng.classifyCIFromCheckRuns("owner", "repo", item, failingCheckRuns)
	snap2, _ := eng.store.Get("owner/repo", 63)
	second := snap2.LabelAppliedAt("fabrik:awaiting-ci")
	if !second.After(first) {
		t.Fatalf("expected the re-application timestamp %v to be strictly after the first application %v", second, first)
	}

	// The read path every gate consults must return the latest application,
	// not the first — this is the "most recent application" contract itself.
	got, err := eng.labelAppliedAt(item, "owner", "repo", "fabrik:awaiting-ci")
	if err != nil {
		t.Fatalf("labelAppliedAt: %v", err)
	}
	if !got.Equal(second) {
		t.Errorf("labelAppliedAt = %v; want the most recent application %v (a first-match-wins bug would return the stale %v)", got, second, first)
	}
}

// TestLabelAppliedAt_StaleGuardDoesNotResetTimestamp is the direct regression
// test for the PR #1369 review finding: recordLabelAppliedAtNow's write sites
// guard on item.Labels, an in-memory snapshot that can be stale relative to
// GitHub's true state. Since AddLabelToIssue is idempotent (GitHub silently
// no-ops a duplicate add), a stale snapshot showing a label as absent when
// it's genuinely still present can still reach applyLabelAdd's success path.
// Before this fix, that would blindly overwrite the cache with a bogus
// time.Now(), silently resetting every downstream timeout with no
// corresponding real GitHub event. This test simulates exactly that: the
// label is genuinely applied once (real timestamp cached), then a second
// guarded write site fires again against a stale item.Labels view that still
// shows the label absent — and asserts the cache is left untouched at the
// true, original timestamp rather than being reset to time.Now().
func TestLabelAppliedAt_StaleGuardDoesNotResetTimestamp(t *testing.T) {
	client := &mockGitHubClient{
		addLabelToIssueFn: func(_, _ string, _ int, _ string) error { return nil }, // idempotent no-op on GitHub's side too
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			t.Fatal("FetchLabelAppliedAt must not be called: the cache is warm from the first genuine application")
			return time.Time{}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 64, Repo: "owner/repo"}

	// Genuine first application, recorded with the real timestamp.
	eng.applyLabelAdd(item, "fabrik:awaiting-ci", false)
	snap, _ := eng.store.Get("owner/repo", 64)
	original := snap.LabelAppliedAt("fabrik:awaiting-ci")
	if original.IsZero() {
		t.Fatal("expected fabrik:awaiting-ci LabelAppliedAt to be recorded after the genuine application")
	}

	time.Sleep(2 * time.Millisecond)

	// Simulate a stale-guard re-add: a write site's item.Labels snapshot still
	// shows the label absent (item.Labels was never updated to include it —
	// modeling GraphQL/webhook lag or a pre-write item snapshot), so its
	// !hasLabel guard passes and applyLabelAdd fires again for a label that,
	// on GitHub, was never actually removed.
	eng.applyLabelAdd(item, "fabrik:awaiting-ci", false)

	snapAfter, _ := eng.store.Get("owner/repo", 64)
	if got := snapAfter.LabelAppliedAt("fabrik:awaiting-ci"); !got.Equal(original) {
		t.Errorf("stale-guard re-add must not reset the cache; got %v, want the untouched original %v", got, original)
	}
}
