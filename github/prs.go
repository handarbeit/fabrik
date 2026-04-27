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
	// MergeableState reflects GitHub's branch-protection-aware mergeable
	// status: "clean" (ready to merge), "unstable" (non-required checks
	// failing but still mergeable), "blocked" (required checks pending or
	// failing), "behind" (head is out of date with base), "dirty" (merge
	// conflict), "draft" (PR is a draft), "has_hooks" (clean but hooks
	// will run on merge), "unknown" (not yet computed). Used by Fabrik's
	// CI gate as the authoritative signal — non-required check_run
	// failures (e.g., workflow cleanup jobs) do not block "clean"/"unstable".
	MergeableState string
}

// FetchPRMergeable returns GitHub's mergeable flag for a single PR.
//
// The returned pointer is nil when GitHub has not yet computed mergeability
// (the field is null on the REST response); callers should treat this as
// "unknown — try again on the next poll". A non-nil *false indicates a
// confirmed conflict with the base branch.
//
// Only the single-PR endpoint (/pulls/{number}) returns this field reliably;
// the list endpoint used by FetchLinkedPR does not.
func (c *Client) FetchPRMergeable(owner, repo string, prNumber int) (*bool, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, prNumber)
	var raw struct {
		Mergeable *bool `json:"mergeable"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return nil, fmt.Errorf("fetching PR #%d mergeable: %w", prNumber, err)
	}
	return raw.Mergeable, nil
}

// FetchPRMergeableState returns GitHub's branch-protection-aware mergeable_state
// for a single PR (e.g. "clean", "unstable", "blocked", "behind", "dirty",
// "draft", "has_hooks", "unknown"). Used by Fabrik's CI gate as the
// authoritative signal for whether non-required check_run failures should
// block a merge.
//
// Returns "" when GitHub has not yet computed it. Only the single-PR endpoint
// returns this field reliably; the list endpoint used by FetchLinkedPR omits
// it (returns null).
func (c *Client) FetchPRMergeableState(owner, repo string, prNumber int) (string, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, prNumber)
	var raw struct {
		MergeableState string `json:"mergeable_state"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return "", fmt.Errorf("fetching PR #%d mergeable_state: %w", prNumber, err)
	}
	return raw.MergeableState, nil
}

// FetchPRDetails retrieves a single pull request via the REST API.
func (c *Client) FetchPRDetails(owner, repo string, prNumber int) (*PRDetails, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, prNumber)
	var raw struct {
		Number         int    `json:"number"`
		Title          string `json:"title"`
		State          string `json:"state"`
		Merged         bool   `json:"merged"`
		Draft          bool   `json:"draft"`
		MergeableState string `json:"mergeable_state"`
		Head           struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return nil, fmt.Errorf("fetching PR #%d: %w", prNumber, err)
	}
	return &PRDetails{
		Number:         raw.Number,
		Title:          raw.Title,
		State:          raw.State,
		Merged:         raw.Merged,
		Draft:          raw.Draft,
		HeadSHA:        raw.Head.SHA,
		MergeableState: raw.MergeableState,
	}, nil
}

// CheckRun holds the result of a single CI check run.
type CheckRun struct {
	ID         int64  // GitHub check run ID
	Name       string
	Status     string // "queued", "in_progress", "completed"
	Conclusion string // "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", or ""
}

// FetchCheckRuns retrieves check runs for a given commit SHA via the REST API.
func (c *Client) FetchCheckRuns(owner, repo, sha string) ([]CheckRun, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs", c.baseURL, owner, repo, sha)
	var raw struct {
		CheckRuns []struct {
			ID         int64  `json:"id"`
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
		out[i] = CheckRun{ID: cr.ID, Name: cr.Name, Status: cr.Status, Conclusion: cr.Conclusion}
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
		Number         int    `json:"number"`
		Title          string `json:"title"`
		State          string `json:"state"`
		Merged         bool   `json:"merged"`
		Draft          bool   `json:"draft"`
		MergeableState string `json:"mergeable_state"`
		Head           struct {
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
		Number:         raw[0].Number,
		Title:          raw[0].Title,
		State:          raw[0].State,
		Merged:         raw[0].Merged,
		Draft:          raw[0].Draft,
		HeadSHA:        raw[0].Head.SHA,
		MergeableState: raw[0].MergeableState,
	}, nil
}

// MergeableStateAccepted reports whether GitHub's mergeable_state value
// indicates the PR is mergeable per branch protection rules. "clean" means
// fully ready; "unstable" means non-required checks have failed but the PR
// is still mergeable. Other values ("blocked", "behind", "dirty", "draft",
// "has_hooks", "unknown", "") fall through to the per-check classification.
//
// "has_hooks" is treated as not-accepted here because pre-merge hooks may
// modify the merge outcome; conservative to use the per-check path.
func MergeableStateAccepted(mergeableState string) bool {
	return mergeableState == "clean" || mergeableState == "unstable"
}

// ErrNotMergeable is returned by MergePR when the PR cannot be merged because
// GitHub reports mergeable as false or null (not yet computed). Callers may
// use errors.Is(err, github.ErrNotMergeable) to distinguish this from API failures.
var ErrNotMergeable = errors.New("PR is not mergeable")

// CreateDraftPR creates a draft pull request for the given issue branch.
// Returns the PR number. Callers should first call FindPRForIssue to avoid duplicates.
// The body parameter is the full PR body; callers are responsible for including "Closes #N".
func (c *Client) CreateDraftPR(owner, repo, title, head, base, body string, _ int) (int, error) {
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

// FindPRForIssue finds the PR associated with an issue by looking for a PR
// whose head branch matches the fabrik/issue-N convention. Returns the PR
// number, or 0 if no matching PR is found.
//
// Uses FetchLinkedPR internally, which hits the core REST pulls endpoint
// (/repos/{owner}/{repo}/pulls?head=...). Previously this used the GitHub
// search API (/search/issues) which has a 30/minute rate limit — heavy
// polling exhausted that quota. Core REST has a 5000/hour limit, ~167x
// more headroom.
func (c *Client) FindPRForIssue(owner, repo string, issueNumber int) (int, error) {
	pr, err := c.FetchLinkedPR(owner, repo, issueNumber)
	if err != nil {
		return 0, err
	}
	if pr == nil {
		return 0, nil
	}
	return pr.Number, nil
}

// GetPRBase fetches the current base branch reference of an open pull request.
// Returns the base branch name (e.g. "main" or "feature/foo"), or an error if
// the API call fails.
func (c *Client) GetPRBase(owner, repo string, prNumber int) (string, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, prNumber)
	var raw struct {
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return "", fmt.Errorf("fetching PR #%d base: %w", prNumber, err)
	}
	return raw.Base.Ref, nil
}

// UpdatePRBase changes the base branch of an open pull request via the GitHub REST API.
// GitHub accepts base-branch changes on open PRs; the PR may become unmergeable if
// the head and new base have diverged, but the API call itself succeeds.
func (c *Client) UpdatePRBase(owner, repo string, prNumber int, newBase string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, prNumber)
	if err := c.restPatch(apiURL, map[string]interface{}{"base": newBase}); err != nil {
		return fmt.Errorf("updating PR #%d base to %q: %w", prNumber, newBase, err)
	}
	return nil
}

// MergePR merges the pull request identified by prNumber. It first checks
// GitHub's mergeable status: if null (not yet computed) or false, it returns
// ErrNotMergeable. It attempts a rebase merge first; if the repository does
// not allow rebase merges (405), it falls back to a regular merge commit.
func (c *Client) MergePR(owner, repo string, prNumber int) error {
	// Check PR state and mergeable status.
	prURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, prNumber)
	var prData struct {
		Merged    bool  `json:"merged"`
		Mergeable *bool `json:"mergeable"`
	}
	if err := c.restGetJSON(prURL, &prData); err != nil {
		return fmt.Errorf("fetching PR mergeable status: %w", err)
	}
	if prData.Merged {
		return nil // already merged (e.g., human merged manually) — nothing to do
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
