package engine

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
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
	mu         sync.RWMutex
	assembling bool          // true while building the trial branch; false once PR is open
	bisecting  bool          // true while halving a red batch to isolate the poisoner (ADR-059 D4)
	prNum      int           // draft CI PR number (set after draft PR is created)
	CIResult   TrainCIResult // final CI result (set by pollTrainCI on exit)
	trialName  string        // trial name of the most recent trial (churns during bisection)
	projectID  string        // board project ID for advanceToNextStage (immutable after dispatch)
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

// effectiveMaxTrainRebaseCycles returns the maximum number of main-moved
// rebase+revalidate cycles permitted per merge-train batch, defaulting to 3
// (mirroring the per-issue MaxRebaseCycles default) when unset (≤ 0). Beyond
// this bound, a batch that keeps falling behind its base is dissolved back to
// Queued (ADR-059 D5, FR-2/FR-5).
func (e *Engine) effectiveMaxTrainRebaseCycles() int {
	if e.cfg.MaxTrainRebaseCycles <= 0 {
		return 3
	}
	return e.cfg.MaxTrainRebaseCycles
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
		bisecting := state.bisecting
		prNum := state.prNum
		ciResult := state.CIResult
		state.mu.RUnlock()
		switch {
		case assembling:
			e.logf(0, "merge-train", "train worker already assembling for %s — skipping\n", repoKey)
		case bisecting:
			e.logf(0, "merge-train", "train worker bisecting red batch for %s — skipping\n", repoKey)
		default:
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

// trialParams bundles the immutable per-worker context threaded through the
// assemble / bisect / land helpers so their signatures stay manageable. baseSHA is
// pinned once at batch start (ADR-059 D-b) and is only re-pinned deliberately, per
// singleton, inside landOneAtATime (the sequential-land base advance).
type trialParams struct {
	owner, repo      string
	baseBranch       string
	baseSHA          string
	wm               *WorktreeManager
	holdingStg       *stages.Stage
	maxTurnsOverride int
	nextTrialName    func() string // returns a unique trial name per call (first == base)
}

// runMergeTrainWorker is the main body of the merge-train goroutine (ADR-059 D3/D4).
// It pins the base SHA once, then runs a re-form loop: assemble+validate the (re-formed)
// batch exactly once; a green result lands immediately (D-d — zero bisection on the common
// path); a red result opens a per-episode cost budget and bisects to isolate and eject the
// poisoner (FR-1/FR-2), re-forming the survivors and re-validating (FR-3); cost-cap
// exhaustion or a non-isolable interaction degrades to the one-at-a-time fallback (FR-5).
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

	holdingStg := holdingStage(e.cfg)
	if holdingStg == nil {
		e.logf(0, "merge-train", "no holding stage configured — aborting train\n")
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

	// Unique, monotonic trial-name generator (first call == base name). Every trial —
	// main-loop re-forms and bisection sub-trials — gets a distinct name so their branches,
	// worktrees, and draft CI PRs never collide.
	baseTrialName := fmt.Sprintf("merge-train-%s-%d", sanitizeBranchName(baseBranch), time.Now().Unix())
	trialSeq := 0
	nextTrialName := func() string {
		n := baseTrialName
		if trialSeq > 0 {
			n = fmt.Sprintf("%s-t%d", baseTrialName, trialSeq)
		}
		trialSeq++
		return n
	}

	p := trialParams{
		owner:            owner,
		repo:             repo,
		baseBranch:       baseBranch,
		wm:               wm,
		holdingStg:       holdingStg,
		maxTurnsOverride: maxTurnsOverride,
		nextTrialName:    nextTrialName,
	}

	// FR-1/FR-4: reconstruct durable in-flight state before forming a fresh batch, so
	// a restart with an empty in-memory map resumes / completes / dissolves an existing
	// train instead of starting a duplicate. Reads only durable artifacts; each terminal
	// route clears the in-flight marker itself.
	if e.reconstructTrainState(ctx, state, p, batch) {
		return
	}

	// Pin the base SHA once (ADR-059 D-b) so every trial — the initial batch and every
	// bisection sub-trial — forks off the same base and a red result is attributable to
	// member composition, not a moving base branch. Skipped under the test seam (no git).
	if e.trainValidateFn == nil {
		fetchCmd := exec.Command("git", "fetch", "origin")
		fetchCmd.Dir = wm.baseDir
		fetchCmd.Env = nonInteractiveGitEnv()
		if out, ferr := fetchCmd.CombinedOutput(); ferr != nil {
			e.logf(0, "merge-train", "warn: fetch origin before pinning base failed: %s\n", strings.TrimSpace(string(out)))
		}
		baseSHA, perr := gitRevParse(wm.baseDir, "refs/remotes/origin/"+baseBranch)
		if perr != nil {
			if baseSHA, perr = gitRevParse(wm.baseDir, baseBranch); perr != nil {
				e.logf(0, "merge-train", "cannot pin base SHA for %s: %v\n", repoKey, perr)
				e.mergeTrainInFlight.Delete(repoKey)
				return
			}
		}
		p.baseSHA = baseSHA
		e.logf(0, "merge-train", "pinned base %s (%s) for %s train\n", baseBranch, baseSHA, repoKey)
	}

	// Resolve each member's linked PR number + head SHA once, ejecting fetch failures.
	current := e.fetchTrainMembers(ctx, owner, repo, batch)
	e.logf(0, "merge-train", "assembled %d train member(s) for %s\n", len(current), repoKey)

	// Re-form loop: validate, land-on-green, or bisect-eject-reform on red.
	for {
		if len(current) == 0 {
			e.logf(0, "merge-train", "no survivors remaining for %s — train complete with nothing to land\n", repoKey)
			e.mergeTrainInFlight.Delete(repoKey)
			return
		}

		trialName := p.nextTrialName()
		state.mu.Lock()
		state.trialName = trialName
		state.assembling = true
		state.mu.Unlock()

		survivors, result, prNum, aerr := e.assembleAndValidate(ctx, p, current, trialName)
		if aerr != nil {
			e.logf(0, "merge-train", "assemble/validate failed for %s: %v\n", repoKey, aerr)
			e.cleanupTrialArtifacts(p.wm, trialName)
			e.mergeTrainInFlight.Delete(repoKey)
			return
		}
		if len(survivors) == 0 {
			// Every member was ejected during assembly (unresolvable conflicts).
			e.logf(0, "merge-train", "entire batch ejected during assembly for %s\n", repoKey)
			e.cleanupTrialArtifacts(p.wm, trialName)
			e.mergeTrainInFlight.Delete(repoKey)
			return
		}

		// Hook 1: check runaway guard after the initial re-form trial (ADR-059 D8).
		if count, tripped := e.isRunawayTripped(repoKey); tripped {
			e.cleanupTrialArtifacts(p.wm, trialName)
			e.fireRunawayGuard(ctx, p.owner, p.repo, membersToItems(current), count)
			e.mergeTrainInFlight.Delete(repoKey)
			return
		}

		state.mu.Lock()
		state.prNum = prNum
		state.assembling = false
		state.CIResult = result
		state.mu.Unlock()

		switch result {
		case TrainCIGreen:
			// D-d hard invariant: a green batch lands immediately, zero bisection.
			// landGreenBatch adds the D5 main-moved landing gate (behind → rebase →
			// revalidate → dissolve-on-exhaustion) around landMergeTrainBatch; both
			// terminal paths clear mergeTrainInFlight.
			e.logf(0, "merge-train", "combined Validate green for %s (%d survivor(s)) — landing\n", repoKey, len(survivors))
			e.landGreenBatch(ctx, state, p, survivors)
			return
		case TrainCIPending:
			e.logf(0, "merge-train", "combined Validate pending/timed out for %s — will retry next poll\n", repoKey)
			e.cleanupTrialArtifacts(p.wm, trialName)
			e.mergeTrainInFlight.Delete(repoKey)
			return
		default: // TrainCIRed
			e.logf(0, "merge-train", "combined Validate RED for %s (%d member(s)) — bisecting to isolate the poisoner\n", repoKey, len(survivors))
			// The red trial's artifacts are unneeded; bisection sub-trials build fresh.
			e.cleanupTrialArtifacts(p.wm, trialName)
			state.mu.Lock()
			state.bisecting = true
			state.mu.Unlock()
			nextSurvivors, fellBack, runaway := e.handleRedBatch(ctx, state, p, survivors)
			state.mu.Lock()
			state.bisecting = false
			state.mu.Unlock()
			if runaway {
				// Runaway guard fired inside bisect or landOneAtATime.
				count, _ := e.isRunawayTripped(repoKey)
				e.fireRunawayGuard(ctx, p.owner, p.repo, membersToItems(survivors), count)
				e.mergeTrainInFlight.Delete(repoKey)
				return
			}
			if fellBack {
				// The one-at-a-time fallback already landed/ejected every member.
				e.mergeTrainInFlight.Delete(repoKey)
				return
			}
			current = nextSurvivors // re-form survivors and re-validate (FR-3)
		}
	}
}

// fetchTrainMembers resolves each batch member's linked PR number and head SHA once
// (reused across every bisection trial and the landing step), ejecting any member whose
// linked PR cannot be fetched or has no head SHA. The returned slice preserves batch order.
func (e *Engine) fetchTrainMembers(ctx context.Context, owner, repo string, batch []gh.ProjectItem) []trainMember {
	var members []trainMember
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
		members = append(members, trainMember{item: member, prNum: pr.Number, headSHA: pr.HeadSHA})
	}
	return members
}

// assembleTrialBranch creates a fresh trial worktree forked off the pinned base SHA (D-b)
// and sequentially merges each member's head SHA into it, resolving conflicts via Claude and
// ejecting members whose conflicts are unresolvable. It returns the survivors (members that
// merged or were resolved), the pushed trial branch HEAD SHA, and any fatal error. A zero-
// survivor result returns (nil, "", nil) — the caller handles the terminal.
func (e *Engine) assembleTrialBranch(ctx context.Context, p trialParams, members []trainMember, trialName string) ([]trainMember, string, error) {
	wtDir, err := p.wm.EnsureTrainWorktreeAt(trialName, p.baseSHA)
	if err != nil {
		return nil, "", fmt.Errorf("creating trial worktree: %w", err)
	}

	// No fetch is needed here: the trial worktree shares the bare repo's object database,
	// and the caller has already run `git fetch origin` in wm.baseDir before assembling —
	// the base-SHA pin at worker start (runMergeTrainWorker) covers the main-loop trial and
	// every bisection sub-trial, and landOneAtATime re-pins per singleton — so each member
	// headSHA (an immutable snapshot captured once in fetchTrainMembers) is already a local
	// object resolvable by `git merge <sha>`. Re-fetching per trial would be a wasted network
	// round-trip on every bisection sub-trial. Keep this invariant if the fetch is refactored.

	var survivors []trainMember
	for _, member := range members {
		mergeCmd := exec.Command("git", "merge", "--no-ff", "--no-edit", member.headSHA)
		mergeCmd.Dir = wtDir
		mergeOut, mergeErr := mergeCmd.CombinedOutput()

		if mergeErr == nil {
			survivors = append(survivors, member)
			e.logf(member.item.Number, "merge-train", "merged #%d cleanly into trial branch\n", member.item.Number)
			continue
		}

		// Conflict — attempt Claude resolution.
		e.logf(member.item.Number, "merge-train", "merge conflict for #%d: %s — attempting Claude resolution\n", member.item.Number, strings.TrimSpace(string(mergeOut)))
		opts := InvokeOptions{BaseBranch: p.baseBranch, MaxTurnsOverride: p.maxTurnsOverride}
		if e.resolveConflictWithClaude(ctx, member.item, wtDir, p.holdingStg, member.headSHA, opts) {
			survivors = append(survivors, member)
			e.logf(member.item.Number, "merge-train", "conflict for #%d resolved by Claude\n", member.item.Number)
			continue
		}

		// Unresolvable — abort merge and eject.
		abortCmd := exec.Command("git", "merge", "--abort")
		abortCmd.Dir = wtDir
		abortCmd.CombinedOutput() // best-effort
		e.logf(member.item.Number, "merge-train", "cannot resolve conflict for #%d — ejecting\n", member.item.Number)
		e.ejectMember(ctx, p.owner, p.repo, member.item, fmt.Sprintf("ejected from merge-train batch — unresolvable conflict (PR SHA %s)", member.headSHA))
	}

	if len(survivors) == 0 {
		return nil, "", nil
	}

	if err := p.wm.PushTrainBranch(trialName); err != nil {
		return nil, "", fmt.Errorf("pushing trial branch: %w", err)
	}
	trialSHA, err := gitRevParse(wtDir, "HEAD")
	if err != nil {
		return nil, "", fmt.Errorf("reading trial branch SHA: %w", err)
	}
	return survivors, trialSHA, nil
}

// assembleAndValidate builds a trial branch for members (off the pinned base SHA), opens a
// draft CI PR, and polls the combined Validate. It returns the survivors, the CI result, and
// the draft PR number. The local trial worktree is always removed before returning; the
// remote branch persists to back the draft CI PR and is cleaned up by the caller (or by
// landMergeTrainBatch on a green landing).
//
// When e.trainValidateFn is set (tests), it short-circuits the whole git/CI path and returns
// (members, e.trainValidateFn(ctx, members), 0, nil), keying the result on batch membership
// alone (ADR-059 D4 test seam). This is the ONLY combined validation on the common path — a
// green result must never trigger bisection (D-d).
func (e *Engine) assembleAndValidate(ctx context.Context, p trialParams, members []trainMember, trialName string) ([]trainMember, TrainCIResult, int, error) {
	e.recordTrial(p.owner + "/" + p.repo)
	if e.trainValidateFn != nil {
		return members, e.trainValidateFn(ctx, members), 0, nil
	}

	survivors, trialSHA, err := e.assembleTrialBranch(ctx, p, members, trialName)
	if err != nil {
		return nil, TrainCIPending, 0, err
	}
	if len(survivors) == 0 {
		return nil, TrainCIPending, 0, nil
	}

	// Open a draft CI PR listing the survivors.
	var memberRefs, closesLines []string
	for _, s := range survivors {
		memberRefs = append(memberRefs, fmt.Sprintf("#%d", s.item.Number))
		closesLines = append(closesLines, fmt.Sprintf("Closes #%d", s.item.Number))
	}
	prTitle := fmt.Sprintf("chore(merge-train): trial integration for %s", strings.Join(memberRefs, " "))
	// The draft CI PR IS the landing integration PR (same trial branch → base). It
	// carries mergeTrainBatchMarker so the landing step's findIntegrationPR reuses
	// it (marking it ready) rather than trying to CreatePR a second PR on the same
	// branch — which GitHub rejects with a 422 "a pull request already exists".
	// The "Closes #N" lines link each member issue to this landing PR and auto-close
	// them when it merges (into the default branch), restoring issue↔landing-PR
	// connectivity — the member PRs are closed-not-merged, so their own Closes #N
	// never fires. (A non-default base won't auto-close; the landing step closes the
	// issues explicitly as a fallback.)
	prBody := fmt.Sprintf("🏭 **Fabrik merge-train integration PR** (trial → %s)\n\n"+
		"This is a disposable trial branch combining the following Queued member PRs:\n%s\n\n"+
		"Do not merge this PR manually — Fabrik manages the landing step.\n"+
		"Orphaned integration PRs (if the train worker crashed) can be closed manually via the GitHub UI.\n\n"+
		"%s\n\n%s",
		p.baseBranch, strings.Join(memberRefs, "\n"), strings.Join(closesLines, "\n"), mergeTrainBatchMarker)

	trialBranch := "fabrik/merge-train/" + trialName
	prNum, err := e.client.CreateDraftPR(p.owner, p.repo, prTitle, trialBranch, p.baseBranch, prBody, 0)
	if err != nil {
		return nil, TrainCIPending, 0, fmt.Errorf("creating draft CI PR: %w", err)
	}
	e.logf(0, "merge-train", "opened draft CI PR #%d for %s/%s (%d survivor(s))\n", prNum, p.owner, p.repo, len(survivors))

	// Remove the local worktree — the trial branch now lives on origin.
	if cleanErr := p.wm.CleanupTrainWorktree(trialName, false); cleanErr != nil {
		e.logf(0, "merge-train", "warn: could not clean up local trial worktree %s: %v\n", trialName, cleanErr)
	}

	result := e.pollTrainCI(ctx, p.owner, p.repo, prNum, trialSHA)
	return survivors, result, prNum, nil
}

// bisect recursively halves a known-red member set to isolate the single poisoning member
// (ADR-059 D4 / FR-1), reusing assembleAndValidate for each trial in the bors-ng test order
// (test half A; if red recurse into A; else test half B; if red recurse into B). It returns
// the isolated poisoner, (nil, true, false) when the redness is a non-isolable cross-PR
// interaction (both halves green) or the per-episode cost budget (*used vs costCap) is
// exhausted — either degrades to the FR-5 one-at-a-time fallback (D-e) — or (nil, false, true)
// when the runaway guard fires. red is assumed to be a validated-red set.
func (e *Engine) bisect(ctx context.Context, p trialParams, red []trainMember, used *int, costCap int) (*trainMember, bool, bool) {
	if len(red) == 1 {
		return &red[0], false, false
	}

	repoKey := p.owner + "/" + p.repo
	mid := len(red) / 2
	for _, half := range [][]trainMember{red[:mid], red[mid:]} {
		if *used >= costCap {
			e.logf(0, "merge-train", "bisection cost cap (%d validations) reached — degrading to one-at-a-time fallback\n", costCap)
			return nil, true, false
		}
		trialName := p.nextTrialName()
		survivors, result, _, err := e.assembleAndValidate(ctx, p, half, trialName)
		*used++
		e.cleanupTrialArtifacts(p.wm, trialName)
		if err != nil {
			e.logf(0, "merge-train", "bisection trial failed to assemble: %v — degrading to one-at-a-time fallback\n", err)
			return nil, true, false
		}
		if _, tripped := e.isRunawayTripped(repoKey); tripped {
			return nil, false, true
		}
		if result == TrainCIRed && len(survivors) > 0 {
			return e.bisect(ctx, p, survivors, used, costCap)
		}
	}

	// Both halves green: the redness spans the split — a non-isolable interaction (D-e).
	return nil, true, false
}

// handleRedBatch bisects a red batch to isolate and eject the poisoning member (FR-1/FR-2),
// then returns the surviving members for the main loop to re-form and re-validate (FR-3).
// When bisection cannot isolate a single culprit within the cost budget (a non-isolable
// interaction or cost-cap exhaustion), it degrades to the one-at-a-time fallback (FR-5),
// which lands/ejects every member itself, and returns (nil, true, false). Returns
// (nil, false, true) when the runaway guard fires inside bisect or landOneAtATime. The
// cost budget is per red-batch episode: it starts at 1 (the initial red validation) and
// is capped at effectiveBisectCap().
func (e *Engine) handleRedBatch(ctx context.Context, state *mergeTrainWorkerState, p trialParams, red []trainMember) ([]trainMember, bool, bool) {
	used := 1 // the initial red validation counts toward the per-episode budget
	costCap := e.effectiveBisectCap()

	poisoner, fellBack, runaway := e.bisect(ctx, p, red, &used, costCap)
	if runaway {
		return nil, false, true
	}
	if fellBack {
		e.logf(0, "merge-train", "could not isolate a single poisoner for %s/%s (%d/%d validations used) — degrading to one-at-a-time landing of %d member(s)\n", p.owner, p.repo, used, costCap, len(red))
		runaway = e.landOneAtATime(ctx, state, p, red)
		return nil, true, runaway
	}

	// Eject the isolated poisoner (D-a shared counter, D-c comment, cap→pause reuse).
	e.logf(poisoner.item.Number, "merge-train", "bisection isolated #%d as the batch poisoner — ejecting\n", poisoner.item.Number)
	e.ejectMember(ctx, p.owner, p.repo, poisoner.item,
		fmt.Sprintf("ejected from merge-train — the combined Validate fails whenever #%d is in the batch (isolated by halving bisection). It will be retried in a future train with a different composition.", poisoner.item.Number))

	var survivors []trainMember
	for i := range red {
		if red[i].item.Number != poisoner.item.Number {
			survivors = append(survivors, red[i])
		}
	}
	return survivors, false, false
}

// landOneAtATime is the FR-5 fallback: it validates and lands each member as its own
// singleton batch, which dissolves any cross-PR interaction by construction (no two members
// co-reside). A green singleton lands via landSingleton; a red singleton fails even in
// isolation and is ejected; a pending singleton is left in Queued to retry. Returns true if
// the runaway guard fires during processing. In the real path the base is re-pinned to the
// current origin/<base> before each singleton so a prior land is visible to the next member's
// validation (this is what actually dissolves a genuine interaction); under the test seam this
// git step is skipped (the membership-keyed fn is stateless — see the ADR-059 D4
// landOneAtATime note in docs/state-machine.md).
func (e *Engine) landOneAtATime(ctx context.Context, state *mergeTrainWorkerState, p trialParams, members []trainMember) bool {
	repoKey := p.owner + "/" + p.repo
	e.logf(0, "merge-train", "one-at-a-time fallback: processing %d member(s) as singleton batches\n", len(members))
	for _, m := range members {
		if e.trainValidateFn == nil {
			// Re-pin the base to current origin/<base> so a prior singleton's land is seen.
			fetchCmd := exec.Command("git", "fetch", "origin")
			fetchCmd.Dir = p.wm.baseDir
			fetchCmd.Env = nonInteractiveGitEnv()
			fetchCmd.CombinedOutput() // best-effort
			if sha, rerr := gitRevParse(p.wm.baseDir, "refs/remotes/origin/"+p.baseBranch); rerr == nil {
				p.baseSHA = sha // local copy; persists across this loop, does not leak to caller
			}
		}

		trialName := p.nextTrialName()
		survivors, result, _, err := e.assembleAndValidate(ctx, p, []trainMember{m}, trialName)
		if err != nil || len(survivors) == 0 {
			e.logf(m.item.Number, "merge-train", "could not assemble #%d in isolation: %v — leaving in Queued\n", m.item.Number, err)
			e.cleanupTrialArtifacts(p.wm, trialName)
			continue
		}
		if _, tripped := e.isRunawayTripped(repoKey); tripped {
			e.cleanupTrialArtifacts(p.wm, trialName)
			return true
		}

		switch result {
		case TrainCIGreen:
			e.landSingleton(ctx, state, p, m, trialName)
		case TrainCIRed:
			e.cleanupTrialArtifacts(p.wm, trialName)
			e.logf(m.item.Number, "merge-train", "#%d fails combined Validate even in isolation — ejecting\n", m.item.Number)
			e.ejectMember(ctx, p.owner, p.repo, m.item,
				fmt.Sprintf("ejected from merge-train — #%d fails the combined Validate even when landed alone.", m.item.Number))
		default: // TrainCIPending
			e.cleanupTrialArtifacts(p.wm, trialName)
			e.logf(m.item.Number, "merge-train", "combined Validate pending for singleton #%d — leaving in Queued\n", m.item.Number)
		}
	}
	return false
}

// landSingleton lands a single member from its own validated-green trial branch. It creates a
// dedicated integration PR WITHOUT the shared batch marker — sequential singleton lands must
// not collide on findIntegrationPR (which matches merged PRs via ListPRs state=all), which
// would make a later singleton skip its own merge and advance without landing its code (a
// data-loss bug; see the ADR-059 D4 landSingleton note). It merges the PR, advances the
// member Queued→Done, closes the member's linked PR, and resets its ejection counter.
func (e *Engine) landSingleton(ctx context.Context, state *mergeTrainWorkerState, p trialParams, m trainMember, trialName string) {
	trialBranch := "fabrik/merge-train/" + trialName
	defer e.cleanupTrialArtifacts(p.wm, trialName)

	title := fmt.Sprintf("[merge-train] singleton: #%d", m.item.Number)
	body := fmt.Sprintf("🏭 **Fabrik merge-train singleton landing PR**\n\n"+
		"Lands #%d — %s — one-at-a-time after the batch could not be landed together.\n\n"+
		"Do not merge manually; Fabrik manages the landing step.",
		m.item.Number, m.item.Title)

	prNum, err := e.client.CreatePR(p.owner, p.repo, title, trialBranch, p.baseBranch, body)
	if err != nil {
		e.logf(m.item.Number, "merge-train", "cannot create singleton landing PR for #%d: %v\n", m.item.Number, err)
		return
	}

	if !e.pollForMergeable(ctx, p.owner, p.repo, prNum, []trainMember{m}) {
		return // timeout / dirty — leave in Queued
	}
	if err := e.client.MergePR(p.owner, p.repo, prNum); err != nil {
		e.logf(m.item.Number, "merge-train", "merge of singleton PR #%d failed: %v\n", prNum, err)
		return
	}
	e.logf(m.item.Number, "merge-train", "merged singleton landing PR #%d for #%d\n", prNum, m.item.Number)

	// Advance Queued → Done (unless already Done from a prior partial run).
	if m.item.Status != "Done" {
		board := &gh.ProjectBoard{ProjectID: state.projectID}
		if e.statusField == nil {
			e.logf(m.item.Number, "merge-train", "warn: statusField unavailable — cannot advance #%d to Done\n", m.item.Number)
		} else if advErr := e.advanceToNextStage(board, m.item, p.holdingStg); advErr != nil {
			e.logf(m.item.Number, "merge-train", "warn: could not advance #%d to Done: %v\n", m.item.Number, advErr)
		} else {
			e.logf(m.item.Number, "merge-train", "advanced #%d to Done\n", m.item.Number)
		}
	}

	// Close the member's linked PR with a landing comment.
	if m.prNum != 0 {
		landedComment := fmt.Sprintf("🏭 **Fabrik merge-train** — Landed one-at-a-time via singleton PR #%d.", prNum)
		if _, commentErr := e.client.AddComment(p.owner, p.repo, m.prNum, landedComment); commentErr != nil {
			e.logf(m.item.Number, "merge-train", "warn: could not post landed comment on PR #%d: %v\n", m.prNum, commentErr)
		}
		if closeErr := e.client.CloseIssue(p.owner, p.repo, m.prNum); closeErr != nil {
			e.logf(m.item.Number, "merge-train", "warn: could not close member PR #%d: %v\n", m.prNum, closeErr)
		}
	}

	// Close the member issue. The singleton landing PR's Closes #N auto-closes it on
	// merge into the default branch; this explicit close is the fallback for
	// non-default bases and auto-close lag (idempotent). Without it the issue is
	// left landed-but-open (the member PR is closed-not-merged).
	if closeErr := e.client.CloseIssue(p.owner, p.repo, m.item.Number); closeErr != nil {
		e.logf(m.item.Number, "merge-train", "warn: could not close member issue #%d: %v\n", m.item.Number, closeErr)
	} else {
		e.logf(m.item.Number, "merge-train", "closed member issue #%d\n", m.item.Number)
	}

	e.resetEjectionCount(p.owner, p.repo, m.item.Number)
	e.resetTrialCounter(p.owner + "/" + p.repo)
}

// cleanupTrialArtifacts removes a trial's local worktree and its local+remote branch (which
// implicitly closes the trial's draft CI PR). It is a no-op under the test seam, where no real
// git artifacts exist. Best-effort: failures are logged, not fatal.
func (e *Engine) cleanupTrialArtifacts(wm *WorktreeManager, trialName string) {
	if e.trainValidateFn != nil {
		return
	}
	if err := wm.CleanupTrainWorktree(trialName, true); err != nil {
		e.logf(0, "merge-train", "warn: could not clean up trial %s: %v\n", trialName, err)
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

// effectiveTrialWindow returns the runaway-guard threshold (N) and rolling window (M),
// applying zero-means-default semantics: N=20, M=60min (ADR-059 D8).
func (e *Engine) effectiveTrialWindow() (int, time.Duration) {
	n := e.cfg.MaxTrainTrialsPerWindow
	if n <= 0 {
		n = 20
	}
	m := e.cfg.TrainTrialWindowDuration
	if m <= 0 {
		m = 60 * time.Minute
	}
	return n, m
}

// recordTrial appends a timestamp for repoKey, prunes entries older than the window,
// and returns the current count. Called at the start of every assembleAndValidate.
func (e *Engine) recordTrial(repoKey string) int {
	n, m := e.effectiveTrialWindow()
	now := time.Now()
	cutoff := now.Add(-m)
	e.mergeTrainTrialsMu.Lock()
	ts := e.mergeTrainTrials[repoKey]
	ts = append(ts, now)
	pruned := ts[:0]
	for _, t := range ts {
		if !t.Before(cutoff) {
			pruned = append(pruned, t)
		}
	}
	e.mergeTrainTrials[repoKey] = pruned
	count := len(pruned)
	e.mergeTrainTrialsMu.Unlock()
	_ = n
	return count
}

// isRunawayTripped returns the current pruned trial count for repoKey and whether it has
// reached the threshold. Called after each trial-producing operation.
func (e *Engine) isRunawayTripped(repoKey string) (int, bool) {
	n, m := e.effectiveTrialWindow()
	cutoff := time.Now().Add(-m)
	e.mergeTrainTrialsMu.Lock()
	ts := e.mergeTrainTrials[repoKey]
	pruned := ts[:0]
	for _, t := range ts {
		if !t.Before(cutoff) {
			pruned = append(pruned, t)
		}
	}
	e.mergeTrainTrials[repoKey] = pruned
	count := len(pruned)
	e.mergeTrainTrialsMu.Unlock()
	return count, count >= n
}

// resetTrialCounter clears the trial counter for repoKey after a successful landing,
// so normal poison bisection (where survivors do land) never accumulates toward the cap.
func (e *Engine) resetTrialCounter(repoKey string) {
	e.mergeTrainTrialsMu.Lock()
	delete(e.mergeTrainTrials, repoKey)
	e.mergeTrainTrialsMu.Unlock()
}

// fireRunawayGuard pauses all Queued members for the repo, posts an alert comment on each,
// and logs the event. Called when the trial counter reaches the runaway threshold (ADR-059 D8).
func (e *Engine) fireRunawayGuard(ctx context.Context, owner, repo string, items []gh.ProjectItem, count int) {
	_, m := e.effectiveTrialWindow()
	repoKey := owner + "/" + repo
	e.logf(0, "merge-train", "runaway guard fired for %s: %d trial(s) with zero successful lands within %s — pausing %d Queued member(s)\n",
		repoKey, count, m, len(items))
	for _, item := range items {
		msg := fmt.Sprintf("🏭 **Fabrik merge-train — runaway guard tripped**\n\n"+
			"The merge-train has created **%d trial branches** for `%s` within the last %s "+
			"with **zero successful landings**. This indicates a persistent infra failure "+
			"(e.g. billing-blocked CI, broken base branch, or all required checks erroring) "+
			"rather than a code-composition issue.\n\n"+
			"**Actions taken:** `fabrik:paused` and `fabrik:awaiting-input` applied to all Queued members.\n\n"+
			"**What to do:**\n"+
			"1. Investigate the infra root cause (check GitHub Actions billing, required check configuration, base branch health).\n"+
			"2. Resolve the underlying issue.\n"+
			"3. Manually remove `fabrik:paused` from each affected Queued member to re-enable the merge-train.",
			count, repoKey, m)
		if _, commentErr := e.client.AddComment(owner, repo, item.Number, msg); commentErr != nil {
			e.logf(item.Number, "merge-train", "warn: could not post runaway guard comment: %v\n", commentErr)
		}
		if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); err != nil {
			e.logf(item.Number, "warn", "could not add fabrik:paused (runaway guard): %v\n", err)
		}
		if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-input"); err != nil {
			e.logf(item.Number, "warn", "could not add fabrik:awaiting-input (runaway guard): %v\n", err)
		}
	}
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
	var lines, closesLines []string
	for _, m := range survivors {
		lines = append(lines, fmt.Sprintf("- #%d — %s", m.item.Number, m.item.Title))
		closesLines = append(closesLines, fmt.Sprintf("Closes #%d", m.item.Number))
	}
	// Closes #N links each member issue to this landing PR and auto-closes them on
	// merge into the default branch (member PRs are closed-not-merged, so their own
	// Closes #N never fires). The landing step also closes them explicitly as a
	// fallback for non-default bases.
	return fmt.Sprintf("🏭 **Fabrik merge-train landing PR**\n\n"+
		"This PR lands the following Queued issues via the internal merge train:\n\n%s\n\n"+
		"%s\n\n%s",
		strings.Join(lines, "\n"), strings.Join(closesLines, "\n"), mergeTrainBatchMarker)
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

// reTrainMember matches "#N" issue references in a train PR body.
var reTrainMember = regexp.MustCompile(`#(\d+)`)

// isTrainPR reports whether pr is a Fabrik merge-train PR — either a landing
// integration PR (body carries the batch marker) or a draft CI PR (identified
// only by its fabrik/merge-train/* head branch, which carries no marker).
func isTrainPR(pr gh.PRDetails) bool {
	return strings.Contains(pr.Body, mergeTrainBatchMarker) ||
		strings.HasPrefix(pr.HeadRefName, trainBranchPrefix)
}

// trialNameFromBranch strips the fabrik/merge-train/ prefix from a trial branch
// head ref, returning the bare trial name (e.g. "merge-train-main-123"). Returns
// "" when headRef is not a merge-train branch.
func trialNameFromBranch(headRef string) string {
	if !strings.HasPrefix(headRef, trainBranchPrefix) {
		return ""
	}
	return strings.TrimPrefix(headRef, trainBranchPrefix)
}

// parseTrainMembers extracts the distinct member issue numbers referenced as "#N"
// in a train PR body, preserving first-seen order.
func parseTrainMembers(body string) []int {
	var nums []int
	seen := map[int]bool{}
	for _, m := range reTrainMember.FindAllStringSubmatch(body, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || seen[n] {
			continue
		}
		seen[n] = true
		nums = append(nums, n)
	}
	return nums
}

// filterBatchByNumbers returns the subset of batch whose issue numbers appear in
// nums, preserving batch (entry) order. Used to intersect a reconstructed PR's
// parsed members with the still-Queued snapshot.
func filterBatchByNumbers(batch []gh.ProjectItem, nums []int) []gh.ProjectItem {
	want := make(map[int]bool, len(nums))
	for _, n := range nums {
		want[n] = true
	}
	var out []gh.ProjectItem
	for _, it := range batch {
		if want[it.Number] {
			out = append(out, it)
		}
	}
	return out
}

// trialBehind reports whether the trial branch has fallen behind its base branch —
// i.e. main advanced (via an external direct push) since the trial forked (ADR-059
// D5, FR-2). It uses the PR-independent GitHub compare API (FetchCommitsBehind), so
// it works under the membership-keyed test seam via the mocked fetchCommitsBehindFn.
func (e *Engine) trialBehind(owner, repo, baseBranch, trialBranch string) bool {
	behind, err := e.client.FetchCommitsBehind(owner, repo, baseBranch, trialBranch)
	if err != nil {
		e.logf(0, "merge-train", "warn: FetchCommitsBehind(%s...%s) failed: %v — assuming up to date\n", baseBranch, trialBranch, err)
		return false
	}
	return behind > 0
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
		e.logf(0, "merge-train", "found existing integration PR #%d (merged=%v, draft=%v) for %s\n", integrationPRNum, alreadyMerged, integrationPR.Draft, repoKey)
		// The reused PR is the trial's draft CI PR — promote it to ready-for-review
		// so it can be merged (GitHub refuses to merge a draft).
		if integrationPR.Draft && !alreadyMerged {
			if rerr := e.client.MarkPRReady(owner, repo, integrationPRNum); rerr != nil {
				e.logf(0, "merge-train", "cannot mark integration PR #%d ready for %s: %v — leaving members in Queued\n", integrationPRNum, repoKey, rerr)
				return
			}
			e.logf(0, "merge-train", "marked integration PR #%d ready for landing (%s)\n", integrationPRNum, repoKey)
		}
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

		// Close the member issue. The integration PR's Closes #N auto-closes it on
		// merge into the default branch; this explicit close is the fallback for
		// non-default bases and any auto-close lag (idempotent — no-op if already
		// closed). The member PR is closed-not-merged, so its own Closes #N never
		// fires — without this the issue is left landed-but-open.
		if closeErr := e.client.CloseIssue(owner, repo, m.item.Number); closeErr != nil {
			e.logf(m.item.Number, "merge-train", "warn: could not close member issue #%d: %v\n", m.item.Number, closeErr)
		} else {
			e.logf(m.item.Number, "merge-train", "closed member issue #%d\n", m.item.Number)
		}

		// Reset ejection counter: this member has landed; prior ejection history
		// from earlier trains must not count toward the pause cap on future trains.
		e.resetEjectionCount(owner, repo, m.item.Number)
	}
	e.resetTrialCounter(repoKey)
	e.logf(0, "merge-train", "landing complete for %s (integration PR #%d, %d members)\n", repoKey, integrationPRNum, len(survivors))
}

// dissolveBatch tears down an in-flight batch and returns every member to the
// Queued column untouched (ADR-059 D5, FR-5). It closes the integration/CI PR (if
// open), deletes the trial branch locally and on origin, posts an explanatory
// comment on each member so the outcome is observable, and clears the in-flight
// marker. Members are never status-rolled-back — they only advance to Done on a
// successful landing, so leaving them in Queued needs no mutation. The next poll
// re-snapshots Queued and forms a fresh train.
//
// Idempotent: CloseIssue on an already-closed PR and CleanupTrainWorktree on an
// already-deleted branch are best-effort no-ops, so a crash mid-dissolve is safe
// to retry (the explanatory comment may double-post — acceptable and observable).
func (e *Engine) dissolveBatch(ctx context.Context, state *mergeTrainWorkerState, p trialParams, prNum int, trialName string, members []gh.ProjectItem, reason string) {
	_ = ctx
	repoKey := p.owner + "/" + p.repo
	e.logf(0, "merge-train", "dissolving batch for %s (%s) — %d member(s) remain in Queued\n", repoKey, reason, len(members))

	// Close the integration/CI PR if we have one (PRs are issues; no ClosePR).
	if prNum != 0 {
		if err := e.client.CloseIssue(p.owner, p.repo, prNum); err != nil {
			e.logf(0, "merge-train", "warn: could not close PR #%d during dissolve: %v\n", prNum, err)
		}
	}

	// Delete the trial branch locally and on origin (also closes the draft CI PR).
	if trialName != "" {
		e.cleanupTrialArtifacts(p.wm, trialName)
	}

	// Post an explanatory comment on each member for observability (FR-5).
	msg := fmt.Sprintf("🏭 **Fabrik merge-train — batch dissolved**\n\n%s\n\n"+
		"This issue remains in the Queued column, untouched, and will be picked up by a fresh train on the next poll.",
		reason)
	for _, m := range members {
		if _, err := e.client.AddComment(p.owner, p.repo, m.Number, msg); err != nil {
			e.logf(m.Number, "merge-train", "warn: could not post dissolve comment: %v\n", err)
		}
	}

	e.mergeTrainInFlight.Delete(repoKey)
}

// membersToItems projects a []trainMember down to the underlying []gh.ProjectItem.
func membersToItems(members []trainMember) []gh.ProjectItem {
	items := make([]gh.ProjectItem, len(members))
	for i, m := range members {
		items[i] = m.item
	}
	return items
}

// landGreenBatch is the landing gate with main-moved recovery (ADR-059 D5,
// FR-2/FR-6). Before merging, it checks whether the validated-green trial branch
// has fallen behind its base (an external direct push advanced main). If up to
// date, it delegates to landMergeTrainBatch unchanged. If behind, it re-pins the
// base to the current origin/<base> and re-assembles+re-validates the survivors
// off the new base (reusing assembleAndValidate, which invokes Claude conflict
// resolution for FR-6), bounded by MaxTrainRebaseCycles. On a green re-validation
// it loops back to the gate; on exhaustion, a non-green re-validation, or an
// assembly wipeout it dissolves the batch (FR-5) and lets the next poll re-form.
//
// The rebase path is deliberately disjoint from red-batch bisection: a red
// re-validation here dissolves rather than bisecting, so the rebase-cycle budget
// and the bisection cost cap never interact (compose-not-duplicate).
func (e *Engine) landGreenBatch(ctx context.Context, state *mergeTrainWorkerState, p trialParams, survivors []trainMember) {
	maxCycles := e.effectiveMaxTrainRebaseCycles()
	cycles := 0

	for {
		state.mu.RLock()
		trialName := state.trialName
		prNum := state.prNum
		state.mu.RUnlock()
		trialBranch := trainBranchPrefix + trialName

		if !e.trialBehind(p.owner, p.repo, p.baseBranch, trialBranch) {
			// Up to date: land via the unchanged terminal path (clears the map).
			e.landMergeTrainBatch(ctx, state, p.owner, p.repo, p.baseBranch, survivors, p.wm)
			return
		}

		// Main moved under the batch.
		if cycles >= maxCycles {
			e.dissolveBatch(ctx, state, p, prNum, trialName, membersToItems(survivors),
				fmt.Sprintf("the base branch advanced under the batch and it still could not catch up after %d rebase attempt(s) (main-moved rebase limit)", maxCycles))
			return
		}
		cycles++
		e.logf(0, "merge-train", "trial %s is behind %s (main moved) — rebasing off the new base (cycle %d/%d)\n", trialName, p.baseBranch, cycles, maxCycles)

		// Re-pin the base to the current origin/<base> so the re-assembly forks off
		// the advanced main (skipped under the test seam — no real git).
		if e.trainValidateFn == nil {
			fetchCmd := exec.Command("git", "fetch", "origin")
			fetchCmd.Dir = p.wm.baseDir
			fetchCmd.Env = nonInteractiveGitEnv()
			fetchCmd.CombinedOutput() // best-effort
			if sha, rerr := gitRevParse(p.wm.baseDir, "refs/remotes/origin/"+p.baseBranch); rerr == nil {
				p.baseSHA = sha // local copy; the loop reuses it, no leak to caller
			}
		}

		// Clean up the now-stale (behind) trial before building the next one.
		oldTrialName := trialName
		newTrialName := p.nextTrialName()
		state.mu.Lock()
		state.trialName = newTrialName
		state.assembling = true
		state.mu.Unlock()

		newSurvivors, result, newPRNum, aerr := e.assembleAndValidate(ctx, p, survivors, newTrialName)
		e.cleanupTrialArtifacts(p.wm, oldTrialName)

		state.mu.Lock()
		state.prNum = newPRNum
		state.assembling = false
		state.CIResult = result
		state.mu.Unlock()

		if aerr != nil || len(newSurvivors) == 0 {
			e.dissolveBatch(ctx, state, p, newPRNum, newTrialName, membersToItems(survivors),
				"the base branch advanced and the batch could not be re-assembled onto it")
			return
		}
		if result != TrainCIGreen {
			// A red/pending re-validation after a rebase dissolves (disjoint from
			// bisection); the next poll re-forms a fresh train that bisects cleanly.
			e.dissolveBatch(ctx, state, p, newPRNum, newTrialName, membersToItems(newSurvivors),
				"the base branch advanced and the re-validated batch was no longer green")
			return
		}
		survivors = newSurvivors // green off the new base — loop back to the gate
	}
}

// reconstructTrainState makes the per-repo in-flight guard durable and restart-safe
// (ADR-059 D5, FR-1/FR-4). Running inside the already-guarded worker goroutine
// (after LoadOrStore, before base pinning), it probes durable artifacts — merge-train
// PRs (via ListPRs) and fabrik/merge-train/* origin branches (via ls-remote) — and
// routes based on the train PR whose members are still in the current Queued snapshot
// (historical PRs from prior completed batches are skipped so they cannot abort or
// corrupt today's fresh batch). It returns true only when it has fully handled an
// in-flight batch (each such terminal route clears the in-flight marker):
//
//   - merged landing PR (batch marker) with members still Queued → complete the
//     deferred member lifecycle (idempotent landMergeTrainBatch advancement);
//   - open train PR backed by an origin branch → resume (poll CI, then land with
//     main-moved recovery);
//   - open PR (with still-Queued members) without a backing branch → dissolve (FR-5);
//   - nothing relevant, or only orphaned remnants → clean any orphaned trial branches
//     silently and return false so the caller forms a fresh train this poll.
//
// It reads only durable state (never the in-flight map) and never launches a
// goroutine, so it survives a restart with an empty map without a duplicate worker.
func (e *Engine) reconstructTrainState(ctx context.Context, state *mergeTrainWorkerState, p trialParams, batch []gh.ProjectItem) bool {
	repoKey := p.owner + "/" + p.repo

	prs, err := e.client.ListPRs(p.owner, p.repo)
	if err != nil {
		e.logf(0, "merge-train", "reconstruct: ListPRs failed for %s: %v — proceeding fresh\n", repoKey, err)
		return false
	}

	// Select the first train PR *relevant to the current Queued members*. ListPRs
	// returns state=all, so it also surfaces merged/closed integration PRs from prior
	// completed batches; those still carry the batch marker but have no members left
	// in today's Queued snapshot (members only leave Queued on a successful land).
	// Reconstructing from such a historical PR would wrongly abort today's fresh batch
	// (complete-deferred finds no still-Queued members and exits early), permanently
	// stalling the train after the first landing — so skip it. A stale *open* train PR
	// (no still-Queued members) is closed and its branch cleaned so it cannot later
	// hijack findIntegrationPR during a fresh batch's landing.
	var trainPR *gh.PRDetails
	for i := range prs {
		if !isTrainPR(prs[i]) {
			continue
		}
		if len(filterBatchByNumbers(batch, parseTrainMembers(prs[i].Body))) > 0 {
			trainPR = &prs[i]
			break
		}
		if prs[i].State == "open" {
			e.logf(0, "merge-train", "reconstruct: closing stale open train PR #%d (no members still Queued) for %s\n", prs[i].Number, repoKey)
			if cerr := e.client.CloseIssue(p.owner, p.repo, prs[i].Number); cerr != nil {
				e.logf(0, "merge-train", "warn: could not close stale train PR #%d: %v\n", prs[i].Number, cerr)
			}
			if tn := trialNameFromBranch(prs[i].HeadRefName); tn != "" {
				e.cleanupTrialArtifacts(p.wm, tn)
			}
		}
	}

	// Probe origin branches. Skipped under the test seam (no real git); tests drive
	// reconstruction through listPRsFn and treat an open PR's branch as present.
	var originBranches []string
	if e.trainValidateFn == nil {
		if b, berr := p.wm.ListTrainBranchesOnOrigin(); berr != nil {
			e.logf(0, "merge-train", "reconstruct: ls-remote failed for %s: %v\n", repoKey, berr)
		} else {
			originBranches = b
		}
	}

	// Route 1: a merged landing PR (batch marker) with still-Queued members → complete
	// the deferred landing (already-landed work is never dropped; checked first).
	if trainPR != nil && trainPR.Merged && strings.Contains(trainPR.Body, mergeTrainBatchMarker) {
		e.completeDeferredLanding(ctx, state, p, *trainPR, batch)
		return true
	}

	// Route 2: an open train PR with still-Queued members.
	if trainPR != nil && trainPR.State == "open" {
		trialName := trialNameFromBranch(trainPR.HeadRefName)
		branchPresent := e.trainValidateFn != nil || // seam: treat as present
			(trialName != "" && containsBranch(originBranches, trainBranchPrefix+trialName))
		if trialName != "" && branchPresent {
			e.resumeTrain(ctx, state, p, *trainPR, trialName, batch)
			return true
		}
		// Open PR without a backing trial branch → orphan → dissolve. Comment only on
		// this PR's own members (never on unrelated fresh Queued items).
		members := filterBatchByNumbers(batch, parseTrainMembers(trainPR.Body))
		e.dissolveBatch(ctx, state, p, trainPR.Number, trialName, members,
			"reconstruct: found an open integration PR without a backing trial branch after a restart")
		return true
	}

	// Route 3: orphaned trial branch(es) on origin but no relevant train PR — a crash
	// remnant. Clean them up SILENTLY and proceed fresh: dissolving with today's members
	// would post confusing "batch dissolved" comments on unrelated fresh Queued items,
	// and returning true would abort today's batch. Returning false lets the current
	// batch form on this poll (a fresh trial gets a new, unique branch name — no clash).
	for _, b := range originBranches {
		if tn := trialNameFromBranch(b); tn != "" {
			e.logf(0, "merge-train", "reconstruct: cleaning up orphaned trial branch %s for %s\n", tn, repoKey)
			e.cleanupTrialArtifacts(p.wm, tn)
		}
	}
	return false
}

// completeDeferredLanding finishes a landing that merged before a crash but whose
// members are still in Queued (ADR-059 D5, FR-4). It parses the merged PR's member
// list, intersects it with the still-Queued snapshot, and runs the idempotent
// landMergeTrainBatch advancement (which finds the already-merged PR, skips the
// merge, and advances each still-Queued member to Done).
func (e *Engine) completeDeferredLanding(ctx context.Context, state *mergeTrainWorkerState, p trialParams, pr gh.PRDetails, batch []gh.ProjectItem) {
	repoKey := p.owner + "/" + p.repo
	items := filterBatchByNumbers(batch, parseTrainMembers(pr.Body))
	if len(items) == 0 {
		e.logf(0, "merge-train", "reconstruct: merged integration PR #%d for %s has no still-Queued members — nothing to complete\n", pr.Number, repoKey)
		e.mergeTrainInFlight.Delete(repoKey)
		return
	}
	e.logf(0, "merge-train", "reconstruct: completing deferred landing for %s from merged PR #%d (%d still-Queued member(s))\n", repoKey, pr.Number, len(items))

	state.mu.Lock()
	state.trialName = trialNameFromBranch(pr.HeadRefName)
	state.mu.Unlock()

	survivors := e.fetchTrainMembers(ctx, p.owner, p.repo, items)
	if len(survivors) == 0 {
		e.logf(0, "merge-train", "reconstruct: no member PRs resolvable for deferred landing of %s — clearing\n", repoKey)
		e.mergeTrainInFlight.Delete(repoKey)
		return
	}
	// landMergeTrainBatch re-finds the merged marker PR, skips FR-2, advances members,
	// and clears the in-flight marker in its deferred cleanup.
	e.landMergeTrainBatch(ctx, state, p.owner, p.repo, p.baseBranch, survivors, p.wm)
}

// resumeTrain re-establishes an in-flight batch from an open train PR after a
// restart (ADR-059 D5, FR-4). It re-resolves the still-Queued members, polls CI on
// the existing trial head, and — on green — lands via landGreenBatch (with
// main-moved recovery). Any non-green outcome (red, pending, or no resolvable
// members) dissolves the batch so the next poll re-forms a fresh, clean train
// rather than re-entering bisection on resume.
func (e *Engine) resumeTrain(ctx context.Context, state *mergeTrainWorkerState, p trialParams, pr gh.PRDetails, trialName string, batch []gh.ProjectItem) {
	repoKey := p.owner + "/" + p.repo
	items := filterBatchByNumbers(batch, parseTrainMembers(pr.Body))
	if len(items) == 0 {
		e.dissolveBatch(ctx, state, p, pr.Number, trialName, items,
			"reconstruct: an open train PR had no members still in Queued after a restart")
		return
	}
	survivors := e.fetchTrainMembers(ctx, p.owner, p.repo, items)
	if len(survivors) == 0 {
		e.dissolveBatch(ctx, state, p, pr.Number, trialName, items,
			"reconstruct: could not resolve any member PRs while resuming the batch")
		return
	}

	state.mu.Lock()
	state.trialName = trialName
	state.prNum = pr.Number
	state.assembling = false
	state.mu.Unlock()

	e.logf(0, "merge-train", "reconstruct: resuming train for %s from open PR #%d (trial %s, %d member(s))\n", repoKey, pr.Number, trialName, len(survivors))

	var result TrainCIResult
	if e.trainValidateFn != nil {
		result = e.trainValidateFn(ctx, survivors)
	} else {
		result = e.pollTrainCI(ctx, p.owner, p.repo, pr.Number, pr.HeadSHA)
	}

	if result == TrainCIGreen {
		e.landGreenBatch(ctx, state, p, survivors)
		return
	}
	e.dissolveBatch(ctx, state, p, pr.Number, trialName, items,
		"reconstruct: the resumed trial did not validate green — re-forming a fresh train")
}

// containsBranch reports whether branch is in slice.
func containsBranch(slice []string, branch string) bool {
	for _, v := range slice {
		if v == branch {
			return true
		}
	}
	return false
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
