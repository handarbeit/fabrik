package github

import (
	"errors"
	"fmt"
	"net/url"
)

// PRDetails holds the fields from a GitHub pull request needed by fabrik watch.
type PRDetails struct {
	Number  int
	Title   string
	State   string // "open", "closed"
	Merged  bool
	Draft   bool
	HeadSHA string
}

// FetchPRDetails retrieves a single pull request via the REST API.
func (c *Client) FetchPRDetails(owner, repo string, prNumber int) (*PRDetails, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, prNumber)
	var raw struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Merged bool   `json:"merged"`
		Draft  bool   `json:"draft"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return nil, fmt.Errorf("fetching PR #%d: %w", prNumber, err)
	}
	return &PRDetails{
		Number:  raw.Number,
		Title:   raw.Title,
		State:   raw.State,
		Merged:  raw.Merged,
		Draft:   raw.Draft,
		HeadSHA: raw.Head.SHA,
	}, nil
}

// CheckRun holds the result of a single CI check run.
type CheckRun struct {
	Name       string
	Status     string // "queued", "in_progress", "completed"
	Conclusion string // "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", or ""
}

// FetchCheckRuns retrieves check runs for a given commit SHA via the REST API.
func (c *Client) FetchCheckRuns(owner, repo, sha string) ([]CheckRun, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs", c.baseURL, owner, repo, sha)
	var raw struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return nil, fmt.Errorf("fetching check runs for %s: %w", sha, err)
	}
	out := make([]CheckRun, len(raw.CheckRuns))
	for i, cr := range raw.CheckRuns {
		out[i] = CheckRun{Name: cr.Name, Status: cr.Status, Conclusion: cr.Conclusion}
	}
	return out, nil
}

// FetchLinkedPR finds the PR linked to an issue by searching for a PR with the
// head branch fabrik/issue-N (Fabrik's naming convention). Returns nil, nil if
// no PR is found.
func (c *Client) FetchLinkedPR(owner, repo string, issueNumber int) (*PRDetails, error) {
	branch := fmt.Sprintf("fabrik/issue-%d", issueNumber)
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls?head=%s:%s&state=all&per_page=1",
		c.baseURL, owner, repo, url.PathEscape(owner), url.PathEscape(branch))
	var raw []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Merged bool   `json:"merged"`
		Draft  bool   `json:"draft"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return nil, fmt.Errorf("fetching linked PR for issue #%d: %w", issueNumber, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return &PRDetails{
		Number:  raw[0].Number,
		Title:   raw[0].Title,
		State:   raw[0].State,
		Merged:  raw[0].Merged,
		Draft:   raw[0].Draft,
		HeadSHA: raw[0].Head.SHA,
	}, nil
}

// ErrNotMergeable is returned by MergePR when the PR cannot be merged because
// GitHub reports mergeable as false or null (not yet computed). Callers may
// use errors.Is(err, github.ErrNotMergeable) to distinguish this from API failures.
var ErrNotMergeable = errors.New("PR is not mergeable")

// CreateDraftPR creates a draft pull request for the given issue branch.
// Returns the PR number. Callers should first call FindPRForIssue to avoid duplicates.
// The body parameter is the full PR body; callers are responsible for including "Closes #N".
func (c *Client) CreateDraftPR(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls", c.baseURL, owner, repo)
	reqBody := map[string]interface{}{
		"title": title,
		"head":  head,
		"base":  base,
		"body":  body,
		"draft": true,
	}
	var result struct {
		Number int `json:"number"`
	}
	if err := c.restPostWithResponse(apiURL, reqBody, &result); err != nil {
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

// MergePR merges the pull request identified by prNumber. It first checks
// GitHub's mergeable status: if null (not yet computed) or false, it returns
// ErrNotMergeable. It attempts a rebase merge first; if the repository does
// not allow rebase merges (405), it falls back to a regular merge commit.
func (c *Client) MergePR(owner, repo string, prNumber int) error {
	// Check mergeable status.
	prURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, prNumber)
	var prData struct {
		Mergeable *bool `json:"mergeable"`
	}
	if err := c.restGetJSON(prURL, &prData); err != nil {
		return fmt.Errorf("fetching PR mergeable status: %w", err)
	}
	if prData.Mergeable == nil || !*prData.Mergeable {
		return ErrNotMergeable
	}

	mergeURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/merge", c.baseURL, owner, repo, prNumber)
	var mergeResult struct {
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}

	// Attempt rebase merge first.
	err := c.restPutWithResponse(mergeURL, map[string]interface{}{"merge_method": "rebase"}, &mergeResult)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrMethodNotAllowed) {
		return fmt.Errorf("merging PR: %w", err)
	}

	// Rebase not allowed — fall back to merge commit.
	if err := c.restPutWithResponse(mergeURL, map[string]interface{}{"merge_method": "merge"}, &mergeResult); err != nil {
		return fmt.Errorf("merging PR (merge commit fallback): %w", err)
	}
	return nil
}
