package boardcache

import (
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
	Label  struct {
		Name string `json:"name"`
	} `json:"label"`
	Issue struct {
		Number    int    `json:"number"`
		NodeID    string `json:"node_id"`
		Title     string `json:"title"`
		Body      string `json:"body"`
		State     string `json:"state"`
		Labels    []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Assignees []struct {
			Login string `json:"login"`
		} `json:"assignees"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type pullRequestPayload struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		State   string `json:"state"`
		Merged  bool   `json:"merged"`
		Draft   bool   `json:"draft"`
		Head    struct {
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
		User       struct {
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

type projectsV2ItemPayload struct {
	Action  string `json:"action"`
	Changes struct {
		FieldValue struct {
			FieldType string `json:"field_type"`
			To        struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"to"`
		} `json:"field_value"`
	} `json:"changes"`
	ProjectsV2Item struct {
		ID            string `json:"id"`
		ContentNodeID string `json:"content_node_id"`
		ContentType   string `json:"content_type"`
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

// ---------------------------------------------------------------------------
// Delta functions
// ---------------------------------------------------------------------------

func (c *CacheImpl) applyIssueCommentDelta(payload []byte) {
	var p issueCommentPayload
	if err := json.Unmarshal(payload, &p); err != nil {
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
		_, changes, _ := c.store.Apply(itemstate.IssueOpened{Item: pi})
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(key)

	case "closed":
		if err := c.ensureIssueInStore(owner, fullRepo, issNum); err != nil {
			c.logFn("[cache] applyIssuesDelta(closed): ensure #%d: %v\n", issNum, err)
			return
		}
		_, changes, _ := c.store.Apply(itemstate.IssueClosed{Repo: fullRepo, Number: issNum})
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(key)

	case "reopened":
		if err := c.ensureIssueInStore(owner, fullRepo, issNum); err != nil {
			c.logFn("[cache] applyIssuesDelta(reopened): ensure #%d: %v\n", issNum, err)
			return
		}
		_, changes, _ := c.store.Apply(itemstate.IssueReopened{Repo: fullRepo, Number: issNum})
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(key)

	case "transferred", "deleted":
		// Remove the item from the Store. Store.Remove handles prToKey index cleanup.
		c.store.Remove(fullRepo, issNum)

	case "edited":
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
		if err := c.ensureIssueInStore(owner, fullRepo, issNum); err != nil {
			c.logFn("[cache] applyIssuesDelta(labeled): ensure #%d: %v\n", issNum, err)
			return
		}
		_, changes, _ := c.store.Apply(itemstate.IssueLabeled{
			Repo:   fullRepo,
			Number: issNum,
			Label:  p.Label.Name,
		})
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(key)

	case "unlabeled":
		if err := c.ensureIssueInStore(owner, fullRepo, issNum); err != nil {
			c.logFn("[cache] applyIssuesDelta(unlabeled): ensure #%d: %v\n", issNum, err)
			return
		}
		_, changes, _ := c.store.Apply(itemstate.IssueUnlabeled{
			Repo:   fullRepo,
			Number: issNum,
			Label:  p.Label.Name,
		})
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(key)

	// milestoned, demilestoned, locked, unlocked, pinned, unpinned:
	// no state in the engine's pipeline depends on these fields.
	}
}

func (c *CacheImpl) applyPullRequestDelta(payload []byte) {
	var p pullRequestPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return
	}

	repo := p.Repository.FullName
	prNum := p.PullRequest.Number

	switch p.Action {
	case "opened", "closed", "synchronize", "reopened":
		// SHA-tracking path — handled after this switch.

	case "ready_for_review":
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
	sha := p.PullRequest.Head.SHA

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
			})
			c.store.Apply(itemstate.PRHeadSHAUpdated{
				Repo:   issRepo,
				Number: issNum2,
				SHA:    sha,
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

	// Auto-heal: resolve PR linkage via REST.
	key, resolvedIssNum, found, healErr := c.resolvePRLinkage(owner, repoName, prNum)

	c.mu.Lock()
	if !found {
		if healErr == nil {
			c.recentMissCache[mk] = time.Now()
		}
		c.mu.Unlock()
		c.logFn("[cache] dropped pull_request delta for PR #%d: no closing issue in cache\n", prNum)
		return
	}
	// Confirm item still in Store before proceeding.
	if _, storeErr := c.store.Get(repo, resolvedIssNum); storeErr != nil {
		c.recentMissCache[mk] = time.Now()
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	// prToKey is populated by PRHeadSHAUpdated via updateIndexes — no explicit write needed.

	c.store.Apply(itemstate.PRDetailsUpdated{
		Repo:     repo,
		Number:   resolvedIssNum,
		PRNumber: prNum,
		Title:    p.PullRequest.Title,
		State:    p.PullRequest.State,
		Merged:   p.PullRequest.Merged,
		Draft:    p.PullRequest.Draft,
	})
	c.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        repo,
		Number:      resolvedIssNum,
		LinkedPRNum: prNum,
		SHA:         sha,
	})
	c.store.Apply(itemstate.DeepFetchInvalidated{Repo: repo, Number: resolvedIssNum})
	c.bumpLocalDeltaAt(key)
	c.logFn("[cache] auto-heal: PR #%d → issue #%d; deep cache invalidated and delta re-applied\n", prNum, resolvedIssNum)
}

func (c *CacheImpl) applyPullRequestReviewDelta(payload []byte) {
	var p pullRequestReviewPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return
	}
	switch p.Action {
	case "submitted":
		// handled below
	case "edited", "dismissed":
		// Review edits and dismissals do not change the set of pending reviewers
		// or the overall approval state tracked by the engine.
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

	review := gh.PRReview{
		Author:     p.Review.User.Login,
		State:      strings.ToUpper(p.Review.State),
		Body:       p.Review.Body,
		DatabaseID: p.Review.DatabaseID,
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

	// Auto-heal: resolve PR linkage via REST.
	key, resolvedIssNum, found, healErr := c.resolvePRLinkage(owner, repoName, prNum)

	c.mu.Lock()
	if !found {
		if healErr == nil {
			c.recentMissCache[mk] = time.Now()
		}
		c.mu.Unlock()
		c.logFn("[cache] dropped pull_request_review delta for PR #%d: no closing issue in cache\n", prNum)
		return
	}
	if _, storeErr := c.store.Get(repo, resolvedIssNum); storeErr != nil {
		c.recentMissCache[mk] = time.Now()
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	// prToKey is populated by PRHeadSHAUpdated via updateIndexes — no explicit write needed.

	c.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        repo,
		Number:      resolvedIssNum,
		LinkedPRNum: prNum,
	})
	c.store.Apply(itemstate.DeepFetchInvalidated{Repo: repo, Number: resolvedIssNum})
	c.store.Apply(itemstate.PRReviewSubmitted{
		Repo:   repo,
		Number: resolvedIssNum,
		Review: review,
	})
	c.bumpLocalDeltaAt(key)
	c.logFn("[cache] auto-heal: PR #%d → issue #%d; deep cache invalidated and delta re-applied\n", prNum, resolvedIssNum)
}

func (c *CacheImpl) applyPullRequestReviewCommentDelta(payload []byte) {
	var p pullRequestReviewCommentPayload
	if err := json.Unmarshal(payload, &p); err != nil {
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

	// Auto-heal: resolve PR linkage via REST.
	key, resolvedIssNum, found, healErr := c.resolvePRLinkage(owner, repoName, prNum)

	c.mu.Lock()
	if !found {
		if healErr == nil {
			c.recentMissCache[mk] = time.Now()
		}
		c.mu.Unlock()
		c.logFn("[cache] dropped pull_request_review_comment delta for PR #%d: no closing issue in cache\n", prNum)
		return
	}
	if _, storeErr := c.store.Get(repo, resolvedIssNum); storeErr != nil {
		c.recentMissCache[mk] = time.Now()
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	// prToKey is populated by PRHeadSHAUpdated via updateIndexes — no explicit write needed.

	c.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        repo,
		Number:      resolvedIssNum,
		LinkedPRNum: prNum,
	})
	c.store.Apply(itemstate.DeepFetchInvalidated{Repo: repo, Number: resolvedIssNum})
	c.store.Apply(itemstate.ReviewThreadCommentAdded{
		Repo:        repo,
		IssueNumber: resolvedIssNum,
		Comment:     comment,
	})
	c.bumpLocalDeltaAt(key)
	c.logFn("[cache] auto-heal: PR #%d → issue #%d; deep cache invalidated and delta re-applied\n", prNum, resolvedIssNum)
}

func (c *CacheImpl) applyCheckRunDelta(payload []byte) {
	var p checkRunPayload
	if err := json.Unmarshal(payload, &p); err != nil {
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

	// Always upsert the check run by ID into CacheImpl's checkRuns map.
	// This keeps pre-linkage runs available via FetchCheckRuns even before
	// the SHA↔item linkage is established in the Store.
	c.mu.Lock()
	runs := c.checkRuns[sha]
	updated := false
	for i, existing := range runs {
		if existing.ID == cr.ID {
			runs[i] = cr
			updated = true
			break
		}
	}
	if !updated {
		runs = append(runs, cr)
	}
	c.checkRuns[sha] = runs
	c.mu.Unlock()

	// Also apply to the Store — only works if SHA is indexed in Store's shaToKey.
	snap, changes, _ := c.store.Apply(itemstate.CheckRunCompleted{
		Repo: repo,
		SHA:  sha,
		Run:  cr,
	})
	if len(changes) > 0 {
		// SHA was known — bump localDeltaAt and return.
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
	key, resolvedIssNum, found, healErr := c.resolvePRLinkage(owner, repoName, prNum)

	c.mu.Lock()
	if !found {
		if healErr == nil {
			c.recentMissCache[msha] = time.Now()
		}
		c.mu.Unlock()
		c.logFn("[cache] dropped check_run delta for SHA %s: no closing issue in cache for PR #%d\n", sha, prNum)
		return
	}
	if _, storeErr := c.store.Get(repo, resolvedIssNum); storeErr != nil {
		c.recentMissCache[msha] = time.Now()
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	// prToKey is populated by PRHeadSHAUpdated via updateIndexes — no explicit write needed.

	// Update Store: set LinkedPRNum + SHA (updates shaToKey index), invalidate deep cache.
	c.store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        repo,
		Number:      resolvedIssNum,
		LinkedPRNum: prNum,
		SHA:         sha,
	})
	c.store.Apply(itemstate.DeepFetchInvalidated{Repo: repo, Number: resolvedIssNum})

	// Now that shaToKey is updated, replay the CheckRunCompleted on the Store.
	c.store.Apply(itemstate.CheckRunCompleted{Repo: repo, SHA: sha, Run: cr})

	c.bumpLocalDeltaAt(key)
	c.logFn("[cache] auto-heal: check_run SHA %s → PR #%d → issue #%d; deep cache invalidated\n", sha, prNum, resolvedIssNum)
}

// applyCheckSuite is a documented no-op. The comment in ApplyDelta's switch explains
// why check_suite is skipped; this stub exists only so the call site compiles.
func (c *CacheImpl) applyCheckSuite(_ []byte) {}

func (c *CacheImpl) applyProjectsV2ItemDelta(payload []byte) {
	var p projectsV2ItemPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return
	}

	switch p.Action {
	case "created":
		// An existing issue was added to the project board. The payload provides only
		// content_node_id; fetch full item details from GraphQL to get number and repo.
		nodeID := p.ProjectsV2Item.ContentNodeID
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
		pi.ItemID = p.ProjectsV2Item.ID
		_, changes, _ := c.store.Apply(itemstate.IssueOpened{Item: *pi})
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(itemKey(pi.Repo, pi.Number))

	case "edited":
		if p.Changes.FieldValue.FieldType != "single_select" {
			return
		}
		newStatus := p.Changes.FieldValue.To.Name
		if newStatus == "" {
			return
		}
		snap, changes, _ := c.store.Apply(itemstate.ProjectV2ItemEdited{
			ItemID:    p.ProjectsV2Item.ID,
			NewStatus: newStatus,
		})
		if len(changes) == 0 {
			return
		}
		c.bumpLocalDeltaAt(itemKey(snap.Repo(), snap.Number()))

	case "deleted", "archived":
		// Remove the item from the Store. Store.RemoveByItemID handles prToKey index cleanup.
		_, _, ok := c.store.RemoveByItemID(p.ProjectsV2Item.ID)
		if !ok {
			return
		}

	// restored: item is back on the board; the next poll reconcile will re-add it.
	// reordered: no position state in the engine.
	}
}
