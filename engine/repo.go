package engine

import (
	"fmt"
	"strconv"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
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

// itemOwnerRepoString returns the "owner/repo" string for an item.
// Uses item.Repo if non-empty; falls back to defaultRepo.
func itemOwnerRepoString(item gh.ProjectItem, defaultRepo string) string {
	if item.Repo != "" {
		return item.Repo
	}
	return defaultRepo
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

// parseIssueKey reverses issueKey: parses "owner/repo#N" into (owner, repo, issueNumber).
// Falls back to (defaultOwner, defaultRepo, 0) on malformed input.
func parseIssueKey(key, defaultOwner, defaultRepo string) (owner, repo string, issueNumber int) {
	// Expected format: "owner/repo#N"
	hashIdx := strings.LastIndex(key, "#")
	if hashIdx < 0 {
		return defaultOwner, defaultRepo, 0
	}
	repoWithOwner := key[:hashIdx]
	numStr := key[hashIdx+1:]
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return defaultOwner, defaultRepo, 0
	}
	o, r := parseOwnerRepo(repoWithOwner)
	if o == "" || r == "" {
		return defaultOwner, defaultRepo, n
	}
	return o, r, n
}

// resolveFabrikEnvOpts resolves the two InvokeOptions fields (#1288) that
// require Engine-only state — FabrikRoot and PRNumber — so every
// InvokeOptions-constructing call site can compute them identically instead
// of each independently reimplementing (and risking drift on) the PR
// resolution logic.
//
// PRNumber resolution mirrors handleBrokenReviewLinkage's exact filter
// (reviews.go): item.LinkedPRNumber is trusted when non-zero; otherwise, and
// only when a PR could plausibly exist yet (stage.PostToPR || stage.CreateDraftPR
// — false for Specify/Research/Plan in the default stage set, so those stages
// never pay for a lookup that can't succeed), FetchLinkedPR is consulted via
// REST. This is required because closedByPullRequestsReferences (the GraphQL
// field LinkedPRNumber is sourced from) is structurally empty on a
// base:<branch> repo. A result that errors, is nil, or isn't an open,
// unmerged PR is treated as "no PR" — non-fatal, logged at warn, never
// blocking the invocation — so FABRIK_PR never shows a PR the review gate
// itself would discount.
func (e *Engine) resolveFabrikEnvOpts(item gh.ProjectItem, stage *stages.Stage) (fabrikRoot string, prNumber int) {
	fabrikRoot = e.fabrikDir

	prNumber = item.LinkedPRNumber
	if prNumber != 0 || !(stage.PostToPR || stage.CreateDraftPR) {
		return fabrikRoot, prNumber
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	pr, err := e.readClient.FetchLinkedPR(owner, repo, item.Number)
	if err != nil || pr == nil || pr.Number == 0 || pr.State != "open" || pr.Merged {
		if err != nil {
			e.logf(item.Number, "warn", "resolveFabrikEnvOpts: FetchLinkedPR failed: %v\n", err)
		}
		return fabrikRoot, 0
	}
	return fabrikRoot, pr.Number
}
