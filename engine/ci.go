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

// checkCIGate interprets the pre-fetched settle result to determine whether the
// CI gate is blocking stage advancement or merge.
//
// The gate is only active when stage.WaitForCI is true. All PR state (mergeable
// fields, check runs) is consumed from the settle parameter — no additional
// GitHub API calls are made by this function.
//
// Returns (blocked, ciFailure, timedOut):
//
//   - (false, false, false) — gate cleared; stage:X:complete added, fabrik:awaiting-ci removed.
//     This includes: no PR, PR merged, all checks green, ADR-033 shortcut (clean/unstable).
//
//   - (true, false, false)  — gate blocked but no confirmed failure; re-evaluate on next poll.
//     Covers: checks still pending, transient/unsettled state, R3 dwell not elapsed.
//     fabrik:awaiting-ci is NOT modified.
//
//   - (true, true, false)   — CI failed; fabrik:awaiting-ci applied; caller should dispatch CI-fix.
//
//   - (false, false, true)  — CI wait timeout elapsed; caller should pause the issue.
//     fabrik:awaiting-ci is removed before returning.
func (e *Engine) checkCIGate(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, settle PRSettleResult) (blocked, ciFailure, timedOut bool) {
	if stage.WaitForCI == nil || !*stage.WaitForCI {
		return false, false, false
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	pr := settle.PR

	prNum := 0
	if pr != nil {
		prNum = pr.Number
	}

	switch settle.Status {
	case PRMergeNoPR:
		e.logf(item.Number, "ci-gate", "no linked PR found; CI gate clears (no PR to check)\n")
		e.addCompleteLabelAndRemoveCI(owner, repo, item, stage)
		return false, false, false

	case PRMergeTerminal:
		// R1: merged; R2: closed without merging.
		if pr != nil && pr.Merged {
			e.logf(item.Number, "ci-gate", "linked PR #%d is merged — CI gate clears; advancing to Done\n", prNum)
			e.addCompleteLabelAndRemoveCI(owner, repo, item, stage)
		} else {
			e.logf(item.Number, "ci-gate", "linked PR #%d closed without merging — pausing\n", prNum)
			e.pauseForPRClosedNotMerged(board, item, stage, prNum)
		}
		return false, false, false

	case PRMergeReady:
		// ADR-033 shortcut (clean/unstable) or all CI checks green — gate clears.
		e.logf(item.Number, "ci-gate", "CI gate clears (%s)\n", settle.Reason)
		e.addCompleteLabelAndRemoveCI(owner, repo, item, stage)
		return false, false, false

	case PRMergeConflicting:
		// Merge gate already applied fabrik:rebase-needed; CI gate just blocks.
		return true, false, false

	case PRMergeQueued:
		// ADR-058 D4 FR-1: the PR is in GitHub's merge queue — a transient hand-off.
		// Block with no fabrik:awaiting-ci churn (mirrors the PRMergeUnsettled
		// fall-through) so the queue owns the merge decision while it waits.
		return true, false, false
	}

	// PRMergeUnsettled or PRMergeBlocked: detailed classification using settle.CheckRuns
	// and settle.MergeableState.
	checkRuns := settle.CheckRuns
	mergeableState := settle.MergeableState

	if len(checkRuns) > 0 {
		// Check runs are available: classify pending vs failed via the shared
		// helper (pending always wins over failed), then apply R7 timeout.
		status, pending, failed := gh.ClassifyCheckRuns(checkRuns)

		// R7: CIWaitTimeout applies to the full CI-await window — both pending and
		// failed checks. Under ADR-032, fabrik:awaiting-ci is present from the moment
		// handleStageComplete fires, so timeout tracking covers "checks still running"
		// as well as "checks failed".
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

		if status != gh.CheckRunsFailed {
			// Checks still running (pending takes precedence over any failed
			// run, whether a sibling check or a stale entry for the same
			// name superseded by a fresh rerun).
			names := make([]string, 0, len(pending))
			for _, cr := range pending {
				names = append(names, cr.Name)
			}
			e.logf(item.Number, "ci-gate", "CI still running — pending: %s\n", strings.Join(names, ", "))
			return true, false, false
		}

		// CI failed — apply fabrik:awaiting-ci idempotently.
		failedNames := make([]string, 0, len(failed))
		for _, cr := range failed {
			failedNames = append(failedNames, fmt.Sprintf("%s (%s)", cr.Name, cr.Conclusion))
		}
		e.logf(item.Number, "ci-gate", "CI check(s) failed: %s\n", strings.Join(failedNames, ", "))

		if !hasLabel(item, "fabrik:awaiting-ci") {
			e.applyLabelAdd(item, "fabrik:awaiting-ci", false)
		}

		return true, true, false
	}

	// No check runs. Use settle.MergeableState to discriminate R3 and
	// branch-protection signals. settle.MergeableState is intentionally empty
	// for hadChecks/dwell/HeadSHA-empty cases so those always reach the generic
	// Unsettled fallback below without triggering R3 or timeout paths.

	if mergeableState == "blocked" {
		// R3: OPEN+BLOCKED+no check runs ever observed — a required check is
		// configured but never triggered by PR events. Use CIWaitTimeout as a
		// false-positive guard.
		timeout := e.cfg.CIWaitTimeout
		if timeout <= 0 {
			timeout = 30 * time.Minute
		}
		if hasLabel(item, "fabrik:awaiting-ci") {
			appliedAt, err := e.client.FetchLabelAppliedAt(owner, repo, item.Number, "fabrik:awaiting-ci")
			if err != nil {
				e.logf(item.Number, "warn", "R3: could not fetch awaiting-ci label timestamp: %v\n", err)
			} else if !appliedAt.IsZero() && time.Since(appliedAt) >= timeout {
				e.logf(item.Number, "ci-gate", "R3: PR #%d OPEN+BLOCKED with no check runs ever — required check likely never triggers on PRs; pausing\n", prNum)
				e.pauseForRequiredNeverRunningCheck(board, item, stage, prNum)
				return false, false, false
			}
		}
		e.logf(item.Number, "ci-gate", "R3: PR #%d OPEN+BLOCKED with no check runs — dwell not yet elapsed; waiting\n", prNum)
		return true, false, false
	}

	if mergeableState != "" && mergeableState != "unknown" {
		// Branch-protection blocking via a signal Fabrik cannot see via check_runs
		// (e.g. Commit Status / legacy Statuses API). Apply CIWaitTimeout.
		timeout := e.cfg.CIWaitTimeout
		if timeout <= 0 {
			timeout = 30 * time.Minute
		}
		if hasLabel(item, "fabrik:awaiting-ci") {
			appliedAt, err := e.client.FetchLabelAppliedAt(owner, repo, item.Number, "fabrik:awaiting-ci")
			if err != nil {
				e.logf(item.Number, "warn", "could not fetch awaiting-ci label timestamp: %v\n", err)
			} else if !appliedAt.IsZero() && time.Since(appliedAt) >= timeout {
				e.logf(item.Number, "warn", "CI wait timeout elapsed for mergeable_state=%q with no check_runs — pausing issue\n", mergeableState)
				e.removeAwaitingCILabel(owner, repo, item)
				return false, false, true
			}
		}
		e.logf(item.Number, "ci-gate", "mergeable_state=%q blocks merge but no check_runs visible — branch protection likely requires a Commit Status or external signal; blocking\n", mergeableState)
		return true, false, false
	}

	// Generic Unsettled: hadChecks/dwell/HeadSHA-empty/mergeable=nil/unknown.
	// Block and re-evaluate on next poll.
	return true, false, false
}

// removeAwaitingCILabel removes fabrik:awaiting-ci if present on the item.
func (e *Engine) removeAwaitingCILabel(owner, repo string, item gh.ProjectItem) {
	for _, l := range item.Labels {
		if l == "fabrik:awaiting-ci" {
			e.applyLabelRemove(item, "fabrik:awaiting-ci", false)
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
	} else if c := e.cache(); c != nil {
		c.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), completeLabel)
	}
	if stage.Name == "Validate" {
		repoStr := owner + "/" + repo
		if snap, snapErr := e.store.Get(repoStr, item.Number); snapErr == nil {
			if lpr := snap.LinkedPR(); lpr != nil && lpr.HeadSHA != "" {
				e.store.Apply(itemstate.ValidateCompletedAtSHA{Repo: repoStr, Number: item.Number, SHA: lpr.HeadSHA})
				e.logf(item.Number, "validate-sha", "recorded CI-completion SHA %s\n", lpr.HeadSHA)
			}
		}
	}
	e.removeAwaitingCILabel(owner, repo, item)
}

// buildCIFixComment constructs the synthetic comment body for a CI-fix reinvocation.
// It uses PR check runs from the settle result and fetches base branch CI status for
// comparison. The base-branch fetch (different SHA) remains a direct API call.
func (e *Engine) buildCIFixComment(item gh.ProjectItem, stage *stages.Stage, workDir string, settle PRSettleResult) gh.Comment {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Use PR-head check runs already fetched by settlePRMergeState.
	prFailures := settle.CheckRuns
	var baseRuns []gh.CheckRun
	var baseBranch string

	// Fetch base branch check runs for comparison (different SHA — not covered by settle).
	wm := e.worktreesFor(item.Repo)
	bb, err := e.baseBranchForItem(item, wm)
	if err == nil {
		baseBranch = bb
		if baseSHA, err := gitRevParse(workDir, "origin/"+baseBranch); err == nil && baseSHA != "" {
			baseRuns, _ = e.readClient.FetchCheckRuns(owner, repo, baseSHA)
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

// dispatchCIFixReinvoke re-invokes the stage agent via processComments with a
// synthetic CI-failure context comment. A thin wrapper over dispatchReinvoke,
// supplying only the CI-fix-specific comment builder (which also snapshots
// HEAD before the reinvoke), the CIFixSkill stage variant, and the post-run
// no-op-SHA recording — the shared goroutine scaffold lives in reinvoke.go.
func (e *Engine) dispatchCIFixReinvoke(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, settle PRSettleResult) {
	itemRepo := itemOwnerRepoString(item, e.defaultRepo())
	var headBefore string

	e.dispatchReinvoke(ctx, board, item, stage, reinvokeOpts{
		tag: "ci-fix-reinvoke",
		build: func(workDir string) []gh.Comment {
			// Snapshot HEAD before reinvoking so a no-op reinvoke (nothing to
			// push because the fix is already in) can be recorded and debounced
			// on the next poll instead of burning further CI-fix cycle budget
			// while the current head's CI is still resolving (#958 leg 2).
			headBefore, _ = gitHeadSHA(workDir)
			return []gh.Comment{e.buildCIFixComment(item, stage, workDir, settle)}
		},
		stageVariant: func(s *stages.Stage) *stages.Stage {
			// Use ci_fix_skill if configured; fall back to comment_skill.
			if s.CIFixSkill == "" {
				return s
			}
			variant := *s
			variant.CommentSkill = s.CIFixSkill
			variant.CommentPrompt = ""
			return &variant
		},
		after: func(workDir string, err error) {
			// Only record a no-op when the reinvoke actually completed: a failed
			// processComments (transient network issue, rate limit, workspace
			// lock) also leaves HEAD unchanged, but recording a no-op for that
			// case would wrongly debounce a retry that never got a chance to push
			// a real fix.
			if err != nil {
				return
			}
			if headAfter, hErr := gitHeadSHA(workDir); hErr == nil && headBefore != "" && headAfter == headBefore {
				e.logf(item.Number, "ci-fix-reinvoke", "no new commit pushed (HEAD still %s) — recording no-op for this head\n",
					headAfter[:min(8, len(headAfter))])
				e.store.Apply(itemstate.CIFixNoOpRecorded{Repo: itemRepo, Number: item.Number, SHA: headAfter})
			}
		},
	})
}

// pauseForCITimeout pauses the issue when the CI wait timeout in the catch-up
// loop elapses. It posts an explanatory comment and applies fabrik:paused +
// fabrik:awaiting-input.
func (e *Engine) pauseForCITimeout(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "ci-timeout", "CI wait timeout elapsed — pausing for human intervention\n")

	msg := fmt.Sprintf(
		"🏭 **Fabrik — CI wait timeout**\n\nThe CI gate for stage **%s** timed out waiting for checks to pass.\n\n"+
			"Fabrik has paused this issue. Please check the PR's CI status, address any failures, and then remove the `fabrik:paused` label to resume.",
		stage.Name,
	)
	e.pauseIssue(item, msg, pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})
}

// pauseForCIFixCycleLimit pauses the issue when the maximum CI-fix
// re-invocation cycle count is reached.
func (e *Engine) pauseForCIFixCycleLimit(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, cycleCount, maxCycles int) {
	e.logf(item.Number, "ci-cycles", "CI-fix cycle limit %d reached — pausing for human intervention\n", maxCycles)

	msg := fmt.Sprintf(
		"🏭 **Fabrik — CI fix cycle limit reached**\n\nThe stage **%s** has been re-invoked to fix CI failures %d time(s), "+
			"which has reached the maximum configured limit (`FABRIK_MAX_CI_FIX_CYCLES=%d`).\n\n"+
			"CI checks are still failing after repeated fix attempts. "+
			"Fabrik has paused this issue for human review. Once the CI situation is resolved, "+
			"remove the `fabrik:paused` label to resume.",
		stage.Name, cycleCount, maxCycles,
	)
	e.pauseIssue(item, msg, pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})
}

// pauseForPRClosedNotMerged pauses the issue when the linked PR was closed
// without merging (R2). Posts an explanatory comment naming the PR, applies
// fabrik:paused + fabrik:awaiting-input, and removes fabrik:awaiting-ci.
func (e *Engine) pauseForPRClosedNotMerged(_ *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, prNumber int) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	e.logf(item.Number, "ci-gate", "PR #%d closed without merging — pausing for human intervention\n", prNumber)

	msg := fmt.Sprintf(
		"🏭 **Fabrik — PR closed without merging**\n\n"+
			"The linked PR #%d was closed without being merged while Fabrik was waiting for CI to pass on stage **%s**.\n\n"+
			"Fabrik has paused this issue. To resume:\n"+
			"- Reopen the PR (or create a new one) and remove the `fabrik:paused` label, or\n"+
			"- Close this issue if the work is no longer needed.",
		prNumber, stage.Name,
	)
	e.pauseIssue(item, msg, pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})
	e.removeAwaitingCILabel(owner, repo, item)
}

// pauseForRequiredNeverRunningCheck pauses the issue when the linked PR is
// OPEN with mergeable_state=blocked and no check runs have ever been observed
// for it (R3). This indicates a required check that is configured in branch
// protection but never triggered by PR events (e.g. converted to workflow_dispatch).
// Posts a distinct comment naming the PR, applies fabrik:paused +
// fabrik:awaiting-input, and removes fabrik:awaiting-ci.
func (e *Engine) pauseForRequiredNeverRunningCheck(_ *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, prNumber int) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	e.logf(item.Number, "ci-gate", "R3: required check never triggers on PR #%d — pausing for human intervention\n", prNumber)

	msg := fmt.Sprintf(
		"🏭 **Fabrik — required check never runs on PR**\n\n"+
			"PR #%d is blocked (`mergeable_state: BLOCKED`) but no CI check runs have ever been observed for this PR's HEAD SHA. "+
			"This typically means a required check is configured in branch protection but is not triggered by pull request events "+
			"(for example, it may have been converted to a `workflow_dispatch` trigger).\n\n"+
			"Fabrik has paused this issue after waiting for stage **%s** to complete. To resume:\n"+
			"- Run the required check manually (e.g. via `workflow_dispatch`) and remove the `fabrik:paused` label once CI passes, or\n"+
			"- Remove the check from the branch protection required-status list if it should no longer be required.",
		prNumber, stage.Name,
	)
	e.pauseIssue(item, msg, pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})
	e.removeAwaitingCILabel(owner, repo, item)
}
