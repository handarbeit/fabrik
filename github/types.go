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
	Items     []ProjectItem
}

// ProjectItem represents an issue or pull request card on the project board.
type ProjectItem struct {
	ID        string
	ItemID    string // The project item ID (needed for mutations)
	Number    int
	Title     string
	Body      string
	Status    string // The column/status on the board
	URL       string
	IsPR      bool // True if this item is a Pull Request (vs an Issue)
	UpdatedAt time.Time
	Labels    []string
	Assignees []string
	Comments  []Comment
	Author    string
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
}

// ReactionGroup represents a reaction type and its count on a comment.
type ReactionGroup struct {
	Content string // e.g. "THUMBS_UP", "EYES", etc.
	Count   int
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
