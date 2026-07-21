package itemstate

import (
	"reflect"

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

	if !reflect.DeepEqual(item.Labels, pi.Labels) {
		item.Labels = copyStrings(pi.Labels)
		flags |= LabelsChanged
	}

	if item.Status != pi.Status {
		item.Status = pi.Status
		flags |= StatusChanged
	}

	if !item.UpdatedAt.Equal(pi.UpdatedAt) {
		item.UpdatedAt = pi.UpdatedAt
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
func applyShallowItem(item *ItemState, pi gh.ProjectItem) ChangeFlags {
	var flags ChangeFlags

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

	if item.State != stateFrom(pi) || item.IsClosed != pi.IsClosed || item.IsPR != pi.IsPR {
		item.State = stateFrom(pi)
		item.IsClosed = pi.IsClosed
		item.IsPR = pi.IsPR
		flags |= StateChanged
	}

	if !reflect.DeepEqual(item.Labels, pi.Labels) {
		item.Labels = copyStrings(pi.Labels)
		flags |= LabelsChanged
	}

	if item.Status != pi.Status {
		item.Status = pi.Status
		item.Terminal = false
		flags |= StatusChanged
	}

	if !item.UpdatedAt.Equal(pi.UpdatedAt) {
		item.UpdatedAt = pi.UpdatedAt
	}

	return flags
}

// applyProbeItem updates only the probe-visible fields of item from pi.
// Labels are explicitly NOT updated — the probe query fetches no label data.
// Wiping Labels here would silently discard the cached label set.
func applyProbeItem(item *ItemState, pi gh.BoardProbeItem) ChangeFlags {
	var flags ChangeFlags

	if item.ID != pi.ContentID && pi.ContentID != "" {
		item.ID = pi.ContentID
	}
	if item.ItemID != pi.ItemID && pi.ItemID != "" {
		item.ItemID = pi.ItemID
	}

	probeState := "open"
	if pi.IsClosed {
		probeState = "closed"
	}
	if item.State != probeState || item.IsClosed != pi.IsClosed || item.IsPR != pi.IsPR {
		item.State = probeState
		item.IsClosed = pi.IsClosed
		item.IsPR = pi.IsPR
		flags |= StateChanged
	}

	if item.Status != pi.Status {
		item.Status = pi.Status
		item.Terminal = false
		flags |= StatusChanged
	}

	if !item.UpdatedAt.Equal(pi.EffectiveUpdatedAt) {
		item.UpdatedAt = pi.EffectiveUpdatedAt
	}

	// Labels are intentionally not touched here. The probe query contains no
	// label data; setting item.Labels from an empty probe would wipe the
	// cached label set populated by FetchItemDetails or the bootstrap query.

	return flags
}

func stateFrom(pi gh.ProjectItem) string {
	if pi.IsClosed {
		return "closed"
	}
	return "open"
}
