package githubauth

import (
	"fmt"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// RepoStatus describes one watched repo's authorization state after a
// verifyRepoAccess pass over a "selected" repository_selection installation.
type RepoStatus struct {
	Repo       string
	Authorized bool
	// Reason and GrantURL are only set when Authorized is false.
	Reason   string
	GrantURL string
}

// verifyRepoAccess is the soft, discovery-path-only generalization of
// pruefer/auth.go's former checkSelectedRepos: for a repository_selection ==
// "selected" installation, checks whether each of owner's watched repos is
// actually granted access, returning a per-repo status instead of a hard
// bootstrap error. A repo excluded from a selected-repos installation is
// surfaced (with a URL to grant it) while every other, already-authorized
// repo keeps working — see the issue's failure-handling requirement.
// installToken must be that installation's own access token —
// GET /installation/repositories is scoped to the installation identity,
// not the App's JWT.
func verifyRepoAccess(baseURL, installToken string, installationID int64, owner string, watchedRepos []string) ([]RepoStatus, error) {
	accessible, err := gh.FetchInstallationRepositories(baseURL, installToken)
	if err != nil {
		return nil, fmt.Errorf("listing accessible repositories for owner %q's installation: %w", owner, err)
	}
	accessibleSet := make(map[string]bool, len(accessible))
	for _, r := range accessible {
		accessibleSet[r] = true
	}

	var statuses []RepoStatus
	for _, repoSpec := range watchedRepos {
		o, _, ok := splitOwnerRepo(repoSpec)
		if !ok || o != owner {
			continue
		}
		if accessibleSet[repoSpec] {
			statuses = append(statuses, RepoStatus{Repo: repoSpec, Authorized: true})
			continue
		}
		statuses = append(statuses, RepoStatus{
			Repo:     repoSpec,
			Reason:   "repository_selection=selected excludes it",
			GrantURL: fmt.Sprintf("https://github.com/settings/installations/%d", installationID),
		})
	}
	return statuses, nil
}

// distinctOwners returns the distinct owners of every well-formed
// "owner/repo" entry in watchedRepos, in first-seen order. Malformed entries
// are skipped here — Reconcile logs and skips them independently.
func distinctOwners(watchedRepos []string) []string {
	seen := make(map[string]bool)
	var owners []string
	for _, spec := range watchedRepos {
		owner, _, ok := splitOwnerRepo(spec)
		if !ok || seen[owner] {
			continue
		}
		seen[owner] = true
		owners = append(owners, owner)
	}
	return owners
}

// splitOwnerRepo splits "owner/repo" into its two parts. Returns ok=false
// for anything else (missing/extra slash, empty parts). Duplicated from
// pruefer/daemon.go rather than extracted — ten lines, no import-direction
// cost, mirrors ADR-1113 Decision 6's precedent for the process-group-kill
// logic.
func splitOwnerRepo(spec string) (owner, repo string, ok bool) {
	parts := strings.Split(spec, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
