package github

import (
	"errors"
	"fmt"
	"net/url"
	"time"
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

// FetchLabelAppliedAt returns the time when labelName was last applied to the
// given issue, using the GitHub issue events API. Returns time.Time{} (zero)
// without error if the event is not found (fail-open: timeout will not fire).
// Pages through all events to find the most recent application.
func (c *Client) FetchLabelAppliedAt(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
	var applied time.Time
	for page := 1; ; page++ {
		apiURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/events?per_page=100&page=%d",
			c.baseURL, owner, repo, issueNumber, page)
		var events []struct {
			Event     string `json:"event"`
			CreatedAt string `json:"created_at"`
			Label     *struct {
				Name string `json:"name"`
			} `json:"label"`
		}
		if err := c.restGetJSON(apiURL, &events); err != nil {
			return time.Time{}, fmt.Errorf("fetching issue events for #%d: %w", issueNumber, err)
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			if ev.Event != "labeled" || ev.Label == nil || ev.Label.Name != labelName {
				continue
			}
			t, err := time.Parse(time.RFC3339, ev.CreatedAt)
			if err != nil {
				continue
			}
			if t.After(applied) {
				applied = t
			}
		}
	}
	return applied, nil
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
