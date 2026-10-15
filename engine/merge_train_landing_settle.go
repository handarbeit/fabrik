package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// ─── Landing settle wait and keep-green-trial (#2297, ADR 2297) ──────────────────────
//
// A green trial is landed by promoting its draft CI PR to ready and merging it. Marking a
// PR ready fires pull_request: ready_for_review, which starts a new workflow run on the
// same head; for a short window its required check is queued, pending or not yet visible
// and GitHub reports mergeable_state "blocked". settleAndMerge owns the whole mark-ready →
// merge window: it waits that window out on the SAME PR and retries a merge refusal there,
// instead of treating either as a failed landing. When a landing attempt nevertheless ends
// without a merge, keepGreenTrial decides whether the (still green, unchanged-base) trial PR
// and branch are kept so the next pass reuses them (reconstructTrainState Route 2) rather
// than rebuilding a CI run that already passed.

// trainBlockedSettleTimeout bounds how long a landing waits when every visible check is
// green but mergeable_state stays "blocked" (GitHub not yet counting a just-created check,
// or one about to appear). It is distinct from — and much shorter than — CIBackstopTimeout,
// which still bounds a wait on checks that are actually pending. Not user-configurable.
const trainBlockedSettleTimeout = 5 * time.Minute

// landingFailureMarker is the hidden body marker that dedups the landing-failure comment
// across restarts: "<!-- fabrik:train-landing-failure pr=N -->".
const landingFailureMarker = "fabrik:train-landing-failure"

// landingState holds the #2297 per-Engine seams and the landing-failure comment dedup set.
// The zero value is production behaviour.
type landingState struct {
	settleOff    atomic.Bool                                                // SetLandingSettleDisabledForTest
	keepOff      atomic.Bool                                                // SetKeepGreenTrialDisabledForTest
	blockedBound atomic.Int64                                               // SetLandingBlockedBoundForTest (ns)
	observer     atomic.Pointer[func(trialName string, deletedBranch bool)] // SetLandingCleanupObserverForTest

	mu     sync.Mutex
	posted map[string]struct{} // "owner/repo#pr" already given a landing-failure comment by this process
}

// SetLandingSettleDisabledForTest restores the pre-#2297 landing: a green PR is merged at
// once, with no mergeable_state gate and no retry of a refused merge. Non-vacuity seam.
func (e *Engine) SetLandingSettleDisabledForTest(v bool) { e.landing.settleOff.Store(v) }

// SetKeepGreenTrialDisabledForTest restores the pre-#2297 cleanup: a landing that ends
// without a merge always deletes the trial branch (closing its PR). Non-vacuity seam.
func (e *Engine) SetKeepGreenTrialDisabledForTest(v bool) { e.landing.keepOff.Store(v) }

// SetLandingBlockedBoundForTest overrides trainBlockedSettleTimeout (zero restores it).
func (e *Engine) SetLandingBlockedBoundForTest(d time.Duration) {
	e.landing.blockedBound.Store(int64(d))
}

// SetLandingCleanupObserverForTest registers fn to be told how each landing's trial
// artifacts were released — deletedBranch false means the PR and origin branch were kept.
// It fires for every landing release, including under the trainValidateFn seam (where the
// git work itself is skipped), so unit tests can observe the decision.
func (e *Engine) SetLandingCleanupObserverForTest(fn func(trialName string, deletedBranch bool)) {
	if fn == nil {
		e.landing.observer.Store(nil)
		return
	}
	e.landing.observer.Store(&fn)
}

func (e *Engine) landingBlockedBoundOrDefault() time.Duration {
	if d := time.Duration(e.landing.blockedBound.Load()); d > 0 {
		return d
	}
	return trainBlockedSettleTimeout
}

// landingOutcome is how one settleAndMerge attempt ended.
type landingOutcome int

const (
	outcomeMerged              landingOutcome = iota // the PR was merged
	outcomeRed                                       // a confirmed failed check run or required context
	outcomeNotPassed                                 // a cancelled/stale/empty check conclusion (#2296)
	outcomeDirty                                     // mergeable_state "dirty"
	outcomeHeadMoved                                 // the PR head is no longer the validated head
	outcomeBlockedBoundExpired                       // all visible checks green but not mergeable within the short bound
	outcomeBackstopExpired                           // checks still pending at CIBackstopTimeout
	outcomeMergeError                                // MergePR failed with something other than a CI-readiness refusal
	outcomeCancelled                                 // ctx cancelled (shutdown / drain)
)

// landingResult is the outcome plus what a failure comment and the logs need.
type landingResult struct {
	outcome landingOutcome
	state   string // last mergeable_state observed
	reason  string
	err     error
}

// keepable reports whether the outcome leaves a trial that may still be green: waiting ran
// out or the merge call failed, but nothing says the trial itself is bad.
func (o landingOutcome) keepable() bool {
	switch o {
	case outcomeBlockedBoundExpired, outcomeBackstopExpired, outcomeMergeError, outcomeCancelled:
		return true
	}
	return false
}

// commentable reports whether the outcome earns the one deduped landing-failure comment.
// Red and not-passed stay log-only (as before); cancellation and a successful merge post
// nothing; a landing that is merely waiting never reaches here.
func (o landingOutcome) commentable() bool {
	switch o {
	case outcomeDirty, outcomeHeadMoved, outcomeBlockedBoundExpired, outcomeBackstopExpired, outcomeMergeError:
		return true
	}
	return false
}

// countPending counts check runs that have not completed.
func countPending(runs []gh.CheckRun) int {
	n := 0
	for _, cr := range runs {
		if cr.Status != "completed" {
			n++
		}
	}
	return n
}

// settleAndMerge waits for landing PR prNum to be both green (classifyLandingCI) and
// mergeable (gh.MergeableStateAccepted) at the validated head, then merges it. It is the
// single place the mark-ready → merge window is handled (#2297):
//
//   - pending check runs or an outstanding suite are waited out under one deadline,
//     CIBackstopTimeout, set at entry and never reset;
//   - all visible checks green but mergeable_state not accepted (blocked/unknown/empty) is
//     "GitHub not caught up yet": waited out for at most the short trainBlockedSettleTimeout;
//   - a MergePR refusal (ErrNotMergeableCI, or ErrNotMergeable for a not-yet-computed
//     mergeable) re-enters that same wait on the same PR — it shares the deadline and the
//     short bound, so a PR refused for ever cannot loop for ever;
//   - a confirmed red/not-passed check, "dirty", the head moving off expectedHead, either
//     bound running out or any other MergePR error ends the attempt.
//
// expectedHead is the head the trial was validated at; "" adopts the head of the first
// successful read. mergeable_state gates the merge but never makes a verdict green — green
// still comes only from classifyLandingCI (ADR-1153/1441).
func (e *Engine) settleAndMerge(ctx context.Context, owner, repo string, prNum int, expectedHead string) landingResult {
	repoKey := owner + "/" + repo
	deadline := time.Now().Add(e.ciBackstopTimeout())
	blockedBound := e.landingBlockedBoundOrDefault()
	settleOff := e.landing.settleOff.Load()
	var blockedSince time.Time
	head := expectedHead
	lastState := ""

	for {
		select {
		case <-ctx.Done():
			e.logfRepo(repoKey, "merge-train", "context cancelled while waiting to land integration PR #%d\n", prNum)
			return landingResult{outcome: outcomeCancelled, state: lastState, reason: "cancelled"}
		default:
		}
		if time.Now().After(deadline) {
			break
		}

		pr, err := e.client.FetchPRDetails(owner, repo, prNum)
		switch {
		case err != nil:
			e.logfRepo(repoKey, "merge-train", "warn: FetchPRDetails failed for integration PR #%d: %v\n", prNum, err)
		case pr.HeadSHA != "" && head != "" && pr.HeadSHA != head:
			e.logfRepo(repoKey, "merge-train", "integration PR #%d head moved from %s to %s — not landing a commit that was not validated\n", prNum, shortSHA(head), shortSHA(pr.HeadSHA))
			return landingResult{outcome: outcomeHeadMoved, state: pr.MergeableState,
				reason: fmt.Sprintf("the PR head moved from %s to %s after the trial was validated", shortSHA(head), shortSHA(pr.HeadSHA))}
		case pr.MergeableState == "dirty":
			e.logfRepo(repoKey, "merge-train", "integration PR #%d has merge conflict (dirty) — cannot land\n", prNum)
			return landingResult{outcome: outcomeDirty, state: pr.MergeableState, reason: "the PR has a merge conflict with its base"}
		default:
			lastState = pr.MergeableState
			if head == "" {
				head = pr.HeadSHA
			}
			var checkRuns []gh.CheckRun
			var checkRunsErr error
			if pr.HeadSHA != "" {
				checkRuns, checkRunsErr = e.client.FetchCheckRuns(owner, repo, pr.HeadSHA)
			}
			if checkRunsErr != nil {
				// A fetch failure is "unknown", not "confirmed zero check runs" — see the
				// ADR-1441 note in classifyLandingCI's callers: skip classification this
				// iteration rather than let the zero-run fallback read an accepted
				// mergeable_state as green.
				e.logfRepo(repoKey, "merge-train", "warn: FetchCheckRuns failed for integration PR #%d: %v\n", prNum, checkRunsErr)
				break
			}
			verdict, detail := e.classifyLandingCI(owner, repo, pr.MergeableState, pr.HeadSHA, checkRuns)
			switch verdict {
			case TrainCIRed:
				e.logfRepo(repoKey, "merge-train", "integration PR #%d not mergeable — %s\n", prNum, detail)
				return landingResult{outcome: outcomeRed, state: pr.MergeableState, reason: detail}
			case TrainCINotPassed:
				// #2296: a cancelled run will not finish on its own; do not wait for it.
				e.logfRepo(repoKey, "merge-train", "integration PR #%d not landable — %s\n", prNum, detail)
				return landingResult{outcome: outcomeNotPassed, state: pr.MergeableState, reason: detail}
			case TrainCIGreen:
				if settleOff || gh.MergeableStateAccepted(pr.MergeableState) {
					mergeErr := e.client.MergePR(owner, repo, prNum)
					if mergeErr == nil {
						e.logfRepo(repoKey, "merge-train", "integration PR #%d ready to land — %s\n", prNum, detail)
						return landingResult{outcome: outcomeMerged, state: pr.MergeableState}
					}
					if settleOff || !(errors.Is(mergeErr, gh.ErrNotMergeableCI) || errors.Is(mergeErr, gh.ErrNotMergeable)) {
						e.logfRepo(repoKey, "merge-train", "merge of integration PR #%d failed: %v\n", prNum, mergeErr)
						return landingResult{outcome: outcomeMergeError, state: pr.MergeableState, err: mergeErr,
							reason: fmt.Sprintf("the merge call failed: %v", mergeErr)}
					}
					// The state flipped between the read and the merge: same PR, same bound.
					e.logfRepo(repoKey, "merge-train", "integration PR #%d merge refused (%v) — retrying on the same PR\n", prNum, mergeErr)
					lastState = fmt.Sprintf("%s (merge refused: %v)", pr.MergeableState, mergeErr)
				}
				if blockedSince.IsZero() {
					blockedSince = time.Now()
				}
				if elapsed := time.Since(blockedSince); elapsed >= blockedBound {
					e.logfRepo(repoKey, "merge-train", "integration PR #%d still not mergeable after %s with all checks green — giving up this attempt\n", prNum, blockedBound)
					return landingResult{outcome: outcomeBlockedBoundExpired, state: lastState,
						reason: fmt.Sprintf("every visible check is green but GitHub still did not report the PR mergeable after %s", blockedBound)}
				}
				e.logfRepo(repoKey, "merge-train", "integration PR #%d blocked after mark-ready, all green (mergeable_state=%q), waiting up to %s\n", prNum, pr.MergeableState, blockedBound)
			default: // TrainCIPending — wait under CIBackstopTimeout
				blockedSince = time.Time{}
				e.logfRepo(repoKey, "merge-train", "integration PR #%d blocked after mark-ready, %d check(s) pending (mergeable_state=%q), waiting up to %s — %s\n", prNum, countPending(checkRuns), pr.MergeableState, e.ciBackstopTimeout(), detail)
			}
		}

		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			e.logfRepo(repoKey, "merge-train", "context cancelled while waiting to land integration PR #%d\n", prNum)
			return landingResult{outcome: outcomeCancelled, state: lastState, reason: "cancelled"}
		case <-time.After(e.trainCIPollIntervalOrDefault()):
		}
	}

	e.logfRepo(repoKey, "merge-train", "timed out waiting for integration PR #%d to become mergeable\n", prNum)
	return landingResult{outcome: outcomeBackstopExpired, state: lastState,
		reason: fmt.Sprintf("checks were still pending after %s", e.ciBackstopTimeout())}
}

// keepGreenTrial reports whether a landing attempt that ended with res leaves the trial PR
// and branch in place: the outcome must be keepable AND a fresh live read must show the base
// has not moved under the trial. A read error counts as moved (fail closed, matching
// trialBehind), so the trial is cleaned up as before and the main-moved path owns recovery.
func (e *Engine) keepGreenTrial(owner, repo, baseBranch, trialBranch string, res landingResult) bool {
	if e.landing.keepOff.Load() || !res.outcome.keepable() {
		return false
	}
	return !e.trialBehind(owner, repo, baseBranch, trialBranch)
}

// releaseLandingTrial is the one place a landing path releases its trial artifacts.
// keep=false is the pre-#2297 cleanup (local worktree, local+remote branch — which closes
// the PR). keep=true removes only the local worktree so the PR and origin branch survive for
// reuse. direct mirrors landMergeTrainBatch's historical behaviour of calling the worktree
// manager even under the trainValidateFn seam; landSingleton went through
// cleanupTrialArtifacts, a no-op there.
func (e *Engine) releaseLandingTrial(repoKey string, wm *WorktreeManager, trialName string, keep, direct bool) {
	if fn := e.landing.observer.Load(); fn != nil {
		(*fn)(trialName, !keep)
	}
	if !keep && !direct {
		e.cleanupTrialArtifacts(repoKey, wm, trialName)
		return
	}
	if e.trainValidateFn != nil && !direct {
		return
	}
	if err := wm.CleanupTrainWorktree(trialName, !keep); err != nil {
		e.logfRepo(repoKey, "merge-train", "warn: cleanup trial worktree for %s failed: %v\n", repoKey, err)
	}
}

// postLandingFailure posts the one "merge failure" comment for a landing attempt that
// failed for a reason in landingOutcome.commentable. A waiting landing never calls it. The
// comment names the PR, the mergeable_state observed and the reason, and is not reposted for
// the same PR: a hidden body marker found in the first survivor's comments survives a
// restart, and an in-memory set covers the window before the cache sees the comment.
func (e *Engine) postLandingFailure(owner, repo string, prNum int, survivors []trainMember, res landingResult) {
	if !res.outcome.commentable() || len(survivors) == 0 {
		return
	}
	key := fmt.Sprintf("%s/%s#%d", owner, repo, prNum)
	marker := fmt.Sprintf("<!-- %s pr=%d -->", landingFailureMarker, prNum)
	for _, c := range survivors[0].item.Comments {
		if strings.Contains(c.Body, marker) {
			return
		}
	}
	e.landing.mu.Lock()
	if e.landing.posted == nil {
		e.landing.posted = make(map[string]struct{})
	}
	if _, done := e.landing.posted[key]; done {
		e.landing.mu.Unlock()
		return
	}
	e.landing.posted[key] = struct{}{}
	e.landing.mu.Unlock()

	state := res.state
	if state == "" {
		state = "unknown"
	}
	msg := fmt.Sprintf("🏭 **Fabrik merge-train — merge failure**\n\n"+
		"Could not land integration PR #%d: %s (mergeable_state observed: `%s`).\n\n"+
		"Batch members remain in the Queued column. Fabrik keeps a still-green trial and retries it on the next train cycle; "+
		"manual intervention may be required if this repeats.\n\n%s",
		prNum, res.reason, state, marker)
	if _, err := e.client.AddComment(owner, repo, survivors[0].item.Number, msg); err != nil {
		e.logfRepo(owner+"/"+repo, "merge-train", "warn: could not post merge-failure comment: %v\n", err)
	}
}
