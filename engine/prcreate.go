package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// PRCreateBlock holds the parsed content of a FABRIK_PR_CREATE_BEGIN/END marker block.
type PRCreateBlock struct {
	// TargetRepo is "owner/repo" from the BEGIN line, or "" for same-repo.
	TargetRepo string
	Title      string
	Body       string
}

// ParsePRCreateBlock scans s for a FABRIK_PR_CREATE_BEGIN/END marker block.
//
// Returns:
//   - (nil, nil)   — no marker found
//   - (nil, err)   — marker frame present but block is malformed
//   - (block, nil) — valid block
//
// Marker format:
//
//	FABRIK_PR_CREATE_BEGIN [owner/repo]
//	TITLE: <single-line title>
//
//	<PR body content — no closing keyword>
//	FABRIK_PR_CREATE_END
func ParsePRCreateBlock(s string) (*PRCreateBlock, error) {
	const beginPrefix = "FABRIK_PR_CREATE_BEGIN"
	const endMarker = "FABRIK_PR_CREATE_END"

	beginIdx := strings.Index(s, beginPrefix)
	if beginIdx == -1 {
		return nil, nil // not found
	}

	// Extract the rest of the BEGIN line for the optional owner/repo arg.
	lineEnd := strings.IndexByte(s[beginIdx:], '\n')
	var beginLine, afterBegin string
	if lineEnd == -1 {
		beginLine = s[beginIdx:]
		afterBegin = ""
	} else {
		beginLine = s[beginIdx : beginIdx+lineEnd]
		afterBegin = s[beginIdx+lineEnd+1:]
	}
	beginLine = strings.TrimRight(beginLine, "\r")

	// Optional owner/repo on the BEGIN line.
	targetRepo := ""
	if parts := strings.Fields(strings.TrimPrefix(beginLine, beginPrefix)); len(parts) > 0 {
		targetRepo = parts[0]
		// Validate exactly "owner/repo": two non-empty parts separated by a single slash.
		repoParts := strings.Split(targetRepo, "/")
		if len(repoParts) != 2 || repoParts[0] == "" || repoParts[1] == "" {
			return nil, fmt.Errorf("FABRIK_PR_CREATE_BEGIN: invalid repo %q — expected owner/repo format", targetRepo)
		}
	}

	// Find the matching END marker.
	endIdx := strings.Index(afterBegin, endMarker)
	if endIdx == -1 {
		return nil, fmt.Errorf("FABRIK_PR_CREATE_BEGIN found but no matching FABRIK_PR_CREATE_END")
	}

	blockContent := afterBegin[:endIdx]

	title, body := parsePRCreateTitleAndBody(blockContent)
	if title == "" {
		return nil, fmt.Errorf("FABRIK_PR_CREATE block is malformed: missing TITLE line")
	}
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("FABRIK_PR_CREATE block is malformed: PR body is empty")
	}

	return &PRCreateBlock{
		TargetRepo: targetRepo,
		Title:      title,
		Body:       strings.TrimSpace(body),
	}, nil
}

// parsePRCreateTitleAndBody extracts the TITLE: line and remaining body from inside
// a FABRIK_PR_CREATE_BEGIN/END block. Returns ("", "") when no TITLE: line is found.
func parsePRCreateTitleAndBody(content string) (title, body string) {
	lines := strings.Split(content, "\n")
	titleIdx := -1
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "TITLE:") {
			title = strings.TrimSpace(strings.TrimPrefix(trimmed, "TITLE:"))
			titleIdx = i
			break
		}
		// First non-empty line isn't TITLE: — malformed.
		return "", ""
	}
	if title == "" || titleIdx == -1 {
		return "", ""
	}
	bodyLines := lines[titleIdx+1:]
	body = strings.Join(bodyLines, "\n")
	return title, body
}

// processPRCreateMarker handles a parsed FABRIK_PR_CREATE block: validates the
// target repo, ensures idempotency, pushes the branch, creates the PR with the
// engine-generated "Closes #N" first line, and posts an acknowledgement comment.
//
// On success, returns (prNumber, nil). On failure, pauses the issue and returns (0, err).
func (e *Engine) processPRCreateMarker(ctx context.Context, item gh.ProjectItem, block *PRCreateBlock, owner, repo, baseBranch, repoStr string) (int, error) {
	// Cross-repo guard: v1 does not support creating PRs in a different repo.
	if block.TargetRepo != "" && block.TargetRepo != owner+"/"+repo {
		msg := fmt.Sprintf(
			"🏭 **Fabrik — PR creation failed**\n\n"+
				"The `FABRIK_PR_CREATE_BEGIN` marker specified a cross-repo target (`%s`), "+
				"but cross-repo PR creation is not supported in this version of Fabrik.\n\n"+
				"The Implement skill must not specify a `FABRIK_PR_CREATE_BEGIN owner/repo` line "+
				"pointing to a different repo. Remove the target repo from the BEGIN line to create "+
				"the PR in the same repo as the issue (`%s/%s`).\n\n"+
				"Remove `fabrik:paused` after correcting the skill output to retry.",
			block.TargetRepo, owner, repo,
		)
		e.addPausedLabelToItem(owner, repo, item)
		e.postComment(item, msg, false, false) //nolint:errcheck // failure already logged by postComment
		return 0, fmt.Errorf("cross-repo PR creation not supported (target: %s)", block.TargetRepo)
	}

	wm := e.worktreesFor(item.Repo)

	// Idempotency: if a PR already exists on this branch, use it rather than creating another.
	existingPR, err := e.client.FetchLinkedPR(owner, repo, item.Number)
	if err == nil && existingPR != nil && existingPR.State == "open" && !existingPR.Merged {
		e.logf(item.Number, "pr", "FABRIK_PR_CREATE: PR #%d already exists — using existing PR\n", existingPR.Number)
		if c := e.cache(); c != nil {
			c.RecordPRLinkage(owner+"/"+repo, existingPR.Number, item.Number)
		}
		return existingPR.Number, nil
	}

	// Push the branch before creating the PR (non-fatal: mirrors ensureDraftPR behavior).
	// Merge-queue awareness (ADR-058 D3 FR-1): skip the push when queued (ejects it).
	if err := e.pushBranchUnlessQueued(item, wm); err != nil {
		e.logf(item.Number, "warn", "could not push branch: pushing branch fabrik/issue-%d: %v\n", item.Number, err)
	}

	// Compose body: engine prepends "Closes #N\n\n" — this is the mechanized guarantee.
	closingLine := fmt.Sprintf("Closes #%d", item.Number)
	finalBody := closingLine + "\n\n" + block.Body

	head := fmt.Sprintf("fabrik/issue-%d", item.Number)
	const maxAttempts = 3
	var lastErr error
	var prNum int
	for attempt := 0; attempt < maxAttempts; attempt++ {
		prNum, lastErr = e.client.CreateDraftPR(owner, repo, block.Title, head, baseBranch, finalBody, item.Number)
		if lastErr == nil {
			break
		}
		if !isTransientError(lastErr) {
			break
		}
		if attempt < maxAttempts-1 {
			time.Sleep(ensureDraftPRRetryDelay << attempt)
		}
	}
	if lastErr != nil || prNum == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("API returned PR number 0")
		}
		msg := fmt.Sprintf(
			"🏭 **Fabrik — PR creation failed**\n\n"+
				"The engine failed to create a draft PR for issue #%d after %d attempt(s): `%v`\n\n"+
				"Remove `fabrik:paused` to retry.",
			item.Number, maxAttempts, lastErr,
		)
		e.addPausedLabelToItem(owner, repo, item)
		e.postComment(item, msg, false, false) //nolint:errcheck // failure already logged by postComment
		return 0, fmt.Errorf("creating PR via FABRIK_PR_CREATE: %w", lastErr)
	}

	e.logf(item.Number, "pr", "FABRIK_PR_CREATE: created draft PR #%d (title: %q)\n", prNum, block.Title)

	// Cache write-through.
	if c := e.cache(); c != nil {
		c.RecordPRLinkage(owner+"/"+repo, prNum, item.Number)
	}
	if e.webhookMgr != nil {
		e.webhookMgr.RegisterEcho("pull_request", "opened", fmt.Sprintf("%s/%s#pr%d", owner, repo, prNum))
	}

	// Acknowledgement comment on the issue.
	ackMsg := fmt.Sprintf("🏭 **Fabrik** — opened PR #%d", prNum)
	e.postComment(item, ackMsg, false, true) //nolint:errcheck // failure already logged by postComment

	return prNum, nil
}

// verifyAndHealLinkage verifies that the linked PR's closingIssuesReferences includes the
// parent issue after Implement completes, and attempts one auto-heal if missing.
//
// Returns true when linkage is confirmed (either already present or healed).
// Returns false when linkage cannot be established — the issue is paused before returning.
func (e *Engine) verifyAndHealLinkage(ctx context.Context, item gh.ProjectItem, prNumber int, stage *stages.Stage, owner, repo, repoStr string) bool {
	if prNumber == 0 {
		return true // no PR to verify
	}

	// Re-fetch item to get fresh closedByPullRequestsReferences data.
	if err := e.client.FetchItemDetails(&item); err != nil {
		e.logf(item.Number, "warn", "verifyAndHealLinkage: FetchItemDetails failed: %v\n", err)
		// Non-fatal: skip verification to avoid false positives on transient errors.
		return true
	}

	if item.LinkedPRNumber != 0 {
		// Linkage is present — nothing to do.
		return true
	}

	// LinkedPRNumber == 0 despite a known prNumber. Try to find the PR by branch name.
	pr, err := e.client.FetchLinkedPR(owner, repo, item.Number)
	if err != nil || pr == nil || pr.Number == 0 || pr.State != "open" || pr.Merged {
		// No active PR found via branch lookup — user has diverged, or PR is closed/merged.
		e.logf(item.Number, "warn", "verifyAndHealLinkage: no active PR found for branch fabrik/issue-%d — skipping heal\n", item.Number)
		return true
	}

	// PR exists but is not linked. Attempt auto-heal.
	prSHA := pr.HeadSHA
	closingLine := fmt.Sprintf("Closes #%d", item.Number)

	// Fetch current PR body.
	currentBody, fetchErr := e.client.GetIssueBody(owner, repo, pr.Number)
	if fetchErr != nil {
		e.logf(item.Number, "warn", "verifyAndHealLinkage: could not fetch PR #%d body: %v\n", pr.Number, fetchErr)
		e.pauseForBrokenLinkage(item, pr.Number, closingLine, "could not fetch PR body for auto-heal")
		return false
	}

	// Body-length safety (FR-015): ensure prepend doesn't overflow GitHub's limit.
	const maxBodyLen = 65300
	if len(currentBody)+len(closingLine)+2 > maxBodyLen {
		e.logf(item.Number, "warn", "verifyAndHealLinkage: PR #%d body is too long (%d chars) to prepend closing keyword\n", pr.Number, len(currentBody))
		e.pauseForBrokenLinkage(item, pr.Number, closingLine, "PR body too long for auto-heal")
		return false
	}

	// Idempotency guard: only attempt one heal per PR head SHA.
	snap, _ := e.store.Get(repoStr, item.Number)
	if snap.LinkageHealAttempted(stage.Name, prSHA) {
		e.logf(item.Number, "warn", "verifyAndHealLinkage: heal already attempted for PR #%d (SHA %s) — pausing\n", pr.Number, prSHA)
		e.pauseForBrokenLinkage(item, pr.Number, closingLine, "auto-heal was already attempted once but linkage is still missing")
		return false
	}

	// Record that we're attempting the heal.
	e.store.Apply(itemstate.LinkageHealAttempted{
		Repo:      repoStr,
		Number:    item.Number,
		StageName: stage.Name,
		PRSHA:     prSHA,
	})

	// Balance any unclosed code fences, then prepend the closing line.
	balanced := balanceFences(currentBody)
	healedBody := closingLine + "\n\n" + balanced

	// no write-through: excluded — issue body is not read from cache for dispatch decisions
	if err := e.client.UpdateIssueBody(owner, repo, pr.Number, healedBody); err != nil {
		e.logf(item.Number, "warn", "verifyAndHealLinkage: could not update PR #%d body: %v\n", pr.Number, err)
		e.pauseForBrokenLinkage(item, pr.Number, closingLine, fmt.Sprintf("UpdateIssueBody failed: %v", err))
		return false
	}
	if e.webhookMgr != nil {
		e.webhookMgr.RegisterEcho("issues", "edited", boardcache.ItemKey(owner+"/"+repo, pr.Number))
	}
	e.logf(item.Number, "pr", "verifyAndHealLinkage: prepended '%s' to PR #%d body\n", closingLine, pr.Number)

	// Re-verify using FetchItemDetails.
	if err := e.client.FetchItemDetails(&item); err != nil {
		e.logf(item.Number, "warn", "verifyAndHealLinkage: re-verification FetchItemDetails failed: %v\n", err)
		// Can't confirm — treat as success (heal likely took effect; GitHub may lag).
		healMsg := fmt.Sprintf("🏭 **Fabrik** — PR body auto-corrected: `%s` prepended (PR was opened without the closing reference). Re-verification fetch failed; please confirm linkage.", closingLine)
		e.postComment(item, healMsg, false, false) //nolint:errcheck // failure already logged by postComment
		return true
	}

	if item.LinkedPRNumber != 0 {
		e.logf(item.Number, "pr", "verifyAndHealLinkage: linkage confirmed for PR #%d\n", pr.Number)
		healMsg := fmt.Sprintf("🏭 **Fabrik** — PR body auto-corrected: `%s` prepended (PR was opened without the closing reference).", closingLine)
		e.postComment(item, healMsg, false, true) //nolint:errcheck // failure already logged by postComment
		return true
	}

	// Still not linked after heal — pause with recovery commands.
	e.logf(item.Number, "warn", "verifyAndHealLinkage: linkage still missing after heal — pausing\n")
	e.pauseForBrokenLinkage(item, pr.Number, closingLine, "auto-heal completed but GitHub still reports no closing-issue linkage")
	return false
}

// pauseForBrokenLinkage pauses the issue with fabrik:paused and posts a comment
// naming the failure mode and providing a copy-paste recovery command.
func (e *Engine) pauseForBrokenLinkage(item gh.ProjectItem, prNumber int, closingLine, reason string) {
	msg := fmt.Sprintf(
		"🏭 **Fabrik — broken PR↔issue linkage**\n\n"+
			"PR #%d exists but is not linked to issue #%d via a closing keyword (`closingIssuesReferences` is empty). "+
			"Cause: %s.\n\n"+
			"To recover, add the closing keyword to the PR body and remove `fabrik:paused`:\n\n"+
			"```bash\n"+
			"gh pr view %d --json body --jq '.body' > /tmp/pr_body.txt && "+
			"printf '%%s\\n\\n' '%s' | cat - /tmp/pr_body.txt | "+
			"gh pr edit %d --body-file -\n"+
			"```\n\n"+
			"Then remove `fabrik:paused` to resume.",
		prNumber, item.Number, reason, prNumber, closingLine, prNumber,
	)
	e.pauseIssue(item, msg, pauseOpts{
		labelEcho: true,
	})
}
