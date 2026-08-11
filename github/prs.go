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
//
// A COMMENTED submission never supersedes a prior formal verdict (APPROVED,
// CHANGES_REQUESTED, or DISMISSED) from the same author — GitHub's own
// reviewDecision computation treats COMMENTED as informational, not a state
// transition, so a reviewer who requests changes and later leaves a comment-only
// follow-up (without re-approving or dismissing) still has an active
// CHANGES_REQUESTED verdict. Only when an author's *stored* entry is itself
// COMMENTED (no verdict established yet) does a later COMMENTED submission
// supersede it — so a comment-only reviewer's collapsed entry tracks their
// latest submission, not just their first. States outside COMMENTED and the
// three formal verdicts (e.g. PENDING, which is visible only to the token
// that created it and may reflect an incomplete submission) are enumerated
// explicitly as never eligible to establish or overwrite a collapsed entry —
// first-seen or not — rather than being treated as a verdict by omission.
// Returns nil, nil on 404.
func (c *Client) FetchPRReviews(owner, repo string, prNumber int) ([]PRReview, error) {
	type rawReview struct {
		ID   int `json:"id"`
		User *struct {
			Login string `json:"login"`
		} `json:"user"`
		State       string `json:"state"`
		Body        string `json:"body"`
		CommitID    string `json:"commit_id"`
		SubmittedAt string `json:"submitted_at"`
	}
	// Paginated (#1539): the collapse below keeps the LAST eligible review per
	// author, so reading only page one silently pins every author's verdict to
	// whatever they last said within the first restPageSize reviews. On a
	// long-lived PR that is an arbitrarily stale verdict — it drove a 315-review
	// re-review loop in Pruefer and reaches the authoritative landing gate.
	raw, err := paginateREST[rawReview](c, fmt.Sprintf("PR #%d reviews", prNumber), func(page int) string {
		return fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews?per_page=%d&page=%d",
			c.baseURL, owner, repo, prNumber, restPageSize, page)
	})
	if err != nil {
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
		if !canEstablishOrSupersedeReview(r.State) {
			// PENDING (or any other state outside COMMENTED and the three
			// formal verdicts) never establishes or overwrites a collapsed
			// entry — a private/incomplete submission must not pin an
			// author's review, first-seen or not.
			continue
		}
		stored, seen := latestByAuthor[r.User.Login]
		if !seen {
			order = append(order, r.User.Login)
		} else if r.State == "COMMENTED" && isFormalReviewVerdict(stored.State) {
			// A comment-only follow-up does not overwrite the author's
			// existing formal verdict.
			continue
		}
		review := PRReview{
			Author:     r.User.Login,
			State:      r.State,
			Body:       r.Body,
			DatabaseID: r.ID,
			CommitID:   r.CommitID,
		}
		if t, err := parseTime(r.SubmittedAt); err == nil {
			review.SubmittedAt = t
		}
		latestByAuthor[r.User.Login] = review
	}
	out := make([]PRReview, 0, len(order))
	for _, author := range order {
		out = append(out, latestByAuthor[author])
	}
	return out, nil
}

// isFormalReviewVerdict reports whether state is one of GitHub's three
// formal review verdicts — the states a COMMENTED follow-up must never
// overwrite.
func isFormalReviewVerdict(state string) bool {
	switch state {
	case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
		return true
	}
	return false
}

// canEstablishOrSupersedeReview reports whether an incoming submission's
// state is ever eligible to become or replace an author's collapsed entry.
// Only COMMENTED and the three formal verdicts qualify; every other state
// (PENDING, or any state GitHub introduces in the future) is excluded
// unconditionally, so it can neither establish a first entry nor silently
// pin an author's collapsed review at a later submission.
func canEstablishOrSupersedeReview(state string) bool {
	return state == "COMMENTED" || isFormalReviewVerdict(state)
}

// fetchPRReviewDecisionQuery is the GraphQL query used by FetchPRReviewDecision.
const fetchPRReviewDecisionQuery = `
query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewDecision
    }
  }
}`

// FetchPRReviewDecision returns GitHub's computed review-decision verdict for a
// pull request via GraphQL, keyed on PR number — mirroring prNodeID's
// single-field-by-number query shape rather than closedByPullRequestsReferences,
// so it works identically for default-branch and base:<branch> PRs (GitHub's
// REST API has no equivalent field; reviewDecision is GraphQL-only).
//
// Returns one of "APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED" when the
// repository has a branch-protection review requirement configured for this
// PR, or "" when GitHub reports no such requirement (reviewDecision is null) —
// callers must treat "" as "no real verdict available", not as a satisfied gate.
// Returns an error (rather than "") when the query resolves with no pullRequest
// object at all (bad prNumber, or an app-token permission gap) — that is
// "unknown state", distinct from a legitimate null reviewDecision, and must
// not be folded into the same "" the no-branch-protection fallback treats as
// meaningful data.
func (c *Client) FetchPRReviewDecision(owner, repo string, prNumber int) (string, error) {
	query := fetchPRReviewDecisionQuery
	vars := map[string]interface{}{
		"owner":  owner,
		"repo":   repo,
		"number": prNumber,
	}
	var result struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					ReviewDecision string `json:"reviewDecision"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := c.graphqlRequest(query, vars, &result); err != nil {
		return "", fmt.Errorf("fetching PR #%d review decision: %w", prNumber, err)
	}
	if result.Data.Repository.PullRequest == nil {
		return "", fmt.Errorf("PR #%d not found in repository %s/%s", prNumber, owner, repo)
	}
	return result.Data.Repository.PullRequest.ReviewDecision, nil
}

// fetchPRReviewThreadsQuery is the GraphQL query used by FetchPRReviewThreads.
const fetchPRReviewThreadsQuery = `
query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 50) {
        totalCount
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          originalLine
          comments(first: 20) {
            totalCount
            nodes {
              author { login }
              body
              createdAt
            }
          }
        }
      }
    }
  }
}`

// FetchPRReviewThreads returns every review thread on a pull request —
// resolved and unresolved alike — via GraphQL, keyed on PR number (same
// base-independent shape as FetchPRReviewDecision, since Pruefer reviews PRs
// directly with no linked issue in scope).
//
// Unlike applyLinkedPRs' engine-facing fetch (which discards resolved-thread
// bodies and only counts them, since only unresolved feedback is actionable
// for a reinvoke), this returns full thread content — including resolved
// threads — because Pruefer's prompt-assembly use case needs to see how a
// prior finding was addressed, not just that it was. See
// adrs/1497-pruefer-prior-review-thread-context.md.
//
// Threads are capped at 50 (GraphQL's reviewThreads(first: 50), matching the
// existing fetchItemDetailsQuery fragment's ceiling) and comments per thread
// at 20; neither is paginated. A PR with more threads than that loses the
// excess at this layer — out of scope for the caller's own prompt-level cap
// (a much smaller number) — but not silently: the second return value,
// threadsTruncated, is set from the reviewThreads connection's totalCount so
// a caller can say so rather than presenting a partial thread list as
// complete, mirroring PRReviewThread.CommentsTruncated's per-thread signal
// for the same "tell the reviewer/log rather than silently drop" pattern.
// Nodes arrive in the connection's default (creation) order with no orderBy
// argument to bias toward unresolved threads, so on a truncated fetch the
// dropped threads are the newest — disproportionately likely to be OPEN.
//
// comments(first: 20) has no orderBy argument — GitHub's schema doesn't
// expose one for PullRequestReviewThread.comments (confirmed via
// introspection), so which 20 of a longer thread are returned relies on the
// connection's own default order, not something this query can pin
// explicitly. Observed behavior returns them oldest-first (thread creation
// order), which is what CommentsTruncated's doc comment and callers'
// omission notes assume; if GitHub ever changes that default, the "oldest
// 20" framing here would need revisiting.
//
// Returns an error (not an empty slice) when the query resolves with no
// pullRequest node, matching FetchPRReviewDecision's convention.
func (c *Client) FetchPRReviewThreads(owner, repo string, prNumber int) ([]PRReviewThread, bool, error) {
	query := fetchPRReviewThreadsQuery
	vars := map[string]interface{}{
		"owner":  owner,
		"repo":   repo,
		"number": prNumber,
	}
	var result struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					ReviewThreads struct {
						TotalCount int `json:"totalCount"`
						Nodes      []struct {
							ID           string `json:"id"`
							IsResolved   bool   `json:"isResolved"`
							IsOutdated   bool   `json:"isOutdated"`
							Path         string `json:"path"`
							Line         *int   `json:"line"`
							OriginalLine *int   `json:"originalLine"`
							Comments     struct {
								TotalCount int `json:"totalCount"`
								Nodes      []struct {
									Author *struct {
										Login string `json:"login"`
									} `json:"author"`
									Body      string `json:"body"`
									CreatedAt string `json:"createdAt"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := c.graphqlRequest(query, vars, &result); err != nil {
		return nil, false, fmt.Errorf("fetching PR #%d review threads: %w", prNumber, err)
	}
	if result.Data.Repository.PullRequest == nil {
		return nil, false, fmt.Errorf("PR #%d not found in repository %s/%s", prNumber, owner, repo)
	}

	var threads []PRReviewThread
	for _, n := range result.Data.Repository.PullRequest.ReviewThreads.Nodes {
		line := 0
		if n.Line != nil {
			line = *n.Line
		} else if n.OriginalLine != nil {
			line = *n.OriginalLine
		}
		t := PRReviewThread{
			ID:                n.ID,
			Path:              n.Path,
			Line:              line,
			IsResolved:        n.IsResolved,
			IsOutdated:        n.IsOutdated,
			CommentsTruncated: n.Comments.TotalCount > len(n.Comments.Nodes),
		}
		for _, cm := range n.Comments.Nodes {
			tc := PRReviewThreadComment{Body: cm.Body}
			if cm.Author != nil {
				tc.Author = cm.Author.Login
			}
			if ts, err := parseTime(cm.CreatedAt); err == nil {
				tc.CreatedAt = ts
			}
			t.Comments = append(t.Comments, tc)
		}
		threads = append(threads, t)
	}
	rt := result.Data.Repository.PullRequest.ReviewThreads
	threadsTruncated := rt.TotalCount > len(rt.Nodes)
	return threads, threadsTruncated, nil
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
	// OutputSummary is the check run's output.summary field — a short
	// Markdown summary the check author chose to surface. May be empty even
	// on a failing run if the check never set it.
	OutputSummary string
	// OutputText is the check run's output.text field — the fuller Markdown
	// body (e.g. a formatted log excerpt). May be empty even on a failing
	// run if the check never set it.
	OutputText string
	// DetailsURL is the check run's details_url — the "Details" link GitHub
	// shows in the checks UI, typically pointing at the CI provider's own
	// run page.
	DetailsURL string
	// HTMLURL is the check run's html_url — the GitHub-hosted permalink to
	// the check run itself.
	HTMLURL string
}

// FetchCheckRuns retrieves check runs for a given commit SHA via the REST API.
//
// Paginated and total_count-verified (#1539). This was the worst of the
// truncating fetchers: it sent no per_page at all, so GitHub's default of 30
// applied, and it discarded the total_count the endpoint reports — a commit with
// more than 30 check runs therefore read as a complete, potentially all-green
// set. Every CI gate decision (checkCIGate, the merge-train trial classifier)
// rests on this list, so a truncated read can advance an item whose remaining
// checks are still pending or failing.
//
// The count check is deliberately an error rather than a warning: a CI verdict
// computed from a known-incomplete set is worse than no verdict, since the
// caller cannot tell the two apart.
func (c *Client) FetchCheckRuns(owner, repo, sha string) ([]CheckRun, error) {
	type rawCheckRun struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		Output     struct {
			Summary string `json:"summary"`
			Text    string `json:"text"`
		} `json:"output"`
		DetailsURL string `json:"details_url"`
		HTMLURL    string `json:"html_url"`
	}
	var all []rawCheckRun
	totalCount := 0
	for page := 1; page <= restMaxPages; page++ {
		apiURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs?per_page=%d&page=%d",
			c.baseURL, owner, repo, sha, restPageSize, page)
		var raw struct {
			TotalCount int           `json:"total_count"`
			CheckRuns  []rawCheckRun `json:"check_runs"`
		}
		if err := c.restGetJSON(apiURL, &raw); err != nil {
			return nil, fmt.Errorf("fetching check runs for %s: %w", sha, err)
		}
		totalCount = raw.TotalCount
		all = append(all, raw.CheckRuns...)
		if len(raw.CheckRuns) < restPageSize {
			break
		}
		if page == restMaxPages {
			return nil, fmt.Errorf("fetching check runs for %s: exceeded %d pages without reaching the end — refusing to return a truncated result",
				sha, restMaxPages)
		}
	}
	// Only cross-check when the server actually reported a positive total.
	// Guarding on >= 0 (as this first did) makes an ABSENT total_count — which
	// decodes to 0 — indistinguishable from a genuine zero, so any deployment
	// that omits the field would hard-fail every CI gate on every commit. Real
	// github.com always sends it (see testdata/recordings/fetch_check_runs.json,
	// a captured response), but v0.0.78 added GHES support and no equivalent
	// recording exists for it, so this fails open on absence and closed only on
	// a real, positive disagreement. A server reporting 0 while returning runs
	// is incoherent and not worth defending against.
	if totalCount > 0 && len(all) != totalCount {
		return nil, fmt.Errorf("fetching check runs for %s: collected %d of %d reported check runs — refusing to return an incomplete set",
			sha, len(all), totalCount)
	}
	out := make([]CheckRun, len(all))
	for i, cr := range all {
		out[i] = CheckRun{
			ID:            cr.ID,
			Name:          cr.Name,
			Status:        cr.Status,
			Conclusion:    cr.Conclusion,
			OutputSummary: cr.Output.Summary,
			OutputText:    cr.Output.Text,
			DetailsURL:    cr.DetailsURL,
			HTMLURL:       cr.HTMLURL,
		}
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
// indicates the PR is mergeable per branch protection rules alone. "clean"
// means fully ready; "unstable" means non-required checks have failed but
// GitHub itself still considers the PR mergeable. Other values ("blocked",
// "behind", "dirty", "draft", "has_hooks", "unknown", "") fall through to the
// per-check classification.
//
// "has_hooks" is treated as not-accepted here because pre-merge hooks may
// modify the merge outcome; conservative to use the per-check path.
//
// ADR-1441: since that issue, this predicate is no longer consulted by the CI
// *advance* gate (settlePRMergeState/checkCIGate, engine/pr_settle.go +
// engine/ci.go) — "unstable" there now falls through to full check-run/
// required-context classification instead of a green shortcut, so a
// confirmed failure on a non-required check blocks the advance regardless of
// what this function reports. Its remaining callers are all on the *merge*
// side of ADR-1441's R3 split, which deliberately keeps deferring to branch
// protection alone (ADR-072's operator note): MergePR's own ADR-072
// self-gate, pollForMergeable's zero-check-runs fallback and pollTrainCI's
// dirty pre-check + zero-check-runs green arm (engine/merge_train.go) — the
// same "no per-check signal to fall back on" case in all three.
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
// selection logic. Pages until exhausted (#1539) — it previously returned only
// the first 100 and logged a warning, so a repo with more open PRs than that
// had the remainder invisible to PR selection and merge-train discovery.
func (c *Client) ListOpenPRs(owner, repo string) ([]PRDetails, error) {
	type rawPR struct {
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
	raw, err := paginateREST[rawPR](c, fmt.Sprintf("open PRs for %s/%s", owner, repo), func(page int) string {
		return fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&per_page=%d&page=%d",
			c.baseURL, owner, repo, restPageSize, page)
	})
	if err != nil {
		return nil, fmt.Errorf("listing open PRs for %s/%s: %w", owner, repo, err)
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

// FetchPRFiles returns the list of file paths changed by a pull request via
// the paginated GET /pulls/{n}/files endpoint, which has no line-count
// ceiling (unlike the .diff media type FetchPRDiff uses) — the fallback path
// for reconstructing a changed-path list when FetchPRDiff returns
// ErrDiffTooLarge. Follows FetchLabelAppliedAt's pagination idiom: 100 per
// page, stop on an empty page. Returns nil, nil on 404.
func (c *Client) FetchPRFiles(owner, repo string, prNumber int) ([]string, error) {
	var out []string
	// Bounded by restMaxPages (#1539 review finding 1). This loop predates
	// paginateREST and was written as `for page := 1; ; page++` with no cap, so a
	// server that never returns an empty page — a misconfigured proxy, a GHES
	// bug, a hostile endpoint — spins forever while `out` grows without bound.
	// Not converted to paginateREST because the accumulation projects to a field
	// rather than collecting whole records; the bound is what actually matters.
	for page := 1; page <= restMaxPages; page++ {
		apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/files?per_page=%d&page=%d",
			c.baseURL, owner, repo, prNumber, restPageSize, page)
		var raw []struct {
			Filename string `json:"filename"`
		}
		if err := c.restGetJSON(apiURL, &raw); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, nil
			}
			return nil, fmt.Errorf("fetching files for PR #%d: %w", prNumber, err)
		}
		for _, f := range raw {
			out = append(out, f.Filename)
		}
		if len(raw) < restPageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("fetching files for PR #%d: exceeded %d pages without reaching the end — refusing to return a truncated result",
		prNumber, restMaxPages)
}

// ReviewEvent selects the wire "event" value SubmitPRReview submits. Its
// field is unexported so no package outside github can construct an
// arbitrary value — the only way to obtain a ReviewEvent is to copy one of
// the two values below (or receive the zero value). This is what makes "no
// APPROVE, ever" a compile-time property rather than a convention: even a
// future in-package mistake can't smuggle an arbitrary string through, and
// SubmitPRReview itself additionally normalizes defensively (see below), so
// the guarantee holds even against a hypothetical bug in this very type's
// definition. See adrs/1251-pruefer-severity-gated-request-changes.md.
type ReviewEvent struct{ raw string }

var (
	// ReviewEventComment submits a non-blocking review comment (GitHub's
	// default "COMMENT" event) — the only event Pruefer V1 ever submitted.
	ReviewEventComment = ReviewEvent{raw: "COMMENT"}
	// ReviewEventRequestChanges submits a blocking "REQUEST_CHANGES" review.
	// Pruefer computes this Go-side from parsed finding severities — never
	// from Claude's prose — see pruefer/review.go's decideEvent.
	ReviewEventRequestChanges = ReviewEvent{raw: "REQUEST_CHANGES"}
)

// SubmitPRReview submits a formal pull_request_review comment against the
// given commit SHA, optionally with line-anchored inline comments posted in
// the same request. event selects COMMENT vs REQUEST_CHANGES — computed
// Go-side by the caller (see ReviewEvent's doc comment), never derived from
// Claude's output text. There is no APPROVE path: only a ReviewEvent whose
// internal string is exactly "REQUEST_CHANGES" escapes the "COMMENT"
// default below, so even an unexpected/zero ReviewEvent value degrades to
// the non-blocking event rather than approving or erroring. Each comment's
// side is likewise hardcoded to "RIGHT" — V1 supports only single-line,
// post-change-side anchors (see adrs/1189-pruefer-inline-review-comments.md).
// The "comments" key is omitted entirely when comments is empty, preserving
// the exact body-only wire shape callers relied on before that parameter
// existed. Returns the numeric review ID.
func (c *Client) SubmitPRReview(owner, repo string, prNumber int, commitSHA, body string, event ReviewEvent, comments []ReviewComment) (int, error) {
	wireEvent := "COMMENT"
	if event.raw == "REQUEST_CHANGES" {
		wireEvent = "REQUEST_CHANGES"
	}
	apiURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews", c.baseURL, owner, repo, prNumber)
	reqBody := map[string]interface{}{
		"commit_id": commitSHA,
		"body":      body,
		"event":     wireEvent,
	}
	if len(comments) > 0 {
		wireComments := make([]map[string]interface{}, len(comments))
		for i, cm := range comments {
			wireComments[i] = map[string]interface{}{
				"path": cm.Path,
				"line": cm.Line,
				"side": "RIGHT",
				"body": cm.Body,
			}
		}
		reqBody["comments"] = wireComments
	}
	var result struct {
		ID int `json:"id"`
	}
	if err := c.restPostWithResponse(apiURL, reqBody, &result); err != nil {
		return 0, fmt.Errorf("submitting review on PR #%d: %w", prNumber, err)
	}
	return result.ID, nil
}

// prNodeIDQuery is the GraphQL query used by prNodeID.
const prNodeIDQuery = `
query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      id
    }
  }
}`

// prNodeID fetches the GraphQL node ID of a pull request by its REST number.
// The node ID is required by every GraphQL mutation that operates on a PR
// (markPullRequestReadyForReview, enablePullRequestAutoMerge, enqueuePullRequest,
// dequeuePullRequest) since none of them accept a plain PR number.
func (c *Client) prNodeID(owner, repo string, prNumber int) (string, error) {
	fetchQuery := prNodeIDQuery
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

// markPRReadyMutation is the GraphQL mutation used by MarkPRReady.
const markPRReadyMutation = `
mutation($prId: ID!) {
  markPullRequestReadyForReview(input: { pullRequestId: $prId }) {
    pullRequest {
      id
    }
  }
}`

// MarkPRReady transitions a draft PR to ready-for-review.
// Uses the GraphQL markPullRequestReadyForReview mutation, which is the supported
// path — REST PATCH does not reliably support draft→ready transitions.
func (c *Client) MarkPRReady(owner, repo string, prNumber int) error {
	nodeID, err := c.prNodeID(owner, repo, prNumber)
	if err != nil {
		return err
	}

	mutation := markPRReadyMutation
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
// enablePullRequestAutoMergeMutation is the GraphQL mutation used by EnablePullRequestAutoMerge.
const enablePullRequestAutoMergeMutation = `
mutation($prId: ID!, $method: PullRequestMergeMethod!) {
  enablePullRequestAutoMerge(input: { pullRequestId: $prId, mergeMethod: $method }) {
    pullRequest {
      id
    }
  }
}`

// Returns ErrAutoMergeNotEnabled when the repository setting is disabled.
func (c *Client) EnablePullRequestAutoMerge(owner, repo string, prNumber int, strategy string) error {
	nodeID, err := c.prNodeID(owner, repo, prNumber)
	if err != nil {
		return err
	}

	mutation := enablePullRequestAutoMergeMutation
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

// DisablePullRequestAutoMerge disables GitHub's native auto-merge on a pull
// request that previously had it enabled via EnablePullRequestAutoMerge.
// Used to stop GitHub from merging underneath Fabrik's review-reinvoke loop
// when an unresolved review thread appears on the current head during the
// convergence window (#1207).
//
// disablePullRequestAutoMergeMutation is the GraphQL mutation used by DisablePullRequestAutoMerge.
const disablePullRequestAutoMergeMutation = `
mutation($prId: ID!) {
  disablePullRequestAutoMerge(input: { pullRequestId: $prId }) {
    pullRequest {
      id
    }
  }
}`

// Uses the same two-step pattern as EnablePullRequestAutoMerge: fetch the PR
// node ID via GraphQL, then call the disablePullRequestAutoMerge mutation.
func (c *Client) DisablePullRequestAutoMerge(owner, repo string, prNumber int) error {
	nodeID, err := c.prNodeID(owner, repo, prNumber)
	if err != nil {
		return err
	}

	mutation := disablePullRequestAutoMergeMutation
	mutVars := map[string]interface{}{
		"prId": nodeID,
	}
	var mutResult struct{}
	if err := c.graphqlRequest(mutation, mutVars, &mutResult); err != nil {
		return fmt.Errorf("disabling auto-merge on PR #%d: %w", prNumber, err)
	}
	return nil
}

// EnqueuePullRequest adds a pull request to the repository's merge queue.
// expectedHeadOID is the current head SHA of the PR; if the PR has been
// force-pushed since the caller read the SHA, the mutation fails safely
// (optimistic concurrency).
//
// enqueuePullRequestMutation is the GraphQL mutation used by EnqueuePullRequest.
const enqueuePullRequestMutation = `
mutation($prId: ID!, $expectedHeadOid: GitObjectID!) {
  enqueuePullRequest(input: { pullRequestId: $prId, expectedHeadOid: $expectedHeadOid }) {
    mergeQueueEntry {
      id
    }
  }
}`

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

	mutation := enqueuePullRequestMutation
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
// dequeuePullRequestMutation is the GraphQL mutation used by DequeuePullRequest.
const dequeuePullRequestMutation = `
mutation($prId: ID!) {
  dequeuePullRequest(input: { id: $prId }) {
    mergeQueueEntry {
      id
    }
  }
}`

// Uses the same two-step pattern as MarkPRReady: fetch the PR node ID via
// GraphQL, then call the dequeuePullRequest mutation.
func (c *Client) DequeuePullRequest(owner, repo string, prNumber int) error {
	nodeID, err := c.prNodeID(owner, repo, prNumber)
	if err != nil {
		return err
	}

	mutation := dequeuePullRequestMutation
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
