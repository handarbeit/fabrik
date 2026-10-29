package github

import (
	"fmt"
	"time"
)

// FetchCommitTime returns the committer date GitHub reports for a commit
// (GET /repos/{o}/{r}/commits/{sha}, commit.committer.date) — #2467.
//
// It is the review-wait gate's "when did this head appear" fact: a GitHub-sourced
// time, re-readable after a daemon restart, available on poll-only daemons and
// repos without CI. It is not a push timestamp (GitHub exposes no reliable
// per-commit one): the committer date is author-controlled and a rebase or
// merge rewrites it to when it ran, so callers bound it (see
// engine.headArrivalTime). A missing or unparseable date is an error, never the
// zero time, so a caller cannot mistake "unknown" for "arrived long ago".
func (c *Client) FetchCommitTime(owner, repo, sha string) (time.Time, error) {
	if sha == "" {
		return time.Time{}, fmt.Errorf("fetching commit time: empty sha")
	}
	apiURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s", c.baseURL, owner, repo, sha)
	var raw struct {
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return time.Time{}, fmt.Errorf("fetching commit %s: %w", sha, err)
	}
	if raw.Commit.Committer.Date == "" {
		return time.Time{}, fmt.Errorf("fetching commit %s: response carried no committer date", sha)
	}
	t, err := time.Parse(time.RFC3339, raw.Commit.Committer.Date)
	if err != nil {
		return time.Time{}, fmt.Errorf("fetching commit %s: parsing committer date %q: %w", sha, raw.Commit.Committer.Date, err)
	}
	return t, nil
}
