package github

import (
	"errors"
	"fmt"
	"time"
)

// AddComment posts a comment on an issue and returns the comment's database ID.
func (c *Client) AddComment(owner, repo string, issueNumber int, body string) (int, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", c.baseURL, owner, repo, issueNumber)
	payload := map[string]interface{}{
		"body": body,
	}
	var resp struct {
		ID int `json:"id"`
	}
	if err := c.restPostWithResponse(apiURL, payload, &resp); err != nil {
		return 0, fmt.Errorf("adding comment to %s/%s#%d: %w", owner, repo, issueNumber, err)
	}
	if resp.ID <= 0 {
		return 0, fmt.Errorf("github: add comment response missing valid id")
	}
	return resp.ID, nil
}

// AddCommentReaction adds a reaction to an issue comment (or issue-level PR
// comment). Content can be "+1", "-1", "eyes", etc. For PR review thread
// (inline) comments, use AddPRReviewCommentReaction instead — they live at a
// different endpoint.
func (c *Client) AddCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d/reactions", c.baseURL, owner, repo, commentDatabaseID)
	payload := map[string]interface{}{
		"content": content,
	}
	if err := c.restPost(apiURL, payload); err != nil {
		return fmt.Errorf("adding %q reaction to comment %d on %s/%s: %w", content, commentDatabaseID, owner, repo, err)
	}
	return nil
}

// AddPRReviewCommentReaction adds a reaction to a PR review thread (inline)
// comment. These live at /repos/.../pulls/comments/{id}/reactions rather than
// /repos/.../issues/comments/{id}/reactions.
func (c *Client) AddPRReviewCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/comments/%d/reactions", c.baseURL, owner, repo, commentDatabaseID)
	payload := map[string]interface{}{
		"content": content,
	}
	if err := c.restPost(apiURL, payload); err != nil {
		return fmt.Errorf("adding %q reaction to PR review comment %d on %s/%s: %w", content, commentDatabaseID, owner, repo, err)
	}
	return nil
}

// resolveReviewThreadMutation is the GraphQL mutation used by ResolveReviewThread.
const resolveReviewThreadMutation = `
mutation($threadId: ID!) {
  resolveReviewThread(input: { threadId: $threadId }) {
    thread { id isResolved }
  }
}`

// ResolveReviewThread marks a PR review thread as resolved ("Resolve
// conversation" in the GitHub UI). threadID is the GraphQL node ID of the
// thread (available via ProjectItem.LinkedPRReviewThreadComments[*].ReviewThreadID).
func (c *Client) ResolveReviewThread(threadID string) error {
	query := resolveReviewThreadMutation
	vars := map[string]interface{}{"threadId": threadID}
	var result struct {
		Data struct {
			ResolveReviewThread struct {
				Thread struct {
					ID         string `json:"id"`
					IsResolved bool   `json:"isResolved"`
				} `json:"thread"`
			} `json:"resolveReviewThread"`
		} `json:"data"`
	}
	if err := c.graphqlRequest(query, vars, &result); err != nil {
		return fmt.Errorf("resolving review thread %s: %w", threadID, err)
	}
	return nil
}

// UpdateComment replaces the body of an existing issue comment.
func (c *Client) UpdateComment(owner, repo string, commentDatabaseID int, body string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d", c.baseURL, owner, repo, commentDatabaseID)
	payload := map[string]interface{}{
		"body": body,
	}
	if err := c.restPatch(apiURL, payload); err != nil {
		return fmt.Errorf("updating comment %d on %s/%s: %w", commentDatabaseID, owner, repo, err)
	}
	return nil
}

// UpdateIssueBody updates the body of an issue.
func (c *Client) UpdateIssueBody(owner, repo string, issueNumber int, body string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.baseURL, owner, repo, issueNumber)
	payload := map[string]interface{}{
		"body": body,
	}
	if err := c.restPatch(apiURL, payload); err != nil {
		return fmt.Errorf("updating body of %s/%s#%d: %w", owner, repo, issueNumber, err)
	}
	return nil
}

// FetchIssueComments fetches the comments on an issue (or PR, since PRs are
// issues on the REST API) via GET /issues/{n}/comments, including each
// comment's reaction summary. Used by Pruefer to detect on-demand
// "/pruefer review" comment commands and apply 👀/🚀 reaction idempotency.
// Returns nil, nil on 404.
func (c *Client) FetchIssueComments(owner, repo string, issueNumber int) ([]Comment, error) {
	type rawComment struct {
		ID        int       `json:"id"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		Reactions struct {
			PlusOne  int `json:"+1"`
			MinusOne int `json:"-1"`
			Laugh    int `json:"laugh"`
			Hooray   int `json:"hooray"`
			Confused int `json:"confused"`
			Heart    int `json:"heart"`
			Rocket   int `json:"rocket"`
			Eyes     int `json:"eyes"`
		} `json:"reactions"`
	}
	// Paginated (#1539): GitHub returns issue comments oldest-first, so reading
	// only page one drops the NEWEST comments on any thread past restPageSize —
	// exactly the ones comment processing exists to react to. A busy issue would
	// silently stop responding to new instructions.
	raw, err := paginateREST[rawComment](c, fmt.Sprintf("comments for %s/%s#%d", owner, repo, issueNumber), func(page int) string {
		return fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments?per_page=%d&page=%d",
			c.baseURL, owner, repo, issueNumber, restPageSize, page)
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("fetching comments for %s/%s#%d: %w", owner, repo, issueNumber, err)
	}
	out := make([]Comment, len(raw))
	for i, rc := range raw {
		var reactions []ReactionGroup
		add := func(content string, count int) {
			if count > 0 {
				reactions = append(reactions, ReactionGroup{Content: content, Count: count})
			}
		}
		add("THUMBS_UP", rc.Reactions.PlusOne)
		add("THUMBS_DOWN", rc.Reactions.MinusOne)
		add("LAUGH", rc.Reactions.Laugh)
		add("HOORAY", rc.Reactions.Hooray)
		add("CONFUSED", rc.Reactions.Confused)
		add("HEART", rc.Reactions.Heart)
		add("ROCKET", rc.Reactions.Rocket)
		add("EYES", rc.Reactions.Eyes)
		out[i] = Comment{
			DatabaseID: rc.ID,
			Author:     rc.User.Login,
			Body:       rc.Body,
			CreatedAt:  rc.CreatedAt,
			Reactions:  reactions,
		}
	}
	return out, nil
}

// GetIssueBody fetches the body of an issue (or PR, since PRs are issues on the REST API).
func (c *Client) GetIssueBody(owner, repo string, issueNumber int) (string, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.baseURL, owner, repo, issueNumber)
	var result struct {
		Body string `json:"body"`
	}
	if err := c.restGetJSON(apiURL, &result); err != nil {
		return "", fmt.Errorf("fetching body of %s/%s#%d: %w", owner, repo, issueNumber, err)
	}
	return result.Body, nil
}
