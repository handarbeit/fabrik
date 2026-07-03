package engine

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TrainCIResult is the typed outcome of the combined-Validate CI poll.
type TrainCIResult int

const (
	TrainCIPending TrainCIResult = iota // CI not yet resolved (timeout or still running)
	TrainCIGreen                        // all required checks passed
	TrainCIRed                          // at least one required check failed
)

// trainMember pairs a batch member's ProjectItem with its linked PR number and head SHA.
// Both are fetched once (fetchTrainMembers) and reused across every bisection trial and
// the landing step to avoid extra API calls.
type trainMember struct {
	item    gh.ProjectItem
	prNum   int
	headSHA string
}

// mergeTrainWorkerState tracks an in-flight or completed merge-train worker.
// Stored in Engine.mergeTrainInFlight keyed by "owner/repo".
// mu guards all fields that the poll loop reads while the goroutine writes them.
type mergeTrainWorkerState struct {
	mu             sync.RWMutex
	assembling     bool          // true while building the trial branch; false once PR is open
	bisecting      bool          // true while halving a red batch to isolate the poisoner (ADR-059 D4)
	prNum          int           // draft CI PR number (set after draft PR is created)
	trialBranchSHA string        // HEAD SHA of the trial branch (set after push)
	CIResult       TrainCIResult // final CI result (set by pollTrainCI on exit)
	trialName      string        // trial name of the most recent trial (churns during bisection)
	projectID      string        // board project ID for advanceToNextStage (immutable after dispatch)
}

// sanitizeBranchName replaces characters that are invalid in directory names
// (particularly '/') so trialName can be used as both a directory segment and
// as the suffix of the trial branch name.
func sanitizeBranchName(s string) string {
	return strings.ReplaceAll(s, "/", "-")
}

// ceilLog2 returns ⌈log₂(n)⌉ for n ≥ 1 and 0 for n ≤ 1. It is the number of
// halving levels needed to bisect a set of n members down to a singleton, and
// underpins the default bisection cost cap (ADR-059 D-f).
func ceilLog2(n int) int {
	if n <= 1 {
		return 0
	}
	bits := 0
	for v := n - 1; v > 0; v >>= 1 {
		bits++
	}
	return bits
}

// effectiveMaxBatchSize returns the configured MaxBatchSize, defaulting to 5
// (ADR-059 D-f) when unset (≤ 0). This caps how many Queued items are snapshotted
// into a single merge-train batch (FR-4).
func (e *Engine) effectiveMaxBatchSize() int {
	if e.cfg.MaxBatchSize <= 0 {
		return 5
	}
	return e.cfg.MaxBatchSize
}

// effectiveBisectCap returns the maximum number of combined validations permitted
// per red-batch episode (the initial red validation plus all bisection trial
// validations), defaulting to 2·⌈log₂(max_batch_size)⌉ + 1 (ADR-059 D-f) when unset
// (≤ 0). Beyond this cap, a red batch degrades to the one-at-a-time fallback (FR-5).
// The default is derived from the configured max_batch_size, not the actual batch
// length, per FR-5.
func (e *Engine) effectiveBisectCap() int {
	if e.cfg.MaxBisectValidations > 0 {
		return e.cfg.MaxBisectValidations
	}
	return 2*ceilLog2(e.effectiveMaxBatchSize()) + 1
}

// capBatch returns the first max items of the batch, preserving entry order
// (ADR-059 D2 / FR-4). max ≤ 0 means no cap. Capping to the first N bounds the
// worst-case bisection cost if the batch turns out red.
func capBatch(items []gh.ProjectItem, max int) []gh.ProjectItem {
	if max <= 0 || len(items) <= max {
		return items
	}
	return items[:max]
}

// dispatchMergeTrainWorker checks whether a train worker is already in-flight for
// the batch's repo and, if not, starts one. Safe to call from the poll goroutine.
// projectID is the GitHub project board ID, threaded so landMergeTrainBatch can
// call advanceToNextStage without fetching the board again.
func (e *Engine) dispatchMergeTrainWorker(ctx context.Context, batch []gh.ProjectItem, projectID string) {
	if len(batch) == 0 {
		return
	}
	owner, repo := itemOwnerRepo(batch[0], e.defaultRepo())
	repoKey := owner + "/" + repo

	// Use LoadOrStore so the check-and-register is atomic: two concurrent callers
	// can never both pass the "not loaded" path and launch duplicate workers.
	candidate := &mergeTrainWorkerState{assembling: true, projectID: projectID}
	existing, loaded := e.mergeTrainInFlight.LoadOrStore(repoKey, candidate)
	if loaded {
		state := existing.(*mergeTrainWorkerState)
		state.mu.RLock()
		assembling := state.assembling
		prNum := state.prNum
		ciResult := state.CIResult
		state.mu.RUnlock()
		if assembling {
			e.logf(0, "merge-train", "train worker already assembling for %s — skipping\n", repoKey)
		} else {
			switch ciResult {
			case TrainCIGreen:
				e.logf(0, "merge-train", "train CI green for %s (PR #%d) — awaiting landing step\n", repoKey, prNum)
			case TrainCIRed:
				e.logf(0, "merge-train", "train CI red for %s (PR #%d) — needs attention\n", repoKey, prNum)
			default:
				e.logf(0, "merge-train", "train CI pending for %s (PR #%d) — still polling\n", repoKey, prNum)
			}
		}
		return
	}

	// candidate was atomically stored — launch the worker with it.
	state := candidate
	e.wg.Add(1)

	go func() {
		defer e.wg.Done()
		e.runMergeTrainWorker(ctx, state, owner, repo, batch)
	}()
}

// runMergeTrainWorker is the main body of the merge-train goroutine.
// It acquires the semaphore, assembles the trial branch, resolves conflicts,
// pushes, opens a draft integration PR, polls CI, and records the result.
func (e *Engine) runMergeTrainWorker(ctx context.Context, state *mergeTrainWorkerState, owner, repo string, batch []gh.ProjectItem) {
	repoKey := owner + "/" + repo

	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		e.logf(0, "merge-train", "context cancelled before semaphore acquired for %s\n", repoKey)
		e.mergeTrainInFlight.Delete(repoKey)
		return
	}
	defer func() { <-e.sem }()

	// Use batch[0] as the repo anchor for ensureRepoReady.
	if err := e.ensureRepoReady(ctx, batch[0]); err != nil {
		if errors.Is(err, ErrSkipItem) {
			e.logf(0, "merge-train", "repo %s not ready, aborting train\n", repoKey)
		} else {
			e.logf(0, "merge-train", "ensureRepoReady failed for %s: %v\n", repoKey, err)
		}
		e.mergeTrainInFlight.Delete(repoKey)
		return
	}

	wm := e.worktreesFor(repoKey)
	baseBranch, err := wm.DefaultBaseBranch()
	if err != nil {
		e.logf(0, "merge-train", "cannot determine base branch for %s: %v\n", repoKey, err)
		e.mergeTrainInFlight.Delete(repoKey)
		return
	}

	// Generate trial name here (after baseBranch is known) so the branch name
	// reflects the target base (e.g. "merge-train-main-1751234567"), matching FR-1.
	trialName := fmt.Sprintf("merge-train-%s-%d", sanitizeBranchName(baseBranch), time.Now().Unix())
	state.trialName = trialName
	e.logf(0, "merge-train", "building trial branch %q for %s (%d members)\n", trialName, repoKey, len(batch))

	wtDir, err := wm.EnsureTrainWorktree(trialName, baseBranch)
	if err != nil {
		e.logf(0, "merge-train", "cannot create trial worktree for %s: %v\n", repoKey, err)
		e.mergeTrainInFlight.Delete(repoKey)
		return
	}

	// Fetch all objects so git merge <sha> can resolve them.
	fetchCmd := exec.Command("git", "fetch", "origin")
	fetchCmd.Dir = wtDir
	fetchCmd.Env = nonInteractiveGitEnv()
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		e.logf(0, "merge-train", "warn: fetch origin in trial worktree failed: %s\n", strings.TrimSpace(string(out)))
	}

	holdingStg := holdingStage(e.cfg)
	if holdingStg == nil {
		e.logf(0, "merge-train", "no holding stage configured — aborting train\n")
		wm.CleanupTrainWorktree(trialName, true) // best-effort
		e.mergeTrainInFlight.Delete(repoKey)
		return
	}

	// Check if any batch member has fabrik:extend-turns — if so, double max_turns.
	extendTurns := false
	for _, m := range batch {
		if hasLabel(m, "fabrik:extend-turns") {
			extendTurns = true
			break
		}
	}
	maxTurnsOverride := 0
	if extendTurns && holdingStg.MaxTurns > 0 {
		maxTurnsOverride = holdingStg.MaxTurns * 2
	}

	// Sequential merge loop — builds trainMembers with the member PR number stored.
	var survivors []trainMember
	for _, member := range batch {
		pr, fetchErr := e.client.FetchLinkedPR(owner, repo, member.Number)
		if fetchErr != nil || pr == nil {
			e.logf(member.Number, "merge-train", "cannot fetch linked PR for #%d: %v — ejecting\n", member.Number, fetchErr)
			e.ejectMember(ctx, owner, repo, member, fmt.Sprintf("ejected from merge-train — could not fetch linked PR: %v", fetchErr))
			continue
		}
		if pr.HeadSHA == "" {
			e.logf(member.Number, "merge-train", "#%d has no PR head SHA — ejecting\n", member.Number)
			e.ejectMember(ctx, owner, repo, member, "ejected from merge-train — linked PR has no head SHA")
			continue
		}

		mergeCmd := exec.Command("git", "merge", "--no-ff", "--no-edit", pr.HeadSHA)
		mergeCmd.Dir = wtDir
		mergeOut, mergeErr := mergeCmd.CombinedOutput()

		if mergeErr == nil {
			// Clean merge.
			survivors = append(survivors, trainMember{item: member, prNum: pr.Number})
			e.logf(member.Number, "merge-train", "merged #%d cleanly into trial branch\n", member.Number)
			continue
		}

		// Conflict — attempt Claude resolution.
		e.logf(member.Number, "merge-train", "merge conflict for #%d: %s — attempting Claude resolution\n", member.Number, strings.TrimSpace(string(mergeOut)))
		opts := InvokeOptions{
			BaseBranch:       baseBranch,
			MaxTurnsOverride: maxTurnsOverride,
		}
		resolved := e.resolveConflictWithClaude(ctx, member, wtDir, holdingStg, pr.HeadSHA, opts)
		if resolved {
			survivors = append(survivors, trainMember{item: member, prNum: pr.Number})
			e.logf(member.Number, "merge-train", "conflict for #%d resolved by Claude\n", member.Number)
			continue
		}

		// Unresolvable — abort merge and eject.
		abortCmd := exec.Command("git", "merge", "--abort")
		abortCmd.Dir = wtDir
		abortCmd.CombinedOutput() // best-effort
		e.logf(member.Number, "merge-train", "cannot resolve conflict for #%d — ejecting\n", member.Number)
		e.ejectMember(ctx, owner, repo, member, fmt.Sprintf("ejected from merge-train batch — unresolvable conflict (PR SHA %s)", pr.HeadSHA))
	}

	// FR-6: zero survivors.
	if len(survivors) == 0 {
		e.logf(0, "merge-train", "entire batch ejected, %d member(s) need attention\n", len(batch))
		wm.CleanupTrainWorktree(trialName, true) // best-effort
		e.mergeTrainInFlight.Delete(repoKey)
		return
	}

	// Push trial branch to origin.
	if err := wm.PushTrainBranch(trialName); err != nil {
		e.logf(0, "merge-train", "cannot push trial branch for %s: %v\n", repoKey, err)
		wm.CleanupTrainWorktree(trialName, true) // best-effort
		e.mergeTrainInFlight.Delete(repoKey)
		return
	}

	// Capture trial branch HEAD SHA.
	trialSHA, err := gitRevParse(wtDir, "HEAD")
	if err != nil {
		e.logf(0, "merge-train", "cannot read trial branch SHA for %s: %v\n", repoKey, err)
		wm.CleanupTrainWorktree(trialName, true) // best-effort
		e.mergeTrainInFlight.Delete(repoKey)
		return
	}
	state.mu.Lock()
	state.trialBranchSHA = trialSHA
	state.mu.Unlock()

	// Build draft CI PR body listing survivors.
	var memberRefs []string
	for _, s := range survivors {
		memberRefs = append(memberRefs, fmt.Sprintf("#%d", s.item.Number))
	}
	prTitle := fmt.Sprintf("chore(merge-train): trial integration for %s", strings.Join(memberRefs, " "))
	prBody := fmt.Sprintf("🏭 **Fabrik merge-train integration PR** (trial → %s)\n\n"+
		"This is a disposable trial branch combining the following Queued member PRs:\n%s\n\n"+
		"Do not merge this PR manually — Fabrik manages the landing step.\n"+
		"Orphaned integration PRs (if the train worker crashed) can be closed manually via the GitHub UI.",
		baseBranch, strings.Join(memberRefs, "\n"))

	trialBranch := "fabrik/merge-train/" + trialName
	prNum, err := e.client.CreateDraftPR(owner, repo, prTitle, trialBranch, baseBranch, prBody, 0)
	if err != nil {
		e.logf(0, "merge-train", "cannot create integration PR for %s: %v\n", repoKey, err)
		wm.CleanupTrainWorktree(trialName, true) // best-effort
		e.mergeTrainInFlight.Delete(repoKey)
		return
	}
	state.mu.Lock()
	state.prNum = prNum
	state.assembling = false
	state.mu.Unlock()
	e.logf(0, "merge-train", "opened draft CI PR #%d for %s (%d survivors)\n", prNum, repoKey, len(survivors))

	// Clean up local worktree — trial branch is now on origin.
	if cleanErr := wm.CleanupTrainWorktree(trialName, false); cleanErr != nil {
		e.logf(0, "merge-train", "warn: could not clean up trial worktree for %s: %v\n", repoKey, cleanErr)
	}

	// Poll CI — this blocks inside the goroutine.
	ciResult := e.pollTrainCI(ctx, owner, repo, prNum, trialSHA)
	state.mu.Lock()
	state.CIResult = ciResult
	state.mu.Unlock()

	switch ciResult {
	case TrainCIGreen:
		e.logf(0, "merge-train", "CI green for draft PR #%d (%s) — proceeding to landing\n", prNum, repoKey)
		e.landMergeTrainBatch(ctx, state, owner, repo, baseBranch, survivors, wm)
		// landMergeTrainBatch calls e.mergeTrainInFlight.Delete via its deferred cleanup.
	case TrainCIRed:
		e.logf(0, "merge-train", "CI red for draft PR #%d (%s) — batch needs attention\n", prNum, repoKey)
		// Clear so the next poll cycle can dispatch a fresh train rather than
		// logging "needs attention" forever with no way to restart.
		e.mergeTrainInFlight.Delete(repoKey)
	default:
		e.logf(0, "merge-train", "CI timed out / pending for draft PR #%d (%s)\n", prNum, repoKey)
		// Same: clear so a new worker can rebuild the trial branch on the next poll.
		e.mergeTrainInFlight.Delete(repoKey)
	}
}

// buildTrainConflictComment constructs a synthetic comment instructing Claude to
// resolve merge conflict markers in the current worktree (inline, without a rebase).
func buildTrainConflictComment(memberItem gh.ProjectItem, prSHA, baseBranch string) gh.Comment {
	body := fmt.Sprintf(
		"🏭 **Fabrik merge-train — conflict resolution required**\n\n"+
			"The merge of PR head `%s` (issue #%d) into the trial integration branch has left "+
			"conflict markers in the working tree. Resolve them and commit the resolution.\n\n"+
			"**Instructions:**\n"+
			"1. Run `git status` to identify conflicted files.\n"+
			"2. Open each conflicted file and resolve every `<<<<<<< / ======= / >>>>>>>` marker.\n"+
			"   Resolve **semantically** — understand what each side contributes and produce the "+
			"correct merged result (do not blindly pick one side).\n"+
			"   Watch for **semantic collisions** (two PRs chose the same counter value, migration "+
			"ID, or ADR number): keep both contributions with the correct identifiers.\n"+
			"3. `git add -A` to stage all resolved files.\n"+
			"4. `git commit -m \"chore(merge-train): resolve conflict for #%d\"` to finalize.\n"+
			"5. Run the project's build + test commands (`go build ./...` and `go vet ./...` at minimum).\n"+
			"6. **Do NOT emit `FABRIK_STAGE_COMPLETE`.** The merge-train engine takes over after resolution.\n\n"+
			"If the conflict cannot be resolved safely (ambiguous intent, requires human judgment), "+
			"abort with `git merge --abort` and explain in your response why resolution is not possible.\n",
		prSHA, memberItem.Number, memberItem.Number,
	)
	_ = baseBranch
	return gh.Comment{
		ID:         "merge-train-conflict-synthetic",
		DatabaseID: 0,
		Body:       body,
		Author:     "fabrik",
	}
}

// resolveConflictWithClaude invokes Claude inline to resolve merge conflicts in the
// trial branch worktree. Returns true if resolution succeeded (no conflict markers remain
// and the resolution is committed).
func (e *Engine) resolveConflictWithClaude(ctx context.Context, memberItem gh.ProjectItem, trainWorkDir string, holdingStg *stages.Stage, prSHA string, opts InvokeOptions) bool {
	comment := buildTrainConflictComment(memberItem, prSHA, opts.BaseBranch)

	output, _, _, err := e.claude.InvokeForComments(ctx, holdingStg, memberItem, []gh.Comment{comment}, trainWorkDir, opts)
	if err != nil {
		e.logf(memberItem.Number, "merge-train", "Claude conflict resolution failed: %v\n", err)
		return false
	}
	_ = output

	// Check whether conflicts remain after Claude's work.
	// Parse line-by-line to avoid false positives from file paths containing
	// "UU", "AA", or "DD" as substrings. Also covers additional unmerged states.
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = trainWorkDir
	statusOut, _ := statusCmd.CombinedOutput()
	for _, line := range strings.Split(string(statusOut), "\n") {
		if len(line) >= 2 {
			code := line[:2]
			if code == "UU" || code == "AA" || code == "DD" ||
				code == "AU" || code == "UD" || code == "UA" || code == "DU" {
				e.logf(memberItem.Number, "merge-train", "conflict markers remain after Claude resolution\n")
				return false
			}
		}
	}

	// Check that there are no staged conflict markers in the diff.
	diffCmd := exec.Command("git", "diff", "--check")
	diffCmd.Dir = trainWorkDir
	if out, diffErr := diffCmd.CombinedOutput(); diffErr != nil {
		e.logf(memberItem.Number, "merge-train", "git diff --check reports conflicts: %s\n", strings.TrimSpace(string(out)))
		return false
	}

	// Verify git considers merge done (index clean or committed).
	checkMergeHead := exec.Command("git", "rev-parse", "--verify", "MERGE_HEAD")
	checkMergeHead.Dir = trainWorkDir
	if err := checkMergeHead.Run(); err == nil {
		// MERGE_HEAD still exists — merge not committed. Try to commit now.
		e.logf(memberItem.Number, "merge-train", "MERGE_HEAD still present after Claude resolution — attempting commit\n")
		addCmd := exec.Command("git", "add", "-A")
		addCmd.Dir = trainWorkDir
		addCmd.CombinedOutput()
		commitCmd := exec.Command("git", "commit", "--no-edit", "-m",
			fmt.Sprintf("chore(merge-train): resolve conflict for #%d", memberItem.Number))
		commitCmd.Dir = trainWorkDir
		if out, commitErr := commitCmd.CombinedOutput(); commitErr != nil {
			e.logf(memberItem.Number, "merge-train", "could not commit resolution: %s\n", strings.TrimSpace(string(out)))
			return false
		}
	}

	return true
}

// ejectMember posts an ejection comment on the member issue, increments the ejection
// counter, and pauses the member after MaxMergeTrainEjections.
func (e *Engine) ejectMember(ctx context.Context, owner, repo string, memberItem gh.ProjectItem, reason string) {
	_ = ctx
	msg := fmt.Sprintf("🏭 **Fabrik merge-train — ejected**\n\n%s\n\n"+
		"This issue remains in the Queued column and will be retried in a future train with a different composition.",
		reason)
	if _, commentErr := e.client.AddComment(owner, repo, memberItem.Number, msg); commentErr != nil {
		e.logf(memberItem.Number, "merge-train", "warn: could not post ejection comment: %v\n", commentErr)
	}

	counterKey := fmt.Sprintf("%s/%s#%d", owner, repo, memberItem.Number)
	e.mergeTrainEjectionsMu.Lock()
	e.mergeTrainEjectionCounts[counterKey]++
	count := e.mergeTrainEjectionCounts[counterKey]
	e.mergeTrainEjectionsMu.Unlock()

	maxEjections := e.cfg.MaxMergeTrainEjections
	if maxEjections <= 0 {
		maxEjections = 3
	}
	if count >= maxEjections {
		// Reset the counter so that if the user manually unpauses the issue,
		// it gets a fresh set of N attempts before being paused again.
		e.mergeTrainEjectionsMu.Lock()
		e.mergeTrainEjectionCounts[counterKey] = 0
		e.mergeTrainEjectionsMu.Unlock()

		e.logf(memberItem.Number, "merge-train", "#%d ejected %d time(s) — pausing\n", memberItem.Number, count)
		pauseMsg := fmt.Sprintf("🏭 **Fabrik merge-train — pausing after %d ejections**\n\n"+
			"This issue has been ejected from the merge-train %d consecutive times. "+
			"Manual intervention is required. Remove `fabrik:paused` after resolving the underlying conflict.",
			count, count)
		if _, err := e.client.AddComment(owner, repo, memberItem.Number, pauseMsg); err != nil {
			e.logf(memberItem.Number, "merge-train", "warn: could not post pause comment: %v\n", err)
		}
		if err := e.client.AddLabelToIssue(owner, repo, memberItem.Number, "fabrik:paused"); err != nil {
			e.logf(memberItem.Number, "warn", "could not add fabrik:paused: %v\n", err)
		}
		if err := e.client.AddLabelToIssue(owner, repo, memberItem.Number, "fabrik:awaiting-input"); err != nil {
			e.logf(memberItem.Number, "warn", "could not add fabrik:awaiting-input: %v\n", err)
		}
	}
}

// resetEjectionCount zeroes the per-member ejection counter after a successful landing
// so ejection history from a prior train doesn't count toward the pause cap on a future train.
func (e *Engine) resetEjectionCount(owner, repo string, memberNum int) {
	counterKey := fmt.Sprintf("%s/%s#%d", owner, repo, memberNum)
	e.mergeTrainEjectionsMu.Lock()
	delete(e.mergeTrainEjectionCounts, counterKey)
	e.mergeTrainEjectionsMu.Unlock()
}

// mergeTrainBatchMarker is the idempotency marker embedded in integration PR bodies.
const mergeTrainBatchMarker = "<!-- fabrik-merge-train-batch -->"

// buildIntegrationPRTitle returns the title for the landing integration PR.
func buildIntegrationPRTitle(survivors []trainMember) string {
	parts := make([]string, len(survivors))
	for i, m := range survivors {
		parts[i] = fmt.Sprintf("#%d", m.item.Number)
	}
	return "[merge-train] batch: " + strings.Join(parts, ", ")
}

// buildIntegrationPRBody returns the body for the landing integration PR.
// Includes the idempotency marker and a human-readable member list.
func buildIntegrationPRBody(survivors []trainMember) string {
	var lines []string
	for _, m := range survivors {
		lines = append(lines, fmt.Sprintf("- #%d — %s", m.item.Number, m.item.Title))
	}
	return fmt.Sprintf("🏭 **Fabrik merge-train landing PR**\n\n"+
		"This PR lands the following Queued issues via the internal merge train:\n\n%s\n\n"+
		"%s",
		strings.Join(lines, "\n"), mergeTrainBatchMarker)
}

// findIntegrationPR searches recent PRs for an existing landing integration PR
// for this batch (idempotency check for restarts). Returns the first PR whose
// body contains the batch marker, or nil if none is found.
func (e *Engine) findIntegrationPR(owner, repo string) (*gh.PRDetails, error) {
	prs, err := e.client.ListPRs(owner, repo)
	if err != nil {
		return nil, fmt.Errorf("listing PRs for integration PR search: %w", err)
	}
	for i := range prs {
		if strings.Contains(prs[i].Body, mergeTrainBatchMarker) {
			return &prs[i], nil
		}
	}
	return nil, nil
}

// pollForMergeable polls the integration PR until its mergeable_state is "clean" or
// "unstable" (per gh.MergeableStateAccepted), blocking up to CIWaitTimeout.
// Returns true when the PR is ready to merge.
// On timeout, posts a warning comment on the first batch member issue and returns false.
func (e *Engine) pollForMergeable(ctx context.Context, owner, repo string, prNum int, survivors []trainMember) bool {
	ciWaitTimeout := e.cfg.CIWaitTimeout
	if ciWaitTimeout <= 0 {
		ciWaitTimeout = 30 * time.Minute
	}
	deadline := time.Now().Add(ciWaitTimeout)

	for {
		select {
		case <-ctx.Done():
			e.logf(0, "merge-train", "context cancelled while polling integration PR #%d for mergeability\n", prNum)
			return false
		default:
		}

		if time.Now().After(deadline) {
			break
		}

		_, mergeableState, err := e.client.FetchPRMergeableFields(owner, repo, prNum)
		if err != nil {
			e.logf(0, "merge-train", "warn: FetchPRMergeableFields failed for integration PR #%d: %v\n", prNum, err)
		} else if gh.MergeableStateAccepted(mergeableState) {
			return true
		} else if mergeableState == "dirty" {
			e.logf(0, "merge-train", "integration PR #%d has merge conflict (dirty) — cannot land\n", prNum)
			return false
		}

		if time.Now().After(deadline) {
			break
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(30 * time.Second):
		}
	}

	e.logf(0, "merge-train", "timed out waiting for integration PR #%d to become mergeable\n", prNum)
	if len(survivors) > 0 {
		msg := fmt.Sprintf("🏭 **Fabrik merge-train — landing timeout**\n\n"+
			"Timed out waiting for integration PR #%d to reach a mergeable state (`clean` or `unstable`). "+
			"Batch members remain in the Queued column and will be retried in the next train cycle.\n\n"+
			"Possible causes: branch protection checks are slow, or the base branch has advanced "+
			"and the integration PR needs rebasing.",
			prNum)
		if _, commentErr := e.client.AddComment(owner, repo, survivors[0].item.Number, msg); commentErr != nil {
			e.logf(0, "merge-train", "warn: could not post timeout comment: %v\n", commentErr)
		}
	}
	return false
}

// landMergeTrainBatch executes FR-1 through FR-5 after a green CI result:
// opens (or finds) the integration PR, polls until mergeable, merges, advances
// each member to Done and closes their PRs, then cleans up trial artifacts.
// baseBranch is the target branch for the integration PR (already known from runMergeTrainWorker).
func (e *Engine) landMergeTrainBatch(ctx context.Context, state *mergeTrainWorkerState, owner, repo, baseBranch string, survivors []trainMember, wm *WorktreeManager) {
	repoKey := owner + "/" + repo
	trialName := state.trialName

	defer func() {
		// FR-4: cleanup trial worktree and remote branch regardless of landing outcome.
		if cleanErr := wm.CleanupTrainWorktree(trialName, true); cleanErr != nil {
			e.logf(0, "merge-train", "warn: cleanup trial worktree for %s failed: %v\n", repoKey, cleanErr)
		}
		e.mergeTrainInFlight.Delete(repoKey)
	}()

	trialBranch := "fabrik/merge-train/" + trialName

	// FR-1 / FR-5: find or create the landing integration PR.
	integrationPR, err := e.findIntegrationPR(owner, repo)
	if err != nil {
		e.logf(0, "merge-train", "cannot search for existing integration PR for %s: %v\n", repoKey, err)
		return
	}

	var integrationPRNum int
	var alreadyMerged bool

	if integrationPR != nil {
		integrationPRNum = integrationPR.Number
		alreadyMerged = integrationPR.Merged
		e.logf(0, "merge-train", "found existing integration PR #%d (merged=%v) for %s\n", integrationPRNum, alreadyMerged, repoKey)
	} else {
		// FR-1: open the landing integration PR (not a draft).
		title := buildIntegrationPRTitle(survivors)
		body := buildIntegrationPRBody(survivors)
		integrationPRNum, err = e.client.CreatePR(owner, repo, title, trialBranch, baseBranch, body)
		if err != nil {
			e.logf(0, "merge-train", "cannot create integration PR for %s: %v\n", repoKey, err)
			return
		}
		e.logf(0, "merge-train", "opened integration PR #%d for %s (%d survivors)\n", integrationPRNum, repoKey, len(survivors))
	}

	// FR-2: poll until mergeable, then merge (skip if already merged).
	if !alreadyMerged {
		if !e.pollForMergeable(ctx, owner, repo, integrationPRNum, survivors) {
			// Timeout or dirty — leave members in Queued.
			return
		}

		if err := e.client.MergePR(owner, repo, integrationPRNum); err != nil {
			e.logf(0, "merge-train", "merge of integration PR #%d failed: %v\n", integrationPRNum, err)
			msg := fmt.Sprintf("🏭 **Fabrik merge-train — merge failure**\n\n"+
				"Failed to merge integration PR #%d: %v\n\n"+
				"Batch members remain in the Queued column. Manual intervention may be required.",
				integrationPRNum, err)
			if len(survivors) > 0 {
				if _, commentErr := e.client.AddComment(owner, repo, survivors[0].item.Number, msg); commentErr != nil {
					e.logf(0, "merge-train", "warn: could not post merge-failure comment: %v\n", commentErr)
				}
			}
			return
		}
		e.logf(0, "merge-train", "merged integration PR #%d for %s\n", integrationPRNum, repoKey)
	}

	// FR-3: advance each member from Queued → Done and close their PR.
	holdingStg := holdingStage(e.cfg)
	if holdingStg == nil {
		e.logf(0, "merge-train", "no holding stage — cannot advance members to Done\n")
		return
	}
	board := &gh.ProjectBoard{ProjectID: state.projectID}

	for _, m := range survivors {
		// Skip members already in Done column (restart safety).
		if m.item.Status == "Done" {
			e.logf(m.item.Number, "merge-train", "#%d already in Done column — skipping\n", m.item.Number)
			// Still reset the ejection counter: this member landed successfully.
			e.resetEjectionCount(owner, repo, m.item.Number)
			continue
		}

		// Advance Queued → Done.
		if e.statusField == nil {
			e.logf(m.item.Number, "merge-train", "warn: statusField not available — cannot advance #%d to Done\n", m.item.Number)
		} else if advErr := e.advanceToNextStage(board, m.item, holdingStg); advErr != nil {
			e.logf(m.item.Number, "merge-train", "warn: could not advance #%d to Done: %v\n", m.item.Number, advErr)
		} else {
			e.logf(m.item.Number, "merge-train", "advanced #%d to Done\n", m.item.Number)
		}

		// Close member PR with a comment citing the integration PR.
		if m.prNum != 0 {
			landedComment := fmt.Sprintf("🏭 **Fabrik merge-train** — Landed via batch PR #%d.", integrationPRNum)
			if _, commentErr := e.client.AddComment(owner, repo, m.prNum, landedComment); commentErr != nil {
				e.logf(m.item.Number, "merge-train", "warn: could not post landed comment on PR #%d: %v\n", m.prNum, commentErr)
			}
			if closeErr := e.client.CloseIssue(owner, repo, m.prNum); closeErr != nil {
				e.logf(m.item.Number, "merge-train", "warn: could not close member PR #%d: %v\n", m.prNum, closeErr)
			} else {
				e.logf(m.item.Number, "merge-train", "closed member PR #%d\n", m.prNum)
			}
		}

		// Reset ejection counter: this member has landed; prior ejection history
		// from earlier trains must not count toward the pause cap on future trains.
		e.resetEjectionCount(owner, repo, m.item.Number)
	}
	e.logf(0, "merge-train", "landing complete for %s (integration PR #%d, %d members)\n", repoKey, integrationPRNum, len(survivors))
}

// pollTrainCI polls the integration PR's required CI checks, returning the typed result.
// Blocks until the result is known or the CIWaitTimeout elapses.
func (e *Engine) pollTrainCI(ctx context.Context, owner, repo string, prNum int, trialSHA string) TrainCIResult {
	ciWaitTimeout := e.cfg.CIWaitTimeout
	if ciWaitTimeout <= 0 {
		ciWaitTimeout = 30 * time.Minute
	}
	deadline := time.Now().Add(ciWaitTimeout)

	for {
		select {
		case <-ctx.Done():
			e.logf(0, "merge-train", "context cancelled during CI poll for integration PR #%d\n", prNum)
			return TrainCIPending
		default:
		}

		if time.Now().After(deadline) {
			e.logf(0, "merge-train", "CI wait timeout for integration PR #%d\n", prNum)
			return TrainCIPending
		}

		// ADR-033 shortcut: check mergeable_state first.
		_, mergeableState, err := e.client.FetchPRMergeableFields(owner, repo, prNum)
		if err != nil {
			e.logf(0, "merge-train", "warn: FetchPRMergeableFields failed for PR #%d: %v\n", prNum, err)
		} else if gh.MergeableStateAccepted(mergeableState) {
			return TrainCIGreen
		} else if mergeableState == "dirty" {
			return TrainCIRed
		}

		// Check individual check runs.
		checkRuns, err := e.client.FetchCheckRuns(owner, repo, trialSHA)
		if err != nil {
			e.logf(0, "merge-train", "warn: FetchCheckRuns failed for %s: %v\n", trialSHA, err)
		} else if len(checkRuns) > 0 {
			var pending, failed int
			for _, cr := range checkRuns {
				switch cr.Status {
				case "queued", "in_progress":
					pending++
				case "completed":
					switch cr.Conclusion {
					case "failure", "timed_out", "action_required":
						failed++
					}
				}
			}
			if failed > 0 {
				return TrainCIRed
			}
			if pending == 0 {
				return TrainCIGreen
			}
		}

		// Check deadline again before the sleep so a short CIWaitTimeout doesn't
		// block unnecessarily in the poll interval when the deadline has already elapsed.
		if time.Now().After(deadline) {
			e.logf(0, "merge-train", "CI wait timeout for integration PR #%d\n", prNum)
			return TrainCIPending
		}

		// Poll again after 30 seconds.
		select {
		case <-ctx.Done():
			return TrainCIPending
		case <-time.After(30 * time.Second):
		}
	}
}
