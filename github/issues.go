package github

import "fmt"

// IssueData holds the fields from a GitHub issue needed by fabrik watch.
type IssueData struct {
	Number int
	Title  string
	State  string
	Labels []string
}

// FetchIssue retrieves a single issue via the REST API.
func (c *Client) FetchIssue(owner, repo string, issueNumber int) (*IssueData, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.baseURL, owner, repo, issueNumber)
	var raw struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return nil, fmt.Errorf("fetching issue #%d: %w", issueNumber, err)
	}
	issue := &IssueData{
		Number: raw.Number,
		Title:  raw.Title,
		State:  raw.State,
	}
	for _, l := range raw.Labels {
		issue.Labels = append(issue.Labels, l.Name)
	}
	return issue, nil
}
