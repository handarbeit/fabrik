package boardcache

import (
	"encoding/json"
	"strings"
	"time"

	gh "github.com/verveguy/fabrik/github"
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
		Number int `json:"number"`
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
		} `json:"head"`
		MergeableState string `json:"mergeable_state"`
	} `json:"pull_request"`
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
// Inner-mutation helpers — called by both normal and post-heal paths
// ---------------------------------------------------------------------------

// applyReviewToItem upserts a PR review into the item's LinkedPRReviews by DatabaseID.
func applyReviewToItem(item *gh.ProjectItem, review gh.PRReview) {
	for i, r := range item.LinkedPRReviews {
		if r.DatabaseID == review.DatabaseID && review.DatabaseID != 0 {
			item.LinkedPRReviews[i] = review
			return
		}
	}
	item.LinkedPRReviews = append(item.LinkedPRReviews, review)
}

// applyReviewCommentToItem appends a review thread comment to the item if not already
// present (idempotent by NodeID), and clears the deepFetched flag so the next
// FetchItemDetails call re-fetches the ReviewThreadID from GitHub.
// Returns true if the comment was added, false if it was a duplicate.
func applyReviewCommentToItem(item *gh.ProjectItem, comment gh.Comment, key string, deepFetched map[string]bool) bool {
	for _, existing := range item.LinkedPRReviewThreadComments {
		if existing.ID == comment.ID {
			return false
		}
	}
	item.LinkedPRReviewThreadComments = append(item.LinkedPRReviewThreadComments, comment)
	delete(deepFetched, key)
	return true
}

// updateSHAToKey removes stale SHA entries for the given key and sets the new SHA.
func updateSHAToKey(shaToKey map[string]string, sha, key string) {
	if sha == "" {
		return
	}
	for existingSHA, existingKey := range shaToKey {
		if existingKey == key && existingSHA != sha {
			delete(shaToKey, existingSHA)
		}
	}
	shaToKey[sha] = key
}

// splitRepo splits "owner/repo" into ("owner", "repo", true). Returns false on invalid input.
func splitRepo(fullName string) (owner, repo string, ok bool) {
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ---------------------------------------------------------------------------
// Delta functions
// ---------------------------------------------------------------------------

func (c *CacheImpl) applyIssueCommentCreated(payload []byte) {
	var p issueCommentPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Action != "created" {
		return
	}
	key := itemKey(p.Repository.FullName, p.Issue.Number)

	createdAt, _ := time.Parse(time.RFC3339, p.Comment.CreatedAt)
	comment := gh.Comment{
		ID:         p.Comment.NodeID,
		DatabaseID: p.Comment.DatabaseID,
		Author:     p.Comment.User.Login,
		Body:       p.Comment.Body,
		CreatedAt:  createdAt,
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[key]
	if !ok {
		return
	}
	// Idempotent: skip if comment with this ID already exists.
	for _, existing := range item.Comments {
		if existing.ID == comment.ID {
			return
		}
	}
	item.Comments = append(item.Comments, comment)
	// Bump UpdatedAt so itemMayNeedWork detects this item as changed on the next poll.
	item.UpdatedAt = time.Now()
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
	key := itemKey(p.Repository.FullName, p.Issue.Number)

	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[key]
	if !ok {
		return
	}
	// Set-membership add: skip if label already present.
	for _, l := range item.Labels {
		if l == p.Label.Name {
			return
		}
	}
	item.Labels = append(item.Labels, p.Label.Name)
	item.UpdatedAt = time.Now()
}

func (c *CacheImpl) applyIssuesUnlabeled(p issuesPayload) {
	key := itemKey(p.Repository.FullName, p.Issue.Number)

	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[key]
	if !ok {
		return
	}
	before := len(item.Labels)
	filtered := item.Labels[:0]
	for _, l := range item.Labels {
		if l != p.Label.Name {
			filtered = append(filtered, l)
		}
	}
	item.Labels = filtered
	if len(item.Labels) < before {
		item.UpdatedAt = time.Now()
	}
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

	c.mu.Lock()

	// Always store/update PR details regardless of whether we find a linked issue.
	c.linkedPRs[pk] = prDetails

	// Find the issue in cache that links this PR and update shaToKey.
	for key, item := range c.items {
		if item.Repo == repo && item.LinkedPRNumber == prNum {
			updateSHAToKey(c.shaToKey, sha, key)
			c.mu.Unlock()
			return
		}
	}

	// Miss: check negative cache.
	mk := missKey(repo, prNum)
	if t, found := c.recentMissCache[mk]; found {
		if time.Since(t) < recentMissTTL {
			c.mu.Unlock()
			return
		}
		delete(c.recentMissCache, mk)
	}

	c.mu.Unlock()

	// Auto-heal: resolve PR linkage via REST.
	key, _, found, healErr := c.resolvePRLinkage(owner, repoName, prNum)

	c.mu.Lock()
	if !found {
		if healErr == nil {
			c.recentMissCache[mk] = time.Now()
		}
		c.mu.Unlock()
		c.logFn("[cache] dropped pull_request delta for PR #%d: no closing issue in cache\n", prNum)
		return
	}
	item, itemOK := c.items[key]
	if !itemOK {
		c.recentMissCache[mk] = time.Now()
		c.mu.Unlock()
		return
	}
	item.LinkedPRNumber = prNum
	delete(c.deepFetched, key)
	item.UpdatedAt = time.Now()
	updateSHAToKey(c.shaToKey, sha, key)
	issNum := item.Number
	c.mu.Unlock()
	c.logFn("[cache] auto-heal: PR #%d → issue #%d; deep cache invalidated and delta re-applied\n", prNum, issNum)
}

func (c *CacheImpl) applyPullRequestReviewSubmitted(payload []byte) {
	var p pullRequestReviewPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Action != "submitted" {
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

	c.mu.Lock()

	// Normal path: find the item with this PR linked.
	for _, item := range c.items {
		if item.Repo == repo && item.LinkedPRNumber == prNum {
			applyReviewToItem(item, review)
			item.UpdatedAt = time.Now()
			c.mu.Unlock()
			return
		}
	}

	// Miss: check negative cache.
	mk := missKey(repo, prNum)
	if t, found := c.recentMissCache[mk]; found {
		if time.Since(t) < recentMissTTL {
			c.mu.Unlock()
			c.logFn("[cache] dropped pull_request_review delta for PR #%d: negative cache hit\n", prNum)
			return
		}
		delete(c.recentMissCache, mk)
	}

	c.mu.Unlock()

	// Auto-heal: resolve PR linkage via REST.
	key, _, found, healErr := c.resolvePRLinkage(owner, repoName, prNum)

	c.mu.Lock()
	if !found {
		if healErr == nil {
			c.recentMissCache[mk] = time.Now()
		}
		c.mu.Unlock()
		c.logFn("[cache] dropped pull_request_review delta for PR #%d: no closing issue in cache\n", prNum)
		return
	}
	item, itemOK := c.items[key]
	if !itemOK {
		c.recentMissCache[mk] = time.Now()
		c.mu.Unlock()
		return
	}
	item.LinkedPRNumber = prNum
	delete(c.deepFetched, key)
	item.UpdatedAt = time.Now()
	applyReviewToItem(item, review)
	issNum := item.Number
	c.mu.Unlock()
	c.logFn("[cache] auto-heal: PR #%d → issue #%d; deep cache invalidated and delta re-applied\n", prNum, issNum)
}

func (c *CacheImpl) applyPullRequestReviewCommentCreated(payload []byte) {
	var p pullRequestReviewCommentPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Action != "created" {
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

	c.mu.Lock()

	// Normal path: find the item with this PR linked.
	for key, item := range c.items {
		if item.Repo == repo && item.LinkedPRNumber == prNum {
			if applyReviewCommentToItem(item, comment, key, c.deepFetched) {
				item.UpdatedAt = time.Now()
			}
			c.mu.Unlock()
			return
		}
	}

	// Miss: check negative cache.
	mk := missKey(repo, prNum)
	if t, found := c.recentMissCache[mk]; found {
		if time.Since(t) < recentMissTTL {
			c.mu.Unlock()
			c.logFn("[cache] dropped pull_request_review_comment delta for PR #%d: negative cache hit\n", prNum)
			return
		}
		delete(c.recentMissCache, mk)
	}

	c.mu.Unlock()

	// Auto-heal: resolve PR linkage via REST.
	key, _, found, healErr := c.resolvePRLinkage(owner, repoName, prNum)

	c.mu.Lock()
	if !found {
		if healErr == nil {
			c.recentMissCache[mk] = time.Now()
		}
		c.mu.Unlock()
		c.logFn("[cache] dropped pull_request_review_comment delta for PR #%d: no closing issue in cache\n", prNum)
		return
	}
	item, itemOK := c.items[key]
	if !itemOK {
		c.recentMissCache[mk] = time.Now()
		c.mu.Unlock()
		return
	}
	item.LinkedPRNumber = prNum
	delete(c.deepFetched, key)
	item.UpdatedAt = time.Now()
	applyReviewCommentToItem(item, comment, key, c.deepFetched)
	issNum := item.Number
	c.mu.Unlock()
	c.logFn("[cache] auto-heal: PR #%d → issue #%d; deep cache invalidated and delta re-applied\n", prNum, issNum)
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

	c.mu.Lock()

	// Always upsert the check run by ID.
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

	// shaToKey reverse lookup: if found, bump UpdatedAt on the linked issue.
	if key, found := c.shaToKey[sha]; found {
		if item, ok := c.items[key]; ok {
			item.UpdatedAt = time.Now()
		}
		c.mu.Unlock()
		return
	}

	// shaToKey miss: check negative cache for this SHA.
	msha := missKeyForSHA(sha)
	if t, found := c.recentMissCache[msha]; found {
		if time.Since(t) < recentMissTTL {
			c.mu.Unlock()
			return
		}
		delete(c.recentMissCache, msha)
	}

	c.mu.Unlock()

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
	key, issNum, found, healErr := c.resolvePRLinkage(owner, repoName, prNum)

	c.mu.Lock()
	if !found {
		if healErr == nil {
			c.recentMissCache[msha] = time.Now()
		}
		c.mu.Unlock()
		c.logFn("[cache] dropped check_run delta for SHA %s: no closing issue in cache for PR #%d\n", sha, prNum)
		return
	}
	item, itemOK := c.items[key]
	if !itemOK {
		c.recentMissCache[msha] = time.Now()
		c.mu.Unlock()
		return
	}
	item.LinkedPRNumber = prNum
	c.shaToKey[sha] = key
	delete(c.deepFetched, key)
	item.UpdatedAt = time.Now()
	c.mu.Unlock()
	c.logFn("[cache] auto-heal: check_run SHA %s → PR #%d → issue #%d; deep cache invalidated\n", sha, prNum, issNum)
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

	c.mu.Lock()
	defer c.mu.Unlock()

	key, ok := c.itemIDToKey[itemID]
	if !ok {
		return
	}
	item, ok := c.items[key]
	if !ok {
		return
	}
	item.Status = newStatus
	item.UpdatedAt = time.Now()
}
