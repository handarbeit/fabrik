package github

import "fmt"

// CommitStatus is a single classic Commit Status (Statuses API) entry, as
// reduced by the combined-status endpoint to one entry per distinct context
// (GitHub itself keeps only the most recent status per context there — no
// client-side dedup is needed, unlike check runs).
type CommitStatus struct {
	Context string
	State   string // "success", "pending", "failure", "error"
}

// FetchCombinedStatus retrieves classic commit statuses for a ref via the
// combined-status REST endpoint. This is the only Fabrik-side visibility into
// a required status posted via the classic Statuses API rather than a GitHub
// Actions check run (e.g. a local-CI-takeover repo's own commit-status
// producer) — see #933. Needs only pull access (the same `repo` scope Fabrik
// already requires), unlike reading branch protection directly.
func (c *Client) FetchCombinedStatus(owner, repo, ref string) ([]CommitStatus, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s/status", c.baseURL, owner, repo, ref)
	var raw struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return nil, fmt.Errorf("fetching combined status for %s: %w", ref, err)
	}
	out := make([]CommitStatus, len(raw.Statuses))
	for i, s := range raw.Statuses {
		out[i] = CommitStatus{Context: s.Context, State: s.State}
	}
	return out, nil
}
