package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
)

// markPRReadyRetryDelay is the base delay for markPRReady retry backoff.
// Declared as a var so tests can set it to 0 to avoid sleeping.
var markPRReadyRetryDelay = 500 * time.Millisecond

// ensureDraftPRRetryDelay is the base delay for ensureDraftPR retry backoff.
// Declared as a var so tests can set it to 0 to avoid sleeping.
var ensureDraftPRRetryDelay = 500 * time.Millisecond

// ensureDraftPR pushes the issue branch and creates a draft PR if one doesn't exist yet.
// Idempotent: checks for an existing open PR first; only pushes and creates if none found.
// Returns (prNumber, nil) on success, (0, err) on failure after retries.
func (e *Engine) ensureDraftPR(item gh.ProjectItem, baseBranch string) (int, error) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	head := fmt.Sprintf("fabrik/issue-%d", item.Number)
	wm := e.worktreesFor(item.Repo)

	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Check for an existing open PR on each attempt — handles the race where
		// GitHub auto-created a PR between retries, or a prior attempt succeeded
		// but the response was lost.
		pr, err := e.client.FetchLinkedPR(owner, repo, item.Number)
		if err != nil {
			if !isTransientError(err) {
				e.logf(item.Number, "pr", "failed to create draft PR for branch %s: %v\n", head, err)
				return 0, fmt.Errorf("checking for existing PR: %w", err)
			}
			lastErr = err
			if attempt < maxAttempts-1 {
				time.Sleep(ensureDraftPRRetryDelay << attempt)
			}
			continue
		}
		if pr != nil && pr.State == "open" && !pr.Merged {
			// An open PR already exists — use it.
			e.logf(item.Number, "pr", "PR #%d already exists (branch: %s, sha: %s), ensuring issue link\n", pr.Number, head, pr.HeadSHA)
			e.ensurePRLinksIssue(item, pr.Number)
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.RecordPRLinkage(owner+"/"+repo, pr.Number, item.Number)
			}
			return pr.Number, nil
		}
		// No open PR (or closed/merged) — push and create.

		// Merge-queue awareness (ADR-058 D3 FR-1): skip the push when the PR is in
		// the queue — pushing ejects it. No-op on non-queue repos (FR-3).
		if err := e.pushBranchUnlessQueued(item, wm); err != nil {
			if !isTransientError(err) {
				e.logf(item.Number, "pr", "failed to create draft PR for branch %s: %v\n", head, err)
				return 0, fmt.Errorf("pushing branch: %w", err)
			}
			lastErr = err
			if attempt < maxAttempts-1 {
				time.Sleep(ensureDraftPRRetryDelay << attempt)
			}
			continue
		}

		// Build seed body from context files; fall back to minimal body on read errors
		workDir := wm.WorktreeDir(item.Number)
		seedBody := e.buildSeedBody(item, workDir)

		// no write-through: excluded — CreateDraftPR affects PR state, not issue/label cache
		prNum, err := e.client.CreateDraftPR(owner, repo, item.Title, head, baseBranch, seedBody, item.Number)
		if err != nil {
			if !isTransientError(err) {
				e.logf(item.Number, "pr", "failed to create draft PR for branch %s: %v\n", head, err)
				return 0, fmt.Errorf("creating draft PR: %w", err)
			}
			lastErr = err
			if attempt < maxAttempts-1 {
				time.Sleep(ensureDraftPRRetryDelay << attempt)
			}
			continue
		}
		if prNum == 0 {
			// A nil error with PR number 0 indicates an unexpected API/decoding issue.
			// Treat as non-transient so the caller enters the escalation path.
			e.logf(item.Number, "pr", "failed to create draft PR for branch %s: API returned 0\n", head)
			return 0, fmt.Errorf("creating draft PR: API returned PR number 0")
		}
		headSHA, _ := gitHeadSHA(workDir)
		e.logf(item.Number, "pr", "created draft PR #%d (branch: %s, sha: %s)\n", prNum, head, headSHA)
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.RecordPRLinkage(owner+"/"+repo, prNum, item.Number)
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("pull_request", "opened", fmt.Sprintf("%s/%s#pr%d", owner, repo, prNum))
		}
		return prNum, nil
	}
	e.logf(item.Number, "pr", "failed to create draft PR for branch %s: %v\n", head, lastErr)
	return 0, fmt.Errorf("creating draft PR after %d attempts: %w", maxAttempts, lastErr)
}

// syncPRBase checks whether the open PR for this issue has the expected base branch and
// updates it via the GitHub API if not. All errors are non-fatal: a warning is logged
// and the stage continues regardless.
//
// Insertion point: called after EnsureWorktree succeeds (baseBranch is resolved and the
// worktree lock is held) but before Claude is invoked.
func (e *Engine) syncPRBase(item gh.ProjectItem, baseBranch string) {
	// Merge-queue awareness (ADR-058 D3 FR-1): a base change on a queued PR disturbs
	// the queue (it ejects the PR). When the PR is in the queue, skip the base sync
	// entirely. Signal is the GraphQL-populated ProjectItem field; false-by-default,
	// so non-queue repos are unchanged (FR-3).
	if prInMergeQueue(item) {
		e.logf(item.Number, "merge-queue", "PR in merge queue — skipping base sync (would eject from queue)\n")
		return
	}

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
	if e.webhookMgr != nil {
		e.webhookMgr.RegisterEcho("issues", "edited", boardcache.ItemKey(owner+"/"+repo, prNumber))
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
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issues", "edited", boardcache.ItemKey(owner+"/"+repo, prNumber))
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
	if e.webhookMgr != nil {
		e.webhookMgr.RegisterEcho("issues", "edited", boardcache.ItemKey(owner+"/"+repo, prNumber))
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
	// Merge-queue awareness (ADR-058 D3 FR-1): skip the push when queued (ejects it).
	if err := e.pushBranchUnlessQueued(item, wm); err != nil {
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
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := e.client.MarkPRReady(owner, repo, prNumber)
		if err == nil {
			if e.webhookMgr != nil {
				e.webhookMgr.RegisterEcho("pull_request", "ready_for_review", fmt.Sprintf("%s/%s#pr%d", owner, repo, prNumber))
			}
			e.logf(item.Number, "pr", "marked PR #%d ready-for-review\n", prNumber)
			return
		}
		if !isTransientError(err) {
			e.logf(item.Number, "warn", "could not mark PR #%d ready: %v\n", prNumber, err)
			return
		}
		lastErr = err
		if attempt < maxAttempts-1 {
			time.Sleep(markPRReadyRetryDelay << attempt)
		}
	}
	e.logf(item.Number, "warn", "could not mark PR #%d ready after %d attempts: %v\n", prNumber, maxAttempts, lastErr)
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
			if e.webhookMgr != nil {
				e.webhookMgr.RegisterEcho("issue_comment", "created", boardcache.ItemKey(owner+"/"+repo, prNumber))
			}
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
			if e.webhookMgr != nil {
				e.webhookMgr.RegisterEcho("issue_comment", "created", boardcache.ItemKey(owner+"/"+repo, item.Number))
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
			if e.webhookMgr != nil {
				e.webhookMgr.RegisterEcho("issue_comment", "created", boardcache.ItemKey(owner+"/"+repo, item.Number))
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
