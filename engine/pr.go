package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/verveguy/fabrik/boardcache"
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
		return prNumber
	}

	// No PR exists — push the branch so GitHub can create a PR against it
	wm := e.worktreesFor(item.Repo)
	if err := wm.PushBranch(item.Number); err != nil {
		e.logf(item.Number, "warn", "could not push branch: %v\n", err)
		return 0
	}

	// Build seed body from context files; fall back to minimal body on read errors
	workDir := wm.WorktreeDir(item.Number)
	seedBody := e.buildSeedBody(item, workDir)

	head := fmt.Sprintf("fabrik/issue-%d", item.Number)
	// no write-through: excluded — CreateDraftPR affects PR state, not issue/label cache
	prNum, err := e.client.CreateDraftPR(owner, repo, item.Title, head, baseBranch, seedBody, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "could not create draft PR: %v\n", err)
		return 0
	}
	e.logf(item.Number, "pr", "created draft PR #%d\n", prNum)
	return prNum
}

// syncPRBase checks whether the open PR for this issue has the expected base branch and
// updates it via the GitHub API if not. All errors are non-fatal: a warning is logged
// and the stage continues regardless.
//
// Insertion point: called after EnsureWorktree succeeds (baseBranch is resolved and the
// worktree lock is held) but before Claude is invoked.
func (e *Engine) syncPRBase(item gh.ProjectItem, baseBranch string) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	prNumber, err := e.client.FindPRForIssue(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "syncPRBase: could not find PR: %v\n", err)
		return
	}
	if prNumber == 0 {
		return // no PR yet — nothing to sync
	}

	currentBase, err := e.client.GetPRBase(owner, repo, prNumber)
	if err != nil {
		e.logf(item.Number, "warn", "syncPRBase: could not fetch PR #%d base: %v\n", prNumber, err)
		return
	}
	if currentBase == baseBranch {
		return // already correct
	}

	if err := e.client.UpdatePRBase(owner, repo, prNumber, baseBranch); err != nil {
		e.logf(item.Number, "warn", "syncPRBase: could not update PR #%d base %q → %q: %v\n", prNumber, currentBase, baseBranch, err)
		return
	}
	e.logf(item.Number, "pr", "updated PR #%d base: %s → %s\n", prNumber, currentBase, baseBranch)
}

// buildSeedBody reads .fabrik-context/issue.md and .fabrik-context/stage-Plan.md from
// workDir and constructs the structured PR seed body. File read errors are non-fatal:
// missing files produce empty strings which buildPRSeedBody handles with placeholders.
func (e *Engine) buildSeedBody(item gh.ProjectItem, workDir string) string {
	issueContent := readContextFile(workDir, "issue.md")
	planContent := readContextFile(workDir, "stage-Plan.md")
	return buildPRSeedBody(issueContent, planContent, item.Number)
}

// readContextFile reads a file from the .fabrik-context/ directory in workDir.
// Returns empty string on any error (missing file, permission error, etc.).
func readContextFile(workDir, filename string) string {
	data, err := os.ReadFile(filepath.Join(workDir, ".fabrik-context", filename))
	if err != nil {
		return ""
	}
	return string(data)
}

// updatePRVerification updates the ## Verification section of the PR body with summary.
// No-op if summary is empty. Warns and skips if the section is not found in the PR body.
func (e *Engine) updatePRVerification(item gh.ProjectItem, prNumber int, summary string) {
	if summary == "" {
		return
	}
	if prNumber <= 0 {
		e.logf(item.Number, "warn", "invalid PR number %d for Verification update — skipping\n", prNumber)
		return
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	currentBody, err := e.client.GetIssueBody(owner, repo, prNumber)
	if err != nil {
		e.logf(item.Number, "warn", "could not fetch PR #%d body for Verification update: %v\n", prNumber, err)
		return
	}

	updatedBody, found := replaceVerificationSection(currentBody, summary)
	if !found {
		e.logf(item.Number, "warn", "PR #%d body has no ## Verification section — skipping update\n", prNumber)
		return
	}

	// no write-through: excluded — issue body is not read from cache for dispatch decisions
	if err := e.client.UpdateIssueBody(owner, repo, prNumber, updatedBody); err != nil {
		e.logf(item.Number, "warn", "could not update PR #%d Verification section: %v\n", prNumber, err)
		return
	}
	e.logf(item.Number, "pr", "updated PR #%d Verification section\n", prNumber)
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

	// Self-heal unclosed code fences that could hide Closes #N from GitHub's parser.
	balanced := balanceFences(body)
	if balanced != body {
		// no write-through: excluded — issue body is not read from cache for dispatch decisions
		if err := e.client.UpdateIssueBody(owner, repo, prNumber, balanced); err != nil {
			e.logf(item.Number, "warn", "could not update PR #%d body to close fence: %v\n", prNumber, err)
			return
		}
		e.logf(item.Number, "pr", "balanced unclosed code fence in PR #%d body\n", prNumber)
	}

	if strings.Contains(balanced, closingKeyword) {
		return // already linked
	}

	// Append closing keyword
	updatedBody := balanced + "\n\n" + closingKeyword
	// no write-through: excluded — issue body is not read from cache for dispatch decisions
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

	// no write-through: excluded — MarkPRReady affects PR state, not issue/label cache
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
		// no write-through: excluded — posts to prNumber (PR comment thread, not issue cache)
		comment := formatOutputComment(stageName, output, footer, branch, commit, mainSHA, timestamp)
		if dbID, err := e.client.AddComment(owner, repo, prNumber, comment); err != nil {
			e.logf(item.Number, "warn", "could not post to PR #%d: %v\n", prNumber, err)
		} else {
			e.logf(item.Number, "post", "detailed %s output posted to PR #%d\n", stageName, prNumber)
			// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
			if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
				e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
			}
		}

		// Post brief summary on the issue
		summary := formatPRSummaryComment(stageName, prNumber, output, branch, commit, mainSHA, timestamp)
		if dbID, err := e.client.AddComment(owner, repo, item.Number, summary); err != nil {
			e.logf(item.Number, "warn", "could not post summary: %v\n", err)
		} else {
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
					DatabaseID: dbID, Body: summary, Author: e.cfg.User, CreatedAt: time.Now(),
				})
			}
			// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
			if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
				e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
			}
		}
	} else {
		// No PR found — fall back to posting on the issue
		e.logf(item.Number, "warn", "no open PR found, posting on issue instead\n")
		comment := formatOutputComment(stageName, output, footer, branch, commit, mainSHA, timestamp)
		if dbID, err := e.client.AddComment(owner, repo, item.Number, comment); err != nil {
			e.logf(item.Number, "warn", "could not post comment: %v\n", err)
		} else {
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
					DatabaseID: dbID, Body: comment, Author: e.cfg.User, CreatedAt: time.Now(),
				})
			}
			// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
			if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
				e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
			}
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

// reviewThreadEntry holds the resolved location data for a single PR review thread.
// Line is already resolved: Comment.Line if nonzero, else Comment.OriginalLine.
// A zero Line means no line number was available.
type reviewThreadEntry struct {
	Path string
	Line int
}

// buildThreadEntries constructs a deduplicated list of reviewThreadEntry values
// from the input comments. One entry per unique ReviewThreadID; first occurrence
// wins. Line is resolved from Comment.Line (nonzero) or Comment.OriginalLine.
func buildThreadEntries(comments []gh.Comment) []reviewThreadEntry {
	seen := make(map[string]bool)
	var entries []reviewThreadEntry
	for _, c := range comments {
		if c.ReviewThreadID == "" {
			continue
		}
		if seen[c.ReviewThreadID] {
			continue
		}
		seen[c.ReviewThreadID] = true
		line := c.Line
		if line == 0 {
			line = c.OriginalLine
		}
		entries = append(entries, reviewThreadEntry{Path: c.Path, Line: line})
	}
	return entries
}

// formatReviewFeedbackComment formats a Fabrik-marked comment for posting on a
// linked PR after review-reinvoke processing completes. It includes:
//   - a header line identifying the stage and action
//   - standard metadata (branch, commit, main SHA, timestamp)
//   - Claude's cleaned output (truncated at 60k)
//   - a per-thread footer listing each addressed thread by path:line
//   - a summary line with thread and comment counts
func formatReviewFeedbackComment(stageName, output, branch, commit, mainSHA, timestamp string, threads []reviewThreadEntry, totalComments int) string {
	const maxLen = 60000
	if len(output) > maxLen {
		output = output[:maxLen] + "\n\n... (truncated)"
	}
	meta := formatMetaLine(branch, commit, mainSHA, timestamp)
	title := stageName + " (review feedback addressed)"

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🏭 **Fabrik — stage: %s**\n%s\n\n%s\n\n---\n**Threads addressed:**\n", title, meta, output))
	for _, e := range threads {
		displayPath := e.Path
		if displayPath == "" {
			displayPath = "(unknown path)"
		}
		if e.Line > 0 {
			sb.WriteString(fmt.Sprintf("- `%s:%d` — resolved\n", displayPath, e.Line))
		} else {
			sb.WriteString(fmt.Sprintf("- `%s` — resolved\n", displayPath))
		}
	}
	sb.WriteString(fmt.Sprintf("\nResolved %d review thread(s) across %d comment(s).", len(threads), totalComments))
	return sb.String()
}
