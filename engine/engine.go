package engine

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

type Config struct {
	Owner       string
	Repo        string
	ProjectNum  int
	User        string
	Token       string
	Yolo          bool
	PollSeconds   int
	MaxConcurrent int
	Stages      []*stages.Stage
}

type Engine struct {
	cfg          Config
	client       *gh.Client
	statusField  *gh.StatusField
	worktrees    *WorktreeManager
	mu           sync.Mutex
	processedSet map[string]time.Time // track what we've processed: "issue#-commentID" -> timestamp
}

func New(cfg Config) (*Engine, error) {
	// Resolve git repo root (works even if launched from a subdirectory)
	repoDir, err := gitToplevel()
	if err != nil {
		return nil, fmt.Errorf("resolving git repo root: %w", err)
	}
	return &Engine{
		cfg:          cfg,
		client:       gh.NewClient(cfg.Token),
		worktrees:    NewWorktreeManager(repoDir),
		processedSet: make(map[string]time.Time),
	}, nil
}

func gitToplevel() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not inside a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (e *Engine) Run() error {
	// Set up graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("\nFabrik is running. Press Ctrl+C to stop.")
	fmt.Println()

	// Main poll loop
	ticker := time.NewTicker(time.Duration(e.cfg.PollSeconds) * time.Second)
	defer ticker.Stop()

	// Run immediately on start, then on tick
	if err := e.poll(); err != nil {
		fmt.Printf("  [warn] poll error: %v\n", err)
	}

	for {
		select {
		case <-sigCh:
			fmt.Println("\nShutting down...")
			return nil
		case <-ticker.C:
			if err := e.poll(); err != nil {
				fmt.Printf("  [warn] poll error: %v\n", err)
			}
		}
	}
}

func (e *Engine) poll() error {
	fmt.Printf("[poll] fetching project board %s/%s#%d\n", e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum)

	board, err := e.client.FetchProjectBoard(e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum)
	if err != nil {
		return err
	}

	// Fetch status field metadata (for mutations) on first poll
	e.mu.Lock()
	if e.statusField == nil && board.ProjectID != "" {
		sf, err := e.client.FetchStatusField(board.ProjectID)
		if err != nil {
			fmt.Printf("  [warn] could not fetch status field: %v\n", err)
		} else {
			e.statusField = sf
		}
	}
	e.mu.Unlock()

	fmt.Printf("[poll] found %d items on board\n", len(board.Items))

	var worked int32
	sem := make(chan struct{}, e.cfg.MaxConcurrent)
	var wg sync.WaitGroup
	for _, item := range board.Items {
		item := item
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := e.processItem(board, item, &worked); err != nil {
				fmt.Printf("  [error] issue #%d: %v\n", item.Number, err)
			}
		}()
	}
	wg.Wait()

	if worked == 0 {
		fmt.Println("[poll] nothing to do")
	}

	return nil
}

func (e *Engine) processItem(board *gh.ProjectBoard, item gh.ProjectItem, worked *int32) error {
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
			fmt.Printf("  [skip] issue #%d locked by another user\n", item.Number)
			return nil
		}
	}

	// Skip if currently being edited
	for _, label := range item.Labels {
		if label == "fabrik:editing" {
			fmt.Printf("  [skip] issue #%d is being edited\n", item.Number)
			return nil
		}
	}

	// Check for new comments from our user
	newComments := e.findNewComments(item)

	// If there are new comments, process them (even if stage is complete)
	if len(newComments) > 0 {
		atomic.AddInt32(worked, 1)
		return e.processComments(board, item, stage, newComments)
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
		fmt.Printf("  [retry] cooldown expired for issue #%d stage %q, retrying\n", item.Number, stage.Name)
	}

	atomic.AddInt32(worked, 1)
	fmt.Printf("\n[process] issue #%d %q — stage: %s\n", item.Number, item.Title, stage.Name)

	// Acquire lock
	if err := e.client.AddLabelToIssue(e.cfg.Owner, e.cfg.Repo, item.Number, lockLabel); err != nil {
		fmt.Printf("  [warn] could not add lock label: %v\n", err)
	}

	// Ensure worktree exists for this issue
	baseBranch := e.worktrees.DefaultBaseBranch()
	workDir, err := e.worktrees.EnsureWorktree(item.Number, baseBranch)
	if err != nil {
		return fmt.Errorf("setting up worktree: %w", err)
	}

	// Pre-stage: create a draft PR if requested and none exists yet
	if stage.CreateDraftPR {
		e.ensureDraftPR(item, baseBranch)
	}

	// Invoke Claude Code in the issue's worktree
	modelOverride := extractModelOverride(item.Labels)
	if modelOverride != "" {
		fmt.Printf("  [model] using model override %q for issue #%d\n", modelOverride, item.Number)
	}
	output, completed, err := InvokeClaude(stage, item, false, workDir, modelOverride)
	if err != nil {
		fmt.Printf("  [warn] claude invocation issue: %v\n", err)
	}

	// Post Claude's output
	if output != "" {
		if stage.PostToPR {
			e.postOutputToPR(item, stage.Name, output)
		} else {
			comment := formatOutputComment(stage.Name, output)
			if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, comment); err != nil {
				fmt.Printf("  [warn] could not post comment: %v\n", err)
			}
		}
	}

	// Record attempt time (used for cooldown if stage didn't complete)
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.processedSet[itemKey] = time.Now()
	}()

	if completed {
		// Post-stage: push branch and mark PR ready if requested
		if stage.MarkPRReadyOnComplete {
			e.markPRReady(item)
		}
		e.handleStageComplete(board, item, stage)
	} else {
		cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
		fmt.Printf("  [wait] stage %q did not complete for issue #%d — will retry after %v\n", stage.Name, item.Number, cooldown)
	}

	return nil
}

// processComments handles new user comments on an issue.
// Flow: 👀 reactions → editing label → invoke Claude → perform actions / update issue body → remove editing label → 🚀 reactions
func (e *Engine) processComments(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, comments []gh.Comment) error {
	fmt.Printf("\n[comments] processing %d new comment(s) on issue #%d — stage: %s\n",
		len(comments), item.Number, stage.Name)

	// Step 1: React with 👀 to all new comments
	for _, c := range comments {
		if err := e.client.AddCommentReaction(e.cfg.Owner, e.cfg.Repo, c.DatabaseID, "eyes"); err != nil {
			fmt.Printf("  [warn] could not add 👀 to comment %s: %v\n", c.ID, err)
		}
	}

	// Step 2: Add editing label
	if err := e.client.AddLabelToIssue(e.cfg.Owner, e.cfg.Repo, item.Number, "fabrik:editing"); err != nil {
		return fmt.Errorf("adding editing label: %w", err)
	}

	// Step 3: Ensure worktree
	baseBranch := e.worktrees.DefaultBaseBranch()
	workDir, err := e.worktrees.EnsureWorktree(item.Number, baseBranch)
	if err != nil {
		e.removeEditingLabel(item.Number)
		return fmt.Errorf("setting up worktree: %w", err)
	}

	// Step 4: Invoke Claude with the comment review prompt
	modelOverride := extractModelOverride(item.Labels)
	if modelOverride != "" {
		fmt.Printf("  [model] using model override %q for issue #%d\n", modelOverride, item.Number)
	}
	output, _, err := InvokeClaudeForComments(stage, item, comments, workDir, modelOverride)
	if err != nil {
		fmt.Printf("  [warn] claude comment review issue: %v\n", err)
		e.removeEditingLabel(item.Number)
		return err
	}

	// Step 5: Parse the updated issue body from Claude's output and apply it
	if updatedBody := extractUpdatedBody(output); updatedBody != "" {
		fmt.Printf("  [edit] updating issue #%d body\n", item.Number)
		if err := e.client.UpdateIssueBody(e.cfg.Owner, e.cfg.Repo, item.Number, updatedBody); err != nil {
			fmt.Printf("  [warn] could not update issue body: %v\n", err)
		}
	} else {
		// No body update — post output as a comment instead
		if output != "" {
			comment := formatOutputComment(stage.Name+" (comment review)", output)
			if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, comment); err != nil {
				fmt.Printf("  [warn] could not post comment: %v\n", err)
			}
		}
	}

	// Step 6: Remove editing label
	e.removeEditingLabel(item.Number)

	// Step 7: React with 🚀 to all processed comments
	for _, c := range comments {
		if err := e.client.AddCommentReaction(e.cfg.Owner, e.cfg.Repo, c.DatabaseID, "rocket"); err != nil {
			fmt.Printf("  [warn] could not add 🚀 to comment %s: %v\n", c.ID, err)
		}
	}

	// Mark comments as processed only after everything succeeded
	e.markCommentsProcessed(item, comments)

	fmt.Printf("  [done] comment processing complete for issue #%d\n", item.Number)
	return nil
}

// markCommentsProcessed records comments as processed so they won't be retried.
func (e *Engine) markCommentsProcessed(item gh.ProjectItem, comments []gh.Comment) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range comments {
		key := fmt.Sprintf("%d-comment-%s", item.Number, c.ID)
		e.processedSet[key] = time.Now()
	}
}

// ensureDraftPR pushes the issue branch and creates a draft PR if one doesn't exist yet.
// Idempotent: skips creation if FindPRForIssue finds an existing PR.
func (e *Engine) ensureDraftPR(item gh.ProjectItem, baseBranch string) {
	// Push the branch so GitHub can create a PR against it
	if err := e.worktrees.PushBranch(item.Number); err != nil {
		fmt.Printf("  [warn] could not push branch for issue #%d: %v\n", item.Number, err)
		return
	}

	// Check if a PR already exists
	prNumber, err := e.client.FindPRForIssue(e.cfg.Owner, e.cfg.Repo, item.Number)
	if err != nil {
		fmt.Printf("  [warn] could not check for existing PR for issue #%d: %v\n", item.Number, err)
		return
	}
	if prNumber > 0 {
		fmt.Printf("  [pr] draft PR #%d already exists for issue #%d, skipping creation\n", prNumber, item.Number)
		return
	}

	head := fmt.Sprintf("fabrik/issue-%d", item.Number)
	prNum, err := e.client.CreateDraftPR(e.cfg.Owner, e.cfg.Repo, item.Title, head, baseBranch, item.Number)
	if err != nil {
		fmt.Printf("  [warn] could not create draft PR for issue #%d: %v\n", item.Number, err)
		return
	}
	fmt.Printf("  [pr] created draft PR #%d for issue #%d\n", prNum, item.Number)
}

// markPRReady pushes the issue branch and transitions its PR from draft to ready-for-review.
func (e *Engine) markPRReady(item gh.ProjectItem) {
	if err := e.worktrees.PushBranch(item.Number); err != nil {
		fmt.Printf("  [warn] could not push branch for issue #%d: %v\n", item.Number, err)
		// Don't return — still try to mark ready if push is a no-op (already up to date)
	}

	prNumber, err := e.client.FindPRForIssue(e.cfg.Owner, e.cfg.Repo, item.Number)
	if err != nil {
		fmt.Printf("  [warn] could not find PR for issue #%d: %v\n", item.Number, err)
		return
	}
	if prNumber == 0 {
		fmt.Printf("  [warn] no open PR found for issue #%d, cannot mark ready\n", item.Number)
		return
	}

	if err := e.client.MarkPRReady(e.cfg.Owner, e.cfg.Repo, prNumber); err != nil {
		fmt.Printf("  [warn] could not mark PR #%d ready: %v\n", prNumber, err)
		return
	}
	fmt.Printf("  [pr] marked PR #%d ready-for-review for issue #%d\n", prNumber, item.Number)
}

// postOutputToPR posts detailed output on the linked PR and a brief summary on the issue.
func (e *Engine) postOutputToPR(item gh.ProjectItem, stageName, output string) {
	prNumber, err := e.client.FindPRForIssue(e.cfg.Owner, e.cfg.Repo, item.Number)
	if err != nil {
		fmt.Printf("  [warn] could not find PR for issue #%d: %v\n", item.Number, err)
	}

	if prNumber > 0 {
		// Post detailed output on the PR
		comment := formatOutputComment(stageName, output)
		if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, prNumber, comment); err != nil {
			fmt.Printf("  [warn] could not post to PR #%d: %v\n", prNumber, err)
		} else {
			fmt.Printf("  [post] detailed %s output posted to PR #%d\n", stageName, prNumber)
		}

		// Post brief summary on the issue
		summary := formatPRSummaryComment(stageName, prNumber, output)
		if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, summary); err != nil {
			fmt.Printf("  [warn] could not post summary to issue #%d: %v\n", item.Number, err)
		}
	} else {
		// No PR found — fall back to posting on the issue
		fmt.Printf("  [warn] no open PR found for issue #%d, posting on issue instead\n", item.Number)
		comment := formatOutputComment(stageName, output)
		if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, comment); err != nil {
			fmt.Printf("  [warn] could not post comment: %v\n", err)
		}
	}
}

func (e *Engine) removeEditingLabel(issueNumber int) {
	if err := e.client.RemoveLabelFromIssue(e.cfg.Owner, e.cfg.Repo, issueNumber, "fabrik:editing"); err != nil {
		fmt.Printf("  [warn] could not remove editing label: %v\n", err)
	}
}

func (e *Engine) handleStageComplete(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	fmt.Printf("  [done] stage %q complete for issue #%d\n", stage.Name, item.Number)

	completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
	if err := e.client.AddLabelToIssue(e.cfg.Owner, e.cfg.Repo, item.Number, completeLabel); err != nil {
		fmt.Printf("  [warn] could not add completion label: %v\n", err)
	}

	shouldAdvance := e.cfg.Yolo
	if stage.AutoAdvance != nil {
		shouldAdvance = *stage.AutoAdvance
	}

	if shouldAdvance {
		if err := e.advanceToNextStage(board, item, stage); err != nil {
			fmt.Printf("  [warn] could not advance: %v\n", err)
		}
	} else {
		fmt.Printf("  [wait] waiting for human to advance issue #%d\n", item.Number)
	}
}

func (e *Engine) findNewComments(item gh.ProjectItem) []gh.Comment {
	var newComments []gh.Comment
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range item.Comments {
		// Only process comments from the configured user
		if c.Author != e.cfg.User {
			continue
		}
		// Skip comments we've already processed
		key := fmt.Sprintf("%d-comment-%s", item.Number, c.ID)
		if _, seen := e.processedSet[key]; seen {
			continue
		}
		// Skip comments that look like Fabrik output
		if strings.HasPrefix(c.Body, "🏭 **Fabrik") {
			continue
		}
		// Skip comments already processed (marked with 🚀 reaction)
		if c.HasReaction("ROCKET") {
			continue
		}
		newComments = append(newComments, c)
	}
	return newComments
}

func (e *Engine) advanceToNextStage(board *gh.ProjectBoard, item gh.ProjectItem, currentStage *stages.Stage) error {
	next := stages.NextStage(e.cfg.Stages, currentStage.Name)
	if next == nil {
		fmt.Printf("  [info] issue #%d has completed all stages\n", item.Number)
		return nil
	}

	if e.statusField == nil {
		return fmt.Errorf("status field metadata not available")
	}

	optionID, ok := e.statusField.Options[next.Name]
	if !ok {
		return fmt.Errorf("no status option %q found on project board (available: %v)",
			next.Name, mapKeys(e.statusField.Options))
	}

	fmt.Printf("  [advance] moving issue #%d to stage %q\n", item.Number, next.Name)
	return e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, e.statusField.FieldID, optionID)
}

func formatOutputComment(stageName, output string) string {
	const maxLen = 60000
	if len(output) > maxLen {
		output = output[:maxLen] + "\n\n... (truncated)"
	}
	return fmt.Sprintf("🏭 **Fabrik — stage: %s**\n\n%s", stageName, output)
}

func formatPRSummaryComment(stageName string, prNumber int, output string) string {
	summary := extractSummary(output)
	if summary == "" {
		summary = "(no summary provided)"
	}
	return fmt.Sprintf("🏭 **Fabrik — stage: %s**\n\nDetailed output posted on PR #%d.\n\n%s", stageName, prNumber, summary)
}

// extractModelOverride scans item labels for the first "model:<name>" label and returns <name>.
// If multiple model labels exist, it uses the first and logs a warning.
// Returns "" if no model label is found.
func extractModelOverride(labels []string) string {
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
				fmt.Printf("  [warn] multiple model: labels found, using %q (ignoring %q)\n", found, name)
			}
		}
	}
	return found
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
