package github

import "time"

// ProjectBoard represents the full state of a GitHub Project (v2) board.
type ProjectBoard struct {
	ProjectID string
	Items     []ProjectItem
}

// ProjectItem represents an issue card on the project board.
type ProjectItem struct {
	ID        string
	ItemID    string // The project item ID (needed for mutations)
	Number    int
	Title     string
	Body      string
	Status    string // The column/status on the board
	URL       string
	Labels    []string
	Assignees []string
	Comments  []Comment
	Author    string
}

// Comment represents a comment on an issue.
type Comment struct {
	ID         string
	DatabaseID int // Numeric ID needed for REST API (reactions, etc.)
	Author     string
	Body       string
	CreatedAt  time.Time
}
