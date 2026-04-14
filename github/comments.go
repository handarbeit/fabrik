package github

import "fmt"

// AddComment posts a comment on an issue.
func (c *Client) AddComment(owner, repo string, issueNumber int, body string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", c.baseURL, owner, repo, issueNumber)
	payload := map[string]interface{}{
		"body": body,
	}
	return c.restPost(apiURL, payload)
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
	return c.restPost(apiURL, payload)
}

// AddPRReviewCommentReaction adds a reaction to a PR review thread (inline)
// comment. These live at /repos/.../pulls/comments/{id}/reactions rather than
// /repos/.../issues/comments/{id}/reactions.
func (c *Client) AddPRReviewCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/comments/%d/reactions", c.baseURL, owner, repo, commentDatabaseID)
	payload := map[string]interface{}{
		"content": content,
	}
	return c.restPost(apiURL, payload)
}

// ResolveReviewThread marks a PR review thread as resolved ("Resolve
// conversation" in the GitHub UI). threadID is the GraphQL node ID of the
// thread (available via ProjectItem.LinkedPRReviewThreadComments[*].ReviewThreadID).
func (c *Client) ResolveReviewThread(threadID string) error {
	const query = `
mutation($threadId: ID!) {
  resolveReviewThread(input: { threadId: $threadId }) {
    thread { id isResolved }
  }
}`
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
	return c.graphqlRequest(query, vars, &result)
}

// UpdateComment replaces the body of an existing issue comment.
func (c *Client) UpdateComment(owner, repo string, commentDatabaseID int, body string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d", c.baseURL, owner, repo, commentDatabaseID)
	payload := map[string]interface{}{
		"body": body,
	}
	return c.restPatch(apiURL, payload)
}

// UpdateIssueBody updates the body of an issue.
func (c *Client) UpdateIssueBody(owner, repo string, issueNumber int, body string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.baseURL, owner, repo, issueNumber)
	payload := map[string]interface{}{
		"body": body,
	}
	return c.restPatch(apiURL, payload)
}

// GetIssueBody fetches the body of an issue (or PR, since PRs are issues on the REST API).
func (c *Client) GetIssueBody(owner, repo string, issueNumber int) (string, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.baseURL, owner, repo, issueNumber)
	var result struct {
		Body string `json:"body"`
	}
	if err := c.restGetJSON(apiURL, &result); err != nil {
		return "", err
	}
	return result.Body, nil
}
