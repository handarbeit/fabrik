package github

import "time"

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

// ReviewRequest represents a pending review request on a pull request.
type ReviewRequest struct {
	Login string // GitHub login of the requested reviewer (user or bot)
}

// PRReview represents a submitted review on a pull request.
type PRReview struct {
	Author     string // GitHub login of the reviewer
	State      string // "APPROVED", "CHANGES_REQUESTED", or "COMMENTED"
	Body       string // Review summary body (may be empty for comment-only reviews)
	DatabaseID int    // Numeric PR review ID (0 if not fetched or unavailable)
}

// ProjectItem represents an issue or pull request card on the project board.
type ProjectItem struct {
	ID                     string
	ItemID                 string // The project item ID (needed for mutations)
	Number                 int
	Title                  string
	Body                   string
	Status                 string // The column/status on the board
	URL                    string
	Repo                   string // "owner/repo" (e.g., "acme/widgets")
	IsPR                   bool   // True if this item is a Pull Request (vs an Issue)
	IsClosed               bool   // True if the underlying GitHub Issue is closed (always false for PRs)
	UpdatedAt              time.Time
	Labels                 []string
	Assignees              []string
	Comments               []Comment
	Author                 string
	BlockedBy              []Dependency    // Issues that must be closed before this one can advance
	LinkedPRReviewRequests []ReviewRequest // Outstanding reviewer requests on the linked PR
	LinkedPRReviews        []PRReview      // Reviews already submitted on the linked PR
	// LinkedPRReviewThreadComments holds the inline (per-line) comments from
	// unresolved review threads on the linked PR. These are real GitHub
	// comments with DatabaseIDs and can be reacted to / resolved.
	LinkedPRReviewThreadComments []Comment
	// LinkedPRResolvedThreadCount is the number of review threads on the linked PR
	// that are currently resolved. Used by progress detection during turn extension.
	LinkedPRResolvedThreadCount int
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

// HasReaction returns true if the comment has at least one reaction of the given type.
func (c Comment) HasReaction(content string) bool {
	for _, r := range c.Reactions {
		if r.Content == content && r.Count > 0 {
			return true
		}
	}
	return false
}
