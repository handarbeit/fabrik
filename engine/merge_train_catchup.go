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
	"github.com/handarbeit/fabrik/internal/channelevents"
)

// Singleton catch-up (#2044, ADR-2044).
//
// A merge-train batch of exactly one member whose own branch is BEHIND the pinned base
// cannot take the singleton fast path (ADR-1644 requires the pinned base to be an
// ancestor of the member's head). Before this feature it built a trial branch, opened a
// draft integration PR and ran 10-25 minutes of CI on it — then discarded the trial and
// left the member's branch as stale as before. trySingletonCatchUp instead merges the
// pinned base SHA into the member's OWN branch (a merge commit — no rebase, no force
// push), pushes it, waits for the member PR's own CI on the new head and lands the member
// through the existing fast path. Anything it cannot do safely falls back to today's
// trial, uncharged.

const (
	// defaultCatchUpNoCIDwell is how long a pushed catch-up head may show zero check
	// runs before the engine concludes CI is not running on it and falls back to the
	// trial. The fast path rejects zero check runs anyway, so this only bounds a wasted
	// wait; a repo whose CI does not trigger on the catch-up push costs this dwell once.
	defaultCatchUpNoCIDwell = 10 * time.Minute
	// defaultCatchUpHeadSyncDwell is how long GitHub may take to report the pushed commit
	// as the PR head before the engine stops waiting for it.
	defaultCatchUpHeadSyncDwell = 3 * time.Minute

	// catchUpEditingLabel mirrors comments.go's literal: a comment-processing session
	// brackets its work with it, so its presence means the member's worktree is in use.
	catchUpEditingLabel = "fabrik:editing"
)

// catchUpTiming holds the dwells above so a test can shrink them. A zero field means
// "use the default".
type catchUpTiming struct {
	noCIDwell     time.Duration
	headSyncDwell time.Duration
}

func (e *Engine) catchUpTimingOrDefault() catchUpTiming {
	t := e.trainCatchUp.timing
	if t.noCIDwell <= 0 {
		t.noCIDwell = defaultCatchUpNoCIDwell
	}
	if t.headSyncDwell <= 0 {
		t.headSyncDwell = defaultCatchUpHeadSyncDwell
	}
	return t
}

// SetCatchUpTimingForTest overrides the catch-up CI-wait dwells. Test-only.
func (e *Engine) SetCatchUpTimingForTest(noCIDwell, headSyncDwell time.Duration) {
	e.trainCatchUp.timing = catchUpTiming{noCIDwell: noCIDwell, headSyncDwell: headSyncDwell}
}

// catchUpState is the Engine's catch-up bookkeeping.
type catchUpState struct {
	mu sync.Mutex
	// attempts counts catch-up pushes per (trainKey, issue) since the member last landed
	// or was ejected. In-memory only: a restart merely allows another bounded set of
	// attempts. It bounds a member on a very busy base — each attempt is a push, a CI run
	// and a review — and past effectiveMaxTrainRebaseCycles the trial path takes over.
	attempts map[string]int
	timing   catchUpTiming
	// markers caches the catch-up marker lookup per (PR, live head), so the gates'
	// recognition read (catchUpFeedbackFilterFor) is not repeated on every poll for a
	// head a review bot has already reviewed. See catchUpMarkerCache.
	markers map[string]catchUpMarkerEntry
	// policy remembers, per repo ("owner/repo"), that catch-up pushes are rejected by
	// repo policy, so singletons there skip the catch-up until the memo expires (#2065,
	// ADR-2065). In-memory only: a restart re-probes.
	policy map[string]catchUpPolicyMemo
}

func catchUpKey(trainKey string, issue int) string { return fmt.Sprintf("%s#%d", trainKey, issue) }

func (e *Engine) catchUpAttemptCount(trainKey string, issue int) int {
	e.trainCatchUp.mu.Lock()
	defer e.trainCatchUp.mu.Unlock()
	return e.trainCatchUp.attempts[catchUpKey(trainKey, issue)]
}

func (e *Engine) recordCatchUpAttempt(trainKey string, issue int) {
	e.trainCatchUp.mu.Lock()
	defer e.trainCatchUp.mu.Unlock()
	if e.trainCatchUp.attempts == nil {
		e.trainCatchUp.attempts = make(map[string]int)
	}
	e.trainCatchUp.attempts[catchUpKey(trainKey, issue)]++
}

func (e *Engine) resetCatchUpAttempts(trainKey string, issue int) {
	e.trainCatchUp.mu.Lock()
	defer e.trainCatchUp.mu.Unlock()
	delete(e.trainCatchUp.attempts, catchUpKey(trainKey, issue))
}

// catchUpGitKind classifies what the git half of a catch-up produced.
type catchUpGitKind int

const (
	// catchUpPushed: the catch-up commit was created and pushed.
	catchUpPushed catchUpGitKind = iota
	// catchUpDefer: the member is not ours to touch right now (busy worktree, moved
	// remote, a stage session, or the Claude usage-limit suspension). It stays Queued,
	// nothing is charged and no trial is built this poll.
	catchUpDefer
	// catchUpFallback: the catch-up cannot be done safely or was refused (push rejected,
	// local branch carries unpushed work, unexpected merge shape, git error). The caller
	// builds today's trial, uncharged. A push the repo's rules positively rejected also
	// carries policyReason, which makes the caller remember the repo (#2065).
	catchUpFallback
	// catchUpConflict: the base conflicts with the member's branch and the conflict could
	// not be resolved. The member is ejected exactly as a trial would eject it.
	catchUpConflict
)

// catchUpGitOutcome is the result of the git half of a catch-up.
type catchUpGitOutcome struct {
	kind    catchUpGitKind
	newHead string // the pushed merge commit (catchUpPushed)
	pure    bool   // true: only base commits were brought in — no conflict-resolution edits
	reason  string // log text for defer/fallback; the ejection reason for catchUpConflict
	// policyReason, set only on a catchUpFallback, is the one-line rejection text GitHub
	// returned for a push positively identified as repo policy (classifyCatchUpPushRejection).
	// Empty for every other fallback, which sets no memo.
	policyReason string
}

// catchUpBusyReason reports why the member's branch is not ours to push to right now, or
// "" when it is. R4: a stage session in flight, a live fabrik:editing label (read live —
// never the batch snapshot — because a comment-processing session sets it between polls)
// or an unreadable label set all defer. The dirty-worktree half lives in
// WorktreeManager.PrepareCatchUp.
func (e *Engine) catchUpBusyReason(p trialParams, m trainMember) string {
	repoStr := p.owner + "/" + p.repo
	if snap, ok := e.store.Peek(repoStr, m.item.Number); ok {
		if w := snap.Worker(); w != nil && !isWorkerStale(w, e.workerStaleTimeout()) {
			return "a stage session is in flight on the member"
		}
	}
	labels, err := e.client.FetchLabels(p.owner, p.repo, m.item.Number)
	if err != nil {
		return fmt.Sprintf("could not read the member's labels: %v", err)
	}
	if hasLabel(labels, catchUpEditingLabel) {
		return "the member carries " + catchUpEditingLabel
	}
	return ""
}

// trySingletonCatchUp is runMergeTrainWorker's step between a declined fast path and the
// trial for a length-1 batch (#2044). It returns (member, true) when the disposition for
// this poll is decided (landed, deferred, ejected, or a landing attempt that must not
// fall through), and (member, false) when the caller should build the trial — with the
// returned member, whose headSHA is the catch-up commit when one was pushed.
func (e *Engine) trySingletonCatchUp(ctx context.Context, state *mergeTrainWorkerState, p trialParams, m trainMember) (trainMember, bool) {
	if !e.singletonCatchUpEnabled() {
		return m, false
	}
	// The trainValidateFn unit-test seam runs no real git; catch-up is skipped there
	// unless a test injects its own git half.
	if e.trainValidateFn != nil && e.trainCatchUpGitFn == nil {
		return m, false
	}
	if p.baseSHA == "" {
		return m, false
	}
	// A repo that rejected a recent catch-up push by policy: skip the attempt entirely — no
	// GitHub call, worktree, merge or push — and take the trial path. Silent by design: the
	// operator was told once when the memo was set (#2065).
	if e.catchUpPolicyBackoff(p.repoKey()) {
		return m, false
	}
	behind, err := e.client.FetchCommitsBehind(p.owner, p.repo, p.baseSHA, m.headSHA)
	if err != nil {
		e.logf(m.item.Number, "merge-train", "singleton catch-up: could not confirm #%d is behind the pinned base: %v — building trial\n", m.item.Number, err)
		return m, false
	}
	if behind <= 0 {
		return m, false
	}
	if n, cap := e.catchUpAttemptCount(p.trainKey, m.item.Number), e.effectiveMaxTrainRebaseCycles(); n >= cap {
		e.logf(m.item.Number, "merge-train", "singleton catch-up: #%d has been caught up %d time(s) without landing (cap %d) — building trial\n", m.item.Number, n, cap)
		return m, false
	}
	if why := e.catchUpBusyReason(p, m); why != "" {
		e.logf(m.item.Number, "merge-train", "singleton catch-up: deferring #%d — %s; leaving it in Queued, nothing charged\n", m.item.Number, why)
		return m, true
	}

	e.logf(m.item.Number, "merge-train", "singleton catch-up: #%d is %d commit(s) behind pinned base %s — merging the base into its own branch\n", m.item.Number, behind, p.baseSHA)
	var out catchUpGitOutcome
	if e.trainCatchUpGitFn != nil {
		out = e.trainCatchUpGitFn(ctx, p, m)
	} else {
		out = e.runCatchUpGit(ctx, p, m)
	}
	switch out.kind {
	case catchUpDefer:
		e.logf(m.item.Number, "merge-train", "singleton catch-up: deferring #%d — %s; leaving it in Queued, nothing charged\n", m.item.Number, out.reason)
		return m, true
	case catchUpFallback:
		e.logf(m.item.Number, "merge-train", "singleton catch-up not done for #%d: %s — building trial\n", m.item.Number, out.reason)
		if out.policyReason != "" && e.recordCatchUpPolicyRejection(p.repoKey(), out.policyReason) {
			e.logf(m.item.Number, "merge-train", "singleton catch-up: %s rejects catch-up pushes by repository policy (%s) — skipping the catch-up on this repo for %s (singletons build a trial instead); set singleton_catch_up: off (--singleton-catch-up / FABRIK_SINGLETON_CATCH_UP) to stop probing permanently\n", p.repoKey(), out.policyReason, catchUpPolicyMemoTTL)
		}
		return m, false
	case catchUpConflict:
		e.logf(m.item.Number, "merge-train", "singleton catch-up: cannot resolve the conflict between pinned base %s and #%d — ejecting\n", p.baseSHA, m.item.Number)
		e.ejectMember(p.owner, p.repo, m.item, out.reason, nil, nil, true)
		e.resetCatchUpAttempts(p.trainKey, m.item.Number)
		return m, true
	}

	// Pushed.
	e.recordCatchUpAttempt(p.trainKey, m.item.Number)
	caughtUp := m
	caughtUp.headSHA = out.newHead
	caughtUp.caughtUpFrom = p.baseSHA
	e.postCatchUpMarker(p, caughtUp, m.headSHA, out.pure)

	verdict, diag := e.waitMemberCI(ctx, state, p, caughtUp)
	switch verdict {
	case memberCIGreen:
		// Re-enter the existing fast path, whose eligibility checks (live head ==
		// caughtUp.headSHA — R5's TOCTOU check — pinned base an ancestor, mergeable,
		// non-zero green and complete CI) are the landing gate, unmodified.
		if remaining, ejected := e.applyPendingReviewEjects(state.projectID, p.repoKey(), []trainMember{caughtUp}); ejected > 0 || len(remaining) == 0 {
			return caughtUp, true
		}
		if e.trySingletonFastPath(ctx, state, p, caughtUp) {
			return caughtUp, true
		}
		e.logf(m.item.Number, "merge-train", "singleton catch-up: #%d is green on its caught-up head but the fast path declined — building trial\n", m.item.Number)
		return caughtUp, false
	case memberCIRed:
		e.ejectRedCatchUpSingleton(state.projectID, p, caughtUp, diag)
		e.resetCatchUpAttempts(p.trainKey, m.item.Number)
		return caughtUp, true
	case memberCINoCI:
		e.logf(m.item.Number, "merge-train", "singleton catch-up: CI did not start on the caught-up head %s of #%d — building trial\n", caughtUp.headSHA, m.item.Number)
		return caughtUp, false
	default: // memberCIPending, memberCIMoved, memberCIEjected
		// Decided for this poll: the member stays Queued (or was already ejected) and the
		// next poll re-evaluates it. Nothing is charged.
		return caughtUp, true
	}
}

// runCatchUpGit is the real git half of a catch-up: ready the member's own worktree,
// merge the pinned base into it, resolve conflicts on the trial's path, stamp the audit
// trailer and push. Every failure path restores the worktree to the member's head so a
// later poll finds it clean and equal to the remote.
func (e *Engine) runCatchUpGit(ctx context.Context, p trialParams, m trainMember) catchUpGitOutcome {
	n := m.item.Number
	wtDir, err := p.wm.PrepareCatchUp(n, p.baseBranch, m.headSHA)
	switch {
	case errors.Is(err, ErrCatchUpBusy):
		return catchUpGitOutcome{kind: catchUpDefer, reason: err.Error()}
	case err != nil:
		return catchUpGitOutcome{kind: catchUpFallback, reason: err.Error()}
	}

	restore := func() {
		for _, args := range [][]string{{"merge", "--abort"}, {"reset", "--hard", m.headSHA}, {"clean", "-fd"}} {
			if out, rerr := gitOutputIn(wtDir, args...); rerr != nil && args[0] != "merge" {
				e.logf(n, "merge-train", "warn: catch-up restore step %v failed: %s\n", args, out)
			}
		}
	}

	mergeMsg := fmt.Sprintf("chore(merge-train): catch up #%d with %s", n, shortSHA(p.baseSHA))
	mergeOut, mergeErr := gitCombined(wtDir, "merge", "--no-ff", "-m", mergeMsg, p.baseSHA)
	pure := mergeErr == nil
	if mergeErr != nil {
		e.logf(n, "merge-train", "catch-up merge conflict for #%d: %s — resolving\n", n, strings.TrimSpace(mergeOut))
		opts := InvokeOptions{BaseBranch: p.baseBranch, MaxTurnsOverride: p.maxTurnsOverride, FabrikRoot: e.fabrikDir, FabrikRepo: e.defaultRepo(), MaxResumeFailures: e.cfg.MaxResumeFailures, NoResume: true, CatchUpBaseSHA: p.baseSHA}
		resolved, diag, resolveErr := e.resolveTrainConflict(ctx, m.item, wtDir, p.holdingStg, p.baseSHA, m.headSHA, mergeOut, opts)
		if resolveErr != nil {
			// Resolution could not even be attempted (account-wide usage-limit
			// suspension, ADR-1120): no verdict on the conflict, so no ejection.
			restore()
			return catchUpGitOutcome{kind: catchUpDefer, reason: fmt.Sprintf("conflict resolution unavailable: %v", resolveErr)}
		}
		if !resolved {
			restore()
			generic := strings.TrimPrefix(buildConflictEjectionReason(diag, m.headSHA), "ejected from merge-train batch — ")
			return catchUpGitOutcome{kind: catchUpConflict, reason: "ejected from merge-train — catching this branch up with the base hit a conflict that could not be resolved: " + generic}
		}
	}

	// The result must be exactly the merge we asked for: a merge commit whose first
	// parent is the member's head and whose second is the pinned base. Anything else
	// (a resolver that committed on top, rebased or aborted) is not a catch-up.
	parents, perr := gitOutputIn(wtDir, "rev-list", "--parents", "-n", "1", "HEAD")
	fields := strings.Fields(parents)
	if perr != nil || len(fields) != 3 || fields[1] != m.headSHA || fields[2] != p.baseSHA {
		restore()
		return catchUpGitOutcome{kind: catchUpFallback, reason: fmt.Sprintf("the catch-up did not produce the expected merge commit (rev-list: %q)", parents)}
	}

	if err := stampCatchUpTrailer(wtDir, p.baseSHA); err != nil {
		restore()
		return catchUpGitOutcome{kind: catchUpFallback, reason: err.Error()}
	}

	// R4, second check: the merge — and a Claude conflict resolution — can take minutes.
	if why := e.catchUpBusyReason(p, m); why != "" {
		restore()
		return catchUpGitOutcome{kind: catchUpDefer, reason: why}
	}

	newHead, herr := gitRevParse(wtDir, "HEAD")
	if herr != nil {
		restore()
		return catchUpGitOutcome{kind: catchUpFallback, reason: fmt.Sprintf("could not read the catch-up commit: %v", herr)}
	}
	if perr := p.wm.PushCatchUp(n, m.headSHA); perr != nil {
		// Branch protection, a signed-commits requirement, or the member moved: nothing
		// was pushed. Reset so the local branch is not left ahead of the remote.
		restore()
		var pushErr *CatchUpPushError
		if errors.As(perr, &pushErr) {
			if reason, policy := classifyCatchUpPushRejection(pushErr.Output); policy {
				return catchUpGitOutcome{kind: catchUpFallback, reason: catchUpPolicyAttemptReason, policyReason: reason}
			}
		}
		return catchUpGitOutcome{kind: catchUpFallback, reason: perr.Error()}
	}
	return catchUpGitOutcome{kind: catchUpPushed, newHead: newHead, pure: pure}
}

// stampCatchUpTrailer adds the `Fabrik-Train-Catch-Up: <base sha>` trailer to the merge
// commit at HEAD (idempotently). The trailer is for audit and a possible future Pruefer
// skip only — the engine never trusts it for review recognition (see catchup_feedback.go).
func stampCatchUpTrailer(wtDir, baseSHA string) error {
	msg, err := gitOutputIn(wtDir, "log", "-1", "--format=%B", "HEAD")
	if err != nil {
		return fmt.Errorf("reading the catch-up commit message: %w", err)
	}
	cmd := exec.Command("git", "interpret-trailers", "--if-exists", "doNothing", "--trailer", catchUpTrailerKey+": "+baseSHA)
	cmd.Dir = wtDir
	cmd.Stdin = strings.NewReader(msg + "\n")
	stamped, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("stamping the catch-up trailer: %w", err)
	}
	amend := exec.Command("git", "commit", "--amend", "--allow-empty", "-F", "-")
	amend.Dir = wtDir
	amend.Stdin = strings.NewReader(string(stamped))
	if out, err := amend.CombinedOutput(); err != nil {
		return fmt.Errorf("amending the catch-up commit with its trailer: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// postCatchUpMarker posts the authenticated catch-up record on the member's PR: a human
// explanation plus the machine marker the review/comment gates read. A failed post (after
// retrying transient errors) is logged and leaves reviews actionable — the fail-closed direction.
func (e *Engine) postCatchUpMarker(p trialParams, caughtUp trainMember, previousHead string, pure bool) {
	mk := catchUpMarker{Head: caughtUp.headSHA, Base: p.baseSHA, Pure: pure}
	// Whatever the gates cached for this head before the marker existed is stale now.
	defer e.forgetCatchUpMarker(p.owner, p.repo, caughtUp.prNum, caughtUp.headSHA)
	var effect string
	if pure {
		effect = "Only base commits were brought in (no conflict-resolution edits), so automated reviews of this push are not treated as actionable for this landing; the PR's own CI decides whether it lands."
	} else {
		effect = "Merge conflicts were resolved in the process, so reviews of this push are actionable as usual."
	}
	body := fmt.Sprintf("🏭 **Fabrik merge-train** — caught up with the base\n\n"+
		"This PR was behind the merge-train's pinned base `%s`. Fabrik merged that base into this branch "+
		"(merge commit `%s`, previously `%s`; a merge commit, so no rebase or force-push) and will land the PR "+
		"directly once its own CI is green and complete on the new head — no trial branch is built. %s\n\n%s",
		shortSHA(p.baseSHA), shortSHA(caughtUp.headSHA), shortSHA(previousHead), effect, formatCatchUpMarker(mk))
	// Retried on transient errors: without the marker every bot review of the pushed head
	// is actionable, and waitMemberCI's eject checkpoint would then eject the very member
	// the catch-up was meant to land.
	if !e.addCommentWithRetry(p.owner, p.repo, caughtUp.item.Number, caughtUp.prNum, body, "the catch-up marker") {
		e.logf(caughtUp.item.Number, "merge-train", "warn: no catch-up marker on PR #%d — reviews of this push stay actionable\n", caughtUp.prNum)
	}
}

// memberCIVerdict is waitMemberCI's outcome.
type memberCIVerdict int

const (
	memberCIGreen   memberCIVerdict = iota // own CI is green and complete on the caught-up head
	memberCIRed                            // a confirmed CI failure (after the one failed-job re-run)
	memberCIPending                        // timed out or cancelled — the member stays Queued
	memberCINoCI                           // no check run appeared within the dwell
	memberCIMoved                          // the PR head moved, the PR closed/merged, or base conflicts again
	memberCIEjected                        // a pending review-finding/comment eject was applied
)

// waitMemberCI blocks, in the worker, until the member PR's own CI on the caught-up head
// is decided (R2). It is deliberately in-worker rather than return-and-re-evaluate: only
// the worker keeps the pinned base meaningful — across polls a busy base would catch the
// member up again and again. The CI judgement is classifyLandingCI/ciSuiteHold, unmodified.
//
// It never retriggers (closes and reopens) the member's PR — that is a trial-PR
// technique and unsafe on a real PR — and it reuses #2052's flaky-job handling the same
// way pollTrainCI does: the first confirmed failure re-runs the failed jobs once and a
// second one is red.
func (e *Engine) waitMemberCI(ctx context.Context, state *mergeTrainWorkerState, p trialParams, m trainMember) (memberCIVerdict, *trainCIDiagnostic) {
	logRepo := p.repoKey()
	n := m.item.Number
	timing := e.catchUpTimingOrDefault()
	infra := e.ciInfraTimingOrDefault()
	started := time.Now()
	deadline := started.Add(e.ciBackstopTimeout())
	var rr rerunState

	for {
		select {
		case <-ctx.Done():
			return memberCIPending, nil
		default:
		}
		if time.Now().After(deadline) {
			e.logfRepo(logRepo, "merge-train", "singleton catch-up: CI wait timed out for #%d on %s — leaving it in Queued\n", n, m.headSHA)
			return memberCIPending, nil
		}
		if _, ejected := e.applyPendingReviewEjects(state.projectID, logRepo, []trainMember{m}); ejected > 0 {
			return memberCIEjected, nil
		}

		pr, err := e.client.FetchPRDetails(p.owner, p.repo, m.prNum)
		switch {
		case err != nil || pr == nil:
			e.logfRepo(logRepo, "merge-train", "warn: singleton catch-up: FetchPRDetails(#%d) failed: %v\n", m.prNum, err)
		case pr.Merged || pr.State == "closed":
			e.logfRepo(logRepo, "merge-train", "singleton catch-up: PR #%d is no longer open — leaving #%d for the next poll\n", m.prNum, n)
			return memberCIMoved, nil
		case pr.HeadSHA != m.headSHA:
			if time.Since(started) > timing.headSyncDwell {
				e.logfRepo(logRepo, "merge-train", "singleton catch-up: PR #%d head is %s, not the caught-up %s — leaving #%d for the next poll\n", m.prNum, pr.HeadSHA, m.headSHA, n)
				return memberCIMoved, nil
			}
		case pr.MergeableState == "dirty":
			e.logfRepo(logRepo, "merge-train", "singleton catch-up: PR #%d no longer merges cleanly onto the moved base — leaving #%d for the next poll\n", m.prNum, n)
			return memberCIMoved, nil
		default:
			runs, rerr := e.client.FetchCheckRuns(p.owner, p.repo, m.headSHA)
			if rerr != nil {
				e.logfRepo(logRepo, "merge-train", "warn: singleton catch-up: FetchCheckRuns(%s) failed: %v\n", m.headSHA, rerr)
				break
			}
			if len(runs) == 0 {
				if time.Since(started) > timing.noCIDwell {
					return memberCINoCI, nil
				}
				break
			}
			status, _, failed := gh.ClassifyCheckRuns(runs)
			if status == gh.CheckRunsFailed {
				rerunPastDwell := rr.done && infra.now().Sub(rr.at) >= infra.rerunSettleDwell
				rerunWaiting := rerunPastDwell && infra.now().Sub(rr.at) < infra.rerunSettleDwell+infra.rerunMaxWait &&
					e.rerunInFlight(logRepo, p.owner, p.repo, m.headSHA, rr.runIDs)
				if !rr.done {
					if ids, ok := e.rerunFailedWorkflowRuns(logRepo, p.owner, p.repo, failed); ok {
						rr = rerunState{done: true, runIDs: ids, firstFailedIDs: checkRunIDSet(failed), at: infra.now()}
						e.logfRepo(logRepo, "merge-train", "singleton catch-up: #%d failed check(s): %s — re-running the failed jobs once before judging it red\n", n, describeCheckRuns(failed))
						break
					}
					return memberCIRed, catchUpRedDiag(m, failed, nil)
				}
				if hasNewCheckRun(failed, rr.firstFailedIDs) || (rerunPastDwell && !rerunWaiting) {
					return memberCIRed, catchUpRedDiag(m, failed, nil)
				}
				break
			}
			result, detail := e.classifyLandingCI(p.owner, p.repo, pr.MergeableState, m.headSHA, runs)
			switch result {
			case TrainCIGreen:
				e.logfRepo(logRepo, "merge-train", "singleton catch-up: #%d green on caught-up head %s — %s\n", n, m.headSHA, detail)
				return memberCIGreen, nil
			case TrainCIRed:
				return memberCIRed, catchUpRedDiag(m, nil, []string{detail})
			}
		}

		select {
		case <-ctx.Done():
			return memberCIPending, nil
		case <-time.After(e.trainCIPollIntervalOrDefault()):
		}
	}
}

// catchUpRedDiag builds the diagnostic for a red caught-up head. The Note says plainly
// that the head includes a Fabrik catch-up merge, so a failure caused by an interaction
// with newer base commits is legible rather than looking like the member's own defect.
func catchUpRedDiag(m trainMember, failed []gh.CheckRun, contexts []string) *trainCIDiagnostic {
	return &trainCIDiagnostic{
		FailedChecks:   failed,
		FailedContexts: contexts,
		PRNum:          m.prNum,
		TrialSHA:       m.headSHA,
		Note:           fmt.Sprintf("The failing head %s includes a Fabrik catch-up merge of base %s, so the failure may come from an interaction between this PR and newer base commits.", shortSHA(m.headSHA), shortSHA(m.caughtUpFrom)),
	}
}

// ejectRedCatchUpSingleton is ejectRedSingleton for a member whose own CI went red on a
// caught-up head: the same reroute-then-pause disposition (R2, "the red-singleton path"),
// with a comment that names the catch-up commit instead of a trial.
func (e *Engine) ejectRedCatchUpSingleton(projectID string, p trialParams, m trainMember, diag *trainCIDiagnostic) {
	if !e.rerouteQueuedMemberOffHolding(projectID, m.item) {
		e.logf(m.item.Number, "merge-train", "#%d is red after a catch-up but could not be rerouted off Queued — leaving untouched for retry on the next poll\n", m.item.Number)
		return
	}
	targetName := "the preceding stage"
	if target := stageBeforeHolding(e.cfg, holdingStage(e.cfg)); target != nil {
		targetName = target.Name
	}
	var detail string
	switch {
	case diag != nil && len(diag.FailedChecks) > 0:
		detail = renderFailedChecks(diag.FailedChecks)
	case diag != nil && len(diag.FailedContexts) > 0:
		detail = renderFailedContexts(diag.FailedContexts)
	}
	sections := []string{
		fmt.Sprintf("#%d's own CI is failing on the head Fabrik pushed when it caught the branch up with the merge-train's base (merge commit `%s`, base `%s`). No trial was built.", m.item.Number, shortSHA(m.headSHA), shortSHA(m.caughtUpFrom)),
	}
	if diag != nil && diag.Note != "" {
		sections = append(sections, diag.Note)
	}
	if detail != "" {
		sections = append(sections, fmt.Sprintf("**Diagnostic** (PR #%d, head %s):\n\n%s", m.prNum, m.headSHA, detail))
	}
	sections = append(sections, reentryInstruction(targetName, "Fix the failing check(s) on this PR (the catch-up merge commit is already on its branch)"))
	msg := fmt.Sprintf("🏭 **Fabrik merge-train — validation failed**\n\n%s", strings.Join(sections, "\n\n"))
	if _, err := e.client.AddComment(p.owner, p.repo, m.item.Number, msg); err != nil {
		e.logf(m.item.Number, "merge-train", "warn: could not post the red catch-up comment: %v\n", err)
	}
	e.logf(m.item.Number, "merge-train", "#%d is red on its caught-up head %s — rerouted to %s and pausing\n", m.item.Number, m.headSHA, targetName)
	e.pauseMergeTrainMember(p.owner, p.repo, m.item.Number)
	e.emitTrainEvent(p.owner, p.repo, m.item.Number, channelevents.MergeTrainFailed, "red-singleton",
		"its own CI is failing on the head Fabrik caught up with the base", diag, nil) // observation only (#1968)
}

// shortSHA abbreviates a SHA for human-readable text.
func shortSHA(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

// gitCombined runs git in dir and returns the untrimmed combined output.
func gitCombined(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
