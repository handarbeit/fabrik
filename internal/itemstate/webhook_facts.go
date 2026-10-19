package itemstate

import (
	"reflect"
	"strconv"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// Mutations for the webhook events added by #2352 (ADR 2352): comment and
// review-comment edit/delete, review-thread resolve/unresolve, repository-wide
// label and milestone changes, and sub-issue links. Every one is applied from a
// payload alone (no GitHub read), ignores a subject the Store does not hold, and
// is ordered by the observed-at rule of ADR 2348 through a per-subject record key.

// IssueCommentEdited replaces the body of a cached issue comment, matched by
// DatabaseID. Nothing else about the comment changes. No-op when the comment is
// unknown or the body is unchanged.
type IssueCommentEdited struct {
	// At is the comment's updated_at (else receipt time).
	At         time.Time
	Repo       string
	Number     int
	DatabaseID int
	Body       string
}

func (IssueCommentEdited) isMutation()       {}
func (m IssueCommentEdited) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// IssueCommentDeleted removes a cached issue comment, matched by DatabaseID.
type IssueCommentDeleted struct {
	Repo       string
	Number     int
	DatabaseID int
}

func (IssueCommentDeleted) isMutation()       {}
func (m IssueCommentDeleted) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// ReviewThreadCommentEdited replaces the body of a cached review-thread comment,
// matched by NodeID.
type ReviewThreadCommentEdited struct {
	At          time.Time
	Repo        string
	IssueNumber int
	NodeID      string
	Body        string
}

func (ReviewThreadCommentEdited) isMutation()       {}
func (m ReviewThreadCommentEdited) itemKey() string { return itemKeyFor(m.Repo, m.IssueNumber) }

// ReviewThreadCommentDeleted removes a cached review-thread comment by NodeID.
type ReviewThreadCommentDeleted struct {
	Repo        string
	IssueNumber int
	NodeID      string
}

func (ReviewThreadCommentDeleted) isMutation()       {}
func (m ReviewThreadCommentDeleted) itemKey() string { return itemKeyFor(m.Repo, m.IssueNumber) }

// ReviewThreadResolved drops the cached comments of a resolved review thread and
// counts the thread as resolved. LinkedPR.ThreadComments holds unresolved
// threads only (the deep fetch keeps nothing else), so the count moves only when
// comments were actually removed: a replay, or a thread the Store never held (the
// deep fetch caps at 50 threads, so "unknown" is not "absent"), changes nothing.
type ReviewThreadResolved struct {
	// At is the delivery's updated_at (else receipt time).
	At          time.Time
	Repo        string
	IssueNumber int
	ThreadID    string
}

func (ReviewThreadResolved) isMutation()       {}
func (m ReviewThreadResolved) itemKey() string { return itemKeyFor(m.Repo, m.IssueNumber) }

// ReviewThreadUnresolved records that a thread was reopened and invalidates the
// item's deep cache so the next fetch rebuilds the thread. A rebuild from the
// payload is not attempted: the delivery carries neither isOutdated nor
// reactions. It also holds the thread's record, so a resolve delivered after it
// (reordered) is dropped as older.
type ReviewThreadUnresolved struct {
	At          time.Time
	Repo        string
	IssueNumber int
	ThreadID    string
}

func (ReviewThreadUnresolved) isMutation()       {}
func (m ReviewThreadUnresolved) itemKey() string { return itemKeyFor(m.Repo, m.IssueNumber) }

// RepoLabelChanged renames (To != "") or removes (To == "") a label on every
// cached item of Repo that carries From. It is a multi-item mutation: no single
// item key. It emits no label deltas (a repository-wide rename would otherwise
// raise one channel event per carrying item); the engine still wakes on
// LabelsChanged.
type RepoLabelChanged struct {
	// At is the receipt time.
	At   time.Time
	Repo string
	From string
	To   string
}

func (RepoLabelChanged) isMutation()       {}
func (m RepoLabelChanged) itemKey() string { return "" }

// RepoMilestoneChanged retitles (Deleted == false) or clears (Deleted == true)
// the milestone numbered Number on every cached item of Repo carrying it.
// Matching is by number, never title.
type RepoMilestoneChanged struct {
	At      time.Time
	Repo    string
	Number  int
	Title   string
	Deleted bool
}

func (RepoMilestoneChanged) isMutation()       {}
func (m RepoMilestoneChanged) itemKey() string { return "" }

// SubIssueLinkedBelow adds or removes Child in the parent's SubIssues.
type SubIssueLinkedBelow struct {
	At     time.Time
	Repo   string
	Number int // the parent
	Child  IssueRef
	Remove bool
}

func (SubIssueLinkedBelow) isMutation()       {}
func (m SubIssueLinkedBelow) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

// SubIssueLinkedUnder sets or clears the child's ParentIssue.
type SubIssueLinkedUnder struct {
	At     time.Time
	Repo   string
	Number int // the child
	Parent IssueRef
	Remove bool
}

func (SubIssueLinkedUnder) isMutation()       {}
func (m SubIssueLinkedUnder) itemKey() string { return itemKeyFor(m.Repo, m.Number) }

func issueRefKey(r IssueRef) string { return itemKeyFor(r.Repo, r.Number) }

// applyWebhookFact applies the single-item mutations of this file. handled is
// false for any other mutation type.
func (s *Store) applyWebhookFact(item *ItemState, m Mutation) (flags ChangeFlags, handled bool) {
	switch v := m.(type) {
	case IssueCommentEdited:
		key := observedCommentPfx + strconv.Itoa(v.DatabaseID)
		if observedStaleWebhook(item, key, v.At) {
			return 0, true
		}
		s.observe(item, key, v.At)
		return editCommentBody(item.Comments, func(c *gh.Comment) bool { return c.DatabaseID == v.DatabaseID && v.DatabaseID != 0 }, v.Body), true

	case IssueCommentDeleted:
		var n int
		item.Comments, n = removeComments(item.Comments, func(c gh.Comment) bool { return c.DatabaseID == v.DatabaseID && v.DatabaseID != 0 })
		if n == 0 {
			return 0, true
		}
		return CommentsChanged, true

	case ReviewThreadCommentEdited:
		if item.LinkedPR == nil {
			return 0, true
		}
		key := observedCommentPfx + v.NodeID
		if observedStaleWebhook(item, key, v.At) {
			return 0, true
		}
		s.observe(item, key, v.At)
		if editCommentBody(item.LinkedPR.ThreadComments, func(c *gh.Comment) bool { return c.ID == v.NodeID && v.NodeID != "" }, v.Body) == 0 {
			return 0, true
		}
		return LinkedPRChanged | CommentsChanged, true

	case ReviewThreadCommentDeleted:
		if item.LinkedPR == nil {
			return 0, true
		}
		var n int
		item.LinkedPR.ThreadComments, n = removeComments(item.LinkedPR.ThreadComments, func(c gh.Comment) bool { return c.ID == v.NodeID && v.NodeID != "" })
		if n == 0 {
			return 0, true
		}
		return LinkedPRChanged | CommentsChanged, true

	case ReviewThreadResolved:
		if item.LinkedPR == nil || v.ThreadID == "" {
			return 0, true
		}
		key := observedThreadPfx + v.ThreadID
		if observedStaleWebhook(item, key, v.At) {
			return 0, true
		}
		var n int
		item.LinkedPR.ThreadComments, n = removeComments(item.LinkedPR.ThreadComments, func(c gh.Comment) bool { return c.ReviewThreadID == v.ThreadID })
		if n == 0 {
			// Unknown thread: record nothing so a later unresolved or fetch is judged
			// on its own merits.
			return 0, true
		}
		s.observe(item, key, v.At)
		item.LinkedPR.ResolvedThreadCount++
		return LinkedPRChanged | CommentsChanged, true

	case ReviewThreadUnresolved:
		if v.ThreadID == "" {
			return 0, true
		}
		key := observedThreadPfx + v.ThreadID
		if observedStaleWebhook(item, key, v.At) {
			return 0, true
		}
		s.observe(item, key, v.At)
		if item.LastDeepFetchAt.IsZero() && item.LastSeenSourceUpdatedAt.IsZero() && item.LastSeenPRUpdatedAt.IsZero() {
			return 0, true // nothing cached to invalidate: the next fetch reads the thread fresh
		}
		item.LastDeepFetchAt = time.Time{}
		item.LastSeenSourceUpdatedAt = time.Time{}
		item.LastSeenPRUpdatedAt = time.Time{}
		return DeepFetchChanged | LinkedPRChanged | CommentsChanged, true

	case SubIssueLinkedBelow:
		key := observedSubLinkPfx + issueRefKey(v.Child)
		if observedStaleWebhook(item, key, v.At) {
			return 0, true
		}
		s.observe(item, key, v.At)
		idx := -1
		for i, r := range item.SubIssues {
			if r == v.Child {
				idx = i
				break
			}
		}
		switch {
		case v.Remove && idx >= 0:
			item.SubIssues = append(append([]IssueRef(nil), item.SubIssues[:idx]...), item.SubIssues[idx+1:]...)
			if len(item.SubIssues) == 0 {
				item.SubIssues = nil
			}
			return SubIssueLinksChanged, true
		case !v.Remove && idx < 0:
			item.SubIssues = append(item.SubIssues, v.Child)
			return SubIssueLinksChanged, true
		}
		return 0, true

	case SubIssueLinkedUnder:
		key := observedSubLinkPfx + issueRefKey(v.Parent)
		if observedStaleWebhook(item, key, v.At) {
			return 0, true
		}
		s.observe(item, key, v.At)
		switch {
		case v.Remove && item.ParentIssue != nil && *item.ParentIssue == v.Parent:
			item.ParentIssue = nil
			return SubIssueLinksChanged, true
		case !v.Remove && (item.ParentIssue == nil || *item.ParentIssue != v.Parent):
			p := v.Parent
			item.ParentIssue = &p
			return SubIssueLinksChanged, true
		}
		return 0, true
	}
	return 0, false
}

// editCommentBody sets Body on the first comment match selects. It reports
// CommentsChanged only when the body actually differed.
func editCommentBody(cs []gh.Comment, match func(*gh.Comment) bool, body string) ChangeFlags {
	for i := range cs {
		if match(&cs[i]) {
			if cs[i].Body == body {
				return 0
			}
			cs[i].Body = body
			return CommentsChanged
		}
	}
	return 0
}

// removeComments returns cs without the comments pred matches, and how many were
// removed. It never aliases the input's backing array when it removes.
func removeComments(cs []gh.Comment, pred func(gh.Comment) bool) ([]gh.Comment, int) {
	n := 0
	for _, c := range cs {
		if pred(c) {
			n++
		}
	}
	if n == 0 {
		return cs, 0
	}
	out := make([]gh.Comment, 0, len(cs)-n)
	for _, c := range cs {
		if !pred(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		out = nil
	}
	return out, n
}

// ---- multi-item fan-out ----

// applyRepoFanout runs fn on every cached item of repo, one lock acquisition per
// item (as applyBoardReconciled does), and notifies observers outside the lock
// with one Change per item that changed.
func (s *Store) applyRepoFanout(repo string, fn func(item *ItemState) ChangeFlags) (Snapshot, []Change, error) {
	s.mu.RLock()
	keys := make([]string, 0)
	for k, it := range s.items {
		if it.Repo == repo {
			keys = append(keys, k)
		}
	}
	s.mu.RUnlock()

	obs := s.captureObservers()
	var changes []Change
	for _, key := range keys {
		s.mu.Lock()
		item, ok := s.items[key]
		if !ok {
			s.mu.Unlock()
			continue
		}
		before := newSnapshot(*item).state
		flags := fn(item)
		unchanged := before
		unchanged.EngineWriteAt = item.EngineWriteAt
		unchanged.ObservedAt = item.ObservedAt
		if flags == 0 || reflect.DeepEqual(unchanged, *item) {
			s.mu.Unlock()
			continue
		}
		s.updateIndexes(before, *item, key)
		snap := newSnapshot(*item)
		r, n := item.Repo, item.Number
		s.mu.Unlock()

		change := Change{Repo: r, Number: n, Fields: flags, Origin: OriginWebhook}
		s.notify(obs, change, snap)
		changes = append(changes, change)
	}
	return Snapshot{}, changes, nil
}

func (s *Store) applyRepoLabelChanged(v RepoLabelChanged) (Snapshot, []Change, error) {
	if v.From == "" || v.Repo == "" {
		return Snapshot{}, nil, nil
	}
	return s.applyRepoFanout(v.Repo, func(item *ItemState) ChangeFlags {
		if !containsString(item.Labels, v.From) {
			return 0
		}
		if s.ordering() && labelStale(item, v.From, v.At) {
			return 0
		}
		s.observe(item, engineWriteLabelKey(v.From), v.At)
		if v.To == "" {
			item.Labels = removeString(item.Labels, v.From)
			return LabelsChanged
		}
		s.observe(item, engineWriteLabelKey(v.To), v.At)
		if containsString(item.Labels, v.To) {
			item.Labels = removeString(item.Labels, v.From) // already carries the new name: no duplicate
			return LabelsChanged
		}
		out := make([]string, len(item.Labels))
		for i, l := range item.Labels {
			if l == v.From {
				l = v.To
			}
			out[i] = l
		}
		item.Labels = out
		return LabelsChanged
	})
}

func (s *Store) applyRepoMilestoneChanged(v RepoMilestoneChanged) (Snapshot, []Change, error) {
	if v.Repo == "" || v.Number == 0 {
		return Snapshot{}, nil, nil
	}
	return s.applyRepoFanout(v.Repo, func(item *ItemState) ChangeFlags {
		if item.Milestone == nil || item.Milestone.Number != v.Number {
			return 0
		}
		if observedStaleWebhook(item, observedMilestoneKey, v.At) {
			return 0
		}
		s.observe(item, observedMilestoneKey, v.At)
		if v.Deleted {
			return applyMilestone(item, nil, true)
		}
		return applyMilestone(item, &gh.Milestone{Title: v.Title, Number: v.Number}, true)
	})
}
