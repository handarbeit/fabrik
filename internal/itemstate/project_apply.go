package itemstate

import (
	"reflect"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// applyProjectItem copies all fields from a gh.ProjectItem into item and returns ChangeFlags.
func applyProjectItem(item *ItemState, pi gh.ProjectItem) ChangeFlags {
	var flags ChangeFlags

	if item.ID != pi.ID && pi.ID != "" {
		item.ID = pi.ID
	}
	if item.ItemID != pi.ItemID {
		item.ItemID = pi.ItemID
	}

	if item.Title != pi.Title || item.Body != pi.Body || item.URL != pi.URL || item.Author != pi.Author {
		item.Title = pi.Title
		item.Body = pi.Body
		item.URL = pi.URL
		item.Author = pi.Author
		flags |= TitleBodyChanged
	}

	if !reflect.DeepEqual(item.Assignees, pi.Assignees) {
		item.Assignees = copyStrings(pi.Assignees)
		flags |= AssigneesChanged
	}

	if item.State != stateFrom(pi) || item.IsClosed != pi.IsClosed || item.IsPR != pi.IsPR {
		item.State = stateFrom(pi)
		item.IsClosed = pi.IsClosed
		item.IsPR = pi.IsPR
		flags |= StateChanged
	}

	if !pi.LabelsAbsent {
		if !reflect.DeepEqual(item.Labels, pi.Labels) {
			item.Labels = copyStrings(pi.Labels)
			flags |= LabelsChanged
		}
		item.LabelsKnown = true
	}

	if item.Status != pi.Status {
		item.Status = pi.Status
		item.StatusEnteredAt = time.Now()
		item.StatusAtGitHub = pi.StatusAtGitHub
		flags |= StatusChanged
	}

	if !item.UpdatedAt.Equal(pi.UpdatedAt) {
		item.UpdatedAt = pi.UpdatedAt
	}
	applyLayerEvidence(item, pi.IssueLayerUpdatedAt, pi.LinkedPRUpdatedAt, pi.IssueCommentTotal, pi.IssueCommentTotalKnown)

	// Copy the milestone only when the producer actually observed it, so write
	// paths that do not carry it (deep fetch, fallback fetch, PR nodes) cannot
	// wipe a captured value (#1967 R10).
	if pi.MilestoneKnown {
		flags |= applyMilestone(item, pi.Milestone, true)
	}

	if !reflect.DeepEqual(item.BlockedBy, pi.BlockedBy) {
		item.BlockedBy = copyDeps(pi.BlockedBy)
		flags |= BlockedByChanged
	}

	if !reflect.DeepEqual(item.Comments, pi.Comments) {
		item.Comments = copyComments(pi.Comments)
		flags |= CommentsChanged
	}

	// Sync LinkedPR fields from ProjectItem.
	if pi.LinkedPRNumber != 0 {
		if item.LinkedPR == nil {
			item.LinkedPR = &LinkedPRState{}
			flags |= LinkedPRChanged
		}
		lpr := item.LinkedPR
		if lpr.Number != pi.LinkedPRNumber ||
			lpr.HeadSHA != "" || // already set by richer fetch — don't overwrite
			!reflect.DeepEqual(lpr.Reviews, pi.LinkedPRReviews) ||
			!reflect.DeepEqual(lpr.ReviewRequests, pi.LinkedPRReviewRequests) ||
			!reflect.DeepEqual(lpr.ThreadComments, pi.LinkedPRReviewThreadComments) ||
			lpr.ResolvedThreadCount != pi.LinkedPRResolvedThreadCount ||
			lpr.IsMergeQueueEnabled != pi.LinkedPRIsMergeQueueEnabled ||
			lpr.IsInMergeQueue != pi.LinkedPRIsInMergeQueue ||
			!reflect.DeepEqual(lpr.MergeQueueEntry, pi.LinkedPRMergeQueueEntry) {
			if lpr.Number != pi.LinkedPRNumber {
				lpr.Number = pi.LinkedPRNumber
				flags |= LinkedPRChanged
			}
			if !reflect.DeepEqual(lpr.Reviews, pi.LinkedPRReviews) {
				lpr.Reviews = copyPRReviews(pi.LinkedPRReviews)
				flags |= LinkedPRChanged
			}
			if !reflect.DeepEqual(lpr.ReviewRequests, pi.LinkedPRReviewRequests) {
				lpr.ReviewRequests = copyReviewRequests(pi.LinkedPRReviewRequests)
				flags |= LinkedPRChanged
			}
			if !reflect.DeepEqual(lpr.ThreadComments, pi.LinkedPRReviewThreadComments) {
				lpr.ThreadComments = copyComments(pi.LinkedPRReviewThreadComments)
				flags |= LinkedPRChanged | CommentsChanged
			}
			if lpr.ResolvedThreadCount != pi.LinkedPRResolvedThreadCount {
				lpr.ResolvedThreadCount = pi.LinkedPRResolvedThreadCount
				flags |= LinkedPRChanged
			}
			if lpr.IsMergeQueueEnabled != pi.LinkedPRIsMergeQueueEnabled {
				lpr.IsMergeQueueEnabled = pi.LinkedPRIsMergeQueueEnabled
				flags |= LinkedPRChanged
			}
			if lpr.IsInMergeQueue != pi.LinkedPRIsInMergeQueue {
				lpr.IsInMergeQueue = pi.LinkedPRIsInMergeQueue
				flags |= LinkedPRChanged
			}
			if !reflect.DeepEqual(lpr.MergeQueueEntry, pi.LinkedPRMergeQueueEntry) {
				lpr.MergeQueueEntry = pi.LinkedPRMergeQueueEntry
				flags |= LinkedPRChanged
			}
		}
	} else if item.LinkedPR != nil && item.LinkedPR.Number != 0 {
		// PR was delinked — only clear the number, preserve any richer state.
		// (Full removal would need an explicit IssuePRDelinked mutation.)
	}

	if flags == 0 && item.Repo == "" {
		// First-time population of identity fields.
		item.Repo = pi.Repo
		item.Number = pi.Number
		return TitleBodyChanged | StatusChanged | LabelsChanged
	}

	item.Repo = pi.Repo
	item.Number = pi.Number

	return flags
}

// applyShallowItem updates only the shallow board fields of item from pi.
// Deep fields (Body, Comments, Assignees, BlockedBy, LinkedPRReviews, etc.)
// are left unchanged. Used by CacheImpl.Reconcile to apply shallow board
// updates without wiping deep-fetched data.
func applyShallowItem(item *ItemState, pi gh.ProjectItem, fetchedAt time.Time, grace time.Duration, now time.Time) ChangeFlags {
	var flags ChangeFlags

	// Engine writes made after the listing was fetched (within the grace
	// margin) keep their local value; see protectShallow (#2159).
	view := protectShallow(item, pi, fetchedAt, grace, now)
	if dropEngineWrites(item, view.Drop) {
		flags |= EngineWriteRecordedChanged
	}

	if item.ID != pi.ID && pi.ID != "" {
		item.ID = pi.ID
	}
	if item.ItemID != pi.ItemID && pi.ItemID != "" {
		item.ItemID = pi.ItemID
	}

	if item.Title != pi.Title {
		item.Title = pi.Title
		flags |= TitleBodyChanged
	}

	if item.URL != pi.URL && pi.URL != "" {
		item.URL = pi.URL
		flags |= TitleBodyChanged
	}

	state, isClosed := stateFrom(pi), pi.IsClosed
	if view.KeepClosed {
		state, isClosed = item.State, item.IsClosed
	}
	if item.State != state || item.IsClosed != isClosed || item.IsPR != pi.IsPR {
		item.State = state
		item.IsClosed = isClosed
		item.IsPR = pi.IsPR
		flags |= StateChanged
	}

	if !pi.LabelsAbsent {
		labels := pi.Labels
		if view.Labels != nil {
			labels = view.Labels
		}
		if view.Labels != nil && sameStringSet(item.Labels, labels) {
			// Only the order differs from the kept local set: leave it untouched.
		} else if !reflect.DeepEqual(item.Labels, labels) {
			item.Labels = copyStrings(labels)
			flags |= LabelsChanged
		}
		item.LabelsKnown = true
	}

	if item.Status != view.Status {
		item.Status = view.Status
		item.Terminal = false
		item.StatusEnteredAt = time.Now()
		item.StatusAtGitHub = pi.StatusAtGitHub
		flags |= StatusChanged
	}

	if !item.UpdatedAt.Equal(pi.UpdatedAt) {
		item.UpdatedAt = pi.UpdatedAt
	}
	applyLayerEvidence(item, pi.IssueLayerUpdatedAt, pi.LinkedPRUpdatedAt, pi.IssueCommentTotal, pi.IssueCommentTotalKnown)

	if pi.MilestoneKnown {
		flags |= applyMilestone(item, pi.Milestone, true)
	}

	return flags
}

// applyMilestone sets item's milestone state and reports MilestoneChanged when
// the (known, title, number) triple differs. The milestone is stored as a copy.
func applyMilestone(item *ItemState, m *gh.Milestone, known bool) ChangeFlags {
	same := item.MilestoneKnown == known
	switch {
	case !same:
	case item.Milestone == nil && m == nil:
	case item.Milestone != nil && m != nil && *item.Milestone == *m:
	default:
		same = false
	}
	if same {
		return 0
	}
	item.MilestoneKnown = known
	if m != nil {
		cp := *m
		item.Milestone = &cp
	} else {
		item.Milestone = nil
	}
	return MilestoneChanged
}

// applyProbeItem updates only the probe-visible fields of item from pi.
// Labels are explicitly NOT updated — the probe query fetches no label data.
// Wiping Labels here would silently discard the cached label set.
//
// Status and closed state honour the engine's recent writes exactly as
// applyShallowItem does (#2178), through the same protectStatusClosed rule: a
// listing fetched before the engine's write plus grace keeps the local value,
// raises no flag and does not reset StatusEnteredAt. Label records are never
// touched, since a probe listing cannot confirm them.
func applyProbeItem(item *ItemState, pi gh.BoardProbeItem, fetchedAt time.Time, grace time.Duration, now time.Time) ChangeFlags {
	var flags ChangeFlags

	view := protectStatusClosed(item, pi.Status, pi.IsClosed, fetchedAt, grace, now)
	if dropEngineWrites(item, view.Drop) {
		flags |= EngineWriteRecordedChanged
	}

	if item.ID != pi.ContentID && pi.ContentID != "" {
		item.ID = pi.ContentID
	}
	if item.ItemID != pi.ItemID && pi.ItemID != "" {
		item.ItemID = pi.ItemID
	}

	isClosed := pi.IsClosed
	if view.KeepClosed {
		isClosed = item.IsClosed
	}
	probeState := "open"
	if isClosed {
		probeState = "closed"
	}
	if item.State != probeState || item.IsClosed != isClosed || item.IsPR != pi.IsPR {
		item.State = probeState
		item.IsClosed = isClosed
		item.IsPR = pi.IsPR
		flags |= StateChanged
	}

	if item.Status != view.Status {
		item.Status = view.Status
		item.Terminal = false
		item.StatusEnteredAt = time.Now()
		item.StatusAtGitHub = pi.StatusAtGitHub
		flags |= StatusChanged
	}

	// UpdatedAt is assigned unconditionally, as before: a lagging listing can
	// move it backwards, which only makes the item look no fresher than it was.
	if !item.UpdatedAt.Equal(pi.EffectiveUpdatedAt) {
		item.UpdatedAt = pi.EffectiveUpdatedAt
	}
	applyLayerEvidence(item, pi.IssueLayerUpdatedAt, pi.LinkedPRUpdatedAt, pi.IssueCommentTotal, pi.IssueCommentTotalKnown)

	// Labels are intentionally not touched here. The probe query contains no
	// label data; setting item.Labels from an empty probe would wipe the
	// cached label set populated by FetchItemDetails or the bootstrap query.

	return flags
}

// applyLayerEvidence copies the layered board evidence (#2180) from a producer
// that captured it. A zero issue layer means the producer did not (a deep or
// fallback fetch built from elsewhere), so the stored evidence is left alone
// rather than wiped; the linked-PR time is copied together with the issue layer
// because zero then genuinely means "no linked PR".
func applyLayerEvidence(item *ItemState, issueLayer, prAt time.Time, commentTotal int, commentTotalKnown bool) {
	if !issueLayer.IsZero() {
		item.IssueLayerUpdatedAt = issueLayer
		item.LinkedPRUpdatedAt = prAt
	}
	if commentTotalKnown {
		item.IssueCommentTotal = commentTotal
		item.IssueCommentTotalKnown = true
	}
}

func stateFrom(pi gh.ProjectItem) string {
	if pi.IsClosed {
		return "closed"
	}
	return "open"
}

// sameStringSet reports whether a and b hold the same strings, ignoring order.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
		if m[x] < 0 {
			return false
		}
	}
	return true
}
