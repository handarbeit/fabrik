package engine

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
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

// mergeTrainWorkerState tracks an in-flight or completed merge-train worker.
// Stored in Engine.mergeTrainInFlight keyed by "owner/repo".
type mergeTrainWorkerState struct {
	assembling     bool          // true while building the trial branch; false once PR is open
	prNum          int           // integration PR number (set after PR is created)
	trialBranchSHA string        // HEAD SHA of the trial branch (set after push)
	CIResult       TrainCIResult // final CI result (set by pollTrainCI on exit)
	trialName      string        // unique trial name (e.g. "merge-train-main-1751234567")
}

// sanitizeBranchName replaces characters that are invalid in directory names
// (particularly '/') so trialName can be used as both a directory segment and
// as the suffix of the trial branch name.
func sanitizeBranchName(s string) string {
	return strings.ReplaceAll(s, "/", "-")
}

// dispatchMergeTrainWorker checks whether a train worker is already in-flight for
// the batch's repo and, if not, starts one. Safe to call from the poll goroutine.
func (e *Engine) dispatchMergeTrainWorker(ctx context.Context, batch []gh.ProjectItem) {
	if len(batch) == 0 {
		return
	}
	owner, repo := itemOwnerRepo(batch[0], e.defaultRepo())
	repoKey := owner + "/" + repo

	if existing, loaded := e.mergeTrainInFlight.Load(repoKey); loaded {
		state := existing.(*mergeTrainWorkerState)
		if state.assembling {
			e.logf(0, "merge-train", "train worker already assembling for %s — skipping\n", repoKey)
		} else {
			switch state.CIResult {
			case TrainCIGreen:
				e.logf(0, "merge-train", "train CI green for %s (PR #%d) — awaiting landing step\n", repoKey, state.prNum)
			case TrainCIRed:
				e.logf(0, "merge-train", "train CI red for %s (PR #%d) — needs attention\n", repoKey, state.prNum)
			default:
				e.logf(0, "merge-train", "train CI pending for %s (PR #%d) — still polling\n", repoKey, state.prNum)
			}
		}
		return
	}

	trialName := fmt.Sprintf("merge-train-%s-%d", sanitizeBranchName(repo), time.Now().Unix())
	state := &mergeTrainWorkerState{
		assembling: true,
		trialName:  trialName,
	}
	e.mergeTrainInFlight.Store(repoKey, state)
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

	trialName := state.trialName
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

	// Sequential merge loop.
	var survivors []gh.ProjectItem
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
			survivors = append(survivors, member)
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
			survivors = append(survivors, member)
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
	state.trialBranchSHA = trialSHA

	// Build integration PR body listing survivors.
	var memberRefs []string
	for _, s := range survivors {
		memberRefs = append(memberRefs, fmt.Sprintf("#%d", s.Number))
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
	state.prNum = prNum
	state.assembling = false
	e.logf(0, "merge-train", "opened draft integration PR #%d for %s (%d survivors)\n", prNum, repoKey, len(survivors))

	// Clean up local worktree — trial branch is now on origin.
	if cleanErr := wm.CleanupTrainWorktree(trialName, false); cleanErr != nil {
		e.logf(0, "merge-train", "warn: could not clean up trial worktree for %s: %v\n", repoKey, cleanErr)
	}

	// Poll CI — this blocks inside the goroutine.
	ciResult := e.pollTrainCI(ctx, owner, repo, prNum, trialSHA)
	state.CIResult = ciResult

	switch ciResult {
	case TrainCIGreen:
		e.logf(0, "merge-train", "CI green for integration PR #%d (%s) — ready for landing\n", prNum, repoKey)
	case TrainCIRed:
		e.logf(0, "merge-train", "CI red for integration PR #%d (%s) — batch needs attention\n", prNum, repoKey)
	default:
		e.logf(0, "merge-train", "CI timed out / pending for integration PR #%d (%s)\n", prNum, repoKey)
	}
	// Do NOT delete in-flight entry — leave for #948 landing step to consume.
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
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = trainWorkDir
	statusOut, _ := statusCmd.CombinedOutput()
	if strings.Contains(string(statusOut), "UU") || strings.Contains(string(statusOut), "AA") || strings.Contains(string(statusOut), "DD") {
		e.logf(memberItem.Number, "merge-train", "conflict markers remain after Claude resolution\n")
		return false
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
