package engine

import (
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
func (e *Engine) applyDeepFetched(repo string, item gh.ProjectItem) {
	if e.cache() == nil {
		e.store.Apply(itemstate.ShallowBoardItemUpdated{Repo: repo, Number: item.Number, Item: item})
	}
	e.store.Apply(itemstate.ItemDeepFetched{Repo: repo, Number: item.Number, FreshState: item})
}
