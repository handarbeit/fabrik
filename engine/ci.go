package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
	"github.com/verveguy/fabrik/tui"
)

// checkCIGate inspects CI check runs for the linked PR to determine whether the
// CI gate is blocking stage advancement or merge.
//
// The gate is only active when stage.WaitForCI is true. It fetches the PR's
// head SHA via FetchLinkedPR (REST), then fetches check runs via FetchCheckRuns.
//
// Returns (blocked, ciFailure, timedOut):
//
//   - (false, false, false) — gate cleared; stage:X:complete added, fabrik:awaiting-ci removed.
//     This includes: no PR found, no check runs when prHasHadChecks is false (R5 — no CI configured), all checks green.
//
//   - (true, false, false)  — gate blocked but no confirmed failure; re-evaluate on next poll.
//     Covers: checks still pending (in_progress/queued) and not yet timed out;
//     transient API errors (FetchLinkedPR or FetchCheckRuns fail);
//     and post-push registration delay — no check runs for new SHA but prHasHadChecks is true (R5).
//     fabrik:awaiting-ci is NOT modified.
//
//   - (true, true, false)   — CI failed; fabrik:awaiting-ci applied; caller should dispatch CI-fix.
//
//   - (false, false, true)  — CI wait timeout elapsed; caller should pause the issue.
//     fabrik:awaiting-ci is removed before returning.
func (e *Engine) checkCIGate(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) (blocked, ciFailure, timedOut bool) {
	if stage.WaitForCI == nil || !*stage.WaitForCI {
		return false, false, false
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	key := issueKey(item, e.defaultRepo())

	pr, err := e.client.FetchLinkedPR(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "ci-gate", "could not fetch linked PR: %v — blocking until API recovers\n", err)
		return true, false, false // transient error; retry on next poll
	}
	if pr == nil || pr.HeadSHA == "" {
		e.logf(item.Number, "ci-gate", "no linked PR found; CI gate clears (no PR to check)\n")
		e.addCompleteLabelAndRemoveCI(owner, repo, item, stage)
		return false, false, false
	}

	// Trust GitHub's branch-protection-aware mergeable_state when positive:
	// "clean" = ready to merge per branch protection; "unstable" = non-required
	// checks failing but still mergeable. Skip the raw check_runs gate in
	// those cases — that gate is over-aggressive (any check_run conclusion
	// in {failure, timed_out, action_required} blocks, including non-required
	// workflow jobs like "Cleanup artifacts" that GitHub itself does not treat
	// as merge blockers). Falls through to per-check classification when
	// mergeable_state is empty/unknown/blocked/etc.
	if mergeableState, msErr := e.client.FetchPRMergeableState(owner, repo, pr.Number); msErr != nil {
		e.logf(item.Number, "warn", "could not fetch mergeable_state: %v — falling back to check-runs gate\n", msErr)
	} else if gh.MergeableStateAccepted(mergeableState) {
		e.logf(item.Number, "ci-gate", "mergeable_state=%q — gate clears (skipping check_runs classification)\n", mergeableState)
		e.addCompleteLabelAndRemoveCI(owner, repo, item, stage)
		return false, false, false
	}

	checkRuns, err := e.client.FetchCheckRuns(owner, repo, pr.HeadSHA)
	if err != nil {
		e.logf(item.Number, "ci-gate", "could not fetch check runs: %v — blocking until API recovers\n", err)
		return true, false, false // transient error; retry on next poll
	}

	if len(checkRuns) > 0 {
		e.mu.Lock()
		e.prHasHadChecks[key] = true
		e.mu.Unlock()
	}

	// R5: no check runs found. If this PR has had checks before, we're likely in
	// the post-push registration window — block and wait rather than clearing.
	if len(checkRuns) == 0 {
		e.mu.Lock()
		hadChecks := e.prHasHadChecks[key]
		e.mu.Unlock()
		if hadChecks {
			e.logf(item.Number, "ci-gate", "no check runs for SHA %s — likely post-push registration delay; waiting\n", pr.HeadSHA[:min(8, len(pr.HeadSHA))])
			return true, false, false
		}
		e.logf(item.Number, "ci-gate", "no check runs found for SHA %s; CI gate clears (no CI configured)\n", pr.HeadSHA[:min(8, len(pr.HeadSHA))])
		e.addCompleteLabelAndRemoveCI(owner, repo, item, stage)
		return false, false, false
	}

	// Classify check runs.
	var pending, failed []gh.CheckRun
	for _, cr := range checkRuns {
		switch cr.Status {
		case "queued", "in_progress":
			pending = append(pending, cr)
		case "completed":
			switch cr.Conclusion {
			case "failure", "timed_out", "action_required":
				failed = append(failed, cr)
			}
		}
	}

	// All checks green (no pending, no failed) — gate clears.
	if len(pending) == 0 && len(failed) == 0 {
		e.logf(item.Number, "ci-gate", "all CI checks passed for SHA %s\n", pr.HeadSHA[:min(8, len(pr.HeadSHA))])
		e.addCompleteLabelAndRemoveCI(owner, repo, item, stage)
		return false, false, false
	}

	// CIWaitTimeout applies to the full CI-await window — both pending and failed
	// checks (R7). Under ADR 032, fabrik:awaiting-ci is present from the moment
	// handleStageComplete fires, so timeout tracking covers "checks still running"
	// as well as "checks failed". Without this, CI stuck in queued/in_progress
	// indefinitely would never time out and pause the issue.
	if hasLabel(item, "fabrik:awaiting-ci") {
		timeout := e.cfg.CIWaitTimeout
		if timeout <= 0 {
			timeout = 30 * time.Minute
		}
		appliedAt, err := e.client.FetchLabelAppliedAt(owner, repo, item.Number, "fabrik:awaiting-ci")
		if err != nil {
			e.logf(item.Number, "warn", "could not fetch awaiting-ci label timestamp: %v\n", err)
		} else if !appliedAt.IsZero() && time.Since(appliedAt) >= timeout {
			allNames := make([]string, 0, len(pending)+len(failed))
			for _, cr := range pending {
				allNames = append(allNames, cr.Name+" (pending)")
			}
			for _, cr := range failed {
				allNames = append(allNames, fmt.Sprintf("%s (%s)", cr.Name, cr.Conclusion))
			}
			e.logf(item.Number, "warn", "CI wait timeout elapsed; pausing issue — checks: %s\n", strings.Join(allNames, ", "))
			e.removeAwaitingCILabel(owner, repo, item)
			return false, false, true
		}
	}

	// Checks still running — gate blocked; fabrik:awaiting-ci already present
	// from handleStageComplete.
	if len(failed) == 0 {
		names := make([]string, 0, len(pending))
		for _, cr := range pending {
			names = append(names, cr.Name)
		}
		e.logf(item.Number, "ci-gate", "CI still running — pending: %s\n", strings.Join(names, ", "))
		return true, false, false
	}

	// CI failed — apply fabrik:awaiting-ci idempotently and return blocked.
	failedNames := make([]string, 0, len(failed))
	for _, cr := range failed {
		failedNames = append(failedNames, fmt.Sprintf("%s (%s)", cr.Name, cr.Conclusion))
	}
	e.logf(item.Number, "ci-gate", "CI check(s) failed: %s\n", strings.Join(failedNames, ", "))

	if !hasLabel(item, "fabrik:awaiting-ci") {
		if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-ci"); err != nil {
			e.logf(item.Number, "warn", "could not add fabrik:awaiting-ci label: %v\n", err)
		}
	}

	return true, true, false
}

// removeAwaitingCILabel removes fabrik:awaiting-ci if present on the item.
func (e *Engine) removeAwaitingCILabel(owner, repo string, item gh.ProjectItem) {
	for _, l := range item.Labels {
		if l == "fabrik:awaiting-ci" {
			if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:awaiting-ci"); err != nil {
				e.logf(item.Number, "warn", "could not remove fabrik:awaiting-ci label: %v\n", err)
			}
			return
		}
	}
}

// addCompleteLabelAndRemoveCI adds stage:X:complete and, only after that succeeds,
// removes fabrik:awaiting-ci when the CI gate clears. If adding the completion label
// fails, fabrik:awaiting-ci is preserved so the next poll cycle retries (R3 — the
// in-flight marker must not be dropped while CI is still being gated).
func (e *Engine) addCompleteLabelAndRemoveCI(owner, repo string, item gh.ProjectItem, stage *stages.Stage) {
	completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); err != nil {
		e.logf(item.Number, "warn", "could not add completion label %s: %v\n", completeLabel, err)
		return // preserve fabrik:awaiting-ci so the next poll retries
	}
	e.removeAwaitingCILabel(owner, repo, item)
}

// buildCIFixComment constructs the synthetic comment body for a CI-fix reinvocation.
// It fetches current PR CI status, fetches base branch CI status for comparison,
// and formats both into a structured message Claude can use to diagnose failures.
func (e *Engine) buildCIFixComment(item gh.ProjectItem, stage *stages.Stage, workDir string) gh.Comment {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	var prFailures, baseRuns []gh.CheckRun
	var baseBranch string

	// Fetch PR check runs.
	pr, err := e.client.FetchLinkedPR(owner, repo, item.Number)
	if err == nil && pr != nil && pr.HeadSHA != "" {
		prFailures, _ = e.client.FetchCheckRuns(owner, repo, pr.HeadSHA)
	}

	// Fetch base branch check runs for comparison.
	wm := e.worktreesFor(item.Repo)
	bb, err := e.baseBranchForItem(item, wm)
	if err == nil {
		baseBranch = bb
		if baseSHA, err := gitRevParse(workDir, "origin/"+baseBranch); err == nil && baseSHA != "" {
			baseRuns, _ = e.client.FetchCheckRuns(owner, repo, baseSHA)
		}
	}

	// Classify PR failures.
	var failedLines []string
	baseFailedNames := make(map[string]bool)
	for _, cr := range baseRuns {
		if cr.Status == "completed" {
			switch cr.Conclusion {
			case "failure", "timed_out", "action_required":
				baseFailedNames[cr.Name] = true
			}
		}
	}
	for _, cr := range prFailures {
		if cr.Status == "completed" {
			switch cr.Conclusion {
			case "failure", "timed_out", "action_required":
				note := "NEW REGRESSION"
				if baseFailedNames[cr.Name] {
					note = "pre-existing (also fails on base branch)"
				}
				failedLines = append(failedLines, fmt.Sprintf("- **%s**: %s [%s]", cr.Name, cr.Conclusion, note))
			}
		}
	}

	// Format base branch status.
	var baseLines []string
	for _, cr := range baseRuns {
		if cr.Status == "completed" {
			baseLines = append(baseLines, fmt.Sprintf("- %s: %s", cr.Name, cr.Conclusion))
		}
	}

	branchName := fmt.Sprintf("fabrik/issue-%d", item.Number)
	var sb strings.Builder
	sb.WriteString("🏭 **Fabrik — CI Fix Required**\n\n")
	sb.WriteString(fmt.Sprintf("The following CI check runs failed for this PR (branch: `%s`):\n\n", branchName))

	if len(failedLines) > 0 {
		sb.WriteString("**Failed checks on PR branch:**\n")
		for _, l := range failedLines {
			sb.WriteString(l + "\n")
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("*(Could not determine specific failed checks — check GitHub Actions for details)*\n\n")
	}

	if len(baseLines) > 0 && baseBranch != "" {
		sb.WriteString(fmt.Sprintf("**Base branch (`%s`) check run status:**\n", baseBranch))
		for _, l := range baseLines {
			sb.WriteString(l + "\n")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("**Instructions:**\n")
	sb.WriteString("1. Checks marked **NEW REGRESSION** were introduced by this PR — fix these.\n")
	sb.WriteString("2. Checks marked **pre-existing** also fail on the base branch — note them but do NOT attempt to fix them.\n")
	sb.WriteString(fmt.Sprintf("3. To investigate failure logs: `gh run list --branch %s --limit 5` then `gh run view <run-id> --log-failed`\n", branchName))
	sb.WriteString("4. After fixing, commit and push. The engine will re-evaluate CI on the next poll cycle.\n")
	sb.WriteString(fmt.Sprintf("5. Do not signal `FABRIK_STAGE_COMPLETE` — the engine will advance once CI passes.\n"))

	return gh.Comment{
		ID:         "ci-fix-synthetic",
		DatabaseID: 0, // synthetic — no GitHub comment to react to
		Body:       sb.String(),
		Author:     "fabrik",
	}
}

// dispatchCIFixReinvoke spawns a goroutine to re-invoke the stage agent via
// processComments with a synthetic CI-failure context comment. It mirrors
// dispatchReviewReinvoke exactly: marks inFlight, acquires semaphore, calls
// processComments, then releases both.
func (e *Engine) dispatchCIFixReinvoke(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	iKey := issueKey(item, e.defaultRepo())

	// Mark in-flight to prevent the next poll cycle from double-dispatching.
	e.inFlight.Store(iKey, item.IsPR)
	e.wg.Add(1)

	itemRepo := itemOwnerRepoString(item, e.defaultRepo())

	go func() {
		defer e.wg.Done()
		defer e.inFlight.Delete(iKey)

		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			e.logf(item.Number, "ci-fix-reinvoke", "context cancelled before semaphore acquired\n")
			return
		}
		defer func() { <-e.sem }()

		if err := e.ensureRepoReady(ctx, item); err != nil {
			if errors.Is(err, ErrSkipItem) {
				e.logf(item.Number, "ci-fix-reinvoke", "repo not ready, skipping reinvoke\n")
				return
			}
			e.logf(item.Number, "warn", "ci-fix reinvoke: ensureRepoReady failed: %v\n", err)
			return
		}

		// Get worktree path for base branch CI comparison.
		wm := e.worktreesFor(item.Repo)
		workDir := wm.WorktreeDir(item.Number)

		// Build the synthetic comment with CI failure context.
		syntheticComment := e.buildCIFixComment(item, stage, workDir)

		// Use ci_fix_skill if configured; fall back to comment_skill.
		ciFixStage := *stage
		if stage.CIFixSkill != "" {
			ciFixStage.CommentSkill = stage.CIFixSkill
			ciFixStage.CommentPrompt = ""
		}

		startTime := time.Now()
		e.emitStructural(tui.JobStartedEvent{
			IssueNumber: item.Number,
			Repo:        itemRepo,
			Title:       item.Title,
			StageName:   stage.Name,
			IsComment:   true,
			StartedAt:   startTime,
		})

		e.logf(item.Number, "ci-fix-reinvoke", "re-invoking stage %q via comment processing with CI failure context\n", stage.Name)
		err := e.processComments(ctx, board, item, &ciFixStage, []gh.Comment{syntheticComment})

		e.mu.Lock()
		usage := e.lastUsage[iKey]
		completed := e.lastCompleted[iKey]
		blocked := e.lastBlocked[iKey]
		delete(e.lastUsage, iKey)
		delete(e.lastCompleted, iKey)
		delete(e.lastBlocked, iKey)
		e.mu.Unlock()
		e.emitStructural(tui.JobCompletedEvent{
			IssueNumber:    item.Number,
			Repo:           itemRepo,
			Title:          item.Title,
			StageName:      stage.Name,
			StageModel:     stage.Model,
			IsComment:      true,
			Success:        err == nil,
			Completed:      completed,
			BlockedOnInput: blocked,
			Duration:       time.Since(startTime),
			CompletedAt:    time.Now(),
			TurnsUsed:      usage.TurnsUsed,
			MaxTurns:       usage.MaxTurns,
			CostUSD:        usage.CostUSD,
		})

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.logf(item.Number, "warn", "CI-fix re-invocation failed: %v\n", err)
		}
	}()
}

// pauseForCITimeout pauses the issue when the CI wait timeout in the catch-up
// loop elapses. It posts an explanatory comment and applies fabrik:paused +
// fabrik:awaiting-input.
func (e *Engine) pauseForCITimeout(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	e.logf(item.Number, "ci-timeout", "CI wait timeout elapsed — pausing for human intervention\n")

	msg := fmt.Sprintf(
		"🏭 **Fabrik — CI wait timeout**\n\nThe CI gate for stage **%s** timed out waiting for checks to pass.\n\n"+
			"Fabrik has paused this issue. Please check the PR's CI status, address any failures, and then remove the `fabrik:paused` label to resume.",
		stage.Name,
	)
	if dbID, err := e.client.AddComment(owner, repo, item.Number, msg); err != nil {
		e.logf(item.Number, "warn", "could not post CI timeout comment: %v\n", err)
	} else if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
		e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:paused: %v\n", err)
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:awaiting-input: %v\n", err)
	}
}

// pauseForCIFixCycleLimit pauses the issue when the maximum CI-fix
// re-invocation cycle count is reached.
func (e *Engine) pauseForCIFixCycleLimit(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, cycleCount, maxCycles int) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	e.logf(item.Number, "ci-cycles", "CI-fix cycle limit %d reached — pausing for human intervention\n", maxCycles)

	msg := fmt.Sprintf(
		"🏭 **Fabrik — CI fix cycle limit reached**\n\nThe stage **%s** has been re-invoked to fix CI failures %d time(s), "+
			"which has reached the maximum configured limit (`FABRIK_MAX_CI_FIX_CYCLES=%d`).\n\n"+
			"CI checks are still failing after repeated fix attempts. "+
			"Fabrik has paused this issue for human review. Once the CI situation is resolved, "+
			"remove the `fabrik:paused` label to resume.",
		stage.Name, cycleCount, maxCycles,
	)
	if dbID, err := e.client.AddComment(owner, repo, item.Number, msg); err != nil {
		e.logf(item.Number, "warn", "could not post CI cycle limit comment: %v\n", err)
	} else if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
		e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:paused: %v\n", err)
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:awaiting-input: %v\n", err)
	}
}

