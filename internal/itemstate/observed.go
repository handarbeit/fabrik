package itemstate

import (
	"sort"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// The observed-at ordering rule (#2348, ADR 2348).
//
// Every write of a GitHub-derived fact carries an authority time on one axis,
// and the Store drops a write whose authority time is older than the last
// applied observation of that fact, whoever made either one:
//
//	engine write                 the write time (daemon clock)
//	listing / probe / refresh /
//	live deep fetch / PR read    fetch start − grace (daemon clock); the grace is
//	                             the listing-lag allowance that #2159 introduced
//	webhook with a GitHub time   that time (updated_at, submitted_at)
//	webhook without one          its receipt time, stamped by the caller
//
// A zero authority time carries no ordering information: it applies and records
// nothing. A tie applies. A fact with no record always applies.
//
// The record of a fact is the later of EngineWriteAt[key] and ObservedAt[key],
// so the engine-write protection of #2159/#2178 is the special case of this rule
// for an engine write against a listing: "fetchedAt < W+grace" is "fetchedAt −
// grace < W".

const (
	observedStatusKey    = engineWriteStatusKey
	observedClosedKey    = engineWriteClosedKey
	observedLabelsKey    = "labels"
	observedHeadKey      = "head"
	observedLinkedPRKey  = "linkedpr"
	observedReviewsKey   = "reviews"
	observedMilestoneKey = "milestone"
	observedBlockedByKey = "blockedBy"
)

// observedReviewAuthorKey is the record key of one reviewer's review. Webhook
// review events are ordered against it, a whole-list deep fetch against both it
// and observedReviewsKey. The set is bounded by the PR's reviewers.
func observedReviewAuthorKey(author string) string { return observedReviewsKey + ":" + author }

// observedLabelRetention is how long a per-label record is kept. Single-valued
// records are bounded by the number of facts and never expire.
const observedLabelRetention = 10 * time.Minute

// recordedAt returns the later of the engine-write and observed records for key,
// zero when the fact has never been observed.
func recordedAt(item *ItemState, key string) time.Time {
	w := item.EngineWriteAt[key]
	if o := item.ObservedAt[key]; o.After(w) {
		return o
	}
	return w
}

// hasRecords reports whether item holds any engine-write or observed record.
func hasRecords(item *ItemState) bool {
	return len(item.EngineWriteAt) > 0 || len(item.ObservedAt) > 0
}

// anyRecordActive reports whether some record still shields item from a listing
// fetched at fetchedAt: fetchedAt < W+grace.
func anyRecordActive(item *ItemState, fetchedAt time.Time, grace time.Duration) bool {
	for _, w := range item.EngineWriteAt {
		if fetchedAt.Before(w.Add(grace)) {
			return true
		}
	}
	for _, w := range item.ObservedAt {
		if fetchedAt.Before(w.Add(grace)) {
			return true
		}
	}
	return false
}

// authorityOf converts a mutation's observation time into its authority time:
// a fetch start is lowered by the listing-lag allowance, any other time is used
// as is. A zero time stays zero (no ordering information).
func authorityOf(at time.Time, fetchStart bool, grace time.Duration) time.Time {
	if at.IsZero() || !fetchStart {
		return at
	}
	return at.Add(-grace)
}

// observedStale reports whether a write to key with authority time auth is older
// than the recorded observation of key and must be dropped. Disabled by the
// ordering seam through the absence of records (see Store.observe).
func observedStale(item *ItemState, key string, auth time.Time) bool {
	if auth.IsZero() {
		return false
	}
	rec := recordedAt(item, key)
	return !rec.IsZero() && auth.Before(rec)
}

// observedStaleWebhook is observedStale for a webhook's own GitHub time. GitHub
// stamps payloads with one-second resolution while the daemon records its own
// writes with sub-second precision, so a change made in the same second as a
// recorded write would compare as older and be lost. The record is therefore
// truncated to the second: a webhook in the same second applies (a tie), as ties
// do everywhere else. The echo of the engine's own write is dropped by the echo
// rule (dropLabelEvent), not by this comparison (#2348).
func observedStaleWebhook(item *ItemState, key string, auth time.Time) bool {
	if auth.IsZero() {
		return false
	}
	rec := recordedAt(item, key)
	return !rec.IsZero() && auth.Before(rec.Truncate(time.Second))
}

// labelStale is observedStaleWebhook for one label: the per-label record and the
// full-label-set baseline both count. Only webhook label events call it.
func labelStale(item *ItemState, label string, auth time.Time) bool {
	return observedStaleWebhook(item, engineWriteLabelKey(label), auth) || observedStaleWebhook(item, observedLabelsKey, auth)
}

// observe records that key was observed with authority time auth, keeping the
// later of the existing record and auth. A zero auth records nothing. Label
// records past their retention are pruned. When the ordering rule is disabled
// (test seam) nothing is recorded, so no write is ever judged stale.
func (s *Store) observe(item *ItemState, key string, auth time.Time) {
	if auth.IsZero() || s.observedOrderingOff.Load() {
		return
	}
	if item.ObservedAt == nil {
		item.ObservedAt = make(map[string]time.Time)
	}
	if cur := item.ObservedAt[key]; auth.After(cur) {
		item.ObservedAt[key] = auth
	}
	pruneObserved(item, time.Now())
}

// pruneObserved drops per-label records older than observedLabelRetention and
// returns the map to nil when empty so equality checks stay stable.
func pruneObserved(item *ItemState, now time.Time) {
	for k, t := range item.ObservedAt {
		if strings.HasPrefix(k, engineWriteLabelPfx) && now.Sub(t) > observedLabelRetention {
			delete(item.ObservedAt, k)
		}
	}
	if len(item.ObservedAt) == 0 {
		item.ObservedAt = nil
	}
}

// ordering reports whether the ordering rule is in force.
func (s *Store) ordering() bool { return !s.observedOrderingOff.Load() }

// SetObservedOrderingDisabledForTest restores the pre-#2348 behaviour: no
// observed-at record is kept or consulted, and Reconcile removes every item
// missing from its listing. Engine-write protection (#2159) is unaffected. Test
// seam only, used to show the sim twins are not vacuous. It does not revert
// everything: engine-write records (#2159) are still kept and still shield Status,
// closed and labels from listings, and head, Status and closed comparisons see
// them through recordedAt.
func (s *Store) SetObservedOrderingDisabledForTest(off bool) {
	s.observedOrderingOff.Store(off)
}

// recordEngine records an engine write of key at now in both the engine-write
// map (the listing protection) and the observed map (ordering against later
// webhooks, which outlives the grace).
func (s *Store) recordEngine(item *ItemState, key string, now time.Time) {
	recordEngineWrite(item, key, now, s.graceLocked())
	s.observe(item, key, now)
}

// mergeReviews folds incoming reviews into cur so that the list holds the latest
// review per author, ordered by (SubmittedAt, DatabaseID). Every producer goes
// through it, so a webhook-filled list and a deep-fetch-filled list have one
// shape (R5). A review with the same DatabaseID as one already held replaces it
// in place first (an edited or dismissed review keeps its identity), then the
// per-author collapse applies. The result is sorted by author for stability.
func mergeReviews(cur []gh.PRReview, incoming ...gh.PRReview) []gh.PRReview {
	all := make([]gh.PRReview, 0, len(cur)+len(incoming))
	all = append(all, cur...)
	for _, r := range incoming {
		replaced := false
		if r.DatabaseID != 0 {
			for i := range all {
				if all[i].DatabaseID == r.DatabaseID {
					if r.SubmittedAt.IsZero() {
						r.SubmittedAt = all[i].SubmittedAt
					}
					all[i] = r
					replaced = true
					break
				}
			}
		}
		if !replaced {
			all = append(all, r)
		}
	}
	return latestReviewPerAuthor(all)
}

// latestReviewPerAuthor keeps one review per author: the one with the greatest
// (SubmittedAt, DatabaseID).
func latestReviewPerAuthor(in []gh.PRReview) []gh.PRReview {
	if len(in) == 0 {
		return nil
	}
	best := make(map[string]gh.PRReview, len(in))
	for _, r := range in {
		b, ok := best[r.Author]
		if !ok || reviewNewer(r, b) {
			best[r.Author] = r
		}
	}
	out := make([]gh.PRReview, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Author < out[j].Author })
	return out
}

// reviewNewer reports whether a was submitted after b; ties and unknown times
// fall back to the larger database ID, then to a (the later arrival).
func reviewNewer(a, b gh.PRReview) bool {
	if !a.SubmittedAt.Equal(b.SubmittedAt) && !a.SubmittedAt.IsZero() && !b.SubmittedAt.IsZero() {
		return a.SubmittedAt.After(b.SubmittedAt)
	}
	if a.DatabaseID != b.DatabaseID && a.DatabaseID != 0 && b.DatabaseID != 0 {
		return a.DatabaseID > b.DatabaseID
	}
	return true
}

// dropLabelEvent reports whether a webhook label event for label must be dropped:
// the echo of an engine write the Store already holds a record of (the write-
// through applied it, so the echo only confirms it, and a late echo of an
// earlier write must not undo a later one), or an event older than the recorded
// observation of the label (#2348, R2).
func (s *Store) dropLabelEvent(item *ItemState, label string, at time.Time, echoOfEngine bool) bool {
	if !s.ordering() {
		return false
	}
	if echoOfEngine {
		if _, ok := item.EngineWriteAt[engineWriteLabelKey(label)]; ok {
			return true
		}
	}
	return labelStale(item, label, at)
}

// applyPartialOpened applies an issues.opened / projects_v2_item.created payload,
// which carries no Status, item ID, comments or update time (#2348, R3). On an
// item the Store has not populated yet it writes what the payload carries; on a
// populated item it only orders the payload's labels and milestone against their
// records. Dependencies, the linked PR and every field the payload does not
// carry are never touched.
func (s *Store) applyPartialOpened(item *ItemState, v IssueOpened) ChangeFlags {
	pi := v.Item
	var flags ChangeFlags
	populated := item.State != ""
	if item.ID != pi.ID && pi.ID != "" {
		item.ID = pi.ID
	}
	adoptItemID(item, pi.ItemID, false)

	if !populated {
		if item.Title != pi.Title || item.Body != pi.Body || item.URL != pi.URL || item.Author != pi.Author {
			item.Title, item.Body, item.URL, item.Author = pi.Title, pi.Body, pi.URL, pi.Author
			flags |= TitleBodyChanged
		}
		if len(pi.Assignees) > 0 {
			item.Assignees = copyStrings(pi.Assignees)
			flags |= AssigneesChanged
		}
		item.State, item.IsClosed, item.IsPR = stateFrom(pi), pi.IsClosed, pi.IsPR
		flags |= StateChanged
		s.observe(item, observedClosedKey, v.At)
		if !pi.LabelsAbsent {
			// A stub (created by an earlier label event, before any populating
			// write) can already hold a newer record for a label: such a label is
			// the business of that event, not of this creation-time payload.
			for _, l := range pi.Labels {
				if !labelStale(item, l, v.At) && !containsString(item.Labels, l) {
					item.Labels = append(item.Labels, l)
				}
				s.observe(item, engineWriteLabelKey(l), v.At)
			}
			item.LabelsKnown = true
			flags |= LabelsChanged
		}
	} else if !pi.LabelsAbsent {
		for _, l := range pi.Labels {
			if !labelStale(item, l, v.At) && !containsString(item.Labels, l) {
				item.Labels = append(item.Labels, l)
				flags |= LabelsChanged
			}
			s.observe(item, engineWriteLabelKey(l), v.At)
		}
		// An opened payload describes the issue at creation, so it can only ever
		// add: a label removed since is the business of its unlabeled event.
	}
	if pi.MilestoneKnown && !observedStale(item, observedMilestoneKey, v.At) {
		s.observe(item, observedMilestoneKey, v.At)
		flags |= applyMilestone(item, pi.Milestone, true)
	}
	return flags
}

// applyDeepFetchedOrdered applies a deep fetch under the observed-at rule: a live
// read judges its blockedBy, milestone, linked-PR and review facts against their
// records by the time its request was issued, and records what it applied. A
// cache-hit copy has no start stamp and applies as before (#2348, FR-011).
func (s *Store) applyDeepFetchedOrdered(item *ItemState, fresh gh.ProjectItem) ChangeFlags {
	grace := s.graceLocked()
	var st deepStale
	var auth time.Time
	if s.ordering() && fresh.DeepFetchLive {
		auth = authorityOf(fresh.DeepFetchStartedAt, true, grace)
		st = deepStale{
			blockedBy: observedStale(item, observedBlockedByKey, auth),
			milestone: observedStale(item, observedMilestoneKey, auth),
			linkedPR:  observedStale(item, observedLinkedPRKey, auth),
			reviews:   observedStale(item, observedReviewsKey, auth),
		}
	}
	if !auth.IsZero() && !st.reviews && item.LinkedPR != nil {
		fresh.LinkedPRReviews = keepNewerReviews(item, fresh.LinkedPRReviews, auth)
	}
	flags := applyDeepFetched(item, fresh, grace, time.Now(), st)
	if !auth.IsZero() {
		if !st.blockedBy {
			s.observe(item, observedBlockedByKey, auth)
		}
		if fresh.MilestoneKnown && !st.milestone {
			s.observe(item, observedMilestoneKey, auth)
		}
		if fresh.LinkedPRNumber != 0 {
			if !st.linkedPR {
				s.observe(item, observedLinkedPRKey, auth)
			}
			if !st.reviews {
				s.observe(item, observedReviewsKey, auth)
			}
		}
		s.observe(item, observedClosedKey, auth)
		if !fresh.LabelsAbsent {
			s.observe(item, observedLabelsKey, auth)
		}
	}
	return flags
}

// keepNewerReviews returns incoming with every author whose recorded review
// observation is newer than auth (a webhook delivery that landed while the fetch
// was in flight) replaced by the review the Store already holds for them, so a
// whole-list read cannot undo or wipe one reviewer's newer review (#2348).
func keepNewerReviews(item *ItemState, incoming []gh.PRReview, auth time.Time) []gh.PRReview {
	newer := make(map[string]bool)
	for _, r := range item.LinkedPR.Reviews {
		if observedStale(item, observedReviewAuthorKey(r.Author), auth) {
			newer[r.Author] = true
		}
	}
	if len(newer) == 0 {
		return incoming
	}
	out := make([]gh.PRReview, 0, len(incoming)+len(newer))
	for _, r := range incoming {
		if !newer[r.Author] {
			out = append(out, r)
		}
	}
	for _, r := range item.LinkedPR.Reviews {
		if newer[r.Author] {
			out = append(out, r)
		}
	}
	return out
}
