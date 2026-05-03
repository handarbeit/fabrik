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
		NodeID     string  `json:"node_id"`
		DatabaseID int     `json:"id"`
		Body       string  `json:"body"`
		CreatedAt  string  `json:"created_at"`
		DiffHunk   string  `json:"diff_hunk"`
		Path       string  `json:"path"`
		Line       *int    `json:"line"`
		OriginalLine *int  `json:"original_line"`
		User       struct {
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
}

func (c *CacheImpl) applyIssuesUnlabeled(p issuesPayload) {
	key := itemKey(p.Repository.FullName, p.Issue.Number)

	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[key]
	if !ok {
		return
	}
	filtered := item.Labels[:0]
	for _, l := range item.Labels {
		if l != p.Label.Name {
			filtered = append(filtered, l)
		}
	}
	item.Labels = filtered
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
	pk := prKey(repo, p.PullRequest.Number)
	prDetails := &gh.PRDetails{
		Number:         p.PullRequest.Number,
		Title:          p.PullRequest.Title,
		State:          p.PullRequest.State,
		Merged:         p.PullRequest.Merged,
		Draft:          p.PullRequest.Draft,
		HeadSHA:        p.PullRequest.Head.SHA,
		MergeableState: p.PullRequest.MergeableState,
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Store/update PR details.
	c.linkedPRs[pk] = prDetails

	// Find the issue in cache that links this PR and update shaToKey.
	sha := p.PullRequest.Head.SHA
	for key, item := range c.items {
		if item.Repo == repo && item.LinkedPRNumber == p.PullRequest.Number {
			if sha != "" {
				// On synchronize, the old SHA is now stale — remove it from the map
				// by overwriting any existing entry for this issue with the new SHA.
				for existingSHA, existingKey := range c.shaToKey {
					if existingKey == key && existingSHA != sha {
						delete(c.shaToKey, existingSHA)
					}
				}
				c.shaToKey[sha] = key
			}
			break
		}
	}

	// Also index by new SHA for check_run lookup.
	if sha != "" {
		// If we don't know the issue yet, the SHA entry will be populated when
		// the item is deep-fetched (LinkedPRNumber set) and the next PR event arrives.
		// For now, try to find via the PR cache entries.
	}
}

func (c *CacheImpl) applyPullRequestReviewSubmitted(payload []byte) {
	var p pullRequestReviewPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Action != "submitted" {
		return
	}
	repo := p.Repository.FullName

	c.mu.Lock()
	defer c.mu.Unlock()

	// Find the issue that has this PR linked.
	for _, item := range c.items {
		if item.Repo == repo && item.LinkedPRNumber == p.PullRequest.Number {
			review := gh.PRReview{
				Author:     p.Review.User.Login,
				State:      strings.ToUpper(p.Review.State),
				Body:       p.Review.Body,
				DatabaseID: p.Review.DatabaseID,
			}
			// Idempotent: upsert by DatabaseID (replace existing review from same author/id).
			updated := false
			for i, r := range item.LinkedPRReviews {
				if r.DatabaseID == review.DatabaseID && review.DatabaseID != 0 {
					item.LinkedPRReviews[i] = review
					updated = true
					break
				}
			}
			if !updated {
				item.LinkedPRReviews = append(item.LinkedPRReviews, review)
			}
			break
		}
	}
}

func (c *CacheImpl) applyPullRequestReviewCommentCreated(payload []byte) {
	var p pullRequestReviewCommentPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Action != "created" {
		return
	}
	repo := p.Repository.FullName

	createdAt, _ := time.Parse(time.RFC3339, p.Comment.CreatedAt)
	comment := gh.Comment{
		ID:         p.Comment.NodeID,
		DatabaseID: p.Comment.DatabaseID,
		Author:     p.Comment.User.Login,
		Body:       p.Comment.Body,
		CreatedAt:  createdAt,
		DiffHunk:   p.Comment.DiffHunk,
		Path:       p.Comment.Path,
		FromPR:     p.PullRequest.Number,
	}
	if p.Comment.Line != nil {
		comment.Line = *p.Comment.Line
	}
	if p.Comment.OriginalLine != nil {
		comment.OriginalLine = *p.Comment.OriginalLine
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, item := range c.items {
		if item.Repo == repo && item.LinkedPRNumber == p.PullRequest.Number {
			// Idempotent: skip if comment with this ID already exists.
			for _, existing := range item.LinkedPRReviewThreadComments {
				if existing.ID == comment.ID {
					return
				}
			}
			item.LinkedPRReviewThreadComments = append(item.LinkedPRReviewThreadComments, comment)
			break
		}
	}
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
	cr := gh.CheckRun{
		ID:         p.CheckRun.ID,
		Name:       p.CheckRun.Name,
		Status:     p.CheckRun.Status,
		Conclusion: p.CheckRun.Conclusion,
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Upsert by ID: replace existing check run with same ID, or append.
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

	// shaToKey reverse lookup is updated by pull_request deltas.
	// The check_run delta is stored by SHA; the engine always has the SHA from the linked PR.
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
}
