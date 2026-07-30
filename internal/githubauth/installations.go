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
	// Keyed by lower-cased "owner/repo": GitHub org/user (and repo) names are
	// case-insensitive, but FetchInstallationRepositories returns each
	// full_name in its canonical case while repoSpec (below) comes verbatim
	// from the user's watched_repos config string — an exact-case map would
	// report a config entry like "Org/Repo" as unauthorized even when
	// access is genuinely granted under GitHub's canonical "org/repo",
	// silently skipping review of that repo.
	accessibleSet := make(map[string]bool, len(accessible))
	for _, r := range accessible {
		accessibleSet[strings.ToLower(r)] = true
	}

	var statuses []RepoStatus
	for _, repoSpec := range watchedRepos {
		o, _, ok := splitOwnerRepo(repoSpec)
		if !ok || !strings.EqualFold(o, owner) {
			continue
		}
		if accessibleSet[strings.ToLower(repoSpec)] {
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
// are silently skipped — see distinctOwnersLogging for the variant that
// also reports them.
func distinctOwners(watchedRepos []string) []string {
	return distinctOwnersLogging(watchedRepos, func(string, ...any) {})
}

// distinctOwnersLogging is distinctOwners' single-pass sibling: it also logs
// a warning for every malformed "owner/repo" entry as it encounters it, so a
// caller that needs both (Reconcile) doesn't have to make a second full
// pass over watchedRepos — re-running the exact same splitOwnerRepo check —
// purely to report what this pass already skips.
//
// Dedup is case-insensitive (keyed on the lower-cased owner, keeping the
// first-seen literal casing in the result): GitHub org/user logins are
// case-insensitive, so "MyOrg/repo1" and "myorg/repo2" name the same
// account. Without folding case here, both survive as "distinct" owners and
// each independently resolves to the same installation downstream (via the
// already-case-insensitive byAccount/accessibleSet lookups), causing
// Reconcile to mint a redundant token, verify repo access, and spawn a
// redundant refresh goroutine twice for one installation.
func distinctOwnersLogging(watchedRepos []string, logf func(string, ...any)) []string {
	seen := make(map[string]bool)
	var owners []string
	for _, spec := range watchedRepos {
		owner, _, ok := splitOwnerRepo(spec)
		if !ok {
			logf("! %q is not a valid \"owner/repo\" watched-repo entry — skipping", spec)
			continue
		}
		key := strings.ToLower(owner)
		if seen[key] {
			continue
		}
		seen[key] = true
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
