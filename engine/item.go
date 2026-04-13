package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// lockVerifyDelay is the time to wait after acquiring a lock before re-fetching
// labels to detect competing locks from other Fabrik instances. Declared as a
// var (not const) so tests can set it to 0 without a 2-second sleep per test.
var lockVerifyDelay = 2 * time.Second

// isEngineManagedPath returns true for paths that are written by the Fabrik
// engine itself and should never be treated as user-generated dirty content.
// These paths must not block cleanup or worktree updates.
func isEngineManagedPath(path string) bool {
	return strings.HasPrefix(path, ".fabrik-context/")
}

// isAwaitingInput returns true iff the item has both fabrik:paused and
// fabrik:awaiting-input labels, indicating it was paused waiting for user input
// (as opposed to a failure-escalation pause).
func isAwaitingInput(item gh.ProjectItem) bool {
	var hasPaused, hasAwaitingInput bool
	for _, label := range item.Labels {
		if label == "fabrik:paused" {
			hasPaused = true
		}
		if label == "fabrik:awaiting-input" {
			hasAwaitingInput = true
		}
	}
	return hasPaused && hasAwaitingInput
}

// itemMayNeedWork does cheap pre-checks using only shallow board data (no comments).
// Items that pass this filter will have their details fetched via FetchItemDetails
// before the full itemNeedsWork check. This avoids expensive deep fetches for items
// that can be ruled out by status, labels, or updatedAt alone.
func (e *Engine) itemMayNeedWork(item gh.ProjectItem) bool {
	// No matching stage = nothing to do
	stage := stages.FindStage(e.cfg.Stages, item.Status)

	// Closed issues are skipped unless the current stage is a cleanup stage —
	// cleanup stages must run after an issue closes to remove the worktree.
	if item.IsClosed && (stage == nil || !stage.CleanupWorktree) {
		return false
	}

	if stage == nil {
		return false
	}

	// Cleanup stages bypass the updatedAt cache — their trigger is worktree
	// existence (a local filesystem check), not issue/PR changes. Board column
	// moves (Validate→Done by a human) don't always bump updatedAt, so cleanup
	// items would be permanently skipped if subject to the cache. The cost is
	// minimal: a local Stat call, no GraphQL impact. Once cleanup runs and
	// removes the worktree, subsequent polls see no worktree and return false.
	if stage.CleanupWorktree {
		key := item.Repo
		if key == "" {
			key = e.defaultRepo()
		}
		e.mu.Lock()
		wm, ok := e.worktreeManagers[key]
		e.mu.Unlock()
		if ok {
			if _, err := os.Stat(wm.WorktreeDir(item.Number)); os.IsNotExist(err) {
				return false
			}
			return true
		}
		// No WM registered yet (e.g. after restart when only cleanup items remain).
		// Fall back to checking the filesystem path directly.
		owner, repo := parseOwnerRepo(key)
		dirName := owner + "-" + repo
		wtDir := filepath.Join(e.fabrikDir, ".fabrik", "worktrees", dirName, fmt.Sprintf("issue-%d", item.Number))
		if _, err := os.Stat(wtDir); os.IsNotExist(err) {
			return false
		}
		return true
	}

	// Skip items that haven't changed since last poll — unless in cooldown retry.
	if !item.UpdatedAt.IsZero() {
		iKey := issueKey(item, e.defaultRepo())
		e.mu.Lock()
		lastSeen, seen := e.lastUpdatedAt[iKey]
		e.mu.Unlock()
		if seen && !item.UpdatedAt.After(lastSeen) {
			// Item unchanged — but still allow cooldown retries
			stageKey := iKey + "-" + stage.Name
			e.mu.Lock()
			lastAttempt, attempted := e.processedSet[stageKey]
			e.mu.Unlock()
			if attempted {
				cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
				if time.Since(lastAttempt) >= cooldown {
					return true // cooldown expired, retry
				}
			}
			// Force deep-fetch for items whose gate condition changes independently
			// of the issue's updatedAt:
			// - fabrik:blocked: a blocking issue may close without updating this issue's updatedAt
			// Note: fabrik:awaiting-input and fabrik:awaiting-review do NOT need forced
			// deep-fetch — adding a comment bumps the issue's updatedAt, and submitting
			// a PR review bumps the linked PR's updatedAt (which Fabrik tracks in the
			// shallow query via closedByPullRequestsReferences).
			for _, l := range item.Labels {
				if l == "fabrik:blocked" {
					return true
				}
			}
			return false
		}
	}

	// Don't check labels or blockedBy here — those require full label data which
	// is only available after deep fetch. Label/lock/dep-gate checks are in
	// itemNeedsWork, which runs after FetchItemDetails populates the full label set.

	// Apply a cooldown for items whose last FetchItemDetails call failed.
	// Without this, a persistent failure (e.g. deleted issue, permission error)
	// would cause an API call on every poll cycle. The cooldown matches the
	// processedSet cooldown used for failed stage retries.
	iKey := issueKey(item, e.defaultRepo())
	e.mu.Lock()
	lastFailure, hadFailure := e.deepFetchFailureTime[iKey]
	e.mu.Unlock()
	if hadFailure {
		cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
		if time.Since(lastFailure) < cooldown {
			return false
		}
	}

	return true
}

// itemNeedsWork does full checks including comment inspection.
// This runs AFTER FetchItemDetails has populated the item's Comments.
func (e *Engine) itemNeedsWork(item gh.ProjectItem) bool {
	stage := stages.FindStage(e.cfg.Stages, item.Status)

	// Closed issues are skipped unless the current stage is a cleanup stage —
	// cleanup stages must run after an issue closes to remove the worktree.
	if item.IsClosed && (stage == nil || !stage.CleanupWorktree) {
		return false
	}

	if stage == nil {
		return false
	}

	// Cleanup stages bypass comment processing and cooldown checks.
	if stage.CleanupWorktree {
		for _, label := range item.Labels {
			if label == "fabrik:paused" {
				return false
			}
		}
		completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
		for _, label := range item.Labels {
			if label == completeLabel {
				return false
			}
		}
		// If no worktree directory exists, there is nothing to clean up.
		// Use a direct map lookup rather than worktreesFor() to avoid a panic —
		// worktreesFor panics if no WorktreeManager is registered, which can happen
		// in multi-repo mode before ensureRepoReady has run for this repo.
		key := item.Repo
		if key == "" {
			key = e.defaultRepo()
		}
		e.mu.Lock()
		wm, ok := e.worktreeManagers[key]
		e.mu.Unlock()
		if !ok {
			return false
		}
		if _, err := os.Stat(wm.WorktreeDir(item.Number)); os.IsNotExist(err) {
			return false
		}
		return true
	}

	// Awaiting-input items (paused + awaiting-input) bypass the paused guard but
	// still respect the lock — items locked by another user must not be processed
	// by this instance even when awaiting input.
	awaitingInput := isAwaitingInput(item)

	// Items locked by another user are not our work — checked before the
	// awaiting-input early return so locks are always respected.
	lockLabel := fmt.Sprintf("fabrik:locked:%s", e.cfg.User)
	otherLockPrefix := "fabrik:locked:"
	for _, label := range item.Labels {
		if strings.HasPrefix(label, otherLockPrefix) && label != lockLabel {
			return false
		}
	}

	// Awaiting-input items: new comment = resume trigger; no comment = skip.
	if awaitingInput {
		return len(e.findNewComments(item)) > 0
	}

	// Paused items: a new user comment is an implicit "resume and handle this."
	// Without a comment, respect the pause.
	isPaused := false
	for _, label := range item.Labels {
		if label == "fabrik:paused" {
			isPaused = true
			break
		}
	}
	newComments := e.findNewComments(item)
	if isPaused {
		if len(newComments) > 0 {
			return true // comment triggers unpause — processItem handles label removal
		}
		return false
	}

	// New comments are always worth processing (even on completed stages)
	if len(newComments) > 0 {
		return true
	}

	// Dependency gate: if past the first stage and has open blockers, skip this
	// item. blockedBy is populated by FetchItemDetails (deep fetch) at this point.
	// Items blocked by open issues are gated by external state (dependency closure)
	// not user comments, so they are safe to defer.
	if !stages.IsFirstStage(e.cfg.Stages, stage.Name) {
		for _, dep := range item.BlockedBy {
			if dep.State != "CLOSED" {
				return false
			}
		}
	}

	// PRs only support comment processing
	if item.IsPR {
		return false
	}

	// Already completed this stage
	completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
	for _, label := range item.Labels {
		if label == completeLabel {
			return false
		}
	}

	// Check cooldown
	iKey := issueKey(item, e.defaultRepo())
	stageKey := iKey + "-" + stage.Name
	e.mu.Lock()
	lastAttempt, attempted := e.processedSet[stageKey]
	e.mu.Unlock()
	if attempted {
		cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
		if time.Since(lastAttempt) < cooldown {
			return false
		}
	}

	return true
}

func (e *Engine) processItem(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem) error {
	// Find the stage config for this item's current status
	stage := stages.FindStage(e.cfg.Stages, item.Status)
	if stage == nil {
		return nil
	}

	// Ensure the repo's WorktreeManager is registered; bare-clones on first access.
	if err := e.ensureRepoReady(ctx, item); err != nil {
		if errors.Is(err, ErrSkipItem) {
			return nil
		}
		return err
	}

	// Derive per-issue owner/repo for all API calls.
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	// Unique key for this issue across all repos.
	iKey := issueKey(item, e.defaultRepo())
	// Unique key for this issue+stage combination.
	stageKey := iKey + "-" + stage.Name

	// Check if this issue is locked by another driver instance
	lockLabel := fmt.Sprintf("fabrik:locked:%s", e.cfg.User)
	otherLockPrefix := "fabrik:locked:"
	for _, label := range item.Labels {
		if strings.HasPrefix(label, otherLockPrefix) && label != lockLabel {
			e.logf(item.Number, "skip", "locked by another user\n")
			return nil
		}
	}

	// Skip if currently being edited
	for _, label := range item.Labels {
		if label == "fabrik:editing" {
			e.logf(item.Number, "skip", "is being edited\n")
			return nil
		}
	}

	// Awaiting-input: paused because Claude needs user input. If the user has
	// responded with a new comment, unblock and route to comment processing.
	if isAwaitingInput(item) {
		newComments := e.findNewComments(item)
		if len(newComments) > 0 {
			e.unblockAwaitingInput(item, stage, stageKey)
			return e.processComments(ctx, board, item, stage, newComments)
		}
		e.logf(item.Number, "skip", "awaiting user input\n")
		return nil
	}

	// Paused items: if the user commented, unpause and fall through to
	// comment processing. Otherwise skip. A user comment on a paused issue
	// is an implicit "resume and handle this."
	for _, label := range item.Labels {
		if label == "fabrik:paused" {
			newComments := e.findNewComments(item)
			if len(newComments) > 0 {
				e.logf(item.Number, "unpause", "user commented on paused issue — unpausing\n")
				if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:paused"); err != nil {
					e.logf(item.Number, "warn", "could not remove fabrik:paused label: %v\n", err)
				}
				// Also clear any failed label so the stage retries cleanly
				e.clearFailedStage(item, stage)
				break // fall through to comment processing below
			}
			e.logf(item.Number, "skip", "is paused\n")
			return nil
		}
	}

	// Dependency gate: block stage start if open blockers exist. This check runs
	// before any stage work (worktree setup, Claude invocation) so blocked issues
	// do not burn Claude turns. checkDependencies handles the fabrik:blocked label
	// and comment idempotently. Returns nil (silent skip) consistent with other
	// skip paths above. The first stage is always exempt (see checkDependencies).
	if e.checkDependencies(board, item, stage) {
		return nil
	}

	// Cleanup stage: remove the worktree (no lock, no Claude, no comment processing needed).
	// Runs before new-comment check — cleanup stages are terminal and should not route
	// comments to processComments. Also handles PR items (no worktree to remove, just label).
	if stage.CleanupWorktree {
		completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
		for _, label := range item.Labels {
			if label == completeLabel {
				return nil
			}
		}

		// Issues have worktrees; PRs on the board do not — skip the removal for PRs.
		if !item.IsPR {
			wm := e.worktreesFor(item.Repo)
			wtDir := wm.WorktreeDir(item.Number)
			statusCmd := exec.Command("git", "status", "--porcelain")
			statusCmd.Dir = wtDir
			if out, err := statusCmd.Output(); err == nil {
				for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
					if line == "" {
						continue
					}
					path := strings.TrimSpace(line[2:])
					if isEngineManagedPath(path) {
						continue
					}
					e.logf(item.Number, "warn", "worktree dirty at cleanup — uncommitted changes will be discarded\n")
					break
				}
			}

			if err := wm.CleanupWorktree(item.Number, false); err != nil {
				e.logf(item.Number, "warn", "could not clean up worktree: %v\n", err)
			}
		}

		if err := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); err != nil {
			e.logf(item.Number, "warn", "could not add completion label: %v\n", err)
		}

		// Archive is handled by archiveDoneCompleteItems in the poll loop,
		// which enforces the 24-hour grace period so completed items remain
		// visible on the board.

		e.mu.Lock()
		e.processedSet[stageKey] = time.Now()
		e.lastCompleted[iKey] = true
		e.mu.Unlock()

		return nil
	}

	// Unpause detection: if this stage has a stage:<name>:failed label but
	// fabrik:paused is gone, the user has investigated — reset state. We check
	// the label (not just the in-memory map) so cleanup works across restarts.
	failedLabel := fmt.Sprintf("stage:%s:failed", stage.Name)
	var hasFailedLabel bool
	for _, label := range item.Labels {
		if label == failedLabel {
			hasFailedLabel = true
			break
		}
	}
	var wasPaused bool
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		wasPaused = e.pausedDueToRetries[stageKey]
	}()
	if wasPaused || hasFailedLabel {
		e.clearFailedStage(item, stage)
	}

	// Check for new comments from our user
	newComments := e.findNewComments(item)

	// If there are new comments, process them (even if stage is complete)
	if len(newComments) > 0 {
		return e.processComments(ctx, board, item, stage, newComments)
	}

	// PRs only support comment processing — skip stage invocation
	if item.IsPR {
		return nil
	}

	// Check for stage completion label — already done
	completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
	for _, label := range item.Labels {
		if label == completeLabel {
			return nil
		}
	}

	// Determine if we need to run the stage
	var lastAttempt time.Time
	var attempted bool
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		lastAttempt, attempted = e.processedSet[stageKey]
	}()

	if attempted {
		// If stage completed, the completion label above would have caught it.
		// If we're here, the stage was attempted but didn't complete.
		// Apply a cooldown to avoid hot-looping.
		cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
		if time.Since(lastAttempt) < cooldown {
			return nil
		}
		e.logf(item.Number, "retry", "cooldown expired for stage %q, retrying\n", stage.Name)
		e.removeFailedLabel(owner, repo, item.Number, stage.Name)
	}

	// Bail early if context was cancelled before starting new work.
	select {
	case <-ctx.Done():
		e.logf(item.Number, "skip", "shutdown requested, skipping\n")
		return nil
	default:
	}
	e.logf(item.Number, "process", "%q — stage: %s\n", item.Title, stage.Name)

	// Acquire lock and in_progress label. These are released only when
	// the stage completes or is permanently abandoned — NOT on every
	// processItem return. This keeps the issue locked through cooldown
	// retries so other instances don't pick it up.
	lockAcquired := false
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, lockLabel); err != nil {
		e.logf(item.Number, "warn", "could not add lock label: %v\n", err)
	} else {
		lockAcquired = true
		e.mu.Lock()
		e.lockedIssues[iKey] = true
		e.mu.Unlock()
	}

	inProgressLabel := fmt.Sprintf("stage:%s:in_progress", stage.Name)
	inProgressAdded := false

	// releaseLock is called when we're truly done with this issue+stage
	// (completed, permanently failed, or paused). NOT called on cooldown retry.
	// Defined here (before in_progress is added) so the lock-then-verify loser
	// path can call it safely with inProgressAdded still false.
	releaseLock := func() {
		if lockAcquired {
			e.removeLockLabel(owner, repo, item.Number, lockLabel)
			e.mu.Lock()
			delete(e.lockedIssues, iKey)
			e.mu.Unlock()
		}
		if inProgressAdded {
			e.removeInProgressLabel(owner, repo, item.Number, stage.Name)
		}
	}

	// Lock-then-verify: after acquiring our lock, wait briefly to let a
	// competing instance place its own lock, then re-check. If another
	// fabrik:locked:* label is present, apply lexicographic tie-breaking:
	// lower username wins (keeps lock and proceeds); higher username loses
	// (releases lock and skips this cycle). This is deterministic — exactly
	// one instance wins any conflict. Note: identical usernames are unsupported
	// and treated as "win" (both proceed), consistent with single-instance use.
	if lockAcquired {
		time.Sleep(lockVerifyDelay)
		labels, err := e.client.FetchLabels(owner, repo, item.Number)
		if err != nil {
			e.logf(item.Number, "warn", "could not re-fetch labels for lock verify: %v\n", err)
		} else {
			for _, label := range labels {
				if strings.HasPrefix(label, "fabrik:locked:") && label != lockLabel {
					competing := strings.TrimPrefix(label, "fabrik:locked:")
					if e.cfg.User > competing {
						e.logf(item.Number, "skip", "lock conflict with %q — yielding (lexicographic tie-break)\n", competing)
						releaseLock()
						return nil
					}
					e.logf(item.Number, "info", "lock conflict with %q — proceeding as winner\n", competing)
					break
				}
			}
		}
	}

	if err := e.client.AddLabelToIssue(owner, repo, item.Number, inProgressLabel); err != nil {
		e.logf(item.Number, "warn", "could not add in_progress label: %v\n", err)
	} else {
		inProgressAdded = true
	}

	// Ensure the WorktreeManager for this item's repo is ready.
	wm := e.worktreesFor(item.Repo)

	// Ensure worktree exists for this issue.
	// On retries (resume=true), skip rebasing onto main — the worktree already
	// has context from the previous attempt and pulling in unrelated changes
	// mid-session confuses Claude.
	baseBranch := wm.DefaultBaseBranch()
	workDir, err := wm.EnsureWorktree(item.Number, baseBranch, attempted)
	if err != nil {
		releaseLock()
		return fmt.Errorf("setting up worktree: %w", err)
	}

	// If this is a read-only stage, stash any unexpected dirty state (including
	// untracked files) before invocation so the stage sees a clean worktree, and
	// restore it afterward.
	stashed := false
	if stage.ReadOnly {
		statusCmd := exec.Command("git", "status", "--porcelain")
		statusCmd.Dir = workDir
		if out, err := statusCmd.Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
			e.logf(item.Number, "warn", "worktree dirty before read-only stage %q — stashing changes\n", stage.Name)
			msg := fmt.Sprintf("fabrik: auto-stash before stage %q for issue #%d", stage.Name, item.Number)
			stashCmd := exec.Command("git", "stash", "push", "-u", "-m", msg)
			stashCmd.Dir = workDir
			if stashOut, stashErr := stashCmd.CombinedOutput(); stashErr != nil {
				e.logf(item.Number, "warn", "could not stash: %s\n", strings.TrimSpace(string(stashOut)))
			} else {
				e.logf(item.Number, "info", "stashed: %s\n", strings.TrimSpace(string(stashOut)))
				stashed = true
			}
		}
	}

	// Write context files after any stash so they are present for Claude but
	// not captured in the stash. Errors are non-fatal.
	e.writeContextFiles(item, stage, workDir, false)

	// Invoke Claude Code in the issue's worktree
	modelOverride := e.extractModelOverride(item.Number, item.Labels)
	if modelOverride != "" {
		e.logf(item.Number, "model", "using model override %q\n", modelOverride)
	}
	resume := attempted // resume session if we've processed this before
	output, completed, usage, err := e.claude.Invoke(ctx, stage, item, nil, resume, workDir, modelOverride)
	if usage.TurnsUsed > 0 || usage.InputTokens > 0 || usage.OutputTokens > 0 {
		if usage.MaxTurns > 0 {
			e.logf(item.Number, "stats", "used %d/%d turns, %dk input / %dk output tokens\n",
				usage.TurnsUsed, usage.MaxTurns, usage.InputTokens/1000, usage.OutputTokens/1000)
		} else {
			e.logf(item.Number, "stats", "used %d turns, %dk input / %dk output tokens\n",
				usage.TurnsUsed, usage.InputTokens/1000, usage.OutputTokens/1000)
		}
	}
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.totalTokens = e.totalTokens.add(usage)
		e.lastUsage[iKey] = usage
	}()

	// Restore any stashed changes now that the read-only stage has finished.
	if stashed {
		popCmd := exec.Command("git", "stash", "pop")
		popCmd.Dir = workDir
		if popOut, popErr := popCmd.CombinedOutput(); popErr != nil {
			e.logf(item.Number, "warn", "could not pop stash: %s\n", strings.TrimSpace(string(popOut)))
		} else {
			e.logf(item.Number, "info", "stash restored after read-only stage\n")
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			e.logf(item.Number, "skip", "cancelled during claude invocation\n")
			releaseLock()
			return nil
		}
		e.logf(item.Number, "warn", "claude invocation issue: %v\n", err)
	}

	// Capture git metadata for the comment header
	branch, commit, mainSHA, timestamp := captureGitMeta(workDir, baseBranch)

	// Check for issue body update markers in stage output.
	// Only stages with UpdateIssueBody=true (e.g., Specify) are allowed to
	// update the issue body. Other stages post output as stage comments only.
	if output != "" {
		if updatedBody := extractUpdatedBody(output); updatedBody != "" {
			if stage.UpdateIssueBody {
				e.logf(item.Number, "edit", "updating issue body from stage output\n")
				if err := e.client.UpdateIssueBody(owner, repo, item.Number, updatedBody); err != nil {
					e.logf(item.Number, "warn", "could not update issue body: %v\n", err)
				}
			} else {
				e.logf(item.Number, "warn", "stage %q produced FABRIK_ISSUE_UPDATE markers but is not allowed to update the issue body — ignoring\n", stage.Name)
			}
			// Always strip the markers from the output
			output = stripMarkers(output, "FABRIK_ISSUE_UPDATE_BEGIN", "FABRIK_ISSUE_UPDATE_END")
		}
	}

	// Strip all Fabrik markers from output before posting as a comment.
	// This must happen after extractUpdatedBody (above) but the raw output is
	// still needed for CheckBlockedOnInput (below), so we strip into a separate
	// variable for posting.
	postOutput := output
	if postOutput != "" {
		postOutput = stripLine(postOutput, "FABRIK_STAGE_COMPLETE")
		postOutput = stripLine(postOutput, "FABRIK_BLOCKED_ON_INPUT")
		postOutput = stripLine(postOutput, "FABRIK_DECOMPOSED")
		postOutput = stripLine(postOutput, "FABRIK_SUMMARY_BEGIN")
		postOutput = stripLine(postOutput, "FABRIK_SUMMARY_END")
		postOutput = strings.TrimSpace(postOutput)
	}

	// Post Claude's output
	if postOutput != "" {
		footer := formatStatsFooter(usage, completed)
		if stage.PostToPR {
			e.postOutputToPR(item, stage.Name, postOutput, footer, branch, commit, mainSHA, timestamp)
		} else {
			comment := formatOutputComment(stage.Name, postOutput, footer, branch, commit, mainSHA, timestamp)
			if err := e.client.AddComment(owner, repo, item.Number, comment); err != nil {
				e.logf(item.Number, "warn", "could not post comment: %v\n", err)
			}
		}
	}

	// Record attempt time only if Claude actually ran.
	// Known start failures (binary not found, command not found, etc.) should
	// not apply the cooldown so the item is retried on the next poll.
	claudeRan := err == nil
	if err != nil {
		// Default to "Claude ran" for errors, and only treat specific
		// start-failure types as "did not run".
		claudeRan = true

		var startErr *exec.Error
		if errors.As(err, &startErr) {
			claudeRan = false
		} else {
			var pathErr *os.PathError
			if errors.As(err, &pathErr) || errors.Is(err, exec.ErrNotFound) {
				claudeRan = false
			}
		}
	}
	if claudeRan {
		func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.processedSet[stageKey] = time.Now()
		}()
	}

	// Warn the user when Claude ran without error but produced no output at all.
	// This makes silent stalls visible on the issue without waiting for MaxRetries.
	if claudeRan && err == nil && strings.TrimSpace(output) == "" {
		e.logf(item.Number, "warn", "stage %q ran without error but produced no output\n", stage.Name)
		warnComment := fmt.Sprintf("🏭 **Fabrik — empty stage output**\n\nStage **%s** ran without error but produced no output.", stage.Name)
		if commentErr := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, warnComment); commentErr != nil {
			e.logf(item.Number, "warn", "could not post empty-output warning: %v\n", commentErr)
		}
	}

	// Commit any uncommitted changes so partial work isn't lost (e.g., max_turns reached).
	// Skip for read-only stages: those don't produce commits, and any dirty state was
	// restored by stash pop above — committing it would misattribute the stash contents.
	if claudeRan && !completed && !stage.ReadOnly {
		e.commitWIP(workDir, item.Number, stage.Name)
	}

	// Always push the branch after a stage runs — preserves work even on failure/max_turns
	if claudeRan {
		if pushErr := wm.PushBranch(item.Number); pushErr != nil {
			e.logf(item.Number, "warn", "could not push branch: %v\n", pushErr)
		}
	}

	// Mark any pre-existing user comments as "seen" by adding a rocket reaction.
	// These comments were included in the prompt as context — they should not
	// trigger the awaiting-input unblock logic on subsequent polls.
	if claudeRan {
		e.markCommentsSeenByStage(item, item.Comments)
		// Evict lastUpdatedAt so the next poll re-evaluates this item, regardless
		// of what updatedAt was cached during the run. This handles the race where
		// a concurrent poll cycle cached the item's updatedAt while the goroutine
		// was in-flight, causing a comment posted during the run to be missed.
		e.mu.Lock()
		delete(e.lastUpdatedAt, iKey)
		e.mu.Unlock()
	}

	// Only honor the blocked-on-input and decomposed markers if Claude ran without error.
	// If there was an error, treat the run as a retry/failure rather than
	// silently pausing the issue.
	blockedOnInput := err == nil && CheckBlockedOnInput(output)
	decomposed := err == nil && CheckDecomposed(output)

	// Store completion/blocked state for TUI event emission in poll.go.
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.lastCompleted[iKey] = completed
		e.lastBlocked[iKey] = blockedOnInput
	}()

	if completed {
		releaseLock()
		// Clear retry tracking for this stage — no longer needed after success.
		func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			delete(e.retryCount, stageKey)
			delete(e.pausedDueToRetries, stageKey)
		}()
		// Post-stage: create draft PR and/or mark ready now that commits exist
		var prNumber int
		if stage.CreateDraftPR {
			prNumber = e.ensureDraftPR(item, baseBranch)
		}
		if stage.MarkPRReadyOnComplete {
			e.markPRReady(item, prNumber)
		}
		e.handleStageComplete(board, item, stage)
	} else if decomposed {
		releaseLock()
		// Clear retry tracking for this stage — issue is decomposed, no retry needed.
		func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			delete(e.retryCount, stageKey)
			delete(e.pausedDueToRetries, stageKey)
		}()
		e.handleDecomposed(board, item, stage)
	} else if blockedOnInput {
		releaseLock()
		e.blockOnInput(item, stage)
	} else {
		cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
		e.logf(item.Number, "wait", "stage %q did not complete — will retry after %v\n", stage.Name, cooldown)
		if claudeRan && e.cfg.MaxRetries > 0 {
			var count int
			func() {
				e.mu.Lock()
				defer e.mu.Unlock()
				e.retryCount[stageKey]++
				count = e.retryCount[stageKey]
			}()
			if count >= e.cfg.MaxRetries {
				e.escalateFailedStage(item, stage)
				releaseLock() // permanently giving up — release the lock
			}
		}
	}

	return nil
}

// escalateFailedStage is called when a stage has failed MaxRetries times. It adds
// fabrik:paused and stage:<name>:failed labels, posts an explanatory comment, and
// records the escalation so clearFailedStage can detect when the user unpauses.
func (e *Engine) escalateFailedStage(item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "escalate", "stage %q failed %d time(s) — pausing issue\n", stage.Name, e.cfg.MaxRetries)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); err != nil {
		e.logf(item.Number, "warn", "could not add paused label: %v\n", err)
	}

	e.addFailedLabel(owner, repo, item.Number, stage.Name)

	comment := fmt.Sprintf(
		"🏭 **Fabrik — stage failed**\n\nStage **%s** failed to complete after %d attempt(s). The issue has been paused (`fabrik:paused`).\n\nTo retry: investigate the failure, make any needed fixes, then remove the `fabrik:paused` label.",
		stage.Name, e.cfg.MaxRetries,
	)
	if err := e.client.AddComment(owner, repo, item.Number, comment); err != nil {
		e.logf(item.Number, "warn", "could not post escalation comment: %v\n", err)
	}

	stageKey := issueKey(item, e.defaultRepo()) + "-" + stage.Name
	e.mu.Lock()
	e.pausedDueToRetries[stageKey] = true
	e.mu.Unlock()
}

// clearFailedStage is called when the user removes fabrik:paused from an issue
// that was paused by the engine due to max retries. It removes the stage:<name>:failed
// label and resets the retry count so the stage can be attempted again.
func (e *Engine) clearFailedStage(item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "unpause", "clearing failed stage %q after manual unpause\n", stage.Name)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	failedLabel := fmt.Sprintf("stage:%s:failed", stage.Name)
	if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, failedLabel); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(item.Number, "warn", "could not remove failed label: %v\n", err)
	}

	stageKey := issueKey(item, e.defaultRepo()) + "-" + stage.Name
	e.mu.Lock()
	delete(e.retryCount, stageKey)
	delete(e.pausedDueToRetries, stageKey)
	delete(e.processedSet, stageKey) // clear cooldown so the stage retries immediately
	e.mu.Unlock()
}

// blockOnInput is called when Claude outputs FABRIK_BLOCKED_ON_INPUT. It pauses
// the issue with fabrik:paused + fabrik:awaiting-input labels so the engine
// knows to auto-unblock when the user responds with a comment.
// It does NOT add a stage:<name>:failed label and does NOT touch retryCount.
func (e *Engine) blockOnInput(item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "block", "stage %q needs user input — pausing with awaiting-input\n", stage.Name)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); err != nil {
		e.logf(item.Number, "warn", "could not add paused label: %v\n", err)
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil {
		e.logf(item.Number, "warn", "could not add awaiting-input label: %v\n", err)
	}
}

// unblockAwaitingInput is called when a user comment arrives on an issue that
// was paused via blockOnInput. It removes both labels and clears the
// processedSet entry so the stage re-runs promptly after comment processing.
func (e *Engine) unblockAwaitingInput(item gh.ProjectItem, stage *stages.Stage, stageKey string) {
	e.logf(item.Number, "unblock", "user comment received — removing awaiting-input pause\n")

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:paused"); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(item.Number, "warn", "could not remove paused label: %v\n", err)
	}
	if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(item.Number, "warn", "could not remove awaiting-input label: %v\n", err)
	}

	e.mu.Lock()
	delete(e.processedSet, stageKey)
	e.mu.Unlock()
}

// extractModelOverride scans item labels for the first "model:<name>" label and returns <name>.
// If multiple model labels exist, it uses the first and logs a warning.
// Returns "" if no model label is found.
func (e *Engine) extractModelOverride(issueNumber int, labels []string) string {
	const prefix = "model:"
	var found string
	for _, label := range labels {
		if strings.HasPrefix(label, prefix) {
			name := strings.TrimPrefix(label, prefix)
			if name == "" {
				continue
			}
			if found == "" {
				found = name
			} else {
				e.logf(issueNumber, "warn", "multiple model: labels found, using %q (ignoring %q)\n", found, name)
			}
		}
	}
	return found
}

func (e *Engine) removeEditingLabel(owner, repo string, issueNumber int) {
	if err := e.client.RemoveLabelFromIssue(owner, repo, issueNumber, "fabrik:editing"); err != nil {
		e.logf(issueNumber, "warn", "could not remove editing label: %v\n", err)
	}
}

func (e *Engine) removeLockLabel(owner, repo string, issueNumber int, label string) {
	if err := e.client.RemoveLabelFromIssue(owner, repo, issueNumber, label); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(issueNumber, "warn", "could not remove lock label: %v\n", err)
	}
}

func (e *Engine) removeInProgressLabel(owner, repo string, issueNumber int, stageName string) {
	label := fmt.Sprintf("stage:%s:in_progress", stageName)
	if err := e.client.RemoveLabelFromIssue(owner, repo, issueNumber, label); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(issueNumber, "warn", "could not remove in_progress label: %v\n", err)
	}
}

func (e *Engine) addFailedLabel(owner, repo string, issueNumber int, stageName string) {
	label := fmt.Sprintf("stage:%s:failed", stageName)
	if err := e.client.AddLabelToIssue(owner, repo, issueNumber, label); err != nil {
		e.logf(issueNumber, "warn", "could not add failed label: %v\n", err)
	}
}

func (e *Engine) removeFailedLabel(owner, repo string, issueNumber int, stageName string) {
	label := fmt.Sprintf("stage:%s:failed", stageName)
	if err := e.client.RemoveLabelFromIssue(owner, repo, issueNumber, label); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(issueNumber, "warn", "could not remove failed label: %v\n", err)
	}
}

// commitWIP commits any uncommitted changes in the worktree as a WIP commit.
// This preserves partial work when Claude hits max_turns or errors out.
func (e *Engine) commitWIP(workDir string, issueNumber int, stageName string) {
	// Check for uncommitted changes
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = workDir
	out, err := statusCmd.Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return // clean worktree, nothing to commit
	}

	// Stage all changes
	addCmd := exec.Command("git", "add", "-A")
	addCmd.Dir = workDir
	if _, err := addCmd.CombinedOutput(); err != nil {
		e.logf(issueNumber, "warn", "could not stage WIP changes: %v\n", err)
		return
	}

	// Unstage any context files that were picked up by git add -A above.
	// This covers the case where context files were previously committed
	// (making them tracked) and then modified — the .gitignore inside
	// .fabrik-context/ only protects untracked files.
	resetCmd := exec.Command("git", "reset", "HEAD", "--", ".fabrik-context/")
	resetCmd.Dir = workDir
	if _, err := resetCmd.CombinedOutput(); err != nil {
		e.logf(issueNumber, "warn", "could not unstage context files: %v\n", err)
		return
	}

	// Commit
	msg := fmt.Sprintf("WIP: %s stage incomplete (partial progress)", stageName)
	commitCmd := exec.Command("git", "commit", "-m", msg)
	commitCmd.Dir = workDir
	if _, err := commitCmd.CombinedOutput(); err != nil {
		e.logf(issueNumber, "warn", "could not commit WIP: %v\n", err)
		return
	}

	e.logf(issueNumber, "info", "committed WIP changes for incomplete %s stage\n", stageName)
}
