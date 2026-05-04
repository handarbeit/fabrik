package boardcache

import (
	"encoding/json"
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
		c.applyIssueCommentCreated(payload)
	case "issues":
		c.applyIssuesDelta(payload)
	case "pull_request":
		c.applyPullRequestDelta(payload)
	case "pull_request_review":
		c.applyPullRequestReviewSubmitted(payload)
	case "pull_request_review_comment":
		c.applyPullRequestReviewCommentCreated(payload)
	case "check_run":
		c.applyCheckRunCompleted(payload)
	case "projects_v2_item":
		c.applyProjectsV2ItemEdited(payload)
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

// ---------------------------------------------------------------------------
// Delta functions
// ---------------------------------------------------------------------------

func (c *CacheImpl) applyIssueCommentCreated(payload []byte) {
	var p issueCommentPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Action != "created" {
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
	switch p.Action {
	case "labeled":
		c.applyIssuesLabeled(p)
	case "unlabeled":
		c.applyIssuesUnlabeled(p)
	}
}

func (c *CacheImpl) applyIssuesLabeled(p issuesPayload) {
	fullRepo := p.Repository.FullName
	issNum := p.Issue.Number
	key := itemKey(fullRepo, issNum)

	if _, err := c.store.Get(fullRepo, issNum); err != nil {
		return
	}

	_, changes, _ := c.store.Apply(itemstate.IssueLabeled{
		Repo:   fullRepo,
		Number: issNum,
		Label:  p.Label.Name,
	})
	if len(changes) == 0 {
		return // no-op (label already present or item missing)
	}
	c.bumpLocalDeltaAt(key)
}

func (c *CacheImpl) applyIssuesUnlabeled(p issuesPayload) {
	fullRepo := p.Repository.FullName
	issNum := p.Issue.Number
	key := itemKey(fullRepo, issNum)

	if _, err := c.store.Get(fullRepo, issNum); err != nil {
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
}

func (c *CacheImpl) applyPullRequestDelta(payload []byte) {
	var p pullRequestPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return
	}
	switch p.Action {
	case "opened", "closed", "synchronize", "reopened":
	default:
		return
	}

	repo := p.Repository.FullName
	prNum := p.PullRequest.Number
	pk := prKey(repo, prNum)
	sha := p.PullRequest.Head.SHA
	prDetails := &gh.PRDetails{
		Number:         prNum,
		Title:          p.PullRequest.Title,
		State:          p.PullRequest.State,
		Merged:         p.PullRequest.Merged,
		Draft:          p.PullRequest.Draft,
		HeadSHA:        sha,
		MergeableState: p.PullRequest.MergeableState,
	}

	owner, repoName, ok := splitRepo(repo)
	if !ok {
		return
	}

	// Always store/update PR details regardless of whether we find a linked issue.
	c.mu.Lock()
	c.linkedPRs[pk] = prDetails
	issKey, issFound := c.prNumToKey[pk]
	c.mu.Unlock()

	if issFound {
		// Normal path: linked issue is already known.
		issRepo, issNum2, parseOK := parseItemKey(issKey)
		if parseOK {
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
	// Confirm item still in Store before updating prNumToKey.
	if _, storeErr := c.store.Get(repo, resolvedIssNum); storeErr != nil {
		c.recentMissCache[mk] = time.Now()
		c.mu.Unlock()
		return
	}
	c.prNumToKey[pk] = key
	c.mu.Unlock()

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

func (c *CacheImpl) applyPullRequestReviewSubmitted(payload []byte) {
	var p pullRequestReviewPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Action != "submitted" {
		return
	}
	repo := p.Repository.FullName
	prNum := p.PullRequest.Number
	pk := prKey(repo, prNum)

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

	// Normal path: look up issue via prNumToKey.
	c.mu.RLock()
	issKey, issFound := c.prNumToKey[pk]
	c.mu.RUnlock()

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
	c.prNumToKey[pk] = key
	c.mu.Unlock()

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

func (c *CacheImpl) applyPullRequestReviewCommentCreated(payload []byte) {
	var p pullRequestReviewCommentPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Action != "created" {
		return
	}
	repo := p.Repository.FullName
	prNum := p.PullRequest.Number
	pk := prKey(repo, prNum)

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

	// Normal path: look up issue via prNumToKey.
	c.mu.RLock()
	issKey, issFound := c.prNumToKey[pk]
	c.mu.RUnlock()

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
	c.prNumToKey[pk] = key
	c.mu.Unlock()

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

func (c *CacheImpl) applyCheckRunCompleted(payload []byte) {
	var p checkRunPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Action != "completed" {
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
	pk := prKey(repo, prNum)
	c.prNumToKey[pk] = key
	c.mu.Unlock()

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

func (c *CacheImpl) applyProjectsV2ItemEdited(payload []byte) {
	var p projectsV2ItemPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return
	}
	if p.Action != "edited" {
		return
	}
	if p.Changes.FieldValue.FieldType != "single_select" {
		return
	}
	newStatus := p.Changes.FieldValue.To.Name
	if newStatus == "" {
		return
	}
	itemID := p.ProjectsV2Item.ID

	snap, changes, _ := c.store.Apply(itemstate.ProjectV2ItemEdited{
		ItemID:    itemID,
		NewStatus: newStatus,
	})
	if len(changes) == 0 {
		return
	}
	key := itemKey(snap.Repo(), snap.Number())
	c.bumpLocalDeltaAt(key)
}
