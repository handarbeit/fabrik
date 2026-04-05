package engine

import (
	"fmt"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// parseOwnerRepo splits a "owner/repo" string into its two components.
// Returns ("", "") for malformed input (no slash, empty owner, or empty repo).
func parseOwnerRepo(nameWithOwner string) (owner, repo string) {
	parts := strings.SplitN(nameWithOwner, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", ""
	}
	return parts[0], parts[1]
}

// repoName returns just the repository part of "owner/repo".
// Returns empty string for malformed input.
func repoName(nameWithOwner string) string {
	_, r := parseOwnerRepo(nameWithOwner)
	return r
}

// issueKey returns a unique string key for an issue that includes its repo identity.
// Format: "owner/repo#N". Uses item.Repo if set; falls back to defaultRepo otherwise.
// defaultRepo should be the engine's configured owner/repo fallback (e.g. "owner/repo").
func issueKey(item gh.ProjectItem, defaultRepo string) string {
	repo := item.Repo
	if repo == "" {
		repo = defaultRepo
	}
	return fmt.Sprintf("%s#%d", repo, item.Number)
}

// itemOwnerRepo returns the (owner, repo) pair for an item.
// Uses item.Repo if non-empty; falls back to defaultRepo.
func itemOwnerRepo(item gh.ProjectItem, defaultRepo string) (owner, repo string) {
	r := item.Repo
	if r == "" {
		r = defaultRepo
	}
	return parseOwnerRepo(r)
}
