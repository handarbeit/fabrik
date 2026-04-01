package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

// itemNeedsWork does cheap pre-checks to determine if an item might need processing.
// This runs in the poll loop BEFORE acquiring a semaphore slot, so it must be fast
// and not make any API calls.
func (e *Engine) itemNeedsWork(item gh.ProjectItem) bool {
	// No matching stage = nothing to do
	stage := stages.FindStage(e.cfg.Stages, item.Status)
	if stage == nil {
		return false
	}

	// Paused items never need work
	for _, label := range item.Labels {
		if label == "fabrik:paused" {
			return false
		}
	}

	// Check for new comments (always worth processing)
	if len(e.findNewComments(item)) > 0 {
		return true
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
	itemKey := fmt.Sprintf("%d-%s", item.Number, stage.Name)
	e.mu.Lock()
	lastAttempt, attempted := e.processedSet[itemKey]
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

	// Check if this issue is locked by another driver instance
	lockLabel := fmt.Sprintf("fabrik:locked:%s", e.cfg.User)
	otherLockPrefix := "fabrik:locked:"
	for _, label := range item.Labels {
		if strings.HasPrefix(label, otherLockPrefix) && label != lockLabel {
			logf(item.Number, "skip", "locked by another user\n")
			return nil
		}
	}

	// Skip if currently being edited
	for _, label := range item.Labels {
		if label == "fabrik:editing" {
			logf(item.Number, "skip", "is being edited\n")
			return nil
		}
	}

	// Skip if paused
	for _, label := range item.Labels {
		if label == "fabrik:paused" {
			logf(item.Number, "skip", "is paused\n")
			return nil
		}
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
	itemKey := fmt.Sprintf("%d-%s", item.Number, stage.Name)
	var lastAttempt time.Time
	var attempted bool
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		lastAttempt, attempted = e.processedSet[itemKey]
	}()

	if attempted {
		// If stage completed, the completion label above would have caught it.
		// If we're here, the stage was attempted but didn't complete.
		// Apply a cooldown to avoid hot-looping.
		cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
		if time.Since(lastAttempt) < cooldown {
			return nil
		}
		logf(item.Number, "retry", "cooldown expired for stage %q, retrying\n", stage.Name)
	}

	// Bail early if context was cancelled before starting new work.
	select {
	case <-ctx.Done():
		logf(item.Number, "skip", "shutdown requested, skipping\n")
		return nil
	default:
	}
	logf(item.Number, "process", "%q — stage: %s\n", item.Title, stage.Name)

	// Acquire lock, register in lockedIssues for shutdown cleanup, and ensure it's released on all exit paths.
	if err := e.client.AddLabelToIssue(e.cfg.Owner, e.cfg.Repo, item.Number, lockLabel); err != nil {
		logf(item.Number, "warn", "could not add lock label: %v\n", err)
	} else {
		e.mu.Lock()
		e.lockedIssues[item.Number] = true
		e.mu.Unlock()
		defer func() {
			e.removeLockLabel(item.Number, lockLabel)
			e.mu.Lock()
			delete(e.lockedIssues, item.Number)
			e.mu.Unlock()
		}()
	}

	// Add in_progress label for this stage and ensure it's removed on all exit paths.
	// Only defer cleanup when the add succeeded to avoid a spurious warning on removal.
	inProgressLabel := fmt.Sprintf("stage:%s:in_progress", stage.Name)
	if err := e.client.AddLabelToIssue(e.cfg.Owner, e.cfg.Repo, item.Number, inProgressLabel); err != nil {
		logf(item.Number, "warn", "could not add in_progress label: %v\n", err)
	} else {
		defer e.removeInProgressLabel(item.Number, stage.Name)
	}

	// Ensure worktree exists for this issue
	baseBranch := e.worktrees.DefaultBaseBranch()
	workDir, err := e.worktrees.EnsureWorktree(item.Number, baseBranch)
	if err != nil {
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
			logf(item.Number, "warn", "worktree dirty before read-only stage %q — stashing changes\n", stage.Name)
			msg := fmt.Sprintf("fabrik: auto-stash before stage %q for issue #%d", stage.Name, item.Number)
			stashCmd := exec.Command("git", "stash", "push", "-u", "-m", msg)
			stashCmd.Dir = workDir
			if stashOut, stashErr := stashCmd.CombinedOutput(); stashErr != nil {
				logf(item.Number, "warn", "could not stash: %s\n", strings.TrimSpace(string(stashOut)))
			} else {
				logf(item.Number, "info", "stashed: %s\n", strings.TrimSpace(string(stashOut)))
				stashed = true
			}
		}
	}

	// Invoke Claude Code in the issue's worktree
	modelOverride := extractModelOverride(item.Number, item.Labels)
	if modelOverride != "" {
		logf(item.Number, "model", "using model override %q\n", modelOverride)
	}
	resume := attempted // resume session if we've processed this before
	output, completed, err := e.claude.Invoke(ctx, stage, item, nil, resume, workDir, modelOverride)

	// Restore any stashed changes now that the read-only stage has finished.
	if stashed {
		popCmd := exec.Command("git", "stash", "pop")
		popCmd.Dir = workDir
		if popOut, popErr := popCmd.CombinedOutput(); popErr != nil {
			logf(item.Number, "warn", "could not pop stash: %s\n", strings.TrimSpace(string(popOut)))
		} else {
			logf(item.Number, "info", "stash restored after read-only stage\n")
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			logf(item.Number, "skip", "cancelled during claude invocation\n")
			return nil
		}
		logf(item.Number, "warn", "claude invocation issue: %v\n", err)
	}

	// Capture git metadata for the comment header
	branch, commit, timestamp := captureGitMeta(workDir)

	// Post Claude's output
	if output != "" {
		if stage.PostToPR {
			e.postOutputToPR(item, stage.Name, output, branch, commit, timestamp)
		} else {
			comment := formatOutputComment(stage.Name, output, branch, commit, timestamp)
			if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, comment); err != nil {
				logf(item.Number, "warn", "could not post comment: %v\n", err)
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
			e.processedSet[itemKey] = time.Now()
		}()
	}

	// Commit any uncommitted changes so partial work isn't lost (e.g., max_turns reached).
	// Skip for read-only stages: those don't produce commits, and any dirty state was
	// restored by stash pop above — committing it would misattribute the stash contents.
	if claudeRan && !completed && !stage.ReadOnly {
		commitWIP(workDir, item.Number, stage.Name)
	}

	// Always push the branch after a stage runs — preserves work even on failure/max_turns
	if claudeRan {
		if pushErr := e.worktrees.PushBranch(item.Number); pushErr != nil {
			logf(item.Number, "warn", "could not push branch: %v\n", pushErr)
		}
	}

	if completed {
		// Post-stage: create draft PR and/or mark ready now that commits exist
		var prNumber int
		if stage.CreateDraftPR {
			prNumber = e.ensureDraftPR(item, baseBranch)
		}
		if stage.MarkPRReadyOnComplete {
			e.markPRReady(item, prNumber)
		}
		e.handleStageComplete(board, item, stage)
	} else {
		cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
		logf(item.Number, "wait", "stage %q did not complete — will retry after %v\n", stage.Name, cooldown)
	}

	return nil
}

// extractModelOverride scans item labels for the first "model:<name>" label and returns <name>.
// If multiple model labels exist, it uses the first and logs a warning.
// Returns "" if no model label is found.
func extractModelOverride(issueNumber int, labels []string) string {
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
				logf(issueNumber, "warn", "multiple model: labels found, using %q (ignoring %q)\n", found, name)
			}
		}
	}
	return found
}

func (e *Engine) removeEditingLabel(issueNumber int) {
	if err := e.client.RemoveLabelFromIssue(e.cfg.Owner, e.cfg.Repo, issueNumber, "fabrik:editing"); err != nil {
		logf(issueNumber, "warn", "could not remove editing label: %v\n", err)
	}
}

func (e *Engine) removeLockLabel(issueNumber int, label string) {
	if err := e.client.RemoveLabelFromIssue(e.cfg.Owner, e.cfg.Repo, issueNumber, label); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		logf(issueNumber, "warn", "could not remove lock label: %v\n", err)
	}
}

func (e *Engine) removeInProgressLabel(issueNumber int, stageName string) {
	label := fmt.Sprintf("stage:%s:in_progress", stageName)
	if err := e.client.RemoveLabelFromIssue(e.cfg.Owner, e.cfg.Repo, issueNumber, label); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		logf(issueNumber, "warn", "could not remove in_progress label: %v\n", err)
	}
}

// commitWIP commits any uncommitted changes in the worktree as a WIP commit.
// This preserves partial work when Claude hits max_turns or errors out.
func commitWIP(workDir string, issueNumber int, stageName string) {
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
		logf(issueNumber, "warn", "could not stage WIP changes: %v\n", err)
		return
	}

	// Commit
	msg := fmt.Sprintf("WIP: %s stage incomplete (partial progress)", stageName)
	commitCmd := exec.Command("git", "commit", "-m", msg)
	commitCmd.Dir = workDir
	if _, err := commitCmd.CombinedOutput(); err != nil {
		logf(issueNumber, "warn", "could not commit WIP: %v\n", err)
		return
	}

	logf(issueNumber, "info", "committed WIP changes for incomplete %s stage\n", stageName)
}
