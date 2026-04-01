package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

const idleUpgradeThreshold = 2

type Config struct {
	Owner         string
	Repo          string
	ProjectNum    int
	User          string
	Token         string
	Yolo          bool
	AutoUpgrade   bool
	PollSeconds   int
	MaxConcurrent int
	Stages        []*stages.Stage
	// ReadyCh is closed once Run() has registered signal handlers. Tests use
	// this to avoid sending SIGINT before signal.Notify is installed.
	ReadyCh chan struct{}
}

type Engine struct {
	cfg          Config
	client       GitHubClient
	claude       ClaudeInvoker
	statusField  *gh.StatusField
	worktrees    *WorktreeManager
	mu           sync.Mutex
	processedSet map[string]time.Time // track what we've processed: "issue#-commentID" -> timestamp
	lockedIssues map[int]bool         // issues that have had fabrik:locked added and not yet released
	idleCount    int                  // consecutive idle polls; triggers self-upgrade at threshold
	sem          chan struct{}         // semaphore bounding concurrent workers across poll cycles
	wg           sync.WaitGroup       // tracks in-flight workers for graceful shutdown
	inFlight     sync.Map             // key: issue number (int), value: struct{}
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
		claude:       &RealClaudeInvoker{},
		worktrees:    NewWorktreeManager(repoDir),
		processedSet: make(map[string]time.Time),
		lockedIssues: make(map[int]bool),
		sem:          make(chan struct{}, cfg.MaxConcurrent),
	}, nil
}

// NewWithDeps creates an Engine with explicit dependencies (for testing).
func NewWithDeps(cfg Config, client GitHubClient, claude ClaudeInvoker, worktrees *WorktreeManager) *Engine {
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return &Engine{
		cfg:          cfg,
		client:       client,
		claude:       claude,
		worktrees:    worktrees,
		processedSet: make(map[string]time.Time),
		lockedIssues: make(map[int]bool),
		sem:          make(chan struct{}, maxConcurrent),
	}
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	if e.cfg.ReadyCh != nil {
		close(e.cfg.ReadyCh)
	}

	fmt.Println("\nFabrik is running. Press Ctrl+C to stop.")
	fmt.Println()

	// Handle signals in a dedicated goroutine so cancel() fires immediately
	// even while poll() is blocking on wg.Wait(). This ensures CommandContext
	// kills in-flight Claude child processes without waiting for the current
	// poll cycle to finish naturally.
	go func() {
		select {
		case sig := <-sigCh:
			fmt.Printf("\nReceived %v — shutting down gracefully (Ctrl-C again to force-quit)...\n", sig)
			cancel()
		case <-ctx.Done():
			return
		}
		// Listen for a second signal during drain and force-exit.
		select {
		case <-sigCh:
			fmt.Println("\nForce-quitting...")
			os.Exit(1)
		case <-ctx.Done():
		}
	}()

	ticker := time.NewTicker(time.Duration(e.cfg.PollSeconds) * time.Second)
	defer ticker.Stop()

	// Run immediately on start, then on tick
	if err := e.poll(ctx); err != nil && ctx.Err() == nil {
		fmt.Printf("  [warn] poll error: %v\n", err)
	}

	for {
		select {
		case <-ctx.Done():
			// Signal goroutine called cancel(); poll() returned because
			// CommandContext killed the child processes.
			e.cleanupLockedIssues()
			return nil
		case <-ticker.C:
			if ctx.Err() != nil {
				e.cleanupLockedIssues()
				return nil
			}
			if err := e.poll(ctx); err != nil {
				fmt.Printf("  [warn] poll error: %v\n", err)
			}
		}
	}
}

// cleanupLockedIssues removes fabrik:locked labels for any issues that were locked
// at shutdown time but never released (e.g., because the worker was killed mid-run).
func (e *Engine) cleanupLockedIssues() {
	e.mu.Lock()
	issues := make([]int, 0, len(e.lockedIssues))
	for num := range e.lockedIssues {
		issues = append(issues, num)
	}
	e.mu.Unlock()

	if len(issues) == 0 {
		return
	}
	lockLabel := fmt.Sprintf("fabrik:locked:%s", e.cfg.User)
	fmt.Printf("[shutdown] removing lock labels from %d issue(s)\n", len(issues))
	for _, num := range issues {
		if err := e.client.RemoveLabelFromIssue(e.cfg.Owner, e.cfg.Repo, num, lockLabel); err != nil {
			logf(num, "warn", "could not remove lock label during shutdown: %v\n", err)
		} else {
			logf(num, "shutdown", "removed lock label\n")
		}
		e.mu.Lock()
		delete(e.lockedIssues, num)
		e.mu.Unlock()
	}
}

func (e *Engine) poll(ctx context.Context) error {
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

	var dispatched int
	for _, item := range board.Items {
		item := item
		// Quick pre-check: skip items that won't need processing.
		// This avoids acquiring a semaphore slot for no-ops.
		if !e.itemNeedsWork(item) {
			continue
		}
		// Skip issues already being processed by a previous poll cycle's worker
		if _, ok := e.inFlight.Load(item.Number); ok {
			continue
		}
		// Acquire semaphore slot, but abort if the context is cancelled so we
		// don't block indefinitely when all slots are taken at shutdown time.
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			goto doneDispatching
		}
		e.inFlight.Store(item.Number, struct{}{})
		e.wg.Add(1)
		dispatched++
		go func() {
			defer e.wg.Done()
			defer func() { <-e.sem }()
			defer e.inFlight.Delete(item.Number)
			if err := e.processItem(ctx, board, item); err != nil {
				logf(item.Number, "error", "%v\n", err)
			}
		}()
	}
doneDispatching:

	if dispatched == 0 {
		// Check whether any workers from a previous poll cycle are still running.
		// If so, the engine is not truly idle — auto-upgrade must not run because
		// checkAndUpgrade calls syscall.Exec which would kill in-flight workers.
		var hasInFlight bool
		e.inFlight.Range(func(_, _ any) bool { hasInFlight = true; return false })

		if hasInFlight {
			fmt.Println("[poll] nothing new to dispatch (workers still in-flight)")
			e.idleCount = 0
		} else {
			fmt.Println("[poll] nothing to do")
			if e.cfg.AutoUpgrade {
				e.idleCount++
				if e.idleCount >= idleUpgradeThreshold {
					e.idleCount = 0
					e.checkAndUpgrade()
				}
			}
		}
	} else {
		e.idleCount = 0
	}

	return nil
}

// checkAndUpgrade checks origin/main for new commits and, if found, performs a
// fast-forward pull, rebuilds the binary, and re-execs the process in place.
func (e *Engine) checkAndUpgrade() {
	baseBranch := e.worktrees.DefaultBaseBranch()
	dir := e.worktrees.BaseDir()

	fmt.Printf("[upgrade] checking origin/%s for new commits\n", baseBranch)

	// Fetch from origin
	fetchCmd := exec.Command("git", "fetch", "origin", baseBranch)
	fetchCmd.Dir = dir
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		fmt.Printf("[upgrade] git fetch failed: %v\n%s\n", err, out)
		return
	}

	// Compare HEAD to origin/baseBranch
	localRef, err := gitRevParse(dir, "HEAD")
	if err != nil {
		fmt.Printf("[upgrade] could not resolve HEAD: %v\n", err)
		return
	}
	remoteRef, err := gitRevParse(dir, "origin/"+baseBranch)
	if err != nil {
		fmt.Printf("[upgrade] could not resolve origin/%s: %v\n", baseBranch, err)
		return
	}
	if localRef == remoteRef {
		fmt.Printf("[upgrade] already up-to-date\n")
		return
	}

	fmt.Printf("[upgrade] new commits detected — pulling origin/%s\n", baseBranch)

	pullCmd := exec.Command("git", "pull", "--ff-only", "origin", baseBranch)
	pullCmd.Dir = dir
	if out, err := pullCmd.CombinedOutput(); err != nil {
		fmt.Printf("[upgrade] git pull --ff-only failed (local changes?): %v\n%s\n", err, out)
		return
	}

	// Determine current executable path
	exe, err := os.Executable()
	if err != nil {
		fmt.Printf("[upgrade] could not determine executable path: %v\n", err)
		return
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		fmt.Printf("[upgrade] could not resolve symlinks for executable: %v\n", err)
		return
	}

	fmt.Printf("[upgrade] rebuilding binary: %s\n", exe)

	buildCmd := exec.Command("go", "build", "-o", exe, ".")
	buildCmd.Dir = dir
	if out, err := buildCmd.CombinedOutput(); err != nil {
		fmt.Printf("[upgrade] build failed: %v\n%s\n", err, out)
		return
	}

	fmt.Printf("[upgrade] re-executing new binary\n")

	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		fmt.Printf("[upgrade] exec failed: %v\n", err)
	}
}

func gitRevParse(dir, ref string) (string, error) {
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

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

	// Invoke Claude Code in the issue's worktree
	modelOverride := extractModelOverride(item.Number, item.Labels)
	if modelOverride != "" {
		logf(item.Number, "model", "using model override %q\n", modelOverride)
	}
	resume := attempted // resume session if we've processed this before
	output, completed, err := e.claude.Invoke(ctx, stage, item, nil, resume, workDir, modelOverride)
	if err != nil {
		if ctx.Err() != nil {
			logf(item.Number, "skip", "cancelled during claude invocation\n")
			return nil
		}
		logf(item.Number, "warn", "claude invocation issue: %v\n", err)
	}

	// Post Claude's output
	if output != "" {
		if stage.PostToPR {
			e.postOutputToPR(item, stage.Name, output)
		} else {
			comment := formatOutputComment(stage.Name, output)
			if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, comment); err != nil {
				logf(item.Number, "warn", "could not post comment: %v\n", err)
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
		// Post-stage: create draft PR and/or mark ready now that commits exist
		if stage.CreateDraftPR {
			e.ensureDraftPR(item, baseBranch)
		}
		if stage.MarkPRReadyOnComplete {
			e.markPRReady(item)
		}
		e.handleStageComplete(board, item, stage)
	} else {
		cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
		logf(item.Number, "wait", "stage %q did not complete — will retry after %v\n", stage.Name, cooldown)
	}

	return nil
}

// processComments handles new user comments on an issue.
// Flow: 👀 reactions → editing label → invoke Claude → perform actions / update issue body → remove editing label → 🚀 reactions
func (e *Engine) processComments(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, comments []gh.Comment) error {
	logf(item.Number, "comments", "processing %d new comment(s) — stage: %s\n",
		len(comments), stage.Name)

	// Step 1: React with 👀 to all new comments
	for _, c := range comments {
		if err := e.client.AddCommentReaction(e.cfg.Owner, e.cfg.Repo, c.DatabaseID, "eyes"); err != nil {
			logf(item.Number, "warn", "could not add 👀 to comment %s: %v\n", c.ID, err)
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
	modelOverride := extractModelOverride(item.Number, item.Labels)
	if modelOverride != "" {
		logf(item.Number, "model", "using model override %q\n", modelOverride)
	}
	output, _, err := InvokeClaudeForComments(ctx, stage, item, comments, workDir, modelOverride)
	if err != nil {
		e.removeEditingLabel(item.Number)
		if ctx.Err() != nil {
			logf(item.Number, "skip", "cancelled during claude comment review\n")
			return nil
		}
		logf(item.Number, "warn", "claude comment review issue: %v\n", err)
		return err
	}

	// Step 5: Parse the updated issue body from Claude's output and apply it
	if updatedBody := extractUpdatedBody(output); updatedBody != "" {
		logf(item.Number, "edit", "updating issue body\n")
		if err := e.client.UpdateIssueBody(e.cfg.Owner, e.cfg.Repo, item.Number, updatedBody); err != nil {
			logf(item.Number, "warn", "could not update issue body: %v\n", err)
		}
	} else {
		// No body update — post output as a comment instead
		if output != "" {
			comment := formatOutputComment(stage.Name+" (comment review)", output)
			if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, comment); err != nil {
				logf(item.Number, "warn", "could not post comment: %v\n", err)
			}
		}
	}

	// Step 6: Remove editing label
	e.removeEditingLabel(item.Number)

	// Step 7: React with 🚀 to all processed comments
	for _, c := range comments {
		if err := e.client.AddCommentReaction(e.cfg.Owner, e.cfg.Repo, c.DatabaseID, "rocket"); err != nil {
			logf(item.Number, "warn", "could not add 🚀 to comment %s: %v\n", c.ID, err)
		}
	}

	// Mark comments as processed only after everything succeeded
	e.markCommentsProcessed(item, comments)

	logf(item.Number, "done", "comment processing complete\n")
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
// Idempotent: checks for an existing PR first; only pushes and creates if none found.
func (e *Engine) ensureDraftPR(item gh.ProjectItem, baseBranch string) {
	// Check for an existing PR first — avoids pushing on retries and handles
	// the case where a push fails but a PR already exists from a prior run.
	prNumber, err := e.client.FindPRForIssue(e.cfg.Owner, e.cfg.Repo, item.Number)
	if err != nil {
		fmt.Printf("  [warn] could not check for existing PR for issue #%d: %v\n", item.Number, err)
		return
	}
	if prNumber > 0 {
		logf(item.Number, "pr", "PR #%d already exists, ensuring issue link\n", prNumber)
		e.ensurePRLinksIssue(prNumber, item.Number)
		return
	}

	// No PR exists — push the branch so GitHub can create a PR against it
	if err := e.worktrees.PushBranch(item.Number); err != nil {
		fmt.Printf("  [warn] could not push branch for issue #%d: %v\n", item.Number, err)
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

// ensurePRLinksIssue checks that a PR body contains "Closes #N" and adds it if missing.
// This ensures closedByPullRequestsReferences links the PR to the issue, which is how
// Fabrik discovers PR comments.
func (e *Engine) ensurePRLinksIssue(prNumber, issueNumber int) {
	closingKeyword := fmt.Sprintf("Closes #%d", issueNumber)

	// Fetch current PR body (PRs are issues on the REST API)
	body, err := e.client.GetIssueBody(e.cfg.Owner, e.cfg.Repo, prNumber)
	if err != nil {
		logf(issueNumber, "warn", "could not fetch PR #%d body: %v\n", prNumber, err)
		return
	}

	if strings.Contains(body, closingKeyword) {
		return // already linked
	}

	// Append closing keyword
	updatedBody := body + "\n\n" + closingKeyword
	if err := e.client.UpdateIssueBody(e.cfg.Owner, e.cfg.Repo, prNumber, updatedBody); err != nil {
		logf(issueNumber, "warn", "could not update PR #%d body: %v\n", prNumber, err)
		return
	}
	logf(issueNumber, "pr", "added '%s' to PR #%d body\n", closingKeyword, prNumber)
}

// markPRReady pushes the issue branch and transitions its PR from draft to ready-for-review.
// If no PR exists yet (e.g., ensureDraftPR failed earlier because there were no commits),
// it attempts to create one before marking it ready.
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
		// No PR yet — ensureDraftPR may have failed earlier (e.g., branch had no commits).
		// Now that commits exist and the branch is pushed, try to create the PR.
		baseBranch := e.worktrees.DefaultBaseBranch()
		head := fmt.Sprintf("fabrik/issue-%d", item.Number)
		prNumber, err = e.client.CreateDraftPR(e.cfg.Owner, e.cfg.Repo, item.Title, head, baseBranch, item.Number)
		if err != nil {
			fmt.Printf("  [warn] could not create PR for issue #%d: %v\n", item.Number, err)
			return
		}
		fmt.Printf("  [pr] created PR #%d for issue #%d\n", prNumber, item.Number)
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
		logf(item.Number, "warn", "could not find PR: %v\n", err)
	}

	if prNumber > 0 {
		// Post detailed output on the PR
		comment := formatOutputComment(stageName, output)
		if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, prNumber, comment); err != nil {
			logf(item.Number, "warn", "could not post to PR #%d: %v\n", prNumber, err)
		} else {
			logf(item.Number, "post", "detailed %s output posted to PR #%d\n", stageName, prNumber)
		}

		// Post brief summary on the issue
		summary := formatPRSummaryComment(stageName, prNumber, output)
		if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, summary); err != nil {
			logf(item.Number, "warn", "could not post summary: %v\n", err)
		}
	} else {
		// No PR found — fall back to posting on the issue
		logf(item.Number, "warn", "no open PR found, posting on issue instead\n")
		comment := formatOutputComment(stageName, output)
		if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, comment); err != nil {
			logf(item.Number, "warn", "could not post comment: %v\n", err)
		}
	}
}

func (e *Engine) removeEditingLabel(issueNumber int) {
	if err := e.client.RemoveLabelFromIssue(e.cfg.Owner, e.cfg.Repo, issueNumber, "fabrik:editing"); err != nil {
		logf(issueNumber, "warn", "could not remove editing label: %v\n", err)
	}
}

func (e *Engine) removeLockLabel(issueNumber int, label string) {
	if err := e.client.RemoveLabelFromIssue(e.cfg.Owner, e.cfg.Repo, issueNumber, label); err != nil &&
		!strings.Contains(err.Error(), "404") {
		logf(issueNumber, "warn", "could not remove lock label: %v\n", err)
	}
}

func (e *Engine) removeInProgressLabel(issueNumber int, stageName string) {
	label := fmt.Sprintf("stage:%s:in_progress", stageName)
	if err := e.client.RemoveLabelFromIssue(e.cfg.Owner, e.cfg.Repo, issueNumber, label); err != nil &&
		!strings.Contains(err.Error(), "404") {
		logf(issueNumber, "warn", "could not remove in_progress label: %v\n", err)
	}
}

func (e *Engine) handleStageComplete(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
	logf(item.Number, "done", "stage %q complete\n", stage.Name)

	completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
	if err := e.client.AddLabelToIssue(e.cfg.Owner, e.cfg.Repo, item.Number, completeLabel); err != nil {
		logf(item.Number, "warn", "could not add completion label: %v\n", err)
	}

	shouldAdvance := e.cfg.Yolo
	if stage.AutoAdvance != nil {
		shouldAdvance = *stage.AutoAdvance
	}

	if shouldAdvance {
		if err := e.advanceToNextStage(board, item, stage); err != nil {
			logf(item.Number, "warn", "could not advance: %v\n", err)
		}
	} else {
		logf(item.Number, "wait", "waiting for human to advance\n")
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
		logf(item.Number, "info", "completed all stages\n")
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

	logf(item.Number, "advance", "moving to stage %q\n", next.Name)
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

func logf(issueNumber int, tag, format string, args ...any) {
	prefix := fmt.Sprintf("[#%d %s] ", issueNumber, tag)
	fmt.Printf(prefix+format, args...)
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
