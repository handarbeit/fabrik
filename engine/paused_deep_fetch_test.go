package engine

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Tests for #1379: fabrik:paused suppresses deep-fetch admission (the
// FetchItemDetails call), not just dispatch. These call selectDeepFetchCandidates
// directly, following the pattern in poll_helpers_test.go, so each test isolates
// the admission loop without driving the full poll() cycle.
//
// AC4's criterion (a) [cycleSet membership] and (b) [cleanup stage] are already
// covered for non-paused items by TestSelectDeepFetchCandidates_DeepFetchesEligibleItem
// and TestSelectDeepFetchCandidates_CleanupStageSkipsDeepFetch in poll_helpers_test.go;
// criteria (c), (d), (e) are covered below.

// AC1 — a paused item admitted only via an expired periodic cooldown (criterion
// (d), no cycleSet entry, no bypass label) is not deep-fetched. Asserts on the
// absence of the FetchItemDetails call itself (mock call count), not merely on
// no stage dispatching — the issue's own Verification Note calls out that the
// latter would pass today and prove nothing.
func TestSelectDeepFetchCandidates_PausedItemSkipsFetch(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	eng.store.Apply(itemstate.CooldownRecorded{
		Repo: "owner/repo", Number: 50, Reason: "periodic-re-eval", Until: time.Now().Add(-time.Minute),
	})

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 50, Title: "Parked item", Status: "Research", Labels: []string{"fabrik:paused"}},
		},
	}

	candidates, deepFetched := eng.selectDeepFetchCandidates(board, "", map[string]bool{}, map[string]bool{})

	client.mu.Lock()
	fetchCalls := len(client.fetchItemDetailsCalls)
	client.mu.Unlock()
	if fetchCalls != 0 {
		t.Errorf("FetchItemDetails must not be called for a paused item with no new activity, got %d call(s)", fetchCalls)
	}
	if deepFetched != 0 {
		t.Errorf("deepFetched = %d, want 0", deepFetched)
	}
	// List membership must be preserved even though the fetch was skipped —
	// runValidatePRTerminalAdvance (ADR-056 D2) and settleRevalidateScan (FR-5)
	// depend on paused items still appearing in deepFetchCandidates (R3/R4).
	if len(candidates) != 1 || candidates[0].Number != 50 {
		t.Fatalf("expected the paused item to remain a candidate (list membership only), got %+v", candidates)
	}
}

// AC3 — a paused item that also carries a bypass label (fabrik:awaiting-review)
// is still not deep-fetched: paused must win over the bypass, or the change
// accomplishes nothing for the common parked-with-outstanding-review case. Must
// fail against unmodified main, since today this item is admitted via
// criterion (c) (hasAwaitingLabel) before ever reaching a paused check.
func TestSelectDeepFetchCandidates_PausedWinsOverBypassLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// Establish a store baseline so the new gate's "already known" branch is
	// exercised, rather than its separate notInStore first-sighting fallback
	// (covered by TestSelectDeepFetchCandidates_PausedNotInStoreGetsBaselineFetch).
	eng.store.Apply(itemstate.CooldownRecorded{
		Repo: "owner/repo", Number: 52, Reason: "prior-poll", Until: time.Now().Add(-time.Hour),
	})

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 52, Title: "Parked item", Status: "Research", Labels: []string{"fabrik:paused", "fabrik:awaiting-review"}},
		},
	}

	candidates, deepFetched := eng.selectDeepFetchCandidates(board, "", map[string]bool{}, map[string]bool{})

	client.mu.Lock()
	fetchCalls := len(client.fetchItemDetailsCalls)
	client.mu.Unlock()
	if fetchCalls != 0 {
		t.Errorf("paused must win over the fabrik:awaiting-review bypass label — expected no FetchItemDetails call, got %d", fetchCalls)
	}
	if deepFetched != 0 {
		t.Errorf("deepFetched = %d, want 0", deepFetched)
	}
	if len(candidates) != 1 || candidates[0].Number != 52 {
		t.Fatalf("expected the paused item to remain a list-membership candidate, got %+v", candidates)
	}
}

// AC2 — removing fabrik:paused re-admits the item within one poll cycle, with
// no restart and no other mutation. Simulates the two real-world signals a
// label removal produces: (1) the shallow per-poll board read (FetchProjectBoard,
// which always runs independent of this gate) reflects the new label set
// directly in board.Items[i].Labels, and (2) the label mutation's LabelsChanged
// event lands the item in cycleSet for the following poll — via the webhook
// delta path directly, or via the pause-unaware runProbeAndDeepFetch picking up
// the updatedAt bump and firing ItemDeepFetched earlier in the same poll (see
// #1379 Research). cycleSet membership is seeded directly here, mirroring the
// established pattern in TestConvergencePausedRecovery_PRMerged_AdvancesToDone,
// rather than driving a full webhook/probe round-trip.
func TestSelectDeepFetchCandidates_UnpauseReadmitsWithinOnePoll(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// Establish a store baseline so the paused poll below hits the new gate's
	// "already known, skip" branch rather than the notInStore first-sighting
	// fallback.
	eng.store.Apply(itemstate.CooldownRecorded{
		Repo: "owner/repo", Number: 51, Reason: "prior-poll", Until: time.Now().Add(-time.Hour),
	})

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 51, Title: "Parked item", Status: "Research", Labels: []string{"fabrik:paused", "fabrik:awaiting-review"}},
		},
	}
	iKey := issueKey(board.Items[0], eng.defaultRepo())

	// Poll N: paused, admitted only via the awaiting-review bypass label, no
	// cycleSet entry — the new gate skips the fetch.
	eng.selectDeepFetchCandidates(board, "", map[string]bool{}, map[string]bool{})
	client.mu.Lock()
	before := len(client.fetchItemDetailsCalls)
	client.mu.Unlock()
	if before != 0 {
		t.Fatalf("setup: expected the paused item to be skipped on the first poll, got %d fetch call(s)", before)
	}

	// Operator removes fabrik:paused on GitHub.
	board.Items[0].Labels = []string{"fabrik:awaiting-review"}
	cycleSet := map[string]bool{iKey: true}

	candidates, deepFetched := eng.selectDeepFetchCandidates(board, "", cycleSet, map[string]bool{})

	client.mu.Lock()
	after := len(client.fetchItemDetailsCalls)
	client.mu.Unlock()
	if after != 1 {
		t.Errorf("expected FetchItemDetails to be called once after unpause, got %d total call(s)", after)
	}
	if deepFetched != 1 {
		t.Errorf("deepFetched = %d, want 1", deepFetched)
	}
	if len(candidates) != 1 || candidates[0].Number != 51 {
		t.Fatalf("expected the item to be a fetched candidate after unpause, got %+v", candidates)
	}
}

// AC4 (c) — a non-paused item admitted via a bypass label (fabrik:awaiting-review)
// is deep-fetched exactly as before this change; the new gate only ever applies
// to paused items.
func TestSelectDeepFetchCandidates_BypassLabelAdmitsNonPausedItem(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 53, Title: "Item", Status: "Research", Labels: []string{"fabrik:awaiting-review"}},
		},
	}

	candidates, deepFetched := eng.selectDeepFetchCandidates(board, "", map[string]bool{}, map[string]bool{})

	client.mu.Lock()
	fetchCalls := len(client.fetchItemDetailsCalls)
	client.mu.Unlock()
	if fetchCalls != 1 {
		t.Errorf("expected FetchItemDetails to be called for a non-paused bypass-label item, got %d call(s)", fetchCalls)
	}
	if deepFetched != 1 {
		t.Errorf("deepFetched = %d, want 1", deepFetched)
	}
	if len(candidates) != 1 || candidates[0].Number != 53 {
		t.Fatalf("expected item to be a candidate, got %+v", candidates)
	}
}

// AC4 (d) — a non-paused item admitted via an expired periodic cooldown is
// deep-fetched exactly as before this change.
func TestSelectDeepFetchCandidates_ExpiredCooldownAdmitsNonPausedItem(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	eng.store.Apply(itemstate.CooldownRecorded{
		Repo: "owner/repo", Number: 54, Reason: "periodic-re-eval", Until: time.Now().Add(-time.Minute),
	})

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 54, Title: "Item", Status: "Research"},
		},
	}

	candidates, deepFetched := eng.selectDeepFetchCandidates(board, "", map[string]bool{}, map[string]bool{})

	client.mu.Lock()
	fetchCalls := len(client.fetchItemDetailsCalls)
	client.mu.Unlock()
	if fetchCalls != 1 {
		t.Errorf("expected FetchItemDetails to be called for a non-paused item with an expired cooldown, got %d call(s)", fetchCalls)
	}
	if deepFetched != 1 {
		t.Errorf("deepFetched = %d, want 1", deepFetched)
	}
	if len(candidates) != 1 || candidates[0].Number != 54 {
		t.Fatalf("expected item to be a candidate, got %+v", candidates)
	}
}

// AC4 (e) — a non-paused item not yet recorded in the store (first poll / fresh
// startup) is deep-fetched exactly as before this change, establishing its
// baseline store entry.
func TestSelectDeepFetchCandidates_NotInStoreAdmitsNonPausedItem(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 55, Title: "Item", Status: "Research"},
		},
	}

	candidates, deepFetched := eng.selectDeepFetchCandidates(board, "", map[string]bool{}, map[string]bool{})

	client.mu.Lock()
	fetchCalls := len(client.fetchItemDetailsCalls)
	client.mu.Unlock()
	if fetchCalls != 1 {
		t.Errorf("expected FetchItemDetails to be called for an item not yet in the store, got %d call(s)", fetchCalls)
	}
	if deepFetched != 1 {
		t.Errorf("deepFetched = %d, want 1", deepFetched)
	}
	if len(candidates) != 1 || candidates[0].Number != 55 {
		t.Fatalf("expected item to be a candidate, got %+v", candidates)
	}
}

// A never-before-seen paused item (not yet recorded in the store) still gets a
// one-time baseline fetch, mirroring the notInStore bypass in the pre-filter
// above it — this prevents a first-sighting paused item from being permanently
// stranded without a store baseline in non-cache configurations where
// runProbeAndDeepFetch never runs.
func TestSelectDeepFetchCandidates_PausedNotInStoreGetsBaselineFetch(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 56, Title: "Never-seen paused item", Status: "Research", Labels: []string{"fabrik:paused"}},
		},
	}

	candidates, deepFetched := eng.selectDeepFetchCandidates(board, "", map[string]bool{}, map[string]bool{})

	client.mu.Lock()
	fetchCalls := len(client.fetchItemDetailsCalls)
	client.mu.Unlock()
	if fetchCalls != 1 {
		t.Errorf("expected a one-time baseline fetch for a never-before-seen paused item, got %d call(s)", fetchCalls)
	}
	if deepFetched != 1 {
		t.Errorf("deepFetched = %d, want 1", deepFetched)
	}
	if len(candidates) != 1 || candidates[0].Number != 56 {
		t.Fatalf("expected item to be a candidate, got %+v", candidates)
	}
}

// R2/AC2, the load-bearing half: a paused item WITH new activity must still be
// deep-fetched. This pins the `!cycleSet[iKey]` half of the gate, which the rest
// of this file does not exercise — removing it suppresses the fetch for every
// paused item unconditionally, and the entire engine suite still passes.
//
// The consequence is not a missed optimisation but a stranded issue. A human
// comment on a paused issue is the documented unpause trigger (#1083 — pause is
// an operator kill-switch that a human comment releases). That release runs
// through dispatchCandidates → itemNeedsWork → the isPaused branch, which reads
// item.Comments. Those comments only exist if FetchItemDetails ran. Suppress the
// fetch for a paused item whose updatedAt just moved and the comment is never
// seen, so the issue can never be unpaused by commenting — the operator's only
// in-band resume path, gone silently.
func TestSelectDeepFetchCandidates_PausedWithNewActivityStillFetched(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// Store baseline, so the gate's notInStore first-sighting fallback is not
	// what admits this item — the cycleSet exemption must be doing the work.
	eng.store.Apply(itemstate.CooldownRecorded{
		Repo: "owner/repo", Number: 52, Reason: "prior-poll", Until: time.Now().Add(-time.Hour),
	})

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 52, Title: "Parked item a human just commented on", Status: "Research", Labels: []string{"fabrik:paused"}},
		},
	}
	// A new comment bumps the issue's updatedAt, which is what places the item in
	// cycleSet — the same mechanism the gate's own comment cites.
	cycleSet := map[string]bool{issueKey(board.Items[0], eng.defaultRepo()): true}

	candidates, deepFetched := eng.selectDeepFetchCandidates(board, "", cycleSet, map[string]bool{})

	client.mu.Lock()
	fetchCalls := len(client.fetchItemDetailsCalls)
	client.mu.Unlock()
	if fetchCalls != 1 {
		t.Errorf("a paused item with new activity must still be deep-fetched so itemNeedsWork can see the human comment that unpauses it, got %d FetchItemDetails call(s), want 1", fetchCalls)
	}
	if deepFetched != 1 {
		t.Errorf("deepFetched = %d, want 1", deepFetched)
	}
	if len(candidates) != 1 || candidates[0].Number != 52 {
		t.Fatalf("expected the item to be a fetched candidate, got %+v", candidates)
	}
}
