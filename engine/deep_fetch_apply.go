package engine

import (
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// applyDeepFetched records a successful FetchItemDetails result in the Store.
//
// item is the poll-start board listing entry the fetch filled in place, so its
// Status (always) and its labels and closed state (on a cache hit) are the
// listing's, not fresh. ItemDeepFetched therefore writes only what a deep fetch
// produced and leaves Status, labels and closed state to the listing paths and
// their engine-write protection (#2344, ADR 2344) — it can no longer revert a
// webhook or engine write that landed after the listing.
//
// An engine with no board cache has no listing path at all: no probe, no
// reconcile, no webhook. There the poll-start listing is the only source of
// Status and labels, so it is applied here through the listing mutation first
// (unprotected, exactly what the old write-back did). Production always has the
// cache (New), so this branch serves the bare-adapter engines NewWithDeps builds.
//
// The live-read markers (DeepFetchLive, DeepFetchStartedAt) describe the fetch
// that just filled item, so they are cleared once the result is applied: the
// caller's copy lives on for the rest of the poll, and applying it again later
// must not present its labels and closed state as freshly read.
func (e *Engine) applyDeepFetched(repo string, item *gh.ProjectItem) {
	if e.cache() == nil {
		e.store.Apply(itemstate.ShallowBoardItemUpdated{Repo: repo, Number: item.Number, Item: *item})
	}
	e.store.Apply(itemstate.ItemDeepFetched{Repo: repo, Number: item.Number, FreshState: *item})
	item.DeepFetchLive = false
	item.DeepFetchStartedAt = time.Time{}
}

// SetLegacyDeepFetchApplyForTest restores the pre-#2344 ItemDeepFetched apply on
// the engine's store (the whole poll-start snapshot written back, baselines
// assigned unconditionally) so a scenario can show it is not vacuous. Test seam
// only (tests/sim); production never calls this.
func (e *Engine) SetLegacyDeepFetchApplyForTest(on bool) { e.store.SetLegacyDeepFetchApplyForTest(on) }

// SetLegacyLocalClockSelfWriteForTest restores the pre-#1945 SelfWriteObserved
// apply (the baseline becomes the daemon's time.Now() instead of the GitHub
// stamp of the write) so a scenario can show its clock-skew assertions bite.
// Test seam only (tests/sim); production never calls this.
func (e *Engine) SetLegacyLocalClockSelfWriteForTest(on bool) {
	e.store.SetLegacyLocalClockSelfWriteForTest(on)
}

// StoreItemForTest returns the store's record for an item with no GitHub
// fallback. Test seam only (tests/sim); production never calls this.
func (e *Engine) StoreItemForTest(repo string, number int) (itemstate.ItemState, bool) {
	snap, ok := e.store.Peek(repo, number)
	if !ok {
		return itemstate.ItemState{}, false
	}
	return snap.State(), true
}

// SubscribeStoreForTest registers an observer on the engine's store. Test seam
// only (tests/sim); production never calls this.
func (e *Engine) SubscribeStoreForTest(o itemstate.Observer) { e.store.Subscribe(o) }
