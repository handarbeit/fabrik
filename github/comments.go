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

// AddCommentReaction adds a reaction to a comment. Content can be "+1", "-1", "eyes", etc.
func (c *Client) AddCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d/reactions", c.baseURL, owner, repo, commentDatabaseID)
	payload := map[string]interface{}{
		"content": content,
	}
	return c.restPost(apiURL, payload)
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
