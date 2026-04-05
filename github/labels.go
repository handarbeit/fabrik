package github

import (
	"errors"
	"fmt"
	"net/url"
)

// FetchLabels returns the current labels on an issue.
func (c *Client) FetchLabels(owner, repo string, issueNumber int) ([]string, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels", c.baseURL, owner, repo, issueNumber)
	var labels []struct {
		Name string `json:"name"`
	}
	if err := c.restGetJSON(apiURL, &labels); err != nil {
		return nil, err
	}
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	return names, nil
}

// AddLabelToIssue adds a label to an issue. Creates the label if it doesn't exist.
func (c *Client) AddLabelToIssue(owner, repo string, issueNumber int, labelName string) error {
	// First ensure the label exists
	if err := c.ensureLabel(owner, repo, labelName); err != nil {
		return err
	}

	// Use REST API for simplicity — add label to issue
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels", c.baseURL, owner, repo, issueNumber)
	body := map[string]interface{}{
		"labels": []string{labelName},
	}
	return c.restPost(apiURL, body)
}

// RemoveLabelFromIssue removes a label from an issue.
func (c *Client) RemoveLabelFromIssue(owner, repo string, issueNumber int, labelName string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels/%s", c.baseURL, owner, repo, issueNumber, url.PathEscape(labelName))
	return c.restDelete(apiURL)
}

func (c *Client) ensureLabel(owner, repo, name string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/labels", c.baseURL, owner, repo)
	body := map[string]interface{}{
		"name":  name,
		"color": "6f42c1",
	}
	// Ignore 422 (label already exists); propagate all other errors.
	if err := c.restPost(apiURL, body); err != nil && !errors.Is(err, ErrUnprocessableEntity) {
		return err
	}
	return nil
}
