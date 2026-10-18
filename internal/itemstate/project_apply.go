package itemstate

import (
	"reflect"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// adoptItemID is the one rule for taking a project-item node ID (PVTI_...)
// from an incoming value (#2345, R4). An empty incoming ID is never taken. A
// known ID is replaced only when authoritative, which is true for the live board
// listing (a genuine re-add of the issue to its board gets a new ID, and the
// listing is how that is recovered) and false for every incremental path, which
// can be fed another project's item identity by an org-level webhook.
func adoptItemID(item *ItemState, incoming string, authoritative bool) {
	if incoming == "" || item.ItemID == incoming {
		return
	}
	if item.ItemID != "" && !authoritative {
		return
	}
	item.ItemID = incoming
}

// applyProjectItem copies all fields from a gh.ProjectItem into item and returns
// ChangeFlags. It never replaces a known item ID (R4, #2345); the authoritative
// board-listing path uses applyProjectItemAuthoritative.
func applyProjectItem(item *ItemState, pi gh.ProjectItem) ChangeFlags {
	return applyProjectItemMode(item, pi, false)
}

// applyProjectItemAuthoritative is applyProjectItem for the live board listing,
// which may replace a known item ID with a different non-empty one.
func applyProjectItemAuthoritative(item *ItemState, pi gh.ProjectItem) ChangeFlags {
	return applyProjectItemMode(item, pi, true)
}

func applyProjectItemMode(item *ItemState, pi gh.ProjectItem, authoritativeItemID bool) ChangeFlags {
	var flags ChangeFlags

	if item.ID != pi.ID && pi.ID != "" {
		item.ID = pi.ID
	}
	adoptItemID(item, pi.ItemID, authoritativeItemID)

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

	flags |= applyLinkedPRFields(item, pi, false, false)

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

// applyDeepFetched applies the result of a deep fetch (live or served from the
// board cache) to item. It writes only the facts a deep fetch produces: title,
// body, URL, author, assignees, blocked-by, comments, milestone, layer evidence
// and the LinkedPR block.
//
// Status is never read by a deep fetch, and a cache hit copies none of Status,
// labels or closed state, so for those the value in pi is the poll-start
// listing's. They are written only as a first population (the Store holds
// nothing yet), after which they reach the Store through the listing paths and
// their engine-write protection. Labels and closed state are the exception when
// pi.DeepFetchLive: a live fetch read them from GitHub just now, and they are
// how a change that arrived without a webhook reaches the Store (the probe
// carries no labels), so they are written — through the engine-write protection
// (protectStatusClosed/protectLabels, the rule every listing path shares), with
// pi.DeepFetchStartedAt as the observation time: an engine write recorded inside
// the grace window of the fetch's start keeps its local value, because the
// response may predate it (or GitHub may still be serving the old read).
// (#2344, ADR 2344)
func applyDeepFetched(item *ItemState, pi gh.ProjectItem, grace time.Duration, now time.Time, st deepStale) ChangeFlags {
	var flags ChangeFlags
	view := deepFetchLiveView(item, pi, grace, now)
	if dropEngineWrites(item, view.Drop) {
		flags |= EngineWriteRecordedChanged
	}
	// State is empty only until the first population: getOrCreate pre-sets
	// Repo/Number, so they cannot tell a new record from a known one.
	closedUnheld := item.State == ""

	if item.ID != pi.ID && pi.ID != "" {
		item.ID = pi.ID
	}
	adoptItemID(item, pi.ItemID, false)

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

	if closedUnheld || pi.DeepFetchLive {
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
	}

	if !pi.LabelsAbsent && (!item.LabelsKnown || pi.DeepFetchLive) {
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

	if item.Status == "" && pi.Status != "" {
		item.Status = pi.Status
		item.StatusEnteredAt = time.Now()
		item.StatusAtGitHub = pi.StatusAtGitHub
		flags |= StatusChanged
	}

	// UpdatedAt is a listing fact: only ever move it forward.
	if pi.UpdatedAt.After(item.UpdatedAt) {
		item.UpdatedAt = pi.UpdatedAt
	}
	applyLayerEvidence(item, pi.IssueLayerUpdatedAt, pi.LinkedPRUpdatedAt, pi.IssueCommentTotal, pi.IssueCommentTotalKnown)

	if pi.MilestoneKnown && !st.milestone {
		flags |= applyMilestone(item, pi.Milestone, true)
	}

	if !st.blockedBy && !reflect.DeepEqual(item.BlockedBy, pi.BlockedBy) {
		item.BlockedBy = copyDeps(pi.BlockedBy)
		flags |= BlockedByChanged
	}

	if !reflect.DeepEqual(item.Comments, pi.Comments) {
		item.Comments = copyComments(pi.Comments)
		flags |= CommentsChanged
	}

	flags |= applyLinkedPRFields(item, pi, st.linkedPR, st.reviews)

	// No first-population shortcut as in applyProjectItem: getOrCreate pre-sets
	// Repo/Number, so an empty Repo cannot be seen here, and a first population
	// already raises StateChanged/LabelsChanged/StatusChanged above.
	item.Repo = pi.Repo
	item.Number = pi.Number
	return flags
}

// deepFetchLiveView is the engine-write protection for the labels and closed
// state of a live deep fetch: the same protectStatusClosed/protectLabels rule the
// reconcile and probe applies use, observed at pi.DeepFetchStartedAt. A cache hit
// (not live) or a fetch with no start stamp protects nothing. Status is not part
// of a deep fetch, so its record is never confirmed or dropped here, and only the
// closed and label records a live read can confirm are returned for dropping.
func deepFetchLiveView(item *ItemState, pi gh.ProjectItem, grace time.Duration, now time.Time) shallowView {
	var v shallowView
	if !pi.DeepFetchLive || pi.DeepFetchStartedAt.IsZero() || len(item.EngineWriteAt) == 0 {
		return v
	}
	sv := protectStatusClosed(item, item.Status, pi.IsClosed, pi.DeepFetchStartedAt, grace, now)
	v.KeepClosed = sv.KeepClosed
	if !pi.LabelsAbsent {
		protectLabels(item, pi, pi.DeepFetchStartedAt, grace, &v)
	}
	for _, k := range sv.Drop {
		if k != engineWriteStatusKey {
			v.Drop = append(v.Drop, k)
		}
	}
	return v
}

// deepStale says which facts of a live deep fetch are older than the Store's
// record of them and must be left alone (#2348, FR-011).
type deepStale struct {
	blockedBy, milestone, linkedPR, reviews bool
}

// applyLinkedPRFields syncs the LinkedPR fields from pi into item and returns
// the flags it raised. Shared by applyProjectItem and applyDeepFetched so there
// is one copy of the field-by-field comparison. skipGroup leaves every field
// except the reviews alone, skipReviews leaves the reviews alone (the observed-at
// rule found them older than the Store's record). The reviews list is stored in
// its one shape, the latest review per author (#2348, R5).
func applyLinkedPRFields(item *ItemState, pi gh.ProjectItem, skipGroup, skipReviews bool) ChangeFlags {
	var flags ChangeFlags
	if pi.LinkedPRNumber == 0 {
		// PR was delinked — only clear the number, preserve any richer state.
		// (Full removal would need an explicit IssuePRDelinked mutation.)
		return flags
	}
	if item.LinkedPR == nil {
		item.LinkedPR = &LinkedPRState{}
		flags |= LinkedPRChanged
	}
	lpr := item.LinkedPR
	if !skipGroup {
		if lpr.Number != pi.LinkedPRNumber {
			lpr.Number = pi.LinkedPRNumber
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
			if e := pi.LinkedPRMergeQueueEntry; e != nil {
				c := *e
				lpr.MergeQueueEntry = &c
			} else {
				lpr.MergeQueueEntry = nil
			}
			flags |= LinkedPRChanged
		}
	}
	if !skipReviews {
		reviews := latestReviewPerAuthor(pi.LinkedPRReviews)
		if !reflect.DeepEqual(lpr.Reviews, reviews) {
			lpr.Reviews = copyPRReviews(reviews)
			flags |= LinkedPRChanged
		}
	}
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
	adoptItemID(item, pi.ItemID, true)

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

	if pi.MilestoneKnown && !observedStale(item, observedMilestoneKey, authorityOf(fetchedAt, true, grace)) {
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
	adoptItemID(item, pi.ItemID, true)

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

// applyStatusRefresh applies a Status read live from GitHub (the Layer 1 status
// refresh, #2347). It is a read, not an engine write:
//
//   - the engine-write protection is the same rule listings use
//     (protectStatusClosed, #2178), evaluated against the item's own closed state
//     so a status-only read never confirms, drops or keeps the closed record;
//   - a read started before the Store last observed a differing move of a held
//     Status (StatusEnteredAt: any source) is dropped, so a stale read can never
//     undo a newer move;
//   - an equal read applies nothing;
//   - it never calls recordEngineWrite, and the Change it yields is not marked as
//     an engine move, so it is not attributed to Fabrik;
//   - the read carries no GitHub move time, so on a change StatusAtGitHub becomes
//     unknown (zero) rather than keeping the previous column's time.
func applyStatusRefresh(item *ItemState, status string, fetchedAt time.Time, grace time.Duration, now time.Time) ChangeFlags {
	if status == "" {
		return 0
	}
	var flags ChangeFlags

	view := protectStatusClosed(item, status, item.IsClosed, fetchedAt, grace, now)
	// A status-only read says nothing about closed state: passing the item's own
	// IsClosed makes protectStatusClosed "confirm" the closed record, which this
	// read must not do. Only an expired closed record is dropped.
	drop := view.Drop[:0:0]
	for _, k := range view.Drop {
		if k == engineWriteClosedKey {
			if w, ok := item.EngineWriteAt[k]; ok && now.Before(w.Add(grace)) {
				continue
			}
		}
		drop = append(drop, k)
	}
	if dropEngineWrites(item, drop) {
		flags |= EngineWriteRecordedChanged
	}

	if view.Status == item.Status {
		return flags
	}
	if item.Status != "" && !fetchedAt.IsZero() && fetchedAt.Before(item.StatusEnteredAt) {
		return flags
	}

	item.Status = view.Status
	item.Terminal = false
	item.StatusEnteredAt = now
	item.StatusAtGitHub = time.Time{}
	return flags | StatusChanged
}
