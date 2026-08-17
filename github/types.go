package github

import (
	"strings"
	"time"
)

// RateLimitStats holds the latest GitHub API rate limit values parsed from response headers.
type RateLimitStats struct {
	Limit     int
	Remaining int
	Used      int
	Reset     time.Time
	UpdatedAt time.Time
}

// ProjectBoard represents the full state of a GitHub Project (v2) board.
type ProjectBoard struct {
	ProjectID string
	Title     string // display name of the project board (from projectV2.title)
	OwnerType string // "organization" or "user", resolved by FetchProjectBoard
	Items     []ProjectItem
}

// Dependency represents a blocking issue relationship fetched from the GitHub API.
type Dependency struct {
	Number int    // Issue number of the blocking issue
	State  string // "OPEN" or "CLOSED" (GitHub GraphQL enum)
	Repo   string // "owner/repo" of the blocking issue; empty if same repo
}

// RepoAccess captures the write-access-relevant fields from GET /repos/{owner}/{repo}
// for the authenticated token. CanPush reflects permissions.push on that response.
type RepoAccess struct {
	AllowAutoMerge bool
	CanPush        bool
}

// ReviewRequest represents a pending review request on a pull request.
type ReviewRequest struct {
	Login string // GitHub login of the requested reviewer (user or bot)
	IsBot bool   // True if the reviewer is a bot (from __typename or login-pattern fallback)
}

// IsBotLogin returns true if the login matches known bot patterns.
// Used as a fallback when the GraphQL __typename field is absent or not "Bot".
//
// Originally a reviewer-classification fallback, where a false positive
// (misclassifying a human as a bot) is harmless. It is now also load-bearing
// for the fabrik:paused / fabrik:awaiting-input resume gate
// (engine/comments.go's filterHuman, ADR 069), where a false positive is not
// harmless: a human login that coincidentally matches one of these
// suffix/prefix heuristics (e.g. a genuine "data-bot" account) would be
// silently unable to resume a pause by commenting. Weigh that cost, not just
// the original fallback use, when adding new patterns here.
func IsBotLogin(login string) bool { return isBotLogin(login) }

// isBotLogin is the unexported implementation; call IsBotLogin from outside this package.
func isBotLogin(login string) bool {
	lower := strings.ToLower(login)
	if strings.HasSuffix(lower, "[bot]") {
		return true
	}
	if strings.HasSuffix(lower, "-bot") {
		return true
	}
	if strings.HasPrefix(lower, "copilot-") {
		return true
	}
	if lower == "dependabot" || lower == "gemini-code-assist" {
		return true
	}
	// coderabbitai: like dependabot/gemini-code-assist above, the GitHub App's
	// API login carries the "[bot]" suffix (already matched above) but its
	// @-mention surface is the bare form — GitHub renders and users type
	// "@coderabbitai", never "@coderabbitai[bot]". Without this literal,
	// IsBotLogin("coderabbitai") is false, so a mention-neutralization check
	// against the bare mention text would silently fail to recognize it (#1141).
	if lower == "coderabbitai" {
		return true
	}
	return false
}

// PRReview represents a submitted review on a pull request.
type PRReview struct {
	Author     string // GitHub login of the reviewer
	State      string // "APPROVED", "CHANGES_REQUESTED", or "COMMENTED"
	Body       string // Review summary body (may be empty for comment-only reviews)
	DatabaseID int    // Numeric PR review ID (0 if not fetched or unavailable)
	// CommitID is the SHA the review was submitted against (GitHub's REST
	// "commit_id" field). Needed to determine whether a review targets the
	// PR's current head SHA or a stale one (Pruefer's GitHub-derived
	// review-state mechanism; see ADR-1113).
	CommitID string
	// SubmittedAt is when the review was submitted (GraphQL "submittedAt" /
	// REST "submitted_at"). Zero value if unparseable or absent — callers
	// needing a display timestamp (e.g. buildReviewBodyComments, #1375) must
	// fall back rather than assume this is always populated.
	SubmittedAt time.Time
}

// MergeQueueEntry holds the merge-queue position and state for a pull request.
// The pointer on ProjectItem and PRDetails is nil when no merge queue entry exists.
type MergeQueueEntry struct {
	State         string // e.g. "QUEUED", "AWAITING_CHECKS", "MERGEABLE", "UNMERGEABLE"
	Position      int    // 1-indexed position in the queue; 0 when not yet positioned
	EnqueuerLogin string // GitHub login of the user who enqueued the PR
}

// ProjectItem represents an issue or pull request card on the project board.
type ProjectItem struct {
	ID             string
	ItemID         string // The project item ID (needed for mutations)
	Number         int
	Title          string
	Body           string
	Status         string // The column/status on the board
	URL            string
	Repo           string // "owner/repo" (e.g., "acme/widgets")
	IsPR           bool   // True if this item is a Pull Request (vs an Issue)
	IsClosed       bool   // True if the underlying GitHub Issue is closed (always false for PRs)
	UpdatedAt      time.Time
	Labels         []string
	Assignees      []string
	Comments       []Comment
	Author         string
	BlockedBy      []Dependency // Issues that must be closed before this one can advance
	LinkedPRNumber int          // PR number of the first linked PR (0 if none); for REST re-request calls
	// LinkedPRNumberShallow is the PR number of the first linked PR from the shallow board query (0 if none).
	// Populated only during shallow board parse. Linkage drift detection was previously performed
	// by Reconcile using this field; that responsibility has moved to the probe loop
	// (runProbeAndDeepFetch via BoardProbeItem.LinkedPRNumber). Retained for compatibility; no longer
	// read by Reconcile.
	LinkedPRNumberShallow  int
	LinkedPRHeadSHA        string          // HeadSHA from headRefOid in the GraphQL query (empty if not fetched)
	LinkedPRReviewRequests []ReviewRequest // Outstanding reviewer requests on the linked PR
	LinkedPRReviews        []PRReview      // Reviews already submitted on the linked PR
	// LinkedPRReviewThreadComments holds the inline (per-line) comments from
	// unresolved review threads on the linked PR. These are real GitHub
	// comments with DatabaseIDs and can be reacted to / resolved.
	LinkedPRReviewThreadComments []Comment
	// LinkedPRResolvedThreadCount is the number of review threads on the linked PR
	// that are currently resolved. Used by progress detection during turn extension.
	LinkedPRResolvedThreadCount int

	// LinkedPRIsMergeQueueEnabled is true when the repository has the merge queue
	// feature enabled. Zero (false) when no queue exists or the field was not fetched.
	LinkedPRIsMergeQueueEnabled bool
	// LinkedPRIsInMergeQueue is true when the linked PR is currently in the merge queue.
	LinkedPRIsInMergeQueue bool
	// LinkedPRMergeQueueEntry holds the queue position and state when the PR is
	// enqueued. Nil when the PR is not in the queue or mergeQueueEntry was null.
	LinkedPRMergeQueueEntry *MergeQueueEntry
}

// Comment represents a comment on an issue or linked PR.
type Comment struct {
	ID         string
	DatabaseID int // Numeric ID needed for REST API (reactions, etc.)
	Author     string
	Body       string
	CreatedAt  time.Time
	Reactions  []ReactionGroup
	FromPR     int // Non-zero if this comment is from a linked PR
	// ReviewThreadID is the GraphQL node ID of the PR review thread this
	// comment belongs to. Empty for non-review-thread comments. Needed to
	// call resolveReviewThread after the feedback is addressed.
	ReviewThreadID string
	// Path is the file path targeted by the PR review thread comment.
	// Empty for regular issue and PR body comments.
	Path string
	// Line is the line number in the current diff. Zero when not applicable
	// (e.g., regular comments) or when the comment targets a deleted line.
	Line int
	// OriginalLine is the line number in the original base diff. Used as a
	// fallback when Line is 0. Zero when not applicable.
	OriginalLine int
	// DiffHunk is the diff context hunk surrounding the comment. Empty for
	// regular issue and PR body comments.
	DiffHunk string
	// IsOutdated mirrors the parent review thread's GitHub-computed isOutdated
	// field: true when the commented lines have been superseded by a later
	// push to the PR (the thread's diff no longer matches the current head).
	// Only meaningful for review-thread comments (ReviewThreadID non-empty).
	IsOutdated bool
}

// PRReviewThread is a single review thread on a pull request, grouped as
// thread -> ordered comments rather than the flat per-comment shape Comment
// uses — Pruefer needs to render "original finding, then author's reply" as
// a coherent unit (see FetchPRReviewThreads, adrs/1497-pruefer-prior-review-thread-context.md).
type PRReviewThread struct {
	ID         string
	Path       string
	Line       int // GraphQL "line", falling back to "originalLine" when line is null (e.g. an outdated thread)
	IsResolved bool
	IsOutdated bool
	Comments   []PRReviewThreadComment
	// CommentsTruncated is true when the thread has more comments than
	// FetchPRReviewThreads' per-thread page size (20) fetched — Comments
	// holds only the oldest 20 (GitHub's comments connection has no orderBy
	// argument to pin this explicitly; "oldest 20" reflects the connection's
	// observed default order, see FetchPRReviewThreads), so a later reply
	// (e.g. the one that actually resolved the thread) may be missing.
	// Callers rendering thread content should surface this rather than
	// presenting Comments as the complete history.
	CommentsTruncated bool
}

// PRReviewThreadComment is a single comment within a PRReviewThread, in
// thread order (the original finding first, followed by any replies).
type PRReviewThreadComment struct {
	Author    string
	Body      string
	CreatedAt time.Time
}

// ReviewComment is a single line-anchored inline comment to submit as part of
// a pull_request_review's comments[] array (outbound request shape only —
// narrower than Comment, which carries response-only fields like ID/Author/
// ReviewThreadID that don't apply to a submission). Line anchors the RIGHT
// (post-change) side only; SubmitPRReview hardcodes "side": "RIGHT" — see
// adrs/1189-pruefer-inline-review-comments.md.
type ReviewComment struct {
	Path string
	Line int
	Body string
}

// ReactionGroup represents a reaction type and its count on a comment.
type ReactionGroup struct {
	Content string // e.g. "THUMBS_UP", "EYES", etc.
	Count   int
}

// LatestRelease represents the response from GET /repos/{owner}/{repo}/releases/latest.
type LatestRelease struct {
	TagName string         `json:"tag_name"`
	Assets  []ReleaseAsset `json:"assets"`
}

// ReleaseAsset represents a single downloadable asset in a GitHub release.
type ReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	APIURL             string `json:"url"` // API URL for downloading with Accept: application/octet-stream
	Size               int64  `json:"size"`
}

// BoardProbeItem is the per-item result from ProbeProjectBoard. It contains
// only scalar identity fields — no labels or full comment history — to minimise
// GraphQL cost on idle polls. effectiveUpdatedAt is max(content.updatedAt,
// projectItem.updatedAt, linkedPR.updatedAt); used for cache staleness checks.
type BoardProbeItem struct {
	ItemID             string
	ContentID          string
	Number             int
	IsPR               bool
	IsClosed           bool
	State              string
	Repo               string
	EffectiveUpdatedAt time.Time
	LinkedPRNumber     int
	LinkedPRUpdatedAt  time.Time
	LinkedPRHeadSHA    string // headRefOid from GraphQL probe query; empty if no linked PR
	Status             string
}

// HasReaction returns true if the comment has at least one reaction of the given type.
func (c Comment) HasReaction(content string) bool {
	for _, r := range c.Reactions {
		if r.Content == content && r.Count > 0 {
			return true
		}
	}
	return false
}
