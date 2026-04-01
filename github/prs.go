package github

import (
	"fmt"
	"net/url"
)

// CreateDraftPR creates a draft pull request for the given issue branch.
// Returns the PR number. Callers should first call FindPRForIssue to avoid duplicates.
func (c *Client) CreateDraftPR(owner, repo, title, head, base string, issueNumber int) (int, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls", c.baseURL, owner, repo)
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
	searchURL := fmt.Sprintf("%s/search/issues?q=%s", c.baseURL, url.QueryEscape(query))

	resp, err := c.restGet(searchURL)
	if err != nil {
		return 0, fmt.Errorf("searching for PR: %w", err)
	}

	if len(resp.Items) == 0 {
		return 0, nil
	}
	return resp.Items[0].Number, nil
}
