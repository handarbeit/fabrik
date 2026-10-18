package boardcache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// ApplyDelta dispatches a webhook payload to the appropriate typed delta function.
// It is a no-op when the cache is paused (stream unhealthy).
func (c *CacheImpl) ApplyDelta(eventType string, payload []byte) {
	if c.IsPaused() {
		return
	}

	switch eventType {
	case "issue_comment":
		c.applyIssueCommentDelta(payload)
	case "issues":
		c.applyIssuesDelta(payload)
	case "pull_request":
		c.applyPullRequestDelta(payload)
	case "pull_request_review":
		c.applyPullRequestReviewDelta(payload)
	case "pull_request_review_comment":
		c.applyPullRequestReviewCommentDelta(payload)
	case "check_run":
		c.applyCheckRunDelta(payload)
	case "check_suite":
		// No-op: check_suite events are coarse-grained aggregates. Individual
		// check run outcomes are tracked via check_run.completed, which provides
		// the per-run Name, Conclusion, and SHA needed for the CI gate.
		c.applyCheckSuite(payload)
	case "projects_v2_item":
		c.applyProjectsV2ItemDelta(payload)
	}
}

// ---------------------------------------------------------------------------
// Webhook payload structs (minimal — only fields needed for delta application)
// ---------------------------------------------------------------------------

type issueCommentPayload struct {
	Action  string `json:"action"`
	Comment struct {
		NodeID     string `json:"node_id"`
		DatabaseID int    `json:"id"`
		Body       string `json:"body"`
		CreatedAt  string `json:"created_at"`
		User       struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	Issue struct {
		Number int `json:"number"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type issuesPayload struct {
	Action string `json:"action"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
	Label struct {
		Name string `json:"name"`
	} `json:"label"`
	Issue struct {
		Number int    `json:"number"`
		NodeID string `json:"node_id"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		State  string `json:"state"`
		// UpdatedAt is the GitHub time of the issue's last change: for a labeled,
		// unlabeled, closed or reopened event, the event's own time. It orders the
		// delivery against every other observation (#2348).
		UpdatedAt lenientString `json:"updated_at"`
		Labels    []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Assignees []struct {
			Login string `json:"login"`
		} `json:"assignees"`
		// Milestone is raw: an explicit null (no milestone, e.g. after
		// "demilestoned") must stay distinguishable from an absent key (#1967 R10).
		Milestone json.RawMessage `json:"milestone"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// milestoneFromPayload decodes issue.milestone from an "issues" webhook payload.
// known is false when the key is absent or malformed — callers must then leave
// the Store's milestone untouched rather than recording "none".
func milestoneFromPayload(raw json.RawMessage) (m *gh.Milestone, known bool) {
	if len(raw) == 0 {
		return nil, false
	}
	if string(raw) == "null" {
		return nil, true
	}
	var ms struct {
		Title  string `json:"title"`
		Number int    `json:"number"`
	}
	if err := json.Unmarshal(raw, &ms); err != nil {
		return nil, false
	}
	return &gh.Milestone{Title: ms.Title, Number: ms.Number}, true
}

type pullRequestPayload struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Merged bool   `json:"merged"`
		Draft  bool   `json:"draft"`
		// UpdatedAt is the GitHub time of the PR's last change (#2348).
		UpdatedAt lenientString `json:"updated_at"`
		Head      struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
		MergeableState     string `json:"mergeable_state"`
		RequestedReviewers []struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"requested_reviewers"`
	} `json:"pull_request"`
	// RequestedReviewer is present in review_requested/review_request_removed events.
	RequestedReviewer struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"requested_reviewer"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type pullRequestReviewPayload struct {
	Action string `json:"action"`
	Review struct {
		DatabaseID int    `json:"id"`
		Body       string `json:"body"`
		State      string `json:"state"`
		// CommitID is the SHA the review was submitted against (#2127 R5): without
		// it a review that arrived by webhook cannot be placed at the PR head.
		CommitID string `json:"commit_id"`
		// SubmittedAt is when the review was submitted (#2348); absent on some
		// deliveries, in which case receipt order applies.
		SubmittedAt lenientString `json:"submitted_at"`
		User        struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"review"`
	PullRequest struct {
		Number int `json:"number"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type pullRequestReviewCommentPayload struct {
	Action  string `json:"action"`
	Comment struct {
		NodeID       string `json:"node_id"`
		DatabaseID   int    `json:"id"`
		Body         string `json:"body"`
		CreatedAt    string `json:"created_at"`
		DiffHunk     string `json:"diff_hunk"`
		Path         string `json:"path"`
		Line         *int   `json:"line"`
		OriginalLine *int   `json:"original_line"`
		User         struct {
			Login string `json:"login"`
		} `json:"user"`
		PullRequestURL string `json:"pull_request_url"`
	} `json:"comment"`
	PullRequest struct {
		Number int `json:"number"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type checkRunPayload struct {
	Action   string `json:"action"`
	CheckRun struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HeadSHA    string `json:"head_sha"`
	} `json:"check_run"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// singleSelectTo is the object form of changes.field_value.to for a
// single_select edit.
type singleSelectTo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// editedStatusName extracts the new Status column name from a single_select
// edit's raw "to". Returns "" when it is not the expected object.
func editedStatusName(to json.RawMessage) string {
	var t singleSelectTo
	if len(to) == 0 || json.Unmarshal(to, &t) != nil {
		return ""
	}
	return t.Name
}

// observedTime is the observation time of a webhook delivery (#2348, R1): the
// GitHub time the payload carries, or the daemon's receipt time when it carries
// none or an unparseable one. Receipt order is never allowed to look older than a
// real GitHub time, because the daemon's clock is compared with GitHub's only
// through the Store's rule (see itemstate/observed.go).
func observedTime(ts lenientString) time.Time {
	if ts != "" {
		if t, err := time.Parse(time.RFC3339, string(ts)); err == nil {
			return t
		}
	}
	return time.Now()
}

// lenientString decodes a JSON string and yields "" for any other JSON type
// (number, bool, null, object, array) instead of failing the enclosing
// Unmarshal. The projects_v2_item payload is declared field by field, so one
// mistyped field would otherwise discard the whole delivery: that is how the
// numeric projects_v2_item.id, once read as a string, dropped every real
// delivery for five months (#2383). A mistyped field a delta relies on becomes
// "" and falls into that field's existing missing-value path.
type lenientString string

// UnmarshalJSON never returns an error.
func (s *lenientString) UnmarshalJSON(b []byte) error {
	var v string
	if json.Unmarshal(b, &v) != nil {
		*s = ""
		return nil
	}
	*s = lenientString(v)
	return nil
}

// projectsV2ItemPayload declares only the fields applyProjectsV2ItemDelta
// reads. Every scalar is a lenientString so an unexpected type cannot drop the
// event (#2383). The shape is that of real deliveries: the board item's ID
// (PVTI_…) is projects_v2_item.node_id, projects_v2_item.id is a different
// numeric value that is deliberately not declared, and the project ID is only
// ever nested under projects_v2_item — no real delivery carries a top-level
// project_node_id.
type projectsV2ItemPayload struct {
	Action  lenientString `json:"action"`
	Changes struct {
		FieldValue struct {
			// FieldNodeID names the project field the edit changed; compared
			// with the board's Status field ID so that edits to Priority, Size
			// and other single-select fields are not read as Status (#2345).
			FieldNodeID lenientString `json:"field_node_id"`
			FieldType   lenientString `json:"field_type"`
			// To is raw because its JSON type depends on field_type: an object
			// ({id, name}) for single_select, but a plain string or null for a
			// text field (#2048). Decoding it as an object up front would turn
			// every display-field echo into an unmarshal error before the echo
			// match runs. It is only decoded for a single_select edit. A Status
			// clear carries no "to" key at all.
			To json.RawMessage `json:"to"`
		} `json:"field_value"`
	} `json:"changes"`
	Sender struct {
		Login lenientString `json:"login"`
	} `json:"sender"`
	ProjectsV2Item struct {
		NodeID        lenientString `json:"node_id"`
		ProjectNodeID lenientString `json:"project_node_id"`
		ContentNodeID lenientString `json:"content_node_id"`
		ContentType   lenientString `json:"content_type"`
		UpdatedAt     lenientString `json:"updated_at"`
	} `json:"projects_v2_item"`
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// splitRepo splits "owner/repo" into ("owner", "repo", true). Returns false on invalid input.
func splitRepo(fullName string) (owner, repo string, ok bool) {
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// bumpLocalDeltaAt records that a webhook has touched the item at key.
// Must be called WITHOUT holding c.mu.
func (c *CacheImpl) bumpLocalDeltaAt(key string) {
	now := time.Now()
	c.mu.Lock()
	c.localDeltaAt[key] = now
	c.mu.Unlock()
}

// ensureIssueInStore guarantees the item (fullRepo, issNum) exists in the Store.
// If the item is already present it is a fast no-op. On miss it fetches a minimal
// ProjectItem from GitHub via FetchProjectItem and applies IssueOpened to the Store.
//
// This implements the "unknown-issue fallback" contract from ADR 034 §4: every
// handler that would silently drop a delta for a missing item calls ensureIssueInStore
// first so the item is populated before the delta is applied.
//
// Must be called WITHOUT holding c.mu (the network fetch must not hold any lock).
func (c *CacheImpl) ensureIssueInStore(owner, fullRepo string, issNum int) error {
	// Fast path: item already in Store.
	if _, err := c.store.Get(fullRepo, issNum); err == nil {
		return nil
	}
	// Miss: fetch from GitHub and populate.
	pi, err := c.fallback.FetchProjectItem(owner, fullRepo[len(owner)+1:], issNum)
	if err != nil {
		return err
	}
	if pi == nil {
		return fmt.Errorf("FetchProjectItem returned nil for %s#%d", fullRepo, issNum)
	}
	if pi.Repo == "" {
		pi.Repo = fullRepo
	}
	c.store.Apply(itemstate.IssueOpened{Item: *pi})
	return nil
}

// resolveOrHealPRLinkage resolves the closing issue for PR #prNum in
// owner/repoName, auto-healing via c.resolvePRLinkage when the PR isn't yet
// in the Store's prToKey index. On failure to resolve, records a negative-
// cache miss at missCacheKey (skipped when resolvePRLinkage itself errored,
// so a transient REST failure is retried on the next webhook rather than
// suppressed for recentMissTTL) and logs notFoundMsg. On success, it
// additionally confirms the resolved issue still exists in the Store —
// store.Get is called deliberately outside c.mu, preserving the
// boardcache.go struct-doc invariant that Store methods must never be called
// while c.mu is held (a Store observer callback into CacheImpl would
// deadlock otherwise). Returns ok=false if resolution or confirmation
// failed; callers must not proceed with any store.Apply in that case.
//
// The PR-scoped negative-cache pre-check (checking recentMissCache[missCacheKey]
// before calling this) stays inline in each caller that has one —
// applyCheckRunDelta doesn't, since it already performs its own SHA-keyed
// negative-cache check earlier (before FetchPRsForSHA), so folding a PR-keyed
// pre-check in here would add a code path that doesn't exist for that handler.
func (c *CacheImpl) resolveOrHealPRLinkage(owner, repoName, repo string, prNum int, missCacheKey, notFoundMsg string) (key string, issNum int, healed, ok bool) {
	key, issNum, found, healed, healErr := c.resolvePRLinkage(owner, repoName, prNum)
	if !found {
		c.mu.Lock()
		if healErr == nil {
			c.recentMissCache[missCacheKey] = time.Now()
		}
		c.mu.Unlock()
		c.logFn("[cache] %s\n", notFoundMsg)
		return "", 0, false, false
	}
	// Confirm item still in Store before proceeding. Must not hold c.mu while
	// calling any Store method (boardcache.go struct-doc invariant).
	_, storeErr := c.store.Get(repo, issNum)
	if storeErr != nil {
		c.mu.Lock()
		c.recentMissCache[missCacheKey] = time.Now()
		c.mu.Unlock()
		return "", 0, false, false
	}
	// prToKey is populated by PRHeadSHAUpdated via updateIndexes — no explicit write needed.
	return key, issNum, healed, true
}

// ---------------------------------------------------------------------------
// Delta functions
// ---------------------------------------------------------------------------

func (c *CacheImpl) applyIssueCommentDelta(payload []byte) {
	var p issueCommentPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		c.logFn("[cache] applyIssueCommentDelta: unmarshal payload: %v\n", err)
		return
	}
	switch p.Action {
	case "created":
		// handled below
	case "edited", "deleted":
		// Comment edits and deletes are not tracked in the cache; the engine
		// re-fetches comments via FetchItemDetails on the next poll cycle.
		return
	default:
		return
	}

	fullRepo := p.Repository.FullName
	issNum := p.Issue.Number
	key := itemKey(fullRepo, issNum)

	if c.matchEchoFn != nil {
		c.matchEchoFn("issue_comment", "created", key)
	}

	createdAt, _ := time.Parse(time.RFC3339, p.Comment.CreatedAt)
	comment := gh.Comment{
		ID:         p.Comment.NodeID,
		DatabaseID: p.Comment.DatabaseID,
		Author:     p.Comment.User.Login,
		Body:       p.Comment.Body,
		CreatedAt:  createdAt,
	}

	// Guard: only apply to items already in the Store.
	if _, err := c.store.Get(fullRepo, issNum); err != nil {
		return
	}

	_, changes, _ := c.store.Apply(itemstate.IssueCommentCreated{
		Repo:    fullRepo,
		Number:  issNum,
		Comment: comment,
	})
	if len(changes) == 0 {
		return // no-op (duplicate comment or item missing)
	}
	c.bumpLocalDeltaAt(key)
}

func (c *CacheImpl) applyIssuesDelta(payload []byte) {
	var p issuesPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		c.logFn("[cache] applyIssuesDelta: unmarshal payload: %v\n", err)
		return
	}

	fullRepo := p.Repository.FullName
	issNum := p.Issue.Number
	key := itemKey(fullRepo, issNum)

	owner, _, ok := splitRepo(fullRepo)
	if !ok {
		return
	}

	switch p.Action {
	case "opened":
		// Build a minimal ProjectItem from the webhook payload.
		labels := make([]string, len(p.Issue.Labels))
		for i, l := range p.Issue.Labels {
			labels[i] = l.Name
		}
		assignees := make([]string, len(p.Issue.Assignees))
		for i, a := range p.Issue.Assignees {
			assignees[i] = a.Login
		}
		pi := gh.ProjectItem{
			ID:        p.Issue.NodeID,
			Number:    issNum,
			Title:     p.Issue.Title,
			Body:      p.Issue.Body,
			Repo:      fullRepo,
			Labels:    labels,
			Assignees: assignees,
		}
		// An "opened" payload always carries issue.milestone (null or object);
		// carry it so the item is not left unknown until the next reconcile.
		pi.Milestone, pi.MilestoneKnown = milestoneFromPayload(p.Issue.Milestone)
		// PreserveBlockedBy: this payload never carries Issue Dependency data,
		// so an "opened" delivery arriving after a synchronous
		// BlockedByEdgeAdded write (spawnChildren, #1783) — e.g. for a child
		// issue this same engine just created — must not wipe it back to
		// empty (#1783 follow-up).
		_, changes, _ := c.store.Apply(itemstate.IssueOpened{Item: pi, PreserveBlockedBy: true, Partial: true, At: observedTime(p.Issue.UpdatedAt)})
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(key)

	case "closed":
		if err := c.ensureIssueInStore(owner, fullRepo, issNum); err != nil {
			c.logFn("[cache] applyIssuesDelta(closed): ensure #%d: %v\n", issNum, err)
			return
		}
		_, changes, _ := c.store.Apply(itemstate.IssueClosed{Repo: fullRepo, Number: issNum, Sender: p.Sender.Login, At: observedTime(p.Issue.UpdatedAt)})
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(key)

	case "reopened":
		if err := c.ensureIssueInStore(owner, fullRepo, issNum); err != nil {
			c.logFn("[cache] applyIssuesDelta(reopened): ensure #%d: %v\n", issNum, err)
			return
		}
		_, changes, _ := c.store.Apply(itemstate.IssueReopened{Repo: fullRepo, Number: issNum, At: observedTime(p.Issue.UpdatedAt)})
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(key)

	case "transferred", "deleted":
		// Remove the item from the Store. Store.Remove handles prToKey index cleanup.
		c.store.Remove(fullRepo, issNum)

	case "edited":
		if c.matchEchoFn != nil {
			c.matchEchoFn("issues", "edited", key)
		}
		if err := c.ensureIssueInStore(owner, fullRepo, issNum); err != nil {
			c.logFn("[cache] applyIssuesDelta(edited): ensure #%d: %v\n", issNum, err)
			return
		}
		_, changes, _ := c.store.Apply(itemstate.IssueEdited{
			Repo:   fullRepo,
			Number: issNum,
			Title:  p.Issue.Title,
			Body:   p.Issue.Body,
		})
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(key)

	case "assigned", "unassigned":
		if err := c.ensureIssueInStore(owner, fullRepo, issNum); err != nil {
			c.logFn("[cache] applyIssuesDelta(%s): ensure #%d: %v\n", p.Action, issNum, err)
			return
		}
		assignees := make([]string, len(p.Issue.Assignees))
		for i, a := range p.Issue.Assignees {
			assignees[i] = a.Login
		}
		_, changes, _ := c.store.Apply(itemstate.IssueAssigneesUpdated{
			Repo:      fullRepo,
			Number:    issNum,
			Assignees: assignees,
		})
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(key)

	case "labeled":
		c.applyIssueLabelMutationDelta(owner, fullRepo, issNum, key, "labeled", p.Label.Name,
			itemstate.IssueLabeled{Repo: fullRepo, Number: issNum, Label: p.Label.Name, Sender: p.Sender.Login, At: observedTime(p.Issue.UpdatedAt)})

	case "unlabeled":
		c.applyIssueLabelMutationDelta(owner, fullRepo, issNum, key, "unlabeled", p.Label.Name,
			itemstate.IssueUnlabeled{Repo: fullRepo, Number: issNum, Label: p.Label.Name, Sender: p.Sender.Login, At: observedTime(p.Issue.UpdatedAt)})

	case "milestoned", "demilestoned":
		// issue.milestone is the post-event value: the new milestone on
		// "milestoned", null on "demilestoned". Applied directly so a change
		// does not wait for the next reconcile (#1967 R10). An absent or
		// malformed key leaves the cached value untouched (never "none").
		ms, known := milestoneFromPayload(p.Issue.Milestone)
		if !known {
			c.logFn("[cache] applyIssuesDelta(%s): #%d payload has no readable issue.milestone; ignoring\n", p.Action, issNum)
			return
		}
		if p.Action == "demilestoned" {
			ms = nil
		}
		// Peek, never ensureIssueInStore: an uncached item would trigger a
		// FetchProjectItem, a GitHub call R10 rules out. The next reconcile
		// captures the milestone for an item the store does not hold yet.
		if _, ok := c.store.Peek(fullRepo, issNum); !ok {
			c.logFn("[cache] applyIssuesDelta(%s): #%d not in store; skipping (next reconcile captures its milestone)\n", p.Action, issNum)
			return
		}
		_, changes, _ := c.store.Apply(itemstate.IssueMilestoneUpdated{Repo: fullRepo, Number: issNum, Milestone: ms, At: observedTime(p.Issue.UpdatedAt)})
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(key)

		// locked, unlocked, pinned, unpinned: no state in the engine's
		// pipeline depends on these fields.
	}
}

// matchEcho runs the best available echo matcher for an inbound webhook and
// reports whether it matched one of the engine's own writes and which overseer
// requester (if any) registered it. With no matcher injected it reports no match.
func (c *CacheImpl) matchEcho(eventType, action, key string) (bool, string) {
	switch {
	case c.matchEchoViaFn != nil:
		return c.matchEchoViaFn(eventType, action, key)
	case c.matchEchoReportedFn != nil:
		return c.matchEchoReportedFn(eventType, action, key), ""
	case c.matchEchoFn != nil:
		c.matchEchoFn(eventType, action, key)
	}
	return false, ""
}

// applyIssueLabelMutationDelta is the shared body for the "labeled"/"unlabeled"
// cases of applyIssuesDelta: echo the match, ensure the issue is in the Store,
// apply the given mutation, and bump the local delta on a real change.
func (c *CacheImpl) applyIssueLabelMutationDelta(owner, fullRepo string, issNum int, key, action, labelName string, mutation itemstate.Mutation) {
	echoed, via := false, ""
	if c.matchEchoViaFn != nil {
		echoed, via = c.matchEchoViaFn("issues", action, key+"+"+labelName)
	} else if c.matchEchoReportedFn != nil {
		echoed = c.matchEchoReportedFn("issues", action, key+"+"+labelName)
	} else if c.matchEchoFn != nil {
		c.matchEchoFn("issues", action, key+"+"+labelName)
	}
	switch v := mutation.(type) {
	case itemstate.IssueLabeled:
		v.EchoOfEngine = echoed
		if echoed {
			v.Requester = via
		}
		mutation = v
	case itemstate.IssueUnlabeled:
		v.EchoOfEngine = echoed
		if echoed {
			v.Requester = via
		}
		mutation = v
	}
	if err := c.ensureIssueInStore(owner, fullRepo, issNum); err != nil {
		c.logFn("[cache] applyIssuesDelta(%s): ensure #%d: %v\n", action, issNum, err)
		return
	}
	_, changes, _ := c.store.Apply(mutation)
	if len(changes) == 0 {
		return
	}
	c.bumpLocalDeltaAt(key)
}

func (c *CacheImpl) applyPullRequestDelta(payload []byte) {
	var p pullRequestPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		c.logFn("[cache] applyPullRequestDelta: unmarshal payload: %v\n", err)
		return
	}

	repo := p.Repository.FullName
	prNum := p.PullRequest.Number

	switch p.Action {
	case "opened", "closed", "synchronize", "reopened":
		// SHA-tracking path — handled after this switch.

	case "ready_for_review":
		if c.matchEchoFn != nil {
			c.matchEchoFn("pull_request", "ready_for_review", prKey(repo, prNum))
		}
		// Look up linked issue via Store's prToKey index (no c.mu held).
		// Requires PR linkage to be established first via PRHeadSHAUpdated.
		issKey, issFound := c.store.GetByPRKey(repo, prNum)
		if !issFound {
			return
		}
		issRepo, issNum, parseOK := parseItemKey(issKey)
		if !parseOK {
			return
		}
		// Read current PR details and apply updated Draft=false.
		snap, snapErr := c.store.Get(issRepo, issNum)
		if snapErr != nil {
			return
		}
		lpr := snap.LinkedPR()
		if lpr == nil {
			return
		}
		c.store.Apply(itemstate.PRDetailsUpdated{
			Repo:     issRepo,
			Number:   issNum,
			PRNumber: lpr.Number,
			Title:    lpr.Title,
			State:    lpr.State,
			Merged:   lpr.Merged,
			Draft:    false,
			At:       observedTime(p.PullRequest.UpdatedAt),
		})
		c.bumpLocalDeltaAt(issKey)
		return

	case "converted_to_draft":
		// Look up linked issue via Store's prToKey index (no c.mu held).
		// Requires PR linkage to be established first via PRHeadSHAUpdated.
		issKey, issFound := c.store.GetByPRKey(repo, prNum)
		if !issFound {
			return
		}
		issRepo, issNum, parseOK := parseItemKey(issKey)
		if !parseOK {
			return
		}
		// Read current PR details and apply updated Draft=true.
		snap, snapErr := c.store.Get(issRepo, issNum)
		if snapErr != nil {
			return
		}
		lpr := snap.LinkedPR()
		if lpr == nil {
			return
		}
		c.store.Apply(itemstate.PRDetailsUpdated{
			Repo:     issRepo,
			Number:   issNum,
			PRNumber: lpr.Number,
			Title:    lpr.Title,
			State:    lpr.State,
			Merged:   lpr.Merged,
			Draft:    true,
			At:       observedTime(p.PullRequest.UpdatedAt),
		})
		c.bumpLocalDeltaAt(issKey)
		return

	case "review_requested":
		issKey, issFound := c.store.GetByPRKey(repo, prNum)
		if !issFound {
			return
		}
		issRepo, issNum, parseOK := parseItemKey(issKey)
		if !parseOK {
			return
		}
		reviewers := make([]gh.ReviewRequest, 0, len(p.PullRequest.RequestedReviewers))
		for _, r := range p.PullRequest.RequestedReviewers {
			reviewers = append(reviewers, gh.ReviewRequest{Login: r.Login, IsBot: r.Type == "Bot" || gh.IsBotLogin(r.Login)})
		}
		c.store.Apply(itemstate.PRReviewRequested{
			Repo:      issRepo,
			Number:    issNum,
			Reviewers: reviewers,
		})
		c.bumpLocalDeltaAt(issKey)
		return

	case "review_request_removed":
		issKey, issFound := c.store.GetByPRKey(repo, prNum)
		if !issFound {
			return
		}
		issRepo, issNum, parseOK := parseItemKey(issKey)
		if !parseOK {
			return
		}
		c.store.Apply(itemstate.PRReviewRequestRemoved{
			Repo:   issRepo,
			Number: issNum,
			Login:  p.RequestedReviewer.Login,
		})
		c.bumpLocalDeltaAt(issKey)
		return

	// labeled, unlabeled, assigned, unassigned, edited: no engine pipeline state
	// depends on PR-level labels/assignees/metadata. These are intentional no-ops.
	default:
		return
	}

	// --- SHA-tracking path (opened, closed, synchronize, reopened) ---
	//
	// Both PRHeadSHAUpdated calls below (the normal path, for an issue already
	// known to the Store, and the auto-heal path, for one discovered via
	// fallback lookup) deliberately do NOT stamp CacheImpl.linkedPRFetchedAt,
	// even though synchronize is precisely the force-push/rebase event whose
	// staleness linkedPRCacheTTL exists to bound.
	// This is load-bearing, not an omission — do not "fix" it by adding a
	// stamp here. linkedPRFetchedAt records live GitHub confirmation only, by
	// design (see its doc comment in boardcache.go); a delta-path stamp would
	// make FetchLinkedPR's freshness trust a function of webhook delivery,
	// which this event-sourced cache's own design (ADR-034) admits can miss
	// events until the next reconciliation (up to 60 min) — precisely the
	// unconfirmed channel that TTL exists not to trust, and precisely the
	// change #1378 proposed and ADR-1551 records the rejection of. See
	// ADR-1551 for the full rationale.
	//
	// Echo-match for opened and closed PR mutations (synchronize/reopened are not tracked).
	if c.matchEchoFn != nil && (p.Action == "opened" || p.Action == "closed") {
		c.matchEchoFn("pull_request", p.Action, prKey(repo, prNum))
	}
	sha := p.PullRequest.Head.SHA
	prAt := observedTime(p.PullRequest.UpdatedAt)

	owner, repoName, ok := splitRepo(repo)
	if !ok {
		return
	}

	// Look up linked issue via Store's prToKey index (no c.mu held).
	issKey, issFound := c.store.GetByPRKey(repo, prNum)

	if issFound {
		// Normal path: linked issue is already known.
		issRepo, issNum2, parseOK := parseItemKey(issKey)
		if parseOK {
			c.store.Apply(itemstate.PRDetailsUpdated{
				Repo:     issRepo,
				Number:   issNum2,
				PRNumber: prNum,
				Title:    p.PullRequest.Title,
				State:    p.PullRequest.State,
				Merged:   p.PullRequest.Merged,
				Draft:    p.PullRequest.Draft,
				At:       prAt,
			})
			c.store.Apply(itemstate.PRHeadSHAUpdated{
				Repo:   issRepo,
				Number: issNum2,
				SHA:    sha,
				At:     prAt,
			})
			c.bumpLocalDeltaAt(issKey)
		}
		return
	}

	// Miss: check negative cache.
	mk := missKey(repo, prNum)
	c.mu.RLock()
	if t, found := c.recentMissCache[mk]; found && time.Since(t) < recentMissTTL {
		c.mu.RUnlock()
		return
	}
	c.mu.RUnlock()

	// Auto-heal: resolve PR linkage via REST or authoritative index.
	key, resolvedIssNum, healed, ok := c.resolveOrHealPRLinkage(owner, repoName, repo, prNum, mk,
		fmt.Sprintf("dropped pull_request delta for PR #%d: no closing issue in cache", prNum))
	if !ok {
		return
	}

	c.store.Apply(itemstate.PRDetailsUpdated{
		Repo:     repo,
		Number:   resolvedIssNum,
		PRNumber: prNum,
		Title:    p.PullRequest.Title,
		State:    p.PullRequest.State,
		Merged:   p.PullRequest.Merged,
		Draft:    p.PullRequest.Draft,
		At:       prAt,
	})
	// Same non-stamping of linkedPRFetchedAt, and the same reason, as the
	// normal-path PRHeadSHAUpdated call above — see that comment.
	c.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        repo,
		Number:      resolvedIssNum,
		LinkedPRNum: prNum,
		SHA:         sha,
		At:          prAt,
	})
	c.bumpLocalDeltaAt(key)
	if healed {
		c.store.Apply(itemstate.DeepFetchInvalidated{Repo: repo, Number: resolvedIssNum})
		c.logFn("[cache] auto-heal: PR #%d → issue #%d; deep cache invalidated and delta re-applied\n", prNum, resolvedIssNum)
	}
}

func (c *CacheImpl) applyPullRequestReviewDelta(payload []byte) {
	var p pullRequestReviewPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		c.logFn("[cache] applyPullRequestReviewDelta: unmarshal payload: %v\n", err)
		return
	}
	switch p.Action {
	case "submitted", "edited", "dismissed":
		// All three action variants carry the full review object and are routed
		// through the same upsert path (keyed by DatabaseID).
	default:
		return
	}
	repo := p.Repository.FullName
	prNum := p.PullRequest.Number

	owner, repoName, ok := splitRepo(repo)
	if !ok {
		return
	}

	// submitted_at is the time of the original submission. It orders a "submitted"
	// event, but an "edited" or "dismissed" one happens later and carries no time
	// of its own, so it is ordered by receipt: stamped with the old submission time
	// it would look older than any read made since, and be dropped.
	reviewAt := time.Now()
	if p.Action == "submitted" {
		reviewAt = observedTime(p.Review.SubmittedAt)
	}
	review := gh.PRReview{
		Author:     p.Review.User.Login,
		State:      strings.ToUpper(p.Review.State),
		Body:       p.Review.Body,
		DatabaseID: p.Review.DatabaseID,
		CommitID:   p.Review.CommitID,
	}
	if p.Review.SubmittedAt != "" {
		review.SubmittedAt, _ = time.Parse(time.RFC3339, string(p.Review.SubmittedAt))
	}

	// Normal path: look up issue via Store's prToKey index (no c.mu held).
	issKey, issFound := c.store.GetByPRKey(repo, prNum)

	if issFound {
		issRepo, issNum, parseOK := parseItemKey(issKey)
		if parseOK {
			c.store.Apply(itemstate.PRReviewSubmitted{
				Repo:   issRepo,
				Number: issNum,
				Review: review,
				At:     reviewAt,
			})
			// Side effect: remove reviewer from LinkedPRReviewRequests. GitHub does not
			// reliably fire review_request_removed for bot reviewers (e.g. Copilot edited
			// reviews), so we treat the review event itself as authoritative (ADR 034).
			c.store.Apply(itemstate.PRReviewRequestRemoved{
				Repo:   issRepo,
				Number: issNum,
				Login:  p.Review.User.Login,
			})
			c.bumpLocalDeltaAt(issKey)
		}
		return
	}

	// Miss: check negative cache.
	mk := missKey(repo, prNum)
	c.mu.RLock()
	if t, found := c.recentMissCache[mk]; found && time.Since(t) < recentMissTTL {
		c.mu.RUnlock()
		c.logFn("[cache] dropped pull_request_review delta for PR #%d: negative cache hit\n", prNum)
		return
	}
	c.mu.RUnlock()

	// Auto-heal: resolve PR linkage via REST or authoritative index.
	key, resolvedIssNum, healed, ok := c.resolveOrHealPRLinkage(owner, repoName, repo, prNum, mk,
		fmt.Sprintf("dropped pull_request_review delta for PR #%d: no closing issue in cache", prNum))
	if !ok {
		return
	}

	// Deliberately does not stamp linkedPRFetchedAt — see the substantive
	// comment on applyPullRequestDelta's SHA-tracking path (boardcache.go's
	// linkedPRFetchedAt/linkedPRCacheTTL doc comments, and ADR-1551).
	c.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        repo,
		Number:      resolvedIssNum,
		LinkedPRNum: prNum,
	})
	c.store.Apply(itemstate.PRReviewSubmitted{
		Repo:   repo,
		Number: resolvedIssNum,
		Review: review,
		At:     reviewAt,
	})
	// Side effect: remove reviewer from LinkedPRReviewRequests (same rationale as normal path).
	c.store.Apply(itemstate.PRReviewRequestRemoved{
		Repo:   repo,
		Number: resolvedIssNum,
		Login:  p.Review.User.Login,
	})
	c.bumpLocalDeltaAt(key)
	if healed {
		c.store.Apply(itemstate.DeepFetchInvalidated{Repo: repo, Number: resolvedIssNum})
		c.logFn("[cache] auto-heal: PR #%d → issue #%d; deep cache invalidated and delta re-applied\n", prNum, resolvedIssNum)
	}
}

func (c *CacheImpl) applyPullRequestReviewCommentDelta(payload []byte) {
	var p pullRequestReviewCommentPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		c.logFn("[cache] applyPullRequestReviewCommentDelta: unmarshal payload: %v\n", err)
		return
	}
	switch p.Action {
	case "created":
		// handled below
	case "edited", "deleted":
		// Comment edits and deletes are not tracked in the cache; the engine
		// re-fetches review thread state via FetchItemDetails on the next poll cycle.
		return
	default:
		return
	}
	repo := p.Repository.FullName
	prNum := p.PullRequest.Number

	owner, repoName, ok := splitRepo(repo)
	if !ok {
		return
	}

	createdAt, _ := time.Parse(time.RFC3339, p.Comment.CreatedAt)
	comment := gh.Comment{
		ID:         p.Comment.NodeID,
		DatabaseID: p.Comment.DatabaseID,
		Author:     p.Comment.User.Login,
		Body:       p.Comment.Body,
		CreatedAt:  createdAt,
		DiffHunk:   p.Comment.DiffHunk,
		Path:       p.Comment.Path,
		FromPR:     prNum,
	}
	if p.Comment.Line != nil {
		comment.Line = *p.Comment.Line
	}
	if p.Comment.OriginalLine != nil {
		comment.OriginalLine = *p.Comment.OriginalLine
	}

	// Normal path: look up issue via Store's prToKey index (no c.mu held).
	issKey, issFound := c.store.GetByPRKey(repo, prNum)

	if issFound {
		issRepo, issNum, parseOK := parseItemKey(issKey)
		if parseOK {
			_, changes, _ := c.store.Apply(itemstate.ReviewThreadCommentAdded{
				Repo:        issRepo,
				IssueNumber: issNum,
				Comment:     comment,
			})
			if len(changes) > 0 {
				// New comment: invalidate deep cache (ReviewThreadID not yet populated).
				c.store.Apply(itemstate.DeepFetchInvalidated{Repo: issRepo, Number: issNum})
				c.bumpLocalDeltaAt(issKey)
			}
		}
		return
	}

	// Miss: check negative cache.
	mk := missKey(repo, prNum)
	c.mu.RLock()
	if t, found := c.recentMissCache[mk]; found && time.Since(t) < recentMissTTL {
		c.mu.RUnlock()
		c.logFn("[cache] dropped pull_request_review_comment delta for PR #%d: negative cache hit\n", prNum)
		return
	}
	c.mu.RUnlock()

	// Auto-heal: resolve PR linkage via REST or authoritative index.
	key, resolvedIssNum, healed, ok := c.resolveOrHealPRLinkage(owner, repoName, repo, prNum, mk,
		fmt.Sprintf("dropped pull_request_review_comment delta for PR #%d: no closing issue in cache", prNum))
	if !ok {
		return
	}

	// Deliberately does not stamp linkedPRFetchedAt — see the substantive
	// comment on applyPullRequestDelta's SHA-tracking path (boardcache.go's
	// linkedPRFetchedAt/linkedPRCacheTTL doc comments, and ADR-1551).
	c.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        repo,
		Number:      resolvedIssNum,
		LinkedPRNum: prNum,
	})
	_, commentChanges, _ := c.store.Apply(itemstate.ReviewThreadCommentAdded{
		Repo:        repo,
		IssueNumber: resolvedIssNum,
		Comment:     comment,
	})
	if len(commentChanges) > 0 {
		// New comment: invalidate deep cache (ReviewThreadID not yet populated).
		c.store.Apply(itemstate.DeepFetchInvalidated{Repo: repo, Number: resolvedIssNum})
	}
	c.bumpLocalDeltaAt(key)
	if healed {
		c.logFn("[cache] auto-heal: PR #%d → issue #%d; delta re-applied\n", prNum, resolvedIssNum)
	}
}

func (c *CacheImpl) applyCheckRunDelta(payload []byte) {
	var p checkRunPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		c.logFn("[cache] applyCheckRunDelta: unmarshal payload: %v\n", err)
		return
	}
	switch p.Action {
	case "completed":
		// handled below
	case "created", "rerequested", "requested_action":
		// created: run started but has no outcome yet.
		// rerequested/requested_action: administrative UX signals.
		// The engine watches outcomes only (completed) for the CI gate.
		return
	default:
		return
	}
	sha := p.CheckRun.HeadSHA
	if sha == "" {
		return
	}
	repo := p.Repository.FullName

	owner, repoName, ok := splitRepo(repo)
	if !ok {
		return
	}

	cr := gh.CheckRun{
		ID:         p.CheckRun.ID,
		Name:       p.CheckRun.Name,
		Status:     p.CheckRun.Status,
		Conclusion: p.CheckRun.Conclusion,
	}

	// Apply to Store. For known SHAs (shaToKey hit), the run is written directly to
	// LinkedPR.CheckRuns and LinkedPRChanged is emitted. For unknown SHAs, the run
	// is buffered in Store.pendingCheckRuns and CheckRunChanged is emitted; the
	// auto-heal path below then resolves the SHA→PR→issue chain and PRHeadSHAUpdated
	// drains the buffer — no explicit replay needed.
	snap, changes, _ := c.store.Apply(itemstate.CheckRunCompleted{
		Repo: repo,
		SHA:  sha,
		Run:  cr,
	})
	if len(changes) > 0 && changes[0].Fields&itemstate.LinkedPRChanged != 0 {
		// SHA was linked — LinkedPR updated directly; bump localDeltaAt and return.
		key := itemKey(snap.Repo(), snap.Number())
		c.bumpLocalDeltaAt(key)
		return
	}

	// SHA not in Store's shaToKey index — check negative cache.
	msha := missKeyForSHA(sha)
	c.mu.RLock()
	if t, found := c.recentMissCache[msha]; found && time.Since(t) < recentMissTTL {
		c.mu.RUnlock()
		return
	}
	c.mu.RUnlock()

	// Auto-heal step 1: fetch PRs associated with this SHA.
	prNums, err := c.fallback.FetchPRsForSHA(owner, repoName, sha)
	if err != nil {
		c.logFn("[cache] applyCheckRunCompleted: FetchPRsForSHA for %s: %v\n", sha, err)
		// Transient error — do not record a negative miss; retry on next webhook.
		return
	}
	if len(prNums) == 0 {
		c.mu.Lock()
		c.recentMissCache[msha] = time.Now()
		c.mu.Unlock()
		c.logFn("[cache] dropped check_run delta for SHA %s: no associated PR found\n", sha)
		return
	}
	prNum := prNums[0]

	// Auto-heal step 2: resolve which issue the PR closes.
	key, resolvedIssNum, healed, ok := c.resolveOrHealPRLinkage(owner, repoName, repo, prNum, msha,
		fmt.Sprintf("dropped check_run delta for SHA %s: no closing issue in cache for PR #%d", sha, prNum))
	if !ok {
		return
	}

	// Update Store: set LinkedPRNum + SHA (updates shaToKey index). PRHeadSHAUpdated
	// drains any pendingCheckRuns[sha] into the item's LinkedPR.CheckRuns automatically
	// — no explicit CheckRunCompleted replay needed.
	//
	// Deliberately does not stamp linkedPRFetchedAt — see the substantive
	// comment on applyPullRequestDelta's SHA-tracking path (boardcache.go's
	// linkedPRFetchedAt/linkedPRCacheTTL doc comments, and ADR-1551).
	c.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        repo,
		Number:      resolvedIssNum,
		LinkedPRNum: prNum,
		SHA:         sha,
	})
	c.bumpLocalDeltaAt(key)
	if healed {
		c.store.Apply(itemstate.DeepFetchInvalidated{Repo: repo, Number: resolvedIssNum})
		c.logFn("[cache] auto-heal: check_run SHA %s → PR #%d → issue #%d; deep cache invalidated\n", sha, prNum, resolvedIssNum)
	}
}

// applyCheckSuite is a documented no-op. The comment in ApplyDelta's switch explains
// why check_suite is skipped; this stub exists only so the call site compiles.
func (c *CacheImpl) applyCheckSuite(_ []byte) {}

func (c *CacheImpl) applyProjectsV2ItemDelta(payload []byte) {
	var p projectsV2ItemPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		c.logFn("[cache] applyProjectsV2ItemDelta: unmarshal payload: %v\n", err)
		return
	}

	action := string(p.Action)
	itemID := string(p.ProjectsV2Item.NodeID)

	filtered := !c.projectFieldFilterDisabledForTest.Load()
	if filtered {
		// Project filter (#2345, R1/R3): org-level delivery carries events for
		// every project in the org. It runs before any side effect — the
		// FetchItemDetails call, the echo match (whose registry key has no
		// project discriminator, so a foreign event must not consume an entry)
		// and the removal. An event without a project ID, or arriving before
		// this instance knows its own project, is never trusted: reconcile
		// corrects the cache.
		own := c.ProjectID()
		ev := string(p.ProjectsV2Item.ProjectNodeID)
		switch {
		case ev == "":
			c.logFn("[cache] projects_v2_item %s ignored: payload has no project_node_id\n", action)
			return
		case own == "":
			c.logFn("[cache] projects_v2_item %s ignored: own project ID not yet known\n", action)
			return
		case ev != own:
			c.logFn("[cache] projects_v2_item %s ignored: foreign project %s\n", action, ev)
			return
		}
	}

	// The board item ID is projects_v2_item.node_id (PVTI_…), the value the
	// Store, itemIDToKey and the echo registry key on. The numeric
	// projects_v2_item.id is a different identifier and is never a fallback
	// (#2383).
	if itemID == "" {
		c.logFn("[cache] projects_v2_item %s ignored: payload has no node_id\n", action)
		return
	}

	switch action {
	case "created":
		// An existing issue was added to the project board. The payload provides only
		// content_node_id; fetch full item details from GraphQL to get number and repo.
		nodeID := string(p.ProjectsV2Item.ContentNodeID)
		if nodeID == "" || p.ProjectsV2Item.ContentType != "Issue" {
			return
		}
		pi := &gh.ProjectItem{ID: nodeID}
		if err := c.fallback.FetchItemDetails(pi); err != nil {
			c.logFn("[cache] applyProjectsV2ItemDelta(created): FetchItemDetails for node %s: %v\n", nodeID, err)
			return
		}
		if pi.Number == 0 || pi.Repo == "" {
			c.logFn("[cache] applyProjectsV2ItemDelta(created): incomplete item for node %s (number=%d repo=%q)\n", nodeID, pi.Number, pi.Repo)
			return
		}
		// Record the board-side item ID (PVTI_xxx) so that subsequent
		// projects_v2_item.edited/deleted/archived events can resolve via itemIDToKey.
		pi.ItemID = itemID
		_, changes, _ := c.store.Apply(itemstate.IssueOpened{Item: *pi, Partial: true, At: observedTime(p.ProjectsV2Item.UpdatedAt)})
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(itemKey(pi.Repo, pi.Number))

	case "edited":
		// Field filter (#2345, R2/R3): only an edit of this board's Status field
		// is a Status change. Like the project filter it precedes matchEcho.
		if filtered {
			evField := string(p.Changes.FieldValue.FieldNodeID)
			if evField == "" {
				c.logFn("[cache] projects_v2_item edited ignored: payload has no field_node_id\n")
				return
			}
			statusID := c.resolveStatusFieldID()
			if statusID == "" {
				c.logFn("[cache] projects_v2_item edited ignored: Status field ID not yet known\n")
				return
			}
			if evField != statusID {
				c.logFn("[cache] projects_v2_item edited ignored: field %s is not the Status field\n", evField)
				return
			}
		}
		echoed, via := c.matchEcho("projects_v2_item", "edited", itemID)
		// Only a Status (single_select) edit changes anything the cache holds.
		// A text-field edit — in particular the engine's own display-only
		// status-line write echoing back (#2048), or a human editing it — is a
		// deliberate no-op here: no Store change, no local-delta stamp (so no
		// wake, no invalidation, no drift count). This guard, not an accident
		// of a later empty-name check, is what makes that a named, testable
		// rule (TestDeltaProjectsV2ItemTextEditIsNoOp).
		suppress := !c.textEditSuppressionDisabledForTest.Load()
		if suppress && p.Changes.FieldValue.FieldType != "single_select" {
			return
		}
		newStatus := editedStatusName(p.Changes.FieldValue.To)
		if !suppress && newStatus == "" {
			// Neutralised (test seam): read any scalar "to" as a Status name, as
			// an unguarded handler would.
			var scalar string
			if json.Unmarshal(p.Changes.FieldValue.To, &scalar) == nil {
				newStatus = scalar
			}
		}
		if newStatus == "" {
			// A Status clear arrives as a Status-field edit with no from/to
			// keys at all (#2383). An empty Status means "unknown" to the
			// Store, which several admission rules key on, so a webhook never
			// writes one: the cached Status stays and the next reconcile
			// corrects it. Deliberately worded without "ignored": it is not a
			// filter drop. matchEcho already ran, so an engine-caused clear
			// consumed its echo entry.
			if isAbsentJSON(p.Changes.FieldValue.To) {
				c.logFn("[cache] projects_v2_item edited: Status clear received for %s; left to reconcile\n", itemID)
			} else {
				c.logFn("[cache] projects_v2_item edited: Status edit for %s carries an unreadable new value; left to reconcile\n", itemID)
			}
			return
		}
		edit := itemstate.ProjectV2ItemEdited{
			ItemID:       itemID,
			NewStatus:    newStatus,
			Sender:       string(p.Sender.Login),
			EchoOfEngine: echoed,
		}
		if echoed {
			edit.Requester = via
		}
		if t, err := time.Parse(time.RFC3339, string(p.ProjectsV2Item.UpdatedAt)); err == nil {
			edit.At = t
		}
		edit.Observed = observedTime(p.ProjectsV2Item.UpdatedAt)
		snap, changes, _ := c.store.Apply(edit)
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(itemKey(snap.Repo(), snap.Number()))

	case "deleted", "archived":
		// Remove the item from the Store. Store.RemoveByItemID handles prToKey index cleanup.
		_, _, ok := c.store.RemoveByItemID(itemID)
		if !ok {
			return
		}

		// restored: item is back on the board; the next poll reconcile will re-add it.
		// reordered: no position state in the engine.
	}
}

// isAbsentJSON reports whether a raw JSON value is missing or the literal null.
func isAbsentJSON(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null"
}

// resolveStatusFieldID returns this board's Status field node ID. When it is
// not yet recorded it asks the fallback client once per call and records a
// successful answer (#2345), so Status webhooks work before the engine's first
// full-board poll has resolved it. A failed lookup returns "" and is retried on
// the next Status edit. mu is never held across the fallback call.
func (c *CacheImpl) resolveStatusFieldID() string {
	if id := c.StatusFieldID(); id != "" {
		return id
	}
	projectID := c.ProjectID()
	if projectID == "" || c.fallback == nil {
		return ""
	}
	sf, err := c.fallback.FetchStatusField(projectID)
	if err != nil || sf == nil {
		if err != nil {
			c.logFn("[cache] projects_v2_item: resolving Status field ID: %v\n", err)
		}
		return ""
	}
	c.SetStatusFieldID(sf.FieldID)
	return sf.FieldID
}
