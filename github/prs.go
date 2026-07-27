package github

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// reClosingKeyword matches GitHub closing keywords followed by a same-repo issue
// reference (#N). Cross-repo references (owner/repo#N) are intentionally excluded.
// Only matches at line-start (optionally preceded by whitespace or a list marker
// like "- " or "* ") to reject mid-sentence prose references like "before fixes #N".
var reClosingKeyword = regexp.MustCompile(`(?im)(?:^|\n)\s*(?:[-*]\s+)?(?:closes|fixes|resolves)\s+#(\d+)`)

// FetchPRClosingIssues returns the issue numbers referenced by GitHub closing keywords
// (Closes, Fixes, Resolves + #N) in the body of the given pull request. Only same-repo
// references are returned; cross-repo references are out of scope.
// Returns nil, nil on 404 or when the PR body contains no recognized closing references.
func (c *Client) FetchPRClosingIssues(owner, repo string, prNumber int) ([]int, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, prNumber)
	var raw struct {
		Body string `json:"body"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("fetching PR #%d body: %w", prNumber, err)
	}
	matches := reClosingKeyword.FindAllStringSubmatch(raw.Body, -1)
	if len(matches) == 0 {
		return nil, nil
	}
	out := make([]int, 0, len(matches))
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// FetchPRReviews returns the latest submitted review per author for a pull request
// via the REST API, keyed on PR number rather than closedByPullRequestsReferences —
// the field GitHub leaves structurally empty for PRs targeting a non-default base
// branch. This is the base-independent counterpart to the GraphQL-nested
// latestReviews field, mirroring FetchPRClosingIssues's REST-by-PR-number precedent
// (see #1047). The REST endpoint returns the full review history — every submission,
// unlimited per author — so results are collapsed to one entry per author (the most
// recent submission, per GitHub's chronological response order) to match latestReviews'
// semantics; otherwise an author's earlier non-DISMISSED review (e.g. a stale COMMENTED
// review) could outlive a later dismissal of their actual current review and falsely
// satisfy the review-gate's hasReviews check.
// Returns nil, nil on 404.
func (c *Client) FetchPRReviews(owner, repo string, prNumber int) ([]PRReview, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews?per_page=100", c.baseURL, owner, repo, prNumber)
	var raw []struct {
		ID   int `json:"id"`
		User *struct {
			Login string `json:"login"`
		} `json:"user"`
		State    string `json:"state"`
		Body     string `json:"body"`
		CommitID string `json:"commit_id"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("fetching PR #%d reviews: %w", prNumber, err)
	}
	latestByAuthor := make(map[string]PRReview, len(raw))
	order := make([]string, 0, len(raw))
	for _, r := range raw {
		if r.User == nil || r.User.Login == "" {
			continue
		}
		if _, seen := latestByAuthor[r.User.Login]; !seen {
			order = append(order, r.User.Login)
		}
		latestByAuthor[r.User.Login] = PRReview{
			Author:     r.User.Login,
			State:      r.State,
			Body:       r.Body,
			DatabaseID: r.ID,
			CommitID:   r.CommitID,
		}
	}
	out := make([]PRReview, 0, len(order))
	for _, author := range order {
		out = append(out, latestByAuthor[author])
	}
	return out, nil
}

// FetchPRReviewRequests returns the outstanding requested reviewers for a pull
// request via the REST API, keyed on PR number — the base-independent counterpart
// to the GraphQL-nested reviewRequests field. Team review requests are ignored,
// matching the GraphQL path's existing behavior (only individual reviewers map to
// ReviewRequest). IsBot reproduces the GraphQL path's dual signal: REST's
// user.type == "Bot" field, with the isBotLogin pattern fallback for reviewers
// (e.g. dependabot) that GitHub doesn't mark with type "Bot".
// Returns nil, nil on 404.
func (c *Client) FetchPRReviewRequests(owner, repo string, prNumber int) ([]ReviewRequest, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/requested_reviewers", c.baseURL, owner, repo, prNumber)
	var raw struct {
		Users []struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"users"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("fetching PR #%d requested reviewers: %w", prNumber, err)
	}
	out := make([]ReviewRequest, 0, len(raw.Users))
	for _, u := range raw.Users {
		if u.Login == "" {
			continue
		}
		out = append(out, ReviewRequest{Login: u.Login, IsBot: u.Type == "Bot" || isBotLogin(u.Login)})
	}
	return out, nil
}

// FetchPRsForSHA returns the PR numbers associated with the given commit SHA via
// GET /repos/{owner}/{repo}/commits/{sha}/pulls. Returns nil, nil on 404 or empty.
func (c *Client) FetchPRsForSHA(owner, repo, sha string) ([]int, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s/pulls", c.baseURL, owner, repo, sha)
	var raw []struct {
		Number int `json:"number"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("fetching PRs for SHA %s: %w", sha, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]int, len(raw))
	for i, pr := range raw {
		out[i] = pr.Number
	}
	return out, nil
}

// PRDetails holds the fields from a GitHub pull request needed by fabrik watch.
type PRDetails struct {
	Number  int
	Title   string
	State   string // "open", "closed"
	Merged  bool
	Draft   bool
	HeadSHA string
	// HeadRefName is the PR's head branch name (e.g. "fabrik/merge-train/…").
	// Populated by ListPRs (from head.ref); other constructors may leave it empty.
	HeadRefName string
	Body        string
	// MergeableState reflects GitHub's branch-protection-aware mergeable
	// status: "clean" (ready to merge), "unstable" (non-required checks
	// failing but still mergeable), "blocked" (required checks pending or
	// failing), "behind" (head is out of date with base), "dirty" (merge
	// conflict), "draft" (PR is a draft), "has_hooks" (clean but hooks
	// will run on merge), "unknown" (not yet computed). Used by Fabrik's
	// CI gate as the authoritative signal — non-required check_run
	// failures (e.g., workflow cleanup jobs) do not block "clean"/"unstable".
	MergeableState string
	// AutoMergeEnabled is true when GitHub's native auto-merge is enabled on
	// the PR (auto_merge field is non-null). False when the user or engine
	// has disabled it, or when it was never enabled.
	AutoMergeEnabled bool

	// IsMergeQueueEnabled is true when the repository has the merge queue feature
	// enabled. Always false when populated via REST (GraphQL-only field).
	IsMergeQueueEnabled bool
	// IsInMergeQueue is true when the PR is currently in the merge queue.
	// Always false when populated via REST (GraphQL-only field).
	IsInMergeQueue bool
	// MergeQueueEntry holds the queue position and state when the PR is enqueued.
	// Nil when not in queue or populated via REST. Pointer because GitHub returns
	// null after dequeueing and Go's json decoder maps null to nil only on pointers.
	MergeQueueEntry *MergeQueueEntry

	// Author is the GitHub login of the PR's author. Populated by ListPRs and
	// ListOpenPRs; other constructors may leave it empty.
	Author string
	// Labels is the list of label names applied to the PR. Populated by ListPRs
	// and ListOpenPRs; other constructors may leave it empty.
	Labels []string
	// BaseRef is the PR's base branch name (e.g. "main"). Populated by
	// ListOpenPRs; other constructors may leave it empty.
	BaseRef string
}

// FetchPRMergeableFields fetches both the mergeable flag and mergeable_state for
// a single PR in one REST call, eliminating the read-after-write window that
// existed when the two fields were fetched separately by different gate functions.
//
// Returns (nil, "", nil) when mergeable is null (GitHub still computing).
func (c *Client) FetchPRMergeableFields(owner, repo string, prNumber int) (mergeable *bool, mergeableState string, err error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, prNumber)
	var raw struct {
		Mergeable      *bool  `json:"mergeable"`
		MergeableState string `json:"mergeable_state"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return nil, "", fmt.Errorf("fetching PR #%d mergeable fields: %w", prNumber, err)
	}
	return raw.Mergeable, raw.MergeableState, nil
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

// FetchPRMerged returns GitHub's authoritative `merged` flag for a single PR.
//
// Only the single-PR endpoint (/pulls/{number}) reports `merged` reliably. The
// list endpoint used by FetchLinkedPR returns merged=false for several seconds
// after a merge (and is generally unreliable for this field), so a PR the engine
// just merged briefly looks like state=closed, merged=false there. Use this to
// confirm whether a PR observed as closed was actually merged before treating it
// as "closed without merging".
func (c *Client) FetchPRMerged(owner, repo string, prNumber int) (bool, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, prNumber)
	var raw struct {
		Merged bool `json:"merged"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return false, fmt.Errorf("fetching PR #%d merged state: %w", prNumber, err)
	}
	return raw.Merged, nil
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
		Number         int         `json:"number"`
		Title          string      `json:"title"`
		State          string      `json:"state"`
		Merged         bool        `json:"merged"`
		Draft          bool        `json:"draft"`
		Body           string      `json:"body"`
		MergeableState string      `json:"mergeable_state"`
		AutoMerge      interface{} `json:"auto_merge"` // non-null object = enabled, null = disabled
		Head           struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return nil, fmt.Errorf("fetching PR #%d: %w", prNumber, err)
	}
	return &PRDetails{
		Number:           raw.Number,
		Title:            raw.Title,
		State:            raw.State,
		Merged:           raw.Merged,
		Draft:            raw.Draft,
		Body:             raw.Body,
		HeadSHA:          raw.Head.SHA,
		MergeableState:   raw.MergeableState,
		AutoMergeEnabled: raw.AutoMerge != nil,
	}, nil
}

// CheckRun holds the result of a single CI check run.
type CheckRun struct {
	ID         int64 // GitHub check run ID
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
		Number         int         `json:"number"`
		Title          string      `json:"title"`
		State          string      `json:"state"`
		Merged         bool        `json:"merged"`
		Draft          bool        `json:"draft"`
		MergeableState string      `json:"mergeable_state"`
		AutoMerge      interface{} `json:"auto_merge"` // non-null object = enabled, null = disabled
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
		Number:           raw[0].Number,
		Title:            raw[0].Title,
		State:            raw[0].State,
		Merged:           raw[0].Merged,
		Draft:            raw[0].Draft,
		HeadSHA:          raw[0].Head.SHA,
		MergeableState:   raw[0].MergeableState,
		AutoMergeEnabled: raw[0].AutoMerge != nil,
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

// ErrNotMergeableCI is returned by MergePR when the PR has no merge conflicts
// (mergeable is true) but mergeable_state is not in the MergeableStateAccepted
// allowlist ({clean, unstable}) — e.g. a required status check is failing or
// still pending ("blocked"), or GitHub has not finished computing the state
// ("unknown"/""). This is distinct from ErrNotMergeable: it is a CI-readiness
// refusal, not a merge conflict, and callers must not route it into the
// fabrik:rebase-needed / rebase-reinvoke path. Callers should treat it as
// "not ready yet" and retry on their existing cadence.
var ErrNotMergeableCI = errors.New("PR mergeable_state is not CI-clean")

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

// CreatePR creates a non-draft pull request. Returns the PR number.
// Unlike CreateDraftPR, the PR is immediately ready for review and CI.
func (c *Client) CreatePR(owner, repo, title, head, base, body string) (int, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls", c.baseURL, owner, repo)
	reqBody := map[string]interface{}{
		"title": title,
		"head":  head,
		"base":  base,
		"body":  body,
		"draft": false,
	}
	var result struct {
		Number int `json:"number"`
	}
	if err := c.restPostWithResponse(apiURL, reqBody, &result); err != nil {
		return 0, fmt.Errorf("creating PR: %w", err)
	}
	return result.Number, nil
}

// ListPRs returns recent pull requests (open and closed) for a repository,
// including their body text. Capped at the 50 most-recently-updated PRs.
// Used for idempotency detection (e.g. finding an existing integration PR).
func (c *Client) ListPRs(owner, repo string) ([]PRDetails, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls?state=all&per_page=50&sort=updated&direction=desc",
		c.baseURL, owner, repo)
	var raw []struct {
		Number   int    `json:"number"`
		Title    string `json:"title"`
		State    string `json:"state"`
		MergedAt string `json:"merged_at"` // non-null when merged; list endpoint omits the boolean "merged" field
		Draft    bool   `json:"draft"`
		Body     string `json:"body"`
		User     struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []rawLabel `json:"labels"`
		Head   struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return nil, fmt.Errorf("listing PRs for %s/%s: %w", owner, repo, err)
	}
	out := make([]PRDetails, len(raw))
	for i, pr := range raw {
		out[i] = PRDetails{
			Number:      pr.Number,
			Title:       pr.Title,
			State:       pr.State,
			Merged:      pr.MergedAt != "",
			Draft:       pr.Draft,
			HeadSHA:     pr.Head.SHA,
			HeadRefName: pr.Head.Ref,
			Body:        pr.Body,
			Author:      pr.User.Login,
			Labels:      labelNames(pr.Labels),
		}
	}
	return out, nil
}

// rawLabel is the shared REST label shape ({"name": "..."}) used by both
// ListPRs and ListOpenPRs.
type rawLabel struct {
	Name string `json:"name"`
}

// labelNames extracts label name strings from a slice of rawLabel.
func labelNames(labels []rawLabel) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = l.Name
	}
	return out
}

// ListOpenPRs returns all open pull requests for a repository (draft and
// non-draft), including author and label metadata needed for Pruefer's PR
// selection logic. Capped at 100 results (GitHub's max per_page); a warning
// is logged (not silently truncated) when exactly 100 are returned, since
// more open PRs may exist.
func (c *Client) ListOpenPRs(owner, repo string) ([]PRDetails, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&per_page=100", c.baseURL, owner, repo)
	var raw []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Draft  bool   `json:"draft"`
		Body   string `json:"body"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []rawLabel `json:"labels"`
		Head   struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return nil, fmt.Errorf("listing open PRs for %s/%s: %w", owner, repo, err)
	}
	if len(raw) == 100 {
		logf(0, "prs", "ListOpenPRs %s/%s: received exactly 100 results — more open PRs may exist (pagination not implemented)\n", owner, repo)
	}
	out := make([]PRDetails, len(raw))
	for i, pr := range raw {
		out[i] = PRDetails{
			Number:      pr.Number,
			Title:       pr.Title,
			State:       pr.State,
			Draft:       pr.Draft,
			Body:        pr.Body,
			HeadSHA:     pr.Head.SHA,
			HeadRefName: pr.Head.Ref,
			Author:      pr.User.Login,
			Labels:      labelNames(pr.Labels),
			BaseRef:     pr.Base.Ref,
		}
	}
	return out, nil
}

// FetchPRDiff fetches the raw unified diff for a pull request via GitHub's
// diff media type, avoiding a local clone just to measure or inspect diff
// size. Returns the diff as plain text.
func (c *Client) FetchPRDiff(owner, repo string, prNumber int) (string, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, prNumber)
	_, respBody, err := c.doWithAccept("GET", apiURL, "application/vnd.github.v3.diff", nil)
	if err != nil {
		return "", fmt.Errorf("fetching diff for PR #%d: %w", prNumber, err)
	}
	return string(respBody), nil
}

// SubmitPRReview submits a formal pull_request_review comment (event=COMMENT)
// against the given commit SHA. The event type is hardcoded and never
// caller-controlled — Pruefer V1 never submits APPROVE or REQUEST_CHANGES
// verdicts (see ADR-1113). Returns the numeric review ID.
func (c *Client) SubmitPRReview(owner, repo string, prNumber int, commitSHA, body string) (int, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews", c.baseURL, owner, repo, prNumber)
	reqBody := map[string]interface{}{
		"commit_id": commitSHA,
		"body":      body,
		"event":     "COMMENT",
	}
	var result struct {
		ID int `json:"id"`
	}
	if err := c.restPostWithResponse(apiURL, reqBody, &result); err != nil {
		return 0, fmt.Errorf("submitting review on PR #%d: %w", prNumber, err)
	}
	return result.ID, nil
}

// prNodeID fetches the GraphQL node ID of a pull request by its REST number.
// The node ID is required by every GraphQL mutation that operates on a PR
// (markPullRequestReadyForReview, enablePullRequestAutoMerge, enqueuePullRequest,
// dequeuePullRequest) since none of them accept a plain PR number.
func (c *Client) prNodeID(owner, repo string, prNumber int) (string, error) {
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
		return "", fmt.Errorf("fetching PR node ID: %w", err)
	}
	nodeID := fetchResult.Data.Repository.PullRequest.ID
	if nodeID == "" {
		return "", fmt.Errorf("PR #%d not found in repository %s/%s", prNumber, owner, repo)
	}
	return nodeID, nil
}

// MarkPRReady transitions a draft PR to ready-for-review.
// Uses the GraphQL markPullRequestReadyForReview mutation, which is the supported
// path — REST PATCH does not reliably support draft→ready transitions.
func (c *Client) MarkPRReady(owner, repo string, prNumber int) error {
	nodeID, err := c.prNodeID(owner, repo, prNumber)
	if err != nil {
		return err
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

// DeleteReviewRequest removes one or more reviewer requests from a pull request.
// GitHub's DELETE endpoint requires a JSON body with the reviewer list, so this
// uses restRequest("DELETE", ...) rather than restDelete (which sends no body).
func (c *Client) DeleteReviewRequest(owner, repo string, prNumber int, reviewers []string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/requested_reviewers", c.baseURL, owner, repo, prNumber)
	body := map[string]interface{}{"reviewers": reviewers}
	if err := c.restRequest("DELETE", apiURL, body); err != nil {
		return fmt.Errorf("deleting review request on PR #%d: %w", prNumber, err)
	}
	return nil
}

// AddReviewRequest adds one or more reviewer requests to a pull request.
func (c *Client) AddReviewRequest(owner, repo string, prNumber int, reviewers []string) error {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/requested_reviewers", c.baseURL, owner, repo, prNumber)
	body := map[string]interface{}{"reviewers": reviewers}
	if err := c.restPost(apiURL, body); err != nil {
		return fmt.Errorf("adding review request on PR #%d: %w", prNumber, err)
	}
	return nil
}

// ErrAutoMergeNotEnabled is returned by EnablePullRequestAutoMerge when the
// repository has not enabled the auto-merge feature in its settings.
var ErrAutoMergeNotEnabled = errors.New("repository has not enabled auto-merge")

// ErrAutoMergeAlreadyClean is returned by EnablePullRequestAutoMerge when the
// PR is already in CLEAN status (all checks passed, immediately mergeable).
// GitHub rejects auto-merge enablement in this state — the caller must merge
// directly instead. Matched on the string "Pull request is in clean status"
// from the GitHub GraphQL API (confirmed from production logs).
var ErrAutoMergeAlreadyClean = errors.New("PR is already in clean status — merge directly")

// EnablePullRequestAutoMerge enables GitHub's native auto-merge on a pull request.
// strategy must be one of "MERGE", "SQUASH", or "REBASE". GitHub merges the PR
// atomically when all branch-protection requirements are satisfied.
//
// Returns ErrAutoMergeNotEnabled when the repository setting is disabled.
func (c *Client) EnablePullRequestAutoMerge(owner, repo string, prNumber int, strategy string) error {
	nodeID, err := c.prNodeID(owner, repo, prNumber)
	if err != nil {
		return err
	}

	mutation := `
mutation($prId: ID!, $method: PullRequestMergeMethod!) {
  enablePullRequestAutoMerge(input: { pullRequestId: $prId, mergeMethod: $method }) {
    pullRequest {
      id
    }
  }
}`
	mutVars := map[string]interface{}{
		"prId":   nodeID,
		"method": strategy,
	}
	var mutResult struct{}
	if err := c.graphqlRequest(mutation, mutVars, &mutResult); err != nil {
		// Surface a recognizable sentinel when the repo setting is disabled.
		if isAutoMergeNotEnabledError(err) {
			return fmt.Errorf("%w: %v", ErrAutoMergeNotEnabled, err)
		}
		// Surface a recognizable sentinel when the PR is already CLEAN.
		// GitHub refuses auto-merge on a PR that is immediately mergeable.
		if isAutoMergeAlreadyCleanError(err) {
			return fmt.Errorf("%w: %v", ErrAutoMergeAlreadyClean, err)
		}
		return fmt.Errorf("enabling auto-merge on PR #%d: %w", prNumber, err)
	}
	return nil
}

// isAutoMergeNotEnabledError reports whether a GraphQL error indicates the
// repository has not enabled the auto-merge feature.
func isAutoMergeNotEnabledError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Pull requests cannot be merged using the auto-merge feature") ||
		strings.Contains(msg, "auto merge is not allowed")
}

// isAutoMergeAlreadyCleanError reports whether a GraphQL error indicates the
// PR is already in CLEAN status (all checks passed, immediately mergeable).
// GitHub refuses auto-merge on such PRs — the caller must merge directly.
func isAutoMergeAlreadyCleanError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "Pull request is in clean status")
}

// EnqueuePullRequest adds a pull request to the repository's merge queue.
// expectedHeadOID is the current head SHA of the PR; if the PR has been
// force-pushed since the caller read the SHA, the mutation fails safely
// (optimistic concurrency).
//
// Uses the same two-step pattern as MarkPRReady: fetch the PR node ID via
// GraphQL, then call the enqueuePullRequest mutation.
func (c *Client) EnqueuePullRequest(owner, repo string, prNumber int, expectedHeadOID string) error {
	if expectedHeadOID == "" {
		return fmt.Errorf("expectedHeadOID cannot be empty")
	}
	nodeID, err := c.prNodeID(owner, repo, prNumber)
	if err != nil {
		return err
	}

	mutation := `
mutation($prId: ID!, $expectedHeadOid: GitObjectID!) {
  enqueuePullRequest(input: { pullRequestId: $prId, expectedHeadOid: $expectedHeadOid }) {
    mergeQueueEntry {
      id
    }
  }
}`
	mutVars := map[string]interface{}{
		"prId":            nodeID,
		"expectedHeadOid": expectedHeadOID,
	}
	var mutResult struct{}
	if err := c.graphqlRequest(mutation, mutVars, &mutResult); err != nil {
		return fmt.Errorf("enqueueing PR #%d: %w", prNumber, err)
	}
	return nil
}

// DequeuePullRequest removes a pull request from the repository's merge queue.
//
// Uses the same two-step pattern as MarkPRReady: fetch the PR node ID via
// GraphQL, then call the dequeuePullRequest mutation.
func (c *Client) DequeuePullRequest(owner, repo string, prNumber int) error {
	nodeID, err := c.prNodeID(owner, repo, prNumber)
	if err != nil {
		return err
	}

	mutation := `
mutation($prId: ID!) {
  dequeuePullRequest(input: { id: $prId }) {
    mergeQueueEntry {
      id
    }
  }
}`
	mutVars := map[string]interface{}{
		"prId": nodeID,
	}
	var mutResult struct{}
	if err := c.graphqlRequest(mutation, mutVars, &mutResult); err != nil {
		return fmt.Errorf("dequeueing PR #%d: %w", prNumber, err)
	}
	return nil
}

// FetchCommitsBehind returns how many commits base is ahead of head using the
// GitHub compare API. A positive result means head is that many commits behind
// base. Returns 0, nil when head is up to date.
func (c *Client) FetchCommitsBehind(owner, repo, base, head string) (int, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/compare/%s...%s", c.baseURL, owner, repo,
		url.PathEscape(base), url.PathEscape(head))
	var raw struct {
		BehindBy int `json:"behind_by"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return 0, fmt.Errorf("comparing %s...%s: %w", base, head, err)
	}
	return raw.BehindBy, nil
}

// mergeMethodAttemptOrder returns the ordered, de-duplicated list of REST
// merge_method values to try, starting with the configured strategy
// (lower-cased, defaulting to "merge" when unset or unrecognized) followed by
// the remaining methods in the fixed fallback order merge -> squash -> rebase.
func mergeMethodAttemptOrder(strategy string) []string {
	first := strings.ToLower(strategy)
	switch first {
	case "merge", "squash", "rebase":
	default:
		first = "merge"
	}
	order := []string{first}
	for _, m := range []string{"merge", "squash", "rebase"} {
		if m != first {
			order = append(order, m)
		}
	}
	return order
}

// MergePR merges the pull request identified by prNumber. It first checks
// GitHub's mergeable status: if null (not yet computed) or false, it returns
// ErrNotMergeable. It then self-gates on CI readiness — regardless of what
// branch protection would otherwise allow (e.g. enforce_admins: false) —
// by fetching mergeable_state via FetchPRMergeableFields and refusing with
// ErrNotMergeableCI unless the state is in the MergeableStateAccepted
// allowlist ({clean, unstable}); see ADR-072. Only then does it attempt the
// configured merge strategy (see SetMergeStrategy) first; if the repository
// does not allow that method (405), it falls back through the remaining
// methods (merge, squash, rebase, minus the one already tried) until one
// succeeds or a non-405 error occurs.
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

	// Self-gate on CI readiness before attempting the merge, independent of
	// whatever branch protection would otherwise allow.
	_, mergeableState, err := c.FetchPRMergeableFields(owner, repo, prNumber)
	if err != nil {
		return fmt.Errorf("fetching PR mergeable_state: %w", err)
	}
	if !MergeableStateAccepted(mergeableState) {
		logf(0, "merge", "PR #%d/%s/%s: refusing merge, mergeable_state=%q not in {clean, unstable}\n", prNumber, owner, repo, mergeableState)
		return fmt.Errorf("%w: mergeable_state=%q", ErrNotMergeableCI, mergeableState)
	}

	mergeURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/merge", c.baseURL, owner, repo, prNumber)
	var mergeResult struct {
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}

	for _, method := range mergeMethodAttemptOrder(c.MergeStrategy()) {
		err = c.restPutWithResponse(mergeURL, map[string]interface{}{"merge_method": method}, &mergeResult)
		if err == nil {
			logf(0, "merge", "PR #%d/%s/%s: merged using method %q\n", prNumber, owner, repo, method)
			return nil
		}
		if !errors.Is(err, ErrMethodNotAllowed) {
			return fmt.Errorf("merging PR: %w", err)
		}
		logf(0, "merge", "PR #%d/%s/%s: merge method %q not allowed, falling back\n", prNumber, owner, repo, method)
	}
	return fmt.Errorf("merging PR (all merge methods exhausted): %w", err)
}
