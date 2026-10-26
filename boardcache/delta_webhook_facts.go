package boardcache

import (
	"encoding/json"
	"time"

	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Webhook deltas added by #2352 (ADR 2352): comment and review-comment
// edited/deleted, pull_request_review_thread, label, milestone and sub_issues.
//
// None of them makes a GitHub call. A subject the Store does not hold (item,
// thread, comment, label, milestone) is ignored with at most one log line; the
// next reconcile or deep fetch remains the source for it. Payloads are declared
// field by field with lenient scalars (ADR 2383), and each handler keys on
// `action`, IDs and `changes.*.from` only: an embedded object (a label name, a
// milestone title, sub_issues_summary) reflects when GitHub rendered the
// delivery, which can be later than the action.

type reviewThreadEventPayload struct {
	Action lenientString `json:"action"`
	Thread struct {
		NodeID lenientString `json:"node_id"`
		// Comments lists the thread's comments; a resolve uses them to find
		// stored comments that carry no thread ID (#2444).
		Comments []struct {
			NodeID lenientString `json:"node_id"`
			ID     lenientInt    `json:"id"`
		} `json:"comments"`
	} `json:"thread"`
	PullRequest struct {
		Number int `json:"number"`
	} `json:"pull_request"`
	// UpdatedAt is the delivery's own time (it carries a non-UTC offset in real
	// captures; time.Parse(RFC3339) handles that).
	UpdatedAt  lenientString `json:"updated_at"`
	Repository struct {
		FullName lenientString `json:"full_name"`
	} `json:"repository"`
}

type labelEventPayload struct {
	Action  lenientString `json:"action"`
	Changes struct {
		Name struct {
			From lenientString `json:"from"`
		} `json:"name"`
	} `json:"changes"`
	Label struct {
		Name lenientString `json:"name"`
	} `json:"label"`
	Repository struct {
		FullName lenientString `json:"full_name"`
	} `json:"repository"`
}

type milestoneEventPayload struct {
	Action    lenientString `json:"action"`
	Milestone struct {
		Number int           `json:"number"`
		Title  lenientString `json:"title"`
	} `json:"milestone"`
	Repository struct {
		FullName lenientString `json:"full_name"`
	} `json:"repository"`
}

type subIssuesEventPayload struct {
	Action      lenientString `json:"action"`
	ParentIssue struct {
		Number int `json:"number"`
	} `json:"parent_issue"`
	SubIssue struct {
		Number int `json:"number"`
	} `json:"sub_issue"`
	// The side's repository object is present on the delivery for the *other*
	// endpoint (sub_issue_repo on the parent-side delivery, parent_issue_repo on
	// the child-side one); the hook's own repository is the remaining endpoint's.
	ParentIssueRepo struct {
		FullName lenientString `json:"full_name"`
	} `json:"parent_issue_repo"`
	SubIssueRepo struct {
		FullName lenientString `json:"full_name"`
	} `json:"sub_issue_repo"`
	Repository struct {
		FullName lenientString `json:"full_name"`
	} `json:"repository"`
}

// resolveCommentItem finds the cached item a comment event belongs to: the issue
// itself, else (a comment on a PR's conversation tab carries the PR number) the
// issue linked to that PR. Both are Store reads; nothing is fetched.
func (c *CacheImpl) resolveCommentItem(repo string, number int) (itemRepo string, itemNumber int, ok bool) {
	if _, found := c.store.Peek(repo, number); found {
		return repo, number, true
	}
	if key, found := c.store.GetByPRKey(repo, number); found {
		if r, n, parsed := parseItemKey(key); parsed {
			return r, n, true
		}
	}
	return "", 0, false
}

func (c *CacheImpl) applyIssueCommentEditDelete(p issueCommentPayload, receivedAt time.Time) {
	repo, num, ok := c.resolveCommentItem(p.Repository.FullName, p.Issue.Number)
	if !ok {
		c.logFn("[cache] issue_comment %s: #%d not in store; skipping (next fetch is the source)\n", p.Action, p.Issue.Number)
		return
	}
	var m itemstate.Mutation
	if p.Action == "deleted" {
		m = itemstate.IssueCommentDeleted{Repo: repo, Number: num, DatabaseID: p.Comment.DatabaseID}
	} else {
		m = itemstate.IssueCommentEdited{Repo: repo, Number: num, DatabaseID: p.Comment.DatabaseID,
			Body: p.Comment.Body, At: observedTime(p.Comment.UpdatedAt, receivedAt)}
	}
	if _, changes, _ := c.store.Apply(m); len(changes) > 0 {
		c.bumpLocalDeltaAt(itemKey(repo, num))
	}
}

func (c *CacheImpl) applyReviewCommentEditDelete(p pullRequestReviewCommentPayload, receivedAt time.Time) {
	repo := p.Repository.FullName
	key, ok := c.store.GetByPRKey(repo, p.PullRequest.Number)
	if !ok {
		c.logFn("[cache] pull_request_review_comment %s: PR #%d not linked in store; skipping (next fetch is the source)\n", p.Action, p.PullRequest.Number)
		return
	}
	issRepo, issNum, parsed := parseItemKey(key)
	if !parsed {
		return
	}
	var m itemstate.Mutation
	if p.Action == "deleted" {
		m = itemstate.ReviewThreadCommentDeleted{Repo: issRepo, IssueNumber: issNum, NodeID: p.Comment.NodeID}
	} else {
		m = itemstate.ReviewThreadCommentEdited{Repo: issRepo, IssueNumber: issNum, NodeID: p.Comment.NodeID,
			Body: p.Comment.Body, At: observedTime(p.Comment.UpdatedAt, receivedAt)}
	}
	if _, changes, _ := c.store.Apply(m); len(changes) > 0 {
		c.bumpLocalDeltaAt(key)
	}
}

func (c *CacheImpl) applyReviewThreadDelta(payload []byte, receivedAt time.Time) {
	var p reviewThreadEventPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		c.logFn("[cache] applyReviewThreadDelta: unmarshal payload: %v\n", err)
		return
	}
	if p.Action != "resolved" && p.Action != "unresolved" {
		return
	}
	repo, thread := string(p.Repository.FullName), string(p.Thread.NodeID)
	if thread == "" {
		c.logFn("[cache] pull_request_review_thread %s: no thread.node_id; ignoring\n", p.Action)
		return
	}
	key, ok := c.store.GetByPRKey(repo, p.PullRequest.Number)
	if !ok {
		c.logFn("[cache] pull_request_review_thread %s: PR #%d not linked in store; skipping (next fetch is the source)\n", p.Action, p.PullRequest.Number)
		return
	}
	issRepo, issNum, parsed := parseItemKey(key)
	if !parsed {
		return
	}
	at := observedTime(p.UpdatedAt, receivedAt)
	resolved := itemstate.ReviewThreadResolved{Repo: issRepo, IssueNumber: issNum, ThreadID: thread, At: at}
	for _, tc := range p.Thread.Comments {
		if tc.NodeID != "" {
			resolved.CommentNodeIDs = append(resolved.CommentNodeIDs, string(tc.NodeID))
		}
		if tc.ID != 0 {
			resolved.CommentDatabaseIDs = append(resolved.CommentDatabaseIDs, int(tc.ID))
		}
	}
	var m itemstate.Mutation = resolved
	if p.Action == "unresolved" {
		m = itemstate.ReviewThreadUnresolved{Repo: issRepo, IssueNumber: issNum, ThreadID: thread, At: at}
	}
	// Deliberately does not stamp linkedPRFetchedAt (ADR 1551): a webhook delta is
	// not a fetch of the PR.
	if _, changes, _ := c.store.Apply(m); len(changes) > 0 {
		c.bumpLocalDeltaAt(key)
	}
}

func (c *CacheImpl) applyLabelDelta(payload []byte, receivedAt time.Time) {
	var p labelEventPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		c.logFn("[cache] applyLabelDelta: unmarshal payload: %v\n", err)
		return
	}
	repo := string(p.Repository.FullName)
	var m itemstate.RepoLabelChanged
	switch p.Action {
	case "edited":
		// A colour or description edit has no changes.name: nothing to rename.
		from, to := string(p.Changes.Name.From), string(p.Label.Name)
		if from == "" || to == "" || from == to {
			return
		}
		m = itemstate.RepoLabelChanged{Repo: repo, From: from, To: to, At: receivedAt}
	case "deleted":
		if p.Label.Name == "" {
			return
		}
		m = itemstate.RepoLabelChanged{Repo: repo, From: string(p.Label.Name), At: receivedAt}
	default: // created, and anything unrecognised: no item changes
		return
	}
	c.applyFanout(m)
}

func (c *CacheImpl) applyMilestoneDelta(payload []byte, receivedAt time.Time) {
	var p milestoneEventPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		c.logFn("[cache] applyMilestoneDelta: unmarshal payload: %v\n", err)
		return
	}
	if p.Milestone.Number == 0 {
		c.logFn("[cache] milestone %s: no milestone.number; ignoring\n", p.Action)
		return
	}
	// Matched by number, never by changes.title.from: a number is unique per
	// repository, a title is not.
	m := itemstate.RepoMilestoneChanged{Repo: string(p.Repository.FullName), Number: p.Milestone.Number, At: receivedAt}
	switch p.Action {
	case "edited":
		if p.Milestone.Title == "" {
			return
		}
		m.Title = string(p.Milestone.Title)
	case "deleted":
		m.Deleted = true
	default:
		return
	}
	c.applyFanout(m)
}

// applyFanout applies a repository-wide mutation and bumps the local-delta stamp
// of each item it changed.
func (c *CacheImpl) applyFanout(m itemstate.Mutation) {
	_, changes, _ := c.store.Apply(m)
	for _, ch := range changes {
		c.bumpLocalDeltaAt(itemKey(ch.Repo, ch.Number))
	}
}

func (c *CacheImpl) applySubIssuesDelta(payload []byte, receivedAt time.Time) {
	var p subIssuesEventPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		c.logFn("[cache] applySubIssuesDelta: unmarshal payload: %v\n", err)
		return
	}
	var remove bool
	switch p.Action {
	case "sub_issue_added", "parent_issue_added":
	case "sub_issue_removed", "parent_issue_removed":
		remove = true
	default:
		return
	}
	hook := string(p.Repository.FullName)
	parent := itemstate.IssueRef{Repo: hook, Number: p.ParentIssue.Number}
	child := itemstate.IssueRef{Repo: hook, Number: p.SubIssue.Number}
	if r := string(p.ParentIssueRepo.FullName); r != "" {
		parent.Repo = r
	}
	if r := string(p.SubIssueRepo.FullName); r != "" {
		child.Repo = r
	}
	if parent.Repo == "" || child.Repo == "" || parent.Number == 0 || child.Number == 0 {
		c.logFn("[cache] sub_issues %s: payload lacks an endpoint; ignoring\n", p.Action)
		return
	}
	// Both deliveries of one change carry both endpoints, so either one writes
	// both sides; whichever endpoint the Store does not hold is skipped. The
	// second delivery finds the state already applied.
	if _, ok := c.store.Peek(parent.Repo, parent.Number); ok {
		c.store.Apply(itemstate.SubIssueLinkedBelow{Repo: parent.Repo, Number: parent.Number, Child: child, Remove: remove, At: receivedAt})
	}
	if _, ok := c.store.Peek(child.Repo, child.Number); ok {
		c.store.Apply(itemstate.SubIssueLinkedUnder{Repo: child.Repo, Number: child.Number, Parent: parent, Remove: remove, At: receivedAt})
	}
}
