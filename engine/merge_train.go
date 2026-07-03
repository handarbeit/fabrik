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

	// Pin the base SHA once (ADR-059 D-b) so every trial — the initial batch and every
	// bisection sub-trial — forks off the same base and a red result is attributable to
	// member composition, not a moving base branch. Skipped under the test seam (no git).
	baseSHA := ""
	if e.trainValidateFn == nil {
		fetchCmd := exec.Command("git", "fetch", "origin")
		fetchCmd.Dir = wm.baseDir
		fetchCmd.Env = nonInteractiveGitEnv()
		if out, ferr := fetchCmd.CombinedOutput(); ferr != nil {
			e.logf(0, "merge-train", "warn: fetch origin before pinning base failed: %s\n", strings.TrimSpace(string(out)))
		}
		baseSHA, err = gitRevParse(wm.baseDir, "refs/remotes/origin/"+baseBranch)
		if err != nil {
			if baseSHA, err = gitRevParse(wm.baseDir, baseBranch); err != nil {
				e.logf(0, "merge-train", "cannot pin base SHA for %s: %v\n", repoKey, err)
				e.mergeTrainInFlight.Delete(repoKey)
				return
			}
		}
		e.logf(0, "merge-train", "pinned base %s (%s) for %s train\n", baseBranch, baseSHA, repoKey)
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
		baseSHA:          baseSHA,
		wm:               wm,
		holdingStg:       holdingStg,
		maxTurnsOverride: maxTurnsOverride,
		nextTrialName:    nextTrialName,
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

		state.mu.Lock()
		state.prNum = prNum
		state.assembling = false
		state.CIResult = result
		state.mu.Unlock()

		switch result {
		case TrainCIGreen:
			// D-d hard invariant: a green batch lands immediately, zero bisection.
			e.logf(0, "merge-train", "combined Validate green for %s (%d survivor(s)) — landing\n", repoKey, len(survivors))
			e.landMergeTrainBatch(ctx, state, owner, repo, baseBranch, survivors, wm)
			// landMergeTrainBatch clears mergeTrainInFlight via its deferred cleanup.
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
			nextSurvivors, fellBack := e.handleRedBatch(ctx, state, p, survivors)
			state.mu.Lock()
			state.bisecting = false
			state.mu.Unlock()
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

	// Fetch all objects so git merge <sha> can resolve member commits.
	fetchCmd := exec.Command("git", "fetch", "origin")
	fetchCmd.Dir = wtDir
	fetchCmd.Env = nonInteractiveGitEnv()
	if out, ferr := fetchCmd.CombinedOutput(); ferr != nil {
		e.logf(0, "merge-train", "warn: fetch origin in trial worktree failed: %s\n", strings.TrimSpace(string(out)))
	}

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
	var memberRefs []string
	for _, s := range survivors {
		memberRefs = append(memberRefs, fmt.Sprintf("#%d", s.item.Number))
	}
	prTitle := fmt.Sprintf("chore(merge-train): trial integration for %s", strings.Join(memberRefs, " "))
	prBody := fmt.Sprintf("🏭 **Fabrik merge-train integration PR** (trial → %s)\n\n"+
		"This is a disposable trial branch combining the following Queued member PRs:\n%s\n\n"+
		"Do not merge this PR manually — Fabrik manages the landing step.\n"+
		"Orphaned integration PRs (if the train worker crashed) can be closed manually via the GitHub UI.",
		p.baseBranch, strings.Join(memberRefs, "\n"))

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
// the isolated poisoner, or (nil, true) when the redness is a non-isolable cross-PR
// interaction (both halves green) or the per-episode cost budget (*used vs costCap) is
// exhausted — either of which degrades to the FR-5 one-at-a-time fallback (D-e). red is
// assumed to be a validated-red set (its redness established by the caller).
func (e *Engine) bisect(ctx context.Context, p trialParams, red []trainMember, used *int, costCap int) (*trainMember, bool) {
	if len(red) == 1 {
		return &red[0], false
	}

	mid := len(red) / 2
	for _, half := range [][]trainMember{red[:mid], red[mid:]} {
		if *used >= costCap {
			e.logf(0, "merge-train", "bisection cost cap (%d validations) reached — degrading to one-at-a-time fallback\n", costCap)
			return nil, true
		}
		trialName := p.nextTrialName()
		survivors, result, _, err := e.assembleAndValidate(ctx, p, half, trialName)
		*used++
		e.cleanupTrialArtifacts(p.wm, trialName)
		if err != nil {
			e.logf(0, "merge-train", "bisection trial failed to assemble: %v — degrading to one-at-a-time fallback\n", err)
			return nil, true
		}
		if result == TrainCIRed && len(survivors) > 0 {
			return e.bisect(ctx, p, survivors, used, costCap)
		}
	}

	// Both halves green: the redness spans the split — a non-isolable interaction (D-e).
	return nil, true
}

// handleRedBatch bisects a red batch to isolate and eject the poisoning member (FR-1/FR-2),
// then returns the surviving members for the main loop to re-form and re-validate (FR-3).
// When bisection cannot isolate a single culprit within the cost budget (a non-isolable
// interaction or cost-cap exhaustion), it degrades to the one-at-a-time fallback (FR-5),
// which lands/ejects every member itself, and returns (nil, true). The cost budget is
// per red-batch episode: it starts at 1 (the initial red validation) and is capped at
// effectiveBisectCap().
func (e *Engine) handleRedBatch(ctx context.Context, state *mergeTrainWorkerState, p trialParams, red []trainMember) ([]trainMember, bool) {
	used := 1 // the initial red validation counts toward the per-episode budget
	costCap := e.effectiveBisectCap()

	poisoner, fellBack := e.bisect(ctx, p, red, &used, costCap)
	if fellBack {
		e.logf(0, "merge-train", "could not isolate a single poisoner for %s/%s (%d/%d validations used) — degrading to one-at-a-time landing of %d member(s)\n", p.owner, p.repo, used, costCap, len(red))
		e.landOneAtATime(ctx, state, p, red)
		return nil, true
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
	return survivors, false
}

// landOneAtATime is the FR-5 fallback: it validates and lands each member as its own
// singleton batch, which dissolves any cross-PR interaction by construction (no two members
// co-reside). A green singleton lands via landSingleton; a red singleton fails even in
// isolation and is ejected; a pending singleton is left in Queued to retry. In the real path
// the base is re-pinned to the current origin/<base> before each singleton so a prior land is
// visible to the next member's validation (this is what actually dissolves a genuine
// interaction); under the test seam this git step is skipped (the membership-keyed fn is
// stateless — see the ADR-059 D4 landOneAtATime note in docs/state-machine.md).
func (e *Engine) landOneAtATime(ctx context.Context, state *mergeTrainWorkerState, p trialParams, members []trainMember) {
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

	e.resetEjectionCount(p.owner, p.repo, m.item.Number)
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
