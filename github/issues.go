package github

import "fmt"

// CreateIssue creates a new GitHub issue via the REST API and returns the issue
// number and GraphQL node ID. The node ID is required for project board and
// blockedBy mutations.
func (c *Client) CreateIssue(owner, repo, title, body string) (number int, nodeID string, err error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues", c.baseURL, owner, repo)
	payload := map[string]interface{}{
		"title": title,
		"body":  body,
	}
	var raw struct {
		Number int    `json:"number"`
		NodeID string `json:"node_id"`
	}
	if err := c.restPostWithResponse(apiURL, payload, &raw); err != nil {
		return 0, "", fmt.Errorf("creating issue in %s/%s: %w", owner, repo, err)
	}
	return raw.Number, raw.NodeID, nil
}

// CloseIssue closes a GitHub issue via the REST API.
func (c *Client) CloseIssue(owner, repo string, issueNumber int) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.baseURL, owner, repo, issueNumber)
	payload := map[string]interface{}{
		"state":        "closed",
		"state_reason": "completed",
	}
	return c.restPatch(apiURL, payload)
}

// IssueData holds the fields from a GitHub issue needed by fabrik watch.
type IssueData struct {
	Number   int
	Title    string
	State    string
	Labels   []string
	Comments int
}

// FetchIssue retrieves a single issue via the REST API.
func (c *Client) FetchIssue(owner, repo string, issueNumber int) (*IssueData, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.baseURL, owner, repo, issueNumber)
	var raw struct {
		Number   int    `json:"number"`
		Title    string `json:"title"`
		State    string `json:"state"`
		Comments int    `json:"comments"`
		Labels   []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return nil, fmt.Errorf("fetching issue #%d: %w", issueNumber, err)
	}
	issue := &IssueData{
		Number:   raw.Number,
		Title:    raw.Title,
		State:    raw.State,
		Comments: raw.Comments,
	}
	for _, l := range raw.Labels {
		issue.Labels = append(issue.Labels, l.Name)
	}
	return issue, nil
}
