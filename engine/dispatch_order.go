package engine

import (
	"sort"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// orderDispatchCandidates sorts stage-dispatch candidates by time without a
// worker, longest first (FR-006, #2232): the oldest last-attempt first, items
// that have never run before all others, ties broken by issue number so the
// order is deterministic. Free worker slots are handed out in this order, not in
// board order, so an item that has just been sliced (turn cap, max_wall_time),
// and so is re-dispatched with no backoff, goes behind every candidate that has
// waited longer rather than taking its own slot straight back. A deferred
// candidate (#2114) is re-admitted by the next poll and sorted by the same rule,
// so a deferral never moves it to the back. The input slice is not modified.
// Holding stages, settle scans and merge-train dispatch do not pass through here.
func (e *Engine) orderDispatchCandidates(cands []gh.ProjectItem) []gh.ProjectItem {
	if dispatchOrderDisabled.Load() || len(cands) < 2 {
		return cands
	}
	type keyed struct {
		item gh.ProjectItem
		at   time.Time
	}
	ks := make([]keyed, len(cands))
	for i, c := range cands {
		ks[i] = keyed{item: c}
		s := stages.FindStage(e.cfg.Stages, c.Status)
		if s == nil {
			continue
		}
		if snap, ok := e.store.Peek(itemOwnerRepoString(c, e.defaultRepo()), c.Number); ok {
			ks[i].at = snap.LastAttemptAt(s.Name)
		}
	}
	sort.SliceStable(ks, func(i, j int) bool {
		if !ks[i].at.Equal(ks[j].at) {
			return ks[i].at.Before(ks[j].at) // zero (never ran) sorts first
		}
		return ks[i].item.Number < ks[j].item.Number
	})
	out := make([]gh.ProjectItem, len(ks))
	for i, k := range ks {
		out[i] = k.item
	}
	return out
}
