package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/verveguy/fabrik/boardcache"
	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/internal/itemstate"
	"github.com/verveguy/fabrik/stages"
)

// lockVerifyDelay is the time to wait after acquiring a lock before re-fetching
// labels to detect competing locks from other Fabrik instances. Declared as a
// var (not const) so tests can set it to 0 without a 2-second sleep per test.
var lockVerifyDelay = 2 * time.Second

// editingLabelRetryDelay is the base delay for removeEditingLabel retry backoff.
// Declared as a var so tests can set it to 0 to avoid sleeping.
var editingLabelRetryDelay = 500 * time.Millisecond

// isEngineManagedPath returns true for paths that are written by the Fabrik
// engine itself and should never be treated as user-generated dirty content.
// These paths must not block cleanup or worktree updates.
func isEngineManagedPath(path string) bool {
	return strings.HasPrefix(path, ".fabrik-context/") || path == ".fabrik/issue.md"
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

// worktreeExistsForItem reports whether a worktree directory exists on disk
// for item. It uses the registered WorktreeManager when available, or falls
// back to the conventional filesystem path when no WM is registered (e.g.,
// after a restart when only cleanup items remain). This is a local-only check
// with no GraphQL cost.
func (e *Engine) worktreeExistsForItem(item gh.ProjectItem) bool {
	key := item.Repo
	if key == "" {
		key = e.defaultRepo()
	}
	e.mu.Lock()
	wm, ok := e.worktreeManagers[key]
	e.mu.Unlock()
	var wtDir string
	if ok {
		wtDir = wm.WorktreeDir(item.Number)
	} else {
		owner, repo := parseOwnerRepo(key)
		dirName := owner + "-" + repo
		wtDir = filepath.Join(e.fabrikDir, ".fabrik", "worktrees", dirName, fmt.Sprintf("issue-%d", item.Number))
	}
	_, err := os.Stat(wtDir)
	return err == nil
}

// hasLabel reports whether item.Labels contains label.
func hasLabel(item gh.ProjectItem, label string) bool {
	for _, l := range item.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// itemMayNeedWork does cheap pre-checks using only shallow board data (no comments).
// Items that pass this filter will have their details fetched via FetchItemDetails
// before the full itemNeedsWork check. This avoids expensive deep fetches for items
// that can be ruled out by status, labels, or updatedAt alone.
func (e *Engine) itemMayNeedWork(item gh.ProjectItem) bool {
	// No matching stage = nothing to do
	stage := stages.FindStage(e.cfg.Stages, item.Status)

	// Closed issues are skipped unless the current stage is a cleanup stage
	// (so cleanup can remove the worktree) OR the current stage is marked
	// complete (so the yolo catch-up can advance to the next stage — e.g.,
	// a PR merge closes an issue sitting in Validate; it needs to move to
	// Done for cleanup).
	if item.IsClosed {
		if stage == nil {
			return false
		}
		// Also admit closed items with fabrik:awaiting-ci so the catch-up loop
		// can complete the CI gate after a PR merge closes the issue.
		if !stage.CleanupWorktree && !hasLabel(item, fmt.Sprintf("stage:%s:complete", stage.Name)) && !hasLabel(item, "fabrik:awaiting-ci") {
			return false
		}
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
		return e.worktreeExistsForItem(item)
	}

	// Don't check labels or blockedBy here — those require full label data which
	// is only available after deep fetch. Label/lock/dep-gate checks are in
	// itemNeedsWork, which runs after FetchItemDetails populates the full label set.
	// The "has this item changed since last poll?" gate was previously implemented
	// here via seenUpdatedAt. It is now handled by the mayNeedWork pre-filter in
	// poll.go (see poll() function), which is populated by Store observers.

	// Apply a cooldown for items whose last FetchItemDetails call failed.
	// Without this, a persistent failure (e.g. deleted issue, permission error)
	// would cause an API call on every poll cycle. The cooldown duration matches
	// the retry window used by LastAttemptAt and CooldownAt.
	if snap, snapErr := e.store.Get(itemOwnerRepoString(item, e.defaultRepo()), item.Number); snapErr == nil {
		if lastFailure := snap.State().LastDeepFetchFailureAt; !lastFailure.IsZero() {
			cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
			if time.Since(lastFailure) < cooldown {
				return false
			}
		}
	}

	return true
}

// itemNeedsWork does full checks including comment inspection.
// This runs AFTER FetchItemDetails has populated the item's Comments.
func (e *Engine) itemNeedsWork(item gh.ProjectItem) bool {
	stage := stages.FindStage(e.cfg.Stages, item.Status)

	// Closed issues are skipped unless the current stage is a cleanup stage
	// (so cleanup can remove the worktree) OR the current stage is already
	// marked complete (so the catch-up loop can advance to the next stage) OR
	// fabrik:awaiting-ci is present (so the catch-up loop can finish the CI gate).
	if item.IsClosed {
		if stage == nil {
			return false
		}
		if !stage.CleanupWorktree && !hasLabel(item, fmt.Sprintf("stage:%s:complete", stage.Name)) && !hasLabel(item, "fabrik:awaiting-ci") {
			return false
		}
	}

	if stage == nil {
		return false
	}

	// Cleanup stages bypass comment processing and cooldown checks.
	if stage.CleanupWorktree {
		if hasLabel(item, "fabrik:paused") {
			return false
		}
		if hasLabel(item, fmt.Sprintf("stage:%s:complete", stage.Name)) {
			return false
		}
		return e.worktreeExistsForItem(item)
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
	isPaused := hasLabel(item, "fabrik:paused")
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

	// Dependency gate is handled by processItem via checkDependencies, which
	// applies fabrik:blocked and sets CooldownAt["dep-blocked"] so itemMayNeedWork
	// triggers periodic re-evaluation on expiry. A silent return here would skip
	// the item without labelling it blocked, leaving it permanently stuck once
	// its updatedAt is cached — even after blockers close.

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

	// CI gate in-flight: catch-up loop evaluates CI via checkCIGate; dispatcher
	// must not re-invoke while CI is being awaited (R3). Scoped to wait_for_ci
	// stages so a stale label on a non-CI-gated stage does not permanently
	// suppress dispatch (e.g., if a user manually moves an item to a different stage).
	if stage.WaitForCI != nil && *stage.WaitForCI && hasLabel(item, "fabrik:awaiting-ci") {
		return false
	}

	// Check dispatch cooldown: suppress re-dispatch while LastAttemptAt is within the
	// retry window. Unlike CooldownAt (deep-fetch suppression), LastAttemptAt is only
	// written when Claude actually runs — never refreshed by mere observation — so it
	// accurately reflects real invocation recency.
	repo := itemOwnerRepoString(item, e.defaultRepo())
	if snap, snapErr := e.store.Get(repo, item.Number); snapErr == nil {
		lastAttempt := snap.LastAttemptAt(stage.Name)
		if !lastAttempt.IsZero() {
			cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
			if time.Since(lastAttempt) < cooldown {
				return false
			}
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
	// "owner/repo" string for store operations.
	repoStr := itemOwnerRepoString(item, e.defaultRepo())

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
			e.unblockAwaitingInput(item, stage)
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
				} else {
					if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
						cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:paused")
					}
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
	// skip paths above. Applies uniformly to every stage, including the first.
	if e.checkDependencies(board, item, stage) {
		// Record CooldownAt["dep-blocked"] so itemMayNeedWork's CooldownAt expiry
		// path triggers periodic re-evaluation. Without this, a blocked item whose
		// updatedAt never changes (GitHub may not propagate a dependency's closure
		// to the blocked item) would be permanently filtered and never unblocked.
		cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
		e.store.Apply(itemstate.CooldownRecorded{
			Repo:   repoStr,
			Number: item.Number,
			Reason: "dep-blocked",
			Until:  time.Now().Add(cooldown),
		})
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
		} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), completeLabel)
		}

		// Remove fabrik:extend-turns at cleanup (Done) stage — this is the designated
		// removal site. The label persists across all intermediate stages so the operator
		// can apply it once and have it take effect on every stage until Done.
		// Called unconditionally (not guarded by hasLabel) because cleanup items are
		// dispatched from shallow board items (labels(first:15)) and the label may be
		// present on GitHub without appearing in item.Labels. ErrNotFound = already gone.
		if removeErr := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:extend-turns"); removeErr != nil &&
			!errors.Is(removeErr, gh.ErrNotFound) {
			e.logf(item.Number, "warn", "could not remove extend-turns label: %v\n", removeErr)
		} else if removeErr == nil {
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:extend-turns")
			}
		}

		// Archive is handled by archiveDoneCompleteItems in the poll loop,
		// which enforces the 24-hour grace period so completed items remain
		// visible on the board.

		// Record CooldownAt["periodic-re-eval"] so itemMayNeedWork suppresses
		// deep-fetches for this terminal item during the cooldown window.
		cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
		e.store.Apply(itemstate.CooldownRecorded{
			Repo:   repoStr,
			Number: item.Number,
			Reason: "periodic-re-eval",
			Until:  time.Now().Add(cooldown),
		})
		e.store.Apply(itemstate.InvocationRecorded{
			Repo:      itemOwnerRepoString(item, e.defaultRepo()),
			Number:    item.Number,
			Completed: true,
		})

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
	if snap, snapErr := e.store.Get(repoStr, item.Number); snapErr == nil {
		wasPaused = snap.PausedByEngine(stage.Name)
	}
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

	// Determine if we need to run the stage. LastAttemptAt is set only when Claude
	// actually runs — never refreshed by observation — so it accurately reflects
	// real invocation recency and prevents hot-looping after an incomplete run.
	var lastAttempt time.Time
	if snap, snapErr := e.store.Get(repoStr, item.Number); snapErr == nil {
		lastAttempt = snap.LastAttemptAt(stage.Name)
	}
	if !lastAttempt.IsZero() {
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
	var workerDone chan struct{}
	workerStartedAt := time.Now()
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, lockLabel); err != nil {
		e.logf(item.Number, "warn", "could not add lock label: %v\n", err)
	} else {
		lockAcquired = true
		e.store.Apply(itemstate.LocalLockAcquired{
			Repo:       repoStr,
			Number:     item.Number,
			User:       e.cfg.User,
			AcquiredAt: workerStartedAt,
			Worker:     &itemstate.WorkerHandle{StageName: stage.Name, StartedAt: workerStartedAt, LastSignAt: workerStartedAt},
		})
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), lockLabel)
		}
		workerDone = make(chan struct{})
		e.startHeartbeat(ctx, repoStr, item.Number, workerDone)
	}
	defer func() {
		if workerDone != nil {
			close(workerDone)
		}
	}()
	defer e.store.Apply(itemstate.WorkerExited{Repo: repoStr, Number: item.Number})

	inProgressLabel := fmt.Sprintf("stage:%s:in_progress", stage.Name)
	inProgressAdded := false

	// releaseLock is called when we're truly done with this issue+stage
	// (completed, permanently failed, or paused). NOT called on cooldown retry.
	// Defined here (before in_progress is added) so the lock-then-verify loser
	// path can call it safely with inProgressAdded still false.
	releaseLock := func() {
		if lockAcquired {
			e.removeLockLabel(owner, repo, item.Number, lockLabel)
			e.store.Apply(itemstate.LocalLockReleased{
				Repo:   itemOwnerRepoString(item, e.defaultRepo()),
				Number: item.Number,
			})
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
		// Lock verification needs live labels, not cached state — another instance
		// may have written its lock label in the window since we read from cache.
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
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), inProgressLabel)
		}
	}

	// Ensure the WorktreeManager for this item's repo is ready.
	wm := e.worktreesFor(item.Repo)

	// Ensure worktree exists for this issue.
	// On retries (resume=true), skip rebasing onto main — the worktree already
	// has context from the previous attempt and pulling in unrelated changes
	// mid-session confuses Claude.
	baseBranch, err := e.baseBranchForItem(item, wm)
	if err != nil {
		releaseLock()
		return fmt.Errorf("setting up worktree for %s/%s: %w", owner, repo, err)
	}
	workDir, err := wm.EnsureWorktree(item.Number, baseBranch, !lastAttempt.IsZero())
	if err != nil {
		releaseLock()
		return fmt.Errorf("setting up worktree for %s/%s: %w", owner, repo, err)
	}

	// If a PR exists and its base branch doesn't match the resolved base, update it.
	e.syncPRBase(item, baseBranch)

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
	effortOverride := e.extractEffortOverride(item.Number, item.Labels)
	if effortOverride != "" {
		e.logf(item.Number, "effort", "using effort override %q\n", effortOverride)
	}
	resume := !lastAttempt.IsZero() // resume session if we've processed this before
	opts := InvokeOptions{
		ModelOverride:  modelOverride,
		EffortOverride: effortOverride,
		BaseBranch:     baseBranch,
		OnPIDReady:     func(pid int) { e.store.Apply(itemstate.WorkerPIDSet{Repo: repoStr, Number: item.Number, PID: pid}) },
	}

	// Snapshot extend-turns presence before any FetchItemDetails re-fetches (which
	// refresh item.Labels). Using a stable boolean ensures the first-budget calc is
	// consistent regardless of what a mid-loop re-fetch changes in item.Labels.
	hadExtendTurnsLabel := hasExtendTurnsLabel(item)

	// Determine initial turn budget. When fabrik:extend-turns is present the first
	// invocation gets a 2× budget (pre-granted extension, no progress check needed).
	firstBudget := stage.MaxTurns
	totalMultiple := 1
	if hadExtendTurnsLabel && stage.MaxTurns > 0 {
		firstBudget = 2 * stage.MaxTurns
		totalMultiple = 2
	}
	baseline := snapshotBaseline(stage, item, workDir)

	// Extension loop: re-invoke with --resume when max_turns is hit and progress is detected.
	// Hard cap is 3× stage.MaxTurns total across all invocations.
	var output string
	var completed bool
	var usage TokenUsage
	currentBudget := firstBudget
	for {
		opts.MaxTurnsOverride = currentBudget
		var invOutput string
		var invUsage TokenUsage
		invOutput, completed, invUsage, err = e.claude.Invoke(ctx, stage, item, nil, resume, workDir, opts)
		output += invOutput
		usage = addTokenUsage(usage, invUsage)

		hitLimit := !completed && err == nil && stage.MaxTurns > 0 && invUsage.TurnsUsed >= currentBudget
		if !hitLimit || totalMultiple >= 3 {
			break
		}
		issueLogf := func(tag, format string, args ...any) {
			e.logf(item.Number, tag, format, args...)
		}
		hasProgress, progressErr := detectProgress(ctx, stage, &item, baseline, workDir, e.client, issueLogf)
		if progressErr != nil {
			e.logf(item.Number, "extend-turns", "progress check failed: %v\n", progressErr)
			break
		}
		if !hasProgress {
			break
		}
		totalMultiple++
		currentBudget = stage.MaxTurns
		e.logf(item.Number, "extend-turns", "extending to %d× budget (%d turns used)\n", totalMultiple, usage.TurnsUsed)
		resume = true
	}
	// Report cumulative budget across all extensions in stats footer.
	usage.MaxTurns = totalMultiple * stage.MaxTurns

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
		e.totalTokens = addTokenUsage(e.totalTokens, usage)
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
	if output != "" {
		if updatedBody := extractUpdatedBody(output); updatedBody != "" {
			e.logf(item.Number, "edit", "updating issue body from stage output\n")
			// no write-through: excluded — issue body is not read from cache for dispatch decisions
			if err := e.client.UpdateIssueBody(owner, repo, item.Number, updatedBody); err != nil {
				e.logf(item.Number, "warn", "could not update issue body: %v\n", err)
			}
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
		// Record that Claude ran. LastAttemptAt is the ONLY write site for this
		// field — it is never refreshed by the deep-fetch defer or any other
		// observation path, which is the structural fix for the #504 regression.
		e.store.Apply(itemstate.StageAttempted{
			Repo:      repoStr,
			Number:    item.Number,
			StageName: stage.Name,
			At:        time.Now(),
		})
	}

	// Warn the user when Claude ran without error but produced no output at all.
	// This makes silent stalls visible on the issue without waiting for MaxRetries.
	if claudeRan && err == nil && strings.TrimSpace(output) == "" {
		e.logf(item.Number, "warn", "stage %q ran without error but produced no output\n", stage.Name)
		warnComment := fmt.Sprintf("🏭 **Fabrik — empty stage output**\n\nStage **%s** ran without error but produced no output.", stage.Name)
		if dbID, commentErr := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, warnComment); commentErr != nil {
			e.logf(item.Number, "warn", "could not post empty-output warning: %v\n", commentErr)
		} else {
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
					DatabaseID: dbID, Body: warnComment, Author: e.cfg.User, CreatedAt: time.Now(),
				})
			}
			// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
			if reactErr := e.client.AddCommentReaction(e.cfg.Owner, e.cfg.Repo, dbID, "rocket"); reactErr != nil {
				e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
			}
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
		// The InvocationObserver fires on InvocationChanged (from InvocationRecorded
		// below) and adds this item to e.mayNeedWork, ensuring it is re-evaluated in
		// the next poll cycle. No explicit eviction is needed.
	}

	// Only honor the blocked-on-input and decomposed markers if Claude ran without error.
	// If there was an error, treat the run as a retry/failure rather than
	// silently pausing the issue.
	blockedOnInput := err == nil && CheckBlockedOnInput(output)
	decomposed := err == nil && CheckDecomposed(output)

	// Store completion/blocked/usage state for TUI event emission in poll.go.
	e.store.Apply(itemstate.InvocationRecorded{
		Repo:      itemOwnerRepoString(item, e.defaultRepo()),
		Number:    item.Number,
		Completed: completed,
		Blocked:   blockedOnInput,
		Usage:     usage,
		Duration:  time.Since(workerStartedAt),
	})

	if completed {
		releaseLock()
		// Clear retry tracking for this stage — no longer needed after success.
		e.store.Apply(itemstate.StageRetryCleared{Repo: repoStr, Number: item.Number, StageName: stage.Name})
		e.store.Apply(itemstate.EngineUnpaused{Repo: repoStr, Number: item.Number, StageName: stage.Name})
		// Post-stage: create draft PR and/or mark ready now that commits exist
		var prNumber int
		if stage.CreateDraftPR {
			prNumber = e.ensureDraftPR(item, baseBranch)
			e.updatePRVerification(item, prNumber, extractSummary(output))
		}
		if stage.MarkPRReadyOnComplete {
			e.markPRReady(item, prNumber)
		}
		e.handleStageComplete(ctx, board, item, stage)
	} else if decomposed {
		releaseLock()
		// Clear retry tracking for this stage — issue is decomposed, no retry needed.
		e.store.Apply(itemstate.StageRetryCleared{Repo: repoStr, Number: item.Number, StageName: stage.Name})
		e.store.Apply(itemstate.EngineUnpaused{Repo: repoStr, Number: item.Number, StageName: stage.Name})
		e.handleDecomposed(board, item, stage)
	} else if blockedOnInput {
		releaseLock()
		e.blockOnInput(item, stage)
	} else {
		cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
		e.logf(item.Number, "wait", "stage %q did not complete — will retry after %v\n", stage.Name, cooldown)
		if claudeRan && e.cfg.MaxRetries > 0 {
			e.store.Apply(itemstate.StageRetryIncremented{Repo: repoStr, Number: item.Number, StageName: stage.Name})
			var count int
			if snap, snapErr := e.store.Get(repoStr, item.Number); snapErr == nil {
				count = snap.Attempts(stage.Name)
			}
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
	} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
		cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:paused")
	}

	e.addFailedLabel(owner, repo, item.Number, stage.Name)

	comment := fmt.Sprintf(
		"🏭 **Fabrik — stage failed**\n\nStage **%s** failed to complete after %d attempt(s). The issue has been paused (`fabrik:paused`).\n\nTo retry: investigate the failure, make any needed fixes, then remove the `fabrik:paused` label.",
		stage.Name, e.cfg.MaxRetries,
	)
	if dbID, err := e.client.AddComment(owner, repo, item.Number, comment); err != nil {
		e.logf(item.Number, "warn", "could not post escalation comment: %v\n", err)
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

	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.EnginePaused{Repo: repoStr, Number: item.Number, StageName: stage.Name})
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
	} else if err == nil {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), failedLabel)
		}
	}

	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.StageRetryCleared{Repo: repoStr, Number: item.Number, StageName: stage.Name})
	e.store.Apply(itemstate.EngineUnpaused{Repo: repoStr, Number: item.Number, StageName: stage.Name})
	e.store.Apply(itemstate.StageLastAttemptCleared{Repo: repoStr, Number: item.Number, StageName: stage.Name})
	e.store.Apply(itemstate.EngineCyclesCleared{Repo: repoStr, Number: item.Number, StageName: stage.Name})
}

// blockOnInput is called when Claude outputs FABRIK_BLOCKED_ON_INPUT. It pauses
// the issue with fabrik:paused + fabrik:awaiting-input labels so the engine
// knows to auto-unblock when the user responds with a comment.
// It does NOT add a stage:<name>:failed label and does NOT touch Attempts.
func (e *Engine) blockOnInput(item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "block", "stage %q needs user input — pausing with awaiting-input\n", stage.Name)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); err != nil {
		e.logf(item.Number, "warn", "could not add paused label: %v\n", err)
	} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
		cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:paused")
	}
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil {
		e.logf(item.Number, "warn", "could not add awaiting-input label: %v\n", err)
	} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
		cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-input")
	}
}

// unblockAwaitingInput is called when a user comment arrives on an issue that
// was paused via blockOnInput. It removes both labels and clears LastAttemptAt
// so the stage re-runs promptly after comment processing.
func (e *Engine) unblockAwaitingInput(item gh.ProjectItem, stage *stages.Stage) {
	e.logf(item.Number, "unblock", "user comment received — removing awaiting-input pause\n")

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:paused"); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(item.Number, "warn", "could not remove paused label: %v\n", err)
	} else if err == nil {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:paused")
		}
	}
	if err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(item.Number, "warn", "could not remove awaiting-input label: %v\n", err)
	} else if err == nil {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-input")
		}
	}

	// Clear LastAttemptAt so the stage re-runs immediately after comment processing,
	// and reset retry/pause state that may have accumulated before the block.
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.StageLastAttemptCleared{Repo: repoStr, Number: item.Number, StageName: stage.Name})
	e.store.Apply(itemstate.StageRetryCleared{Repo: repoStr, Number: item.Number, StageName: stage.Name})
	e.store.Apply(itemstate.EngineUnpaused{Repo: repoStr, Number: item.Number, StageName: stage.Name})
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

// effortLevelRank lists effort levels from lowest to highest priority.
// Iterating from the end returns the highest-ranked level (max > high > medium > low).
var effortLevelRank = []string{"low", "medium", "high", "max"}

// extractEffortOverride scans item labels for "effort:<level>" labels and returns the
// highest-priority level found. If multiple effort: labels are present, it picks the
// highest-ranked value (max > high > medium > low) and logs a warning listing all found labels.
// Returns "" if no effort: label is found.
func (e *Engine) extractEffortOverride(issueNumber int, labels []string) string {
	const prefix = "effort:"
	found := make(map[string]bool)
	for _, label := range labels {
		if strings.HasPrefix(label, prefix) {
			level := strings.TrimPrefix(label, prefix)
			if level != "" {
				found[level] = true
			}
		}
	}
	if len(found) == 0 {
		return ""
	}
	if len(found) > 1 {
		all := make([]string, 0, len(found))
		for l := range found {
			all = append(all, "effort:"+l)
		}
		e.logf(issueNumber, "warn", "multiple effort: labels found (%s); using highest-ranked\n", strings.Join(all, ", "))
	}
	// Return highest-ranked level present.
	for i := len(effortLevelRank) - 1; i >= 0; i-- {
		if found[effortLevelRank[i]] {
			return effortLevelRank[i]
		}
	}
	// Unknown level — return the single value if only one was found.
	for l := range found {
		return l
	}
	return ""
}

// baseBranchForItem scans item labels for a "base:<branch>" label and returns the
// named branch if it exists on the remote. If multiple base: labels are present, it
// uses the first and logs a warning. If the named branch does not exist on the remote,
// it logs a warning, posts an issue comment, and falls back to DefaultBaseBranch.
// Returns an error only when DefaultBaseBranch itself fails.
func (e *Engine) baseBranchForItem(item gh.ProjectItem, wm *WorktreeManager) (string, error) {
	const prefix = "base:"
	var candidate string
	for _, label := range item.Labels {
		if strings.HasPrefix(label, prefix) {
			branch := strings.TrimPrefix(label, prefix)
			if branch == "" {
				continue
			}
			if candidate == "" {
				candidate = branch
			} else {
				e.logf(item.Number, "warn", "multiple base: labels found, using %q (ignoring %q)\n", candidate, branch)
			}
		}
	}

	if candidate == "" {
		return wm.DefaultBaseBranch()
	}

	wm.mu.Lock()
	exists := wm.branchExists("origin/" + candidate)
	wm.mu.Unlock()
	if exists {
		e.logf(item.Number, "base", "using base branch %q from label\n", candidate)
		return candidate, nil
	}

	// Branch not found — log warning, post comment once, fall back to default.
	e.logf(item.Number, "warn", "base: label branch %q not found on remote, falling back to default\n", candidate)
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	warnKey := fmt.Sprintf("%s/%s#%d:%s", owner, repo, item.Number, candidate)
	if _, alreadyWarned := e.baseBranchWarnedSet.LoadOrStore(warnKey, true); !alreadyWarned {
		body := fmt.Sprintf("🏭 **Fabrik — base branch not found**\n\nFabrik could not find branch `%s` on the remote (from `base:%s` label). Falling back to the repository default branch.", candidate, candidate)
		if dbID, err := e.client.AddComment(owner, repo, item.Number, body); err != nil {
			e.logf(item.Number, "warn", "could not post base branch fallback comment: %v\n", err)
		} else {
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
					DatabaseID: dbID, Body: body, Author: e.cfg.User, CreatedAt: time.Now(),
				})
			}
			// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
			if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
				e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
			}
		}
	}
	return wm.DefaultBaseBranch()
}

// isTransientError reports whether err represents a transient failure that is
// safe to retry: network-layer errors, unexpected EOF on partial responses, HTTP
// 5xx responses from GitHub, or connection-reset/i/o-timeout strings.
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := err.Error()
	if strings.Contains(msg, "GitHub API returned 5") {
		return true
	}
	if strings.Contains(msg, "connection reset") || strings.Contains(msg, "i/o timeout") {
		return true
	}
	return false
}

func (e *Engine) removeEditingLabel(owner, repo string, issueNumber int) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := e.client.RemoveLabelFromIssue(owner, repo, issueNumber, "fabrik:editing")
		if err == nil {
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, issueNumber), "fabrik:editing")
			}
			return
		}
		if errors.Is(err, gh.ErrNotFound) {
			// Label already absent — treat as success.
			return
		}
		if !isTransientError(err) {
			e.logf(issueNumber, "warn", "could not remove editing label: %v\n", err)
			return
		}
		lastErr = err
		delay := editingLabelRetryDelay << attempt
		time.Sleep(delay)
	}
	e.logf(issueNumber, "warn", "could not remove editing label after %d attempts: %v\n", maxAttempts, lastErr)
}

func (e *Engine) removeLockLabel(owner, repo string, issueNumber int, label string) {
	if err := e.client.RemoveLabelFromIssue(owner, repo, issueNumber, label); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(issueNumber, "warn", "could not remove lock label: %v\n", err)
	} else if err == nil {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, issueNumber), label)
		}
	}
}

func (e *Engine) removeInProgressLabel(owner, repo string, issueNumber int, stageName string) {
	label := fmt.Sprintf("stage:%s:in_progress", stageName)
	if err := e.client.RemoveLabelFromIssue(owner, repo, issueNumber, label); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(issueNumber, "warn", "could not remove in_progress label: %v\n", err)
	} else if err == nil {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, issueNumber), label)
		}
	}
}

func (e *Engine) addFailedLabel(owner, repo string, issueNumber int, stageName string) {
	label := fmt.Sprintf("stage:%s:failed", stageName)
	if err := e.client.AddLabelToIssue(owner, repo, issueNumber, label); err != nil {
		e.logf(issueNumber, "warn", "could not add failed label: %v\n", err)
	} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
		cacheImpl.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, issueNumber), label)
	}
}

func (e *Engine) removeFailedLabel(owner, repo string, issueNumber int, stageName string) {
	label := fmt.Sprintf("stage:%s:failed", stageName)
	if err := e.client.RemoveLabelFromIssue(owner, repo, issueNumber, label); err != nil &&
		!errors.Is(err, gh.ErrNotFound) {
		e.logf(issueNumber, "warn", "could not remove failed label: %v\n", err)
	} else if err == nil {
		if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, issueNumber), label)
		}
	}
}

// commitWIP commits any uncommitted changes in the worktree as a WIP commit.
// This preserves partial work when Claude hits max_turns or errors out.
func (e *Engine) commitWIP(workDir string, issueNumber int, stageName string) {
	dirty, err := isWorkingTreeDirty(workDir)
	if err != nil || !dirty {
		return // clean worktree or error checking, nothing to commit
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

// progressBaseline captures observable progress signals at the start of a stage
// invocation. Used by detectProgress to determine whether extension is warranted
// when Claude hits max_turns.
type progressBaseline struct {
	gitHeadSHA          string // HEAD commit SHA in the worktree (Implement, Review)
	commentCount        int    // total comment count on item (Validate)
	resolvedThreadCount int    // resolved PR review threads (Review)
	workingTreeDirty    bool   // true if worktree had uncommitted changes at baseline (Implement)
}

// hasExtendTurnsLabel returns true if item carries the "fabrik:extend-turns" label.
func hasExtendTurnsLabel(item gh.ProjectItem) bool {
	return hasLabel(item, "fabrik:extend-turns")
}

// snapshotBaseline captures observable progress state for stage before the first invocation.
func snapshotBaseline(stage *stages.Stage, item gh.ProjectItem, workDir string) progressBaseline {
	var b progressBaseline
	switch stage.Name {
	case "Implement":
		if sha, err := gitHeadSHA(workDir); err == nil {
			b.gitHeadSHA = sha
		}
		if dirty, err := isWorkingTreeDirty(workDir); err == nil {
			b.workingTreeDirty = dirty
		}
	case "Review":
		if sha, err := gitHeadSHA(workDir); err == nil {
			b.gitHeadSHA = sha
		}
		b.resolvedThreadCount = item.LinkedPRResolvedThreadCount
	case "Validate":
		b.commentCount = len(item.Comments)
	}
	return b
}

// gitHeadSHA runs "git rev-parse HEAD" in dir and returns the trimmed SHA.
func gitHeadSHA(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// isWorkingTreeDirty returns true if dir has uncommitted changes other than
// engine-managed files (.fabrik-context/, .fabrik/issue.md).
func isWorkingTreeDirty(dir string) (bool, error) {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git status --porcelain: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		path := strings.TrimSpace(line[2:])
		if isEngineManagedPath(path) {
			continue
		}
		return true, nil
	}
	return false, nil
}

// detectProgress checks whether measurable progress was made since baseline.
// Implement: new commits OR (baseline was clean AND working tree is now dirty).
// Review: new commits OR resolved reviewer thread count increased (GitHub re-fetch).
// Validate: total comment count increased (GitHub re-fetch).
// All other stages return false — no extension.
// logfFn is called exactly once per invocation with the verdict and evaluated signals.
// If a GitHub re-fetch fails, returns false and the error (conservative: fail as today).
func detectProgress(_ context.Context, stage *stages.Stage, item *gh.ProjectItem, baseline progressBaseline, workDir string, client GitHubClient, logfFn func(tag, format string, args ...any)) (bool, error) {
	switch stage.Name {
	case "Implement":
		sha, err := gitHeadSHA(workDir)
		if err != nil {
			logfFn("extend-turns", "progress check: git HEAD lookup failed: %v, has_progress=false — no extension\n", err)
			return false, err
		}
		if sha != baseline.gitHeadSHA {
			logfFn("extend-turns", "progress check: HEAD %s → %s (new commits), has_progress=true\n", baseline.gitHeadSHA, sha)
			return true, nil
		}
		// HEAD unchanged — check for uncommitted working-tree changes, but only
		// if the baseline was clean. A pre-existing dirty worktree does not count.
		dirty, err := isWorkingTreeDirty(workDir)
		if err != nil {
			logfFn("extend-turns", "progress check: HEAD %s (unchanged), working-tree check failed: %v, has_progress=false — no extension\n", sha, err)
			return false, nil
		}
		if !baseline.workingTreeDirty && dirty {
			logfFn("extend-turns", "progress check: HEAD %s (unchanged), working-tree dirty (baseline was clean), has_progress=true\n", sha)
			return true, nil
		}
		reason := "working-tree clean"
		if baseline.workingTreeDirty {
			reason = "baseline already dirty"
		}
		logfFn("extend-turns", "progress check: HEAD %s (unchanged), %s, has_progress=false — no extension\n", sha, reason)
		return false, nil
	case "Review":
		sha, err := gitHeadSHA(workDir)
		if err != nil {
			logfFn("extend-turns", "progress check: git HEAD lookup failed: %v, has_progress=false — no extension\n", err)
			return false, err
		}
		if sha != baseline.gitHeadSHA {
			logfFn("extend-turns", "progress check: HEAD %s → %s (new commits), has_progress=true\n", baseline.gitHeadSHA, sha)
			return true, nil
		}
		// No new commits — re-fetch to check resolved reviewer threads.
		if err := client.FetchItemDetails(item); err != nil {
			logfFn("extend-turns", "progress check: HEAD %s (unchanged), re-fetch failed: %v, has_progress=false — no extension\n", sha, err)
			return false, fmt.Errorf("re-fetching item for progress check: %w", err)
		}
		progress := item.LinkedPRResolvedThreadCount > baseline.resolvedThreadCount
		if progress {
			logfFn("extend-turns", "progress check: HEAD %s (unchanged), resolved threads %d → %d, has_progress=true\n",
				sha, baseline.resolvedThreadCount, item.LinkedPRResolvedThreadCount)
		} else {
			logfFn("extend-turns", "progress check: HEAD %s (unchanged), resolved threads %d (unchanged), has_progress=false — no extension\n",
				sha, baseline.resolvedThreadCount)
		}
		return progress, nil
	case "Validate":
		if err := client.FetchItemDetails(item); err != nil {
			fetchErr := fmt.Errorf("re-fetching item for progress check: %w", err)
			logfFn("extend-turns", "progress check: comments %d (fetch failed), has_progress=false, err=%v\n", baseline.commentCount, fetchErr)
			return false, fetchErr
		}
		progress := len(item.Comments) > baseline.commentCount
		if progress {
			logfFn("extend-turns", "progress check: comments %d → %d, has_progress=true\n", baseline.commentCount, len(item.Comments))
		} else {
			logfFn("extend-turns", "progress check: comments %d (unchanged), has_progress=false — no extension\n", baseline.commentCount)
		}
		return progress, nil
	}
	logfFn("extend-turns", "progress check: stage %s has no progress signal, has_progress=false\n", stage.Name)
	return false, nil
}
