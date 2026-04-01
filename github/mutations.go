package github

import (
	"fmt"
	"net/url"
)

// AddLabelToIssue adds a label to an issue. Creates the label if it doesn't exist.
func (c *Client) AddLabelToIssue(owner, repo string, issueNumber int, labelName string) error {
	// First ensure the label exists
	if err := c.ensureLabel(owner, repo, labelName); err != nil {
		return err
	}

	// Use REST API for simplicity — add label to issue
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/labels", owner, repo, issueNumber)
	body := map[string]interface{}{
		"labels": []string{labelName},
	}
	return c.restPost(url, body)
}

// AddComment posts a comment on an issue.
func (c *Client) AddComment(owner, repo string, issueNumber int, body string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments", owner, repo, issueNumber)
	payload := map[string]interface{}{
		"body": body,
	}
	return c.restPost(url, payload)
}

// UpdateProjectItemStatus moves an item to a different status column on the project board.
func (c *Client) UpdateProjectItemStatus(projectID, itemID, statusFieldID, statusOptionID string) error {
	query := `
mutation($projectId: ID!, $itemId: ID!, $fieldId: ID!, $optionId: String!) {
  updateProjectV2ItemFieldValue(input: {
    projectId: $projectId,
    itemId: $itemId,
    fieldId: $fieldId,
    value: { singleSelectOptionId: $optionId }
  }) {
    projectV2Item {
      id
    }
  }
}`
	vars := map[string]interface{}{
		"projectId": projectID,
		"itemId":    itemID,
		"fieldId":   statusFieldID,
		"optionId":  statusOptionID,
	}

	var result struct{}
	return c.graphqlRequest(query, vars, &result)
}

// FetchStatusField retrieves the Status field ID and its option IDs for a project.
func (c *Client) FetchStatusField(projectID string) (*StatusField, error) {
	query := `
query($projectId: ID!) {
  node(id: $projectId) {
    ... on ProjectV2 {
      field(name: "Status") {
        ... on ProjectV2SingleSelectField {
          id
          options {
            id
            name
          }
        }
      }
    }
  }
}`
	vars := map[string]interface{}{
		"projectId": projectID,
	}

	var result struct {
		Data struct {
			Node struct {
				Field struct {
					ID      string `json:"id"`
					Options []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"options"`
				} `json:"field"`
			} `json:"node"`
		} `json:"data"`
	}

	if err := c.graphqlRequest(query, vars, &result); err != nil {
		return nil, err
	}

	sf := &StatusField{
		FieldID: result.Data.Node.Field.ID,
		Options: make(map[string]string),
	}
	for _, opt := range result.Data.Node.Field.Options {
		sf.Options[opt.Name] = opt.ID
	}

	return sf, nil
}

// StatusField holds the Status field metadata for a project.
type StatusField struct {
	FieldID string
	Options map[string]string // status name -> option ID
}

// AddCommentReaction adds a reaction to a comment. Content can be "+1", "-1", "eyes", etc.
func (c *Client) AddCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/comments/%d/reactions", owner, repo, commentDatabaseID)
	payload := map[string]interface{}{
		"content": content,
	}
	return c.restPost(url, payload)
}

// UpdateIssueBody updates the body of an issue.
func (c *Client) UpdateIssueBody(owner, repo string, issueNumber int, body string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", owner, repo, issueNumber)
	payload := map[string]interface{}{
		"body": body,
	}
	return c.restPatch(url, payload)
}

// RemoveLabelFromIssue removes a label from an issue.
func (c *Client) RemoveLabelFromIssue(owner, repo string, issueNumber int, labelName string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/labels/%s", owner, repo, issueNumber, url.PathEscape(labelName))
	return c.restDelete(url)
}

// CreateDraftPR creates a draft pull request for the given issue branch.
// Returns the PR number. Callers should first call FindPRForIssue to avoid duplicates.
func (c *Client) CreateDraftPR(owner, repo, title, head, base string, issueNumber int) (int, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls", owner, repo)
	body := map[string]interface{}{
		"title": title,
		"head":  head,
		"base":  base,
		"body":  fmt.Sprintf("Closes #%d", issueNumber),
		"draft": true,
	}
	var result struct {
		Number int `json:"number"`
	}
	if err := c.restPostWithResponse(apiURL, body, &result); err != nil {
		return 0, fmt.Errorf("creating draft PR: %w", err)
	}
	return result.Number, nil
}

// MarkPRReady transitions a draft PR to ready-for-review.
// Uses the GraphQL markPullRequestReadyForReview mutation, which is the supported
// path — REST PATCH does not reliably support draft→ready transitions.
func (c *Client) MarkPRReady(owner, repo string, prNumber int) error {
	// Fetch the PR node ID (required for the GraphQL mutation)
	fetchQuery := `
query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      id
    }
  }
}`
	fetchVars := map[string]interface{}{
		"owner":  owner,
		"repo":   repo,
		"number": prNumber,
	}
	var fetchResult struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ID string `json:"id"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := c.graphqlRequest(fetchQuery, fetchVars, &fetchResult); err != nil {
		return fmt.Errorf("fetching PR node ID: %w", err)
	}
	nodeID := fetchResult.Data.Repository.PullRequest.ID
	if nodeID == "" {
		return fmt.Errorf("PR #%d not found in repository %s/%s", prNumber, owner, repo)
	}

	mutation := `
mutation($prId: ID!) {
  markPullRequestReadyForReview(input: { pullRequestId: $prId }) {
    pullRequest {
      id
    }
  }
}`
	mutVars := map[string]interface{}{
		"prId": nodeID,
	}
	var mutResult struct{}
	return c.graphqlRequest(mutation, mutVars, &mutResult)
}

// FindPRForIssue finds the open PR associated with an issue by looking for
// a PR whose head branch matches the fabrik/issue-N convention.
// Returns the PR number, or 0 if no matching PR is found.
func (c *Client) FindPRForIssue(owner, repo string, issueNumber int) (int, error) {
	query := fmt.Sprintf("repo:%s/%s is:pr is:open head:fabrik/issue-%d", owner, repo, issueNumber)
	searchURL := fmt.Sprintf("https://api.github.com/search/issues?q=%s", url.QueryEscape(query))

	resp, err := c.restGet(searchURL)
	if err != nil {
		return 0, fmt.Errorf("searching for PR: %w", err)
	}

	if len(resp.Items) == 0 {
		return 0, nil
	}
	return resp.Items[0].Number, nil
}

func (c *Client) ensureLabel(owner, repo, name string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/labels", owner, repo)
	body := map[string]interface{}{
		"name":  name,
		"color": "6f42c1",
	}
	// Ignore 422 (already exists)
	_ = c.restPost(url, body)
	return nil
}
