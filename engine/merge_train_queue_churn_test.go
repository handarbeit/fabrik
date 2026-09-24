package engine

import (
	"fmt"
	"testing"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// This file is #1833's AC3 non-vacuity proof, exercised against the actual
// mechanism that produces the churn — boardcache.CacheImpl.FetchProjectBoard
// reading itemstate.Store.All(), whose Go map iteration order is randomized
// per call — rather than a synthetic shuffle. tests/sim (ADR-1449) cannot host
// this scenario: its harness always wires the bare boardcache.GitHubAdapter
// pass-through (NewWithDeps's own doc comment), which reads simgh's project
// board via a slice preserving insertion order (tests/sim/simgh/board.go's
// liveItemRefs) — fully deterministic regardless of the sort toggle, so a
// poll-driven sim scenario built on it can never actually reproduce the
// map-order churn this issue fixes. This integration test wires the same
// boardcache.CacheImpl + itemstate.Store pairing production uses (see
// engine.New, ADR-1772/ADR-1833 precedent in poll_test.go's
// TestGroupQueuedByRepoAndBase_UnhydratedItem_ExcludedNotPinned and siblings)
// directly, which is both where the real bug lives and, unlike a sim poll
// scenario, actually capable of demonstrating it.

// churnTestQueueSize exceeds effectiveMaxBatchSize's default (5) so a
// selection is actually a truncation, not a no-op pass-through of the whole
// set — the shape #1833's issue itself reports the churn requires.
const churnTestQueueSize = 8

// churnTestRepo/Status are arbitrary; the test cares only about relative
// ordering, never these literal values.
const (
	churnTestRepo   = "owner/repo"
	churnTestStatus = "BatchHold"
)

// seedChurnTestItems populates eng.store with n deep-fetched Queued members via
// itemstate.ItemDeepFetched — the same event production's own deep-fetch path
// applies (engine/terminal.go), routed through applyProjectItem so each
// genuine ""→Queued transition stamps StatusEnteredAt (#1833) exactly as it
// would in production. Sequential calls in a tight loop may occasionally share
// a StatusEnteredAt value (real wall-clock resolution); Number remains a total,
// unique tie-break regardless, so the sorted path's determinism claim never
// depends on the timestamps actually differing.
func seedChurnTestItems(eng *Engine, n int) {
	for i := 1; i <= n; i++ {
		eng.store.Apply(itemstate.ItemDeepFetched{
			Repo:   churnTestRepo,
			Number: i,
			FreshState: gh.ProjectItem{
				Number: i,
				Status: churnTestStatus,
				Repo:   churnTestRepo,
			},
		})
	}
}

// capBatchMembers runs the given items through capBatch at effectiveMaxBatchSize's
// default (5, matching production's own default and this file's churnTestQueueSize
// margin) and returns the selected issue Numbers.
func capBatchMembers(items []gh.ProjectItem) []int {
	capped := capBatch(items, 5)
	nums := make([]int, len(capped))
	for i, it := range capped {
		nums[i] = it.Number
	}
	return nums
}

// memberSetsEqual reports whether a and b contain the same issue Numbers,
// ignoring order — what "the same batch was selected" means for this test,
// independent of any incidental internal ordering.
func memberSetsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	inA := make(map[int]bool, len(a))
	for _, n := range a {
		inA[n] = true
	}
	for _, n := range b {
		if !inA[n] {
			return false
		}
	}
	return true
}

// TestMergeTrainQueueChurn_UnsortedSelectionVariesAcrossCalls is AC3's
// non-vacuity proof: feeding boardcache.CacheImpl.FetchProjectBoard's real,
// store-map-order-dependent output through groupQueuedItemsByRepoUnordered
// (the pre-sort core, i.e. the sort "neutralised") and capBatch, repeated
// over many calls against an unchanged Queued set, selects different
// membership at least once. Without this test actually failing against the
// unsorted code path, TestMergeTrainQueueChurn_SortedSelectionIsStable below
// would prove nothing — it could pass merely because nothing ever varies.
func TestMergeTrainQueueChurn_UnsortedSelectionVariesAcrossCalls(t *testing.T) {
	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo", MaxConcurrent: 1, Stages: testStages()}, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	cache := boardcache.NewCacheImpl(&mockGitHubClient{}, eng.store, func(string, ...any) {})
	eng.readClient = cache
	seedChurnTestItems(eng, churnTestQueueSize)

	var selections [][]int
	const calls = 40
	for i := 0; i < calls; i++ {
		board, err := eng.readClient.FetchProjectBoard("owner", "repo", 1, "organization")
		if err != nil {
			t.Fatalf("call %d: FetchProjectBoard: %v", i, err)
		}
		groups := groupQueuedItemsByRepoUnordered(board.Items, churnTestStatus, churnTestRepo)
		if len(groups) != 1 {
			t.Fatalf("call %d: expected 1 group, got %d", i, len(groups))
		}
		selections = append(selections, capBatchMembers(groups[0].items))
	}

	differed := false
	for i := 1; i < len(selections); i++ {
		if !memberSetsEqual(selections[0], selections[i]) {
			differed = true
			break
		}
	}
	if !differed {
		t.Fatalf("expected churn: unsorted selection must differ across at least one of %d calls against an unchanged Queued set — got identical membership every time: %v", calls, selections[0])
	}
}

// TestMergeTrainQueueChurn_SortedSelectionIsStable is AC1/AC2 exercised at the
// same real store/cache integration level as the churn-reproduction test
// above: with the deterministic (StatusEnteredAt, Number) sort applied (the
// production default, via groupQueuedByRepoAndBase), repeated
// FetchProjectBoard/capBatch calls against the same unchanged, over-capacity
// Queued set select the identical batch every time.
func TestMergeTrainQueueChurn_SortedSelectionIsStable(t *testing.T) {
	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo", MaxConcurrent: 1, Stages: testStages()}, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	cache := boardcache.NewCacheImpl(&mockGitHubClient{}, eng.store, func(string, ...any) {})
	eng.readClient = cache
	seedChurnTestItems(eng, churnTestQueueSize)

	var first []int
	const calls = 40
	for i := 0; i < calls; i++ {
		board, err := eng.readClient.FetchProjectBoard("owner", "repo", 1, "organization")
		if err != nil {
			t.Fatalf("call %d: FetchProjectBoard: %v", i, err)
		}
		groups := eng.groupQueuedByRepoAndBase(board.Items, churnTestStatus, churnTestRepo)
		if len(groups) != 1 {
			t.Fatalf("call %d: expected 1 partition, got %d: %+v", i, len(groups), groups)
		}
		got := capBatchMembers(groups[0].items)
		if i == 0 {
			first = got
			continue
		}
		if !memberSetsEqual(first, got) {
			t.Fatalf("expected identical batch membership across every call with the sort applied (#1833) — call 0 selected %v, call %d selected %v",
				first, i, got)
		}
	}
	if len(first) != 5 {
		t.Fatalf("expected a capped batch of 5, got %d: %v", len(first), first)
	}
	// The sorted selection must also be the actual lowest-Number five: every
	// seeded item's StatusEnteredAt was stamped in ascending Number order
	// (seedChurnTestItems), so a correct (StatusEnteredAt, Number) sort always
	// resolves to #1-#5 here regardless of ties.
	want := fmt.Sprintf("%v", []int{1, 2, 3, 4, 5})
	got := fmt.Sprintf("%v", first)
	if got != want {
		t.Errorf("expected the five longest-waiting members %s, got %s", want, got)
	}
}
