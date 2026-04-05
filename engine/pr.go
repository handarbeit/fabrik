package engine

import (
	"fmt"
	"strings"

	gh "github.com/verveguy/fabrik/github"
)

// ensureDraftPR pushes the issue branch and creates a draft PR if one doesn't exist yet.
// Idempotent: checks for an existing PR first; only pushes and creates if none found.
func (e *Engine) ensureDraftPR(item gh.ProjectItem, baseBranch string) int {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Check for an existing PR first — avoids pushing on retries and handles
	// the case where a push fails but a PR already exists from a prior run.
	prNumber, err := e.client.FindPRForIssue(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "could not check for existing PR: %v\n", err)
		return 0
	}
	if prNumber > 0 {
		e.logf(item.Number, "pr", "PR #%d already exists, ensuring issue link\n", prNumber)
		e.ensurePRLinksIssue(item, prNumber)
		e.ensurePRTaskList(item, prNumber)
		return prNumber
	}

	// No PR exists — push the branch so GitHub can create a PR against it
	wm := e.worktreesFor(item.Repo)
	if err := wm.PushBranch(item.Number); err != nil {
		e.logf(item.Number, "warn", "could not push branch: %v\n", err)
		return 0
	}

	head := fmt.Sprintf("fabrik/issue-%d", item.Number)
	prNum, err := e.client.CreateDraftPR(owner, repo, item.Title, head, baseBranch, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "could not create draft PR: %v\n", err)
		return 0
	}
	e.logf(item.Number, "pr", "created draft PR #%d\n", prNum)

	// Inject the Plan stage task list into the PR body (if available)
	e.ensurePRTaskList(item, prNum)

	return prNum
}

// ensurePRTaskList extracts the task list from the Plan stage comment (between
// FABRIK_TASK_LIST_BEGIN/END markers) and adds it to the PR body. This makes the
// PR body the canonical location for the task checklist. If the Plan stage comment
// predates the markers or doesn't exist, this is a no-op.
func (e *Engine) ensurePRTaskList(item gh.ProjectItem, prNumber int) {
	// Find the Plan stage comment from the issue's comments
	planComment := findStageComment(item.Comments, "Plan")
	if planComment == nil {
		return
	}

	taskList := extractBetweenMarkers(planComment.Body, "FABRIK_TASK_LIST_BEGIN", "FABRIK_TASK_LIST_END")
	if taskList == "" {
		return
	}

	// Fetch current PR body
	body, err := e.client.GetIssueBody(e.cfg.Owner, e.cfg.Repo, prNumber)
	if err != nil {
		e.logf(item.Number, "warn", "could not fetch PR #%d body for task list: %v\n", prNumber, err)
		return
	}

	// Check if task list is already present (idempotent)
	if strings.Contains(body, "FABRIK_TASK_LIST_BEGIN") || strings.Contains(body, taskList) {
		return
	}

	// Append task list to existing body, preserving Closes #N and any other content
	updatedBody := body + "\n\n" + taskList
	if err := e.client.UpdateIssueBody(e.cfg.Owner, e.cfg.Repo, prNumber, updatedBody); err != nil {
		e.logf(item.Number, "warn", "could not update PR #%d body with task list: %v\n", prNumber, err)
		return
	}
	e.logf(item.Number, "pr", "added Plan task list to PR #%d body\n", prNumber)
}

// ensurePRLinksIssue checks that a PR body contains "Closes #N" and adds it if missing.
// This ensures closedByPullRequestsReferences links the PR to the issue, which is how
// Fabrik discovers PR comments.
func (e *Engine) ensurePRLinksIssue(item gh.ProjectItem, prNumber int) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	closingKeyword := fmt.Sprintf("Closes #%d", item.Number)

	// Fetch current PR body (PRs are issues on the REST API)
	body, err := e.client.GetIssueBody(owner, repo, prNumber)
	if err != nil {
		e.logf(item.Number, "warn", "could not fetch PR #%d body: %v\n", prNumber, err)
		return
	}

	if strings.Contains(body, closingKeyword) {
		return // already linked
	}

	// Append closing keyword
	updatedBody := body + "\n\n" + closingKeyword
	if err := e.client.UpdateIssueBody(owner, repo, prNumber, updatedBody); err != nil {
		e.logf(item.Number, "warn", "could not update PR #%d body: %v\n", prNumber, err)
		return
	}
	e.logf(item.Number, "pr", "added '%s' to PR #%d body\n", closingKeyword, prNumber)
}

// markPRReady pushes the issue branch and transitions its PR from draft to ready-for-review.
// If no PR exists yet (e.g., ensureDraftPR failed earlier because there were no commits),
// it attempts to create one before marking it ready.
// knownPR is the PR number from ensureDraftPR (avoids search API race); 0 falls back to search.
func (e *Engine) markPRReady(item gh.ProjectItem, knownPR int) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	wm := e.worktreesFor(item.Repo)
	if err := wm.PushBranch(item.Number); err != nil {
		e.logf(item.Number, "warn", "could not push branch: %v\n", err)
		// Don't return — still try to mark ready if push is a no-op (already up to date)
	}

	prNumber := knownPR
	if prNumber == 0 {
		var err error
		prNumber, err = e.client.FindPRForIssue(owner, repo, item.Number)
		if err != nil {
			e.logf(item.Number, "warn", "could not find PR: %v\n", err)
			return
		}
	}
	if prNumber == 0 {
		e.logf(item.Number, "warn", "no PR found to mark ready\n")
		return
	}

	if err := e.client.MarkPRReady(owner, repo, prNumber); err != nil {
		e.logf(item.Number, "warn", "could not mark PR #%d ready: %v\n", prNumber, err)
		return
	}
	e.logf(item.Number, "pr", "marked PR #%d ready-for-review\n", prNumber)
}

// postOutputToPR posts detailed output on the linked PR and a brief summary on the issue.
func (e *Engine) postOutputToPR(item gh.ProjectItem, stageName, output, footer, branch, commit, mainSHA, timestamp string) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	prNumber, err := e.client.FindPRForIssue(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "could not find PR: %v\n", err)
	}

	if prNumber > 0 {
		// Post detailed output on the PR
		comment := formatOutputComment(stageName, output, footer, branch, commit, mainSHA, timestamp)
		if err := e.client.AddComment(owner, repo, prNumber, comment); err != nil {
			e.logf(item.Number, "warn", "could not post to PR #%d: %v\n", prNumber, err)
		} else {
			e.logf(item.Number, "post", "detailed %s output posted to PR #%d\n", stageName, prNumber)
		}

		// Post brief summary on the issue
		summary := formatPRSummaryComment(stageName, prNumber, output, branch, commit, mainSHA, timestamp)
		if err := e.client.AddComment(owner, repo, item.Number, summary); err != nil {
			e.logf(item.Number, "warn", "could not post summary: %v\n", err)
		}
	} else {
		// No PR found — fall back to posting on the issue
		e.logf(item.Number, "warn", "no open PR found, posting on issue instead\n")
		comment := formatOutputComment(stageName, output, footer, branch, commit, mainSHA, timestamp)
		if err := e.client.AddComment(owner, repo, item.Number, comment); err != nil {
			e.logf(item.Number, "warn", "could not post comment: %v\n", err)
		}
	}
}

// formatOutputComment formats Claude's output as a GitHub comment.
// footer is appended after any truncation so it is never cut off.
// mainSHA is the origin/{baseBranch} SHA at the time of capture; if empty,
// the main: field is omitted for backward compatibility with older comments.
func formatOutputComment(stageName, output, footer, branch, commit, mainSHA, timestamp string) string {
	const maxLen = 60000
	if len(output) > maxLen {
		output = output[:maxLen] + "\n\n... (truncated)"
	}
	meta := formatMetaLine(branch, commit, mainSHA, timestamp)
	return fmt.Sprintf("🏭 **Fabrik — stage: %s**\n%s\n\n%s%s", stageName, meta, output, footer)
}

func formatPRSummaryComment(stageName string, prNumber int, output, branch, commit, mainSHA, timestamp string) string {
	summary := extractSummary(output)
	if summary == "" {
		summary = "(no summary provided)"
	}
	meta := formatMetaLine(branch, commit, mainSHA, timestamp)
	return fmt.Sprintf("🏭 **Fabrik — stage: %s**\n%s\n\nDetailed output posted on PR #%d.\n\n%s", stageName, meta, prNumber, summary)
}

// formatMetaLine builds the italicized metadata line for comment headers.
// When mainSHA is non-empty, includes a main: field. The full SHA is stored
// (not abbreviated) because it is later used as a git revision in
// writeCodebaseChanges — abbreviated SHAs can become ambiguous over time.
func formatMetaLine(branch, commit, mainSHA, timestamp string) string {
	if mainSHA != "" {
		return fmt.Sprintf("*branch: %s | commit: %s | main: %s | %s*", branch, commit, mainSHA, timestamp)
	}
	return fmt.Sprintf("*branch: %s | commit: %s | %s*", branch, commit, timestamp)
}
