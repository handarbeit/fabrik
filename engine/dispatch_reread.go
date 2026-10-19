package engine

import (
	"reflect"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// refreshDispatchItem re-reads a dispatch candidate from the Store immediately
// before dispatch decides on it (#2350, ADR 2350, audit §3.1). The candidate is a
// copy captured by selectDeepFetchCandidates at the start of the poll; Phase 1,
// Phase 2 and the settle scans then pause, relabel and revalidate items, and only
// the Store carries those changes. The returned item is the copy with the facts
// those phases can change overlaid from the Store: labels, board Status, closed
// state, comments (additive only — a comment the copy lacks is added, none is
// dropped) and blockedBy. Every other field (the linked-PR block, the DeepFetch
// markers, ...) stays as captured, so the item handed on to processItem is one
// consistent value: the stage chosen from Status and the decision agree.
//
// changed reports whether the overlay altered anything. An item the Store does
// not hold, an engine with the re-read disabled (test seam) and a store record
// with no populated labels return the copy unchanged.
func (e *Engine) refreshDispatchItem(item gh.ProjectItem) (gh.ProjectItem, bool) {
	if e.dispatchRereadOff.Load() {
		return item, false
	}
	snap, ok := e.store.Peek(itemOwnerRepoString(item, e.defaultRepo()), item.Number)
	if !ok {
		return item, false
	}
	st := snap.State()
	out := item
	if snap.LabelsKnown() && !sameLabelSet(item.Labels, st.Labels) {
		out.Labels = append([]string(nil), st.Labels...)
	}
	if st.Status != "" && st.Status != item.Status {
		out.Status = st.Status
	}
	if st.State != "" && st.IsClosed != item.IsClosed {
		out.IsClosed = st.IsClosed
	}
	if added := commentsMissingFrom(item.Comments, st.Comments); len(added) > 0 {
		out.Comments = append(append([]gh.Comment(nil), item.Comments...), added...)
	}
	if !st.LastDeepFetchAt.IsZero() && !reflect.DeepEqual(item.BlockedBy, st.BlockedBy) && !(len(item.BlockedBy) == 0 && len(st.BlockedBy) == 0) {
		out.BlockedBy = append([]gh.Dependency(nil), st.BlockedBy...)
	}
	changed := !sameLabelSet(item.Labels, out.Labels) ||
		item.Status != out.Status ||
		item.IsClosed != out.IsClosed ||
		len(item.Comments) != len(out.Comments) ||
		len(item.BlockedBy) != len(out.BlockedBy) ||
		!reflect.DeepEqual(item.BlockedBy, out.BlockedBy)
	return out, changed
}

// overlayStoreFacts returns item, a copy just read live by liveItem, with the
// facts the Store holds on top of the raw read (#2350). A live deep fetch keeps in
// the Store a comment recorded after the read began (a webhook delivery mid-read);
// the raw copy lacks it, so a gate deciding on the copy alone would miss it. The
// overlay is additive for comments (by node or database ID), takes blockedBy from
// the Store once a deep fetch has populated it, and only ADDS labels: a label the
// Store holds that the read lacks (an engine write the read predates) is added, but
// a label the read shows is never hidden, so a fail-closed guard such as the pause
// guard still sees a pause GitHub reports even when an engine removal is
// protected in the Store (see liveLabels). Status and closed state are left as read.
func (e *Engine) overlayStoreFacts(item gh.ProjectItem) gh.ProjectItem {
	if e.liveOverlayOff.Load() {
		return item
	}
	snap, ok := e.store.Peek(itemOwnerRepoString(item, e.defaultRepo()), item.Number)
	if !ok {
		return item
	}
	st := snap.State()
	out := item
	if added := commentsMissingFrom(item.Comments, st.Comments); len(added) > 0 {
		out.Comments = append(append([]gh.Comment(nil), item.Comments...), added...)
	}
	if !st.LastDeepFetchAt.IsZero() && !(len(item.BlockedBy) == 0 && len(st.BlockedBy) == 0) {
		out.BlockedBy = append([]gh.Dependency(nil), st.BlockedBy...)
	}
	if snap.LabelsKnown() {
		var extra []string
		for _, l := range st.Labels {
			if !hasLabel(item.Labels, l) {
				extra = append(extra, l)
			}
		}
		if len(extra) > 0 {
			out.Labels = append(append([]string(nil), item.Labels...), extra...)
		}
	}
	return out
}

// SetLiveOverlayDisabledForTest makes the gates decide on the raw live copy, as
// before the Store overlay. Test seam only.
func (e *Engine) SetLiveOverlayDisabledForTest(off bool) { e.liveOverlayOff.Store(off) }

// SetDispatchRereadDisabledForTest restores the pre-#2350 dispatch decision on
// the poll-start copy. Test seam only (tests/sim): it shows a scenario is not
// vacuous.
func (e *Engine) SetDispatchRereadDisabledForTest(off bool) { e.dispatchRereadOff.Store(off) }

func sameLabelSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, l := range a {
		seen[l]++
	}
	for _, l := range b {
		seen[l]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// commentsMissingFrom returns the comments of store that cur does not contain,
// matched by node ID or database ID.
func commentsMissingFrom(cur, store []gh.Comment) []gh.Comment {
	if len(store) == 0 {
		return nil
	}
	haveID := make(map[string]bool, len(cur))
	haveDB := make(map[int]bool, len(cur))
	for _, c := range cur {
		if c.ID != "" {
			haveID[c.ID] = true
		}
		if c.DatabaseID != 0 {
			haveDB[c.DatabaseID] = true
		}
	}
	var out []gh.Comment
	for _, c := range store {
		if (c.ID != "" && haveID[c.ID]) || (c.DatabaseID != 0 && haveDB[c.DatabaseID]) {
			continue
		}
		if c.ID == "" && c.DatabaseID == 0 {
			continue // unidentifiable: cannot be deduplicated, so never added
		}
		out = append(out, c)
	}
	return out
}

// dispatchSkipReason names why a re-read candidate is no longer dispatched, for
// the single log line of the skip.
func dispatchSkipReason(before, after gh.ProjectItem) string {
	var parts []string
	if hasLabel(after.Labels, "fabrik:paused") && !hasLabel(before.Labels, "fabrik:paused") {
		parts = append(parts, "paused")
	}
	if hasLabel(after.Labels, "fabrik:blocked") && !hasLabel(before.Labels, "fabrik:blocked") {
		parts = append(parts, "blocked")
	}
	if after.IsClosed && !before.IsClosed {
		parts = append(parts, "closed")
	}
	if after.Status != before.Status {
		parts = append(parts, "moved to "+after.Status)
	}
	if len(parts) == 0 {
		parts = append(parts, "state changed earlier in this poll")
	}
	return strings.Join(parts, ", ")
}
