package github

import "fmt"

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
