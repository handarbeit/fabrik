package engine

import (
	"fmt"
	"strings"

	gh "github.com/verveguy/fabrik/github"
)

// ensureDraftPR pushes the issue branch and creates a draft PR if one doesn't exist yet.
// Idempotent: checks for an existing PR first; only pushes and creates if none found.
func (e *Engine) ensureDraftPR(item gh.ProjectItem, baseBranch string) int {
	// Check for an existing PR first — avoids pushing on retries and handles
	// the case where a push fails but a PR already exists from a prior run.
	prNumber, err := e.client.FindPRForIssue(e.cfg.Owner, e.cfg.Repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "could not check for existing PR: %v\n", err)
		return 0
	}
	if prNumber > 0 {
		e.logf(item.Number, "pr", "PR #%d already exists, ensuring issue link\n", prNumber)
		e.ensurePRLinksIssue(prNumber, item.Number)
		return prNumber
	}

	// No PR exists — push the branch so GitHub can create a PR against it
	if err := e.worktrees.PushBranch(item.Number); err != nil {
		e.logf(item.Number, "warn", "could not push branch: %v\n", err)
		return 0
	}

	head := fmt.Sprintf("fabrik/issue-%d", item.Number)
	prNum, err := e.client.CreateDraftPR(e.cfg.Owner, e.cfg.Repo, item.Title, head, baseBranch, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "could not create draft PR: %v\n", err)
		return 0
	}
	e.logf(item.Number, "pr", "created draft PR #%d\n", prNum)
	return prNum
}

// ensurePRLinksIssue checks that a PR body contains "Closes #N" and adds it if missing.
// This ensures closedByPullRequestsReferences links the PR to the issue, which is how
// Fabrik discovers PR comments.
func (e *Engine) ensurePRLinksIssue(prNumber, issueNumber int) {
	closingKeyword := fmt.Sprintf("Closes #%d", issueNumber)

	// Fetch current PR body (PRs are issues on the REST API)
	body, err := e.client.GetIssueBody(e.cfg.Owner, e.cfg.Repo, prNumber)
	if err != nil {
		e.logf(issueNumber, "warn", "could not fetch PR #%d body: %v\n", prNumber, err)
		return
	}

	if strings.Contains(body, closingKeyword) {
		return // already linked
	}

	// Append closing keyword
	updatedBody := body + "\n\n" + closingKeyword
	if err := e.client.UpdateIssueBody(e.cfg.Owner, e.cfg.Repo, prNumber, updatedBody); err != nil {
		e.logf(issueNumber, "warn", "could not update PR #%d body: %v\n", prNumber, err)
		return
	}
	e.logf(issueNumber, "pr", "added '%s' to PR #%d body\n", closingKeyword, prNumber)
}

// markPRReady pushes the issue branch and transitions its PR from draft to ready-for-review.
// If no PR exists yet (e.g., ensureDraftPR failed earlier because there were no commits),
// it attempts to create one before marking it ready.
// knownPR is the PR number from ensureDraftPR (avoids search API race); 0 falls back to search.
func (e *Engine) markPRReady(item gh.ProjectItem, knownPR int) {
	if err := e.worktrees.PushBranch(item.Number); err != nil {
		e.logf(item.Number, "warn", "could not push branch: %v\n", err)
		// Don't return — still try to mark ready if push is a no-op (already up to date)
	}

	prNumber := knownPR
	if prNumber == 0 {
		var err error
		prNumber, err = e.client.FindPRForIssue(e.cfg.Owner, e.cfg.Repo, item.Number)
		if err != nil {
			e.logf(item.Number, "warn", "could not find PR: %v\n", err)
			return
		}
	}
	if prNumber == 0 {
		e.logf(item.Number, "warn", "no PR found to mark ready\n")
		return
	}

	if err := e.client.MarkPRReady(e.cfg.Owner, e.cfg.Repo, prNumber); err != nil {
		e.logf(item.Number, "warn", "could not mark PR #%d ready: %v\n", prNumber, err)
		return
	}
	e.logf(item.Number, "pr", "marked PR #%d ready-for-review\n", prNumber)
}

// postOutputToPR posts detailed output on the linked PR and a brief summary on the issue.
func (e *Engine) postOutputToPR(item gh.ProjectItem, stageName, output, branch, commit, timestamp string) {
	prNumber, err := e.client.FindPRForIssue(e.cfg.Owner, e.cfg.Repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "could not find PR: %v\n", err)
	}

	if prNumber > 0 {
		// Post detailed output on the PR
		comment := formatOutputComment(stageName, output, branch, commit, timestamp)
		if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, prNumber, comment); err != nil {
			e.logf(item.Number, "warn", "could not post to PR #%d: %v\n", prNumber, err)
		} else {
			e.logf(item.Number, "post", "detailed %s output posted to PR #%d\n", stageName, prNumber)
		}

		// Post brief summary on the issue
		summary := formatPRSummaryComment(stageName, prNumber, output, branch, commit, timestamp)
		if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, summary); err != nil {
			e.logf(item.Number, "warn", "could not post summary: %v\n", err)
		}
	} else {
		// No PR found — fall back to posting on the issue
		e.logf(item.Number, "warn", "no open PR found, posting on issue instead\n")
		comment := formatOutputComment(stageName, output, branch, commit, timestamp)
		if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, comment); err != nil {
			e.logf(item.Number, "warn", "could not post comment: %v\n", err)
		}
	}
}

func formatOutputComment(stageName, output, branch, commit, timestamp string) string {
	const maxLen = 60000
	if len(output) > maxLen {
		output = output[:maxLen] + "\n\n... (truncated)"
	}
	meta := fmt.Sprintf("*branch: %s | commit: %s | %s*", branch, commit, timestamp)
	return fmt.Sprintf("🏭 **Fabrik — stage: %s**\n%s\n\n%s", stageName, meta, output)
}

func formatPRSummaryComment(stageName string, prNumber int, output, branch, commit, timestamp string) string {
	summary := extractSummary(output)
	if summary == "" {
		summary = "(no summary provided)"
	}
	meta := fmt.Sprintf("*branch: %s | commit: %s | %s*", branch, commit, timestamp)
	return fmt.Sprintf("🏭 **Fabrik — stage: %s**\n%s\n\nDetailed output posted on PR #%d.\n\n%s", stageName, meta, prNumber, summary)
}
