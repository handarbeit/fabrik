package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Multi-member trial conflict write-back (#2572, ADR 2572).
//
// When a trial has two or more members and one of them conflicts while the trial branch is
// assembled, the resolution used to live only on the disposable trial branch (and in the
// shared rr-cache). An abandoned, bisected or rebuilt trial, or an ejected member, lost it
// and the next trial — or the member's own PR — hit the same conflict again.
//
// After assembleTrialBranch resolves a conflict for member M in a multi-member trial it now
// tries to write the resolution back: merge the pinned base into M's OWN branch (replaying
// the same resolution through rerere / generated-file regeneration — never a second Claude
// call), push it under a lease on M's verified head (the singleton catch-up's PushCatchUp),
// post a `kind=writeback` marker and continue building the trial from M's new head. The
// write-back is strictly best-effort: it never changes the trial's content, never fails
// assembly and never charges the member (no ejection, retry, trial-rate entry or pause).

// errWriteBackNeedsClaude is returned by resolveConflictWithClaude under
// InvokeOptions.noClaude: the conflict is not replayable and the write-back is skipped.
var errWriteBackNeedsClaude = errors.New("write-back: the conflict needs a new Claude resolution")

type writeBackKind int

const (
	// wbWritten: the resolved base merge was pushed to the member's own branch.
	wbWritten writeBackKind = iota
	// wbSkipped: nothing was written (ownership check, not replayable, clean base merge,
	// git doubt). Uncharged; the trial continues as before.
	wbSkipped
	// wbRejected: the push was refused (lease, policy, transport). Uncharged.
	wbRejected
)

// writeBackOutcome is the result of the git half of a write-back.
type writeBackOutcome struct {
	kind    writeBackKind
	newHead string // the pushed merge commit (wbWritten)
	reason  string // log text for wbSkipped / wbRejected
	// policyReason is set only on a wbRejected push positively identified as repo policy
	// (classifyCatchUpPushRejection); it feeds the shared per-repo memo (ADR 2065).
	policyReason string
}

// writeBackState is the Engine's write-back bookkeeping: the (train, member, base SHA)
// triples already attempted, so rebuilds at one pinned base do not push again and again.
// In memory only: a restart merely allows one more attempt.
type writeBackState struct {
	mu        sync.Mutex
	attempted map[string]struct{}
}

func writeBackKey(trainKey string, issue int, baseSHA string) string {
	return fmt.Sprintf("%s#%d@%s", trainKey, issue, baseSHA)
}

// takeWriteBackAttempt marks the triple attempted and reports whether it was new.
func (e *Engine) takeWriteBackAttempt(trainKey string, issue int, baseSHA string) bool {
	e.writeBack.mu.Lock()
	defer e.writeBack.mu.Unlock()
	if e.writeBack.attempted == nil {
		e.writeBack.attempted = make(map[string]struct{})
	}
	k := writeBackKey(trainKey, issue, baseSHA)
	if _, ok := e.writeBack.attempted[k]; ok {
		return false
	}
	if len(e.writeBack.attempted) >= 4096 {
		e.writeBack.attempted = make(map[string]struct{})
	}
	e.writeBack.attempted[k] = struct{}{}
	return true
}

// SetTrainWriteBackDisabledForTest disables the write-back (#2572 FR-009): a resolved
// trial conflict stays on the trial branch only, exactly the pre-#2572 behaviour. Test-only.
func (e *Engine) SetTrainWriteBackDisabledForTest(disabled bool) {
	e.writeBackDisabledForTest.Store(disabled)
}

// SetWriteBackSHAExemptionDisabledForTest makes the Validate SHA-invalidation scan treat a
// written-back head like any other non-pure head (pre-#2572 behaviour). Test-only.
func (e *Engine) SetWriteBackSHAExemptionDisabledForTest(disabled bool) {
	e.writeBackSHAExemptionDisabledForTest.Store(disabled)
}

// writeBackGateOpen reports whether a write-back may be attempted for member after its
// trial-assembly conflict was resolved. Silent skips: the feature is off, the batch is not
// multi-member, no pinned base, the repo is in the policy memo or this triple was already
// attempted. Anything past the gate is an attempt and logs exactly one outcome line.
func (e *Engine) writeBackGateOpen(p trialParams, batchLen int, m trainMember) bool {
	if batchLen < 2 || e.writeBackDisabledForTest.Load() || !e.singletonCatchUpEnabled() || p.baseSHA == "" {
		return false
	}
	// Like the catch-up, the real git half is skipped under trainValidateFn unless a test
	// injects its own.
	if e.trainValidateFn != nil && e.trainWriteBackGitFn == nil {
		return false
	}
	if e.catchUpPolicyBackoff(p.repoKey()) {
		return false
	}
	return e.takeWriteBackAttempt(p.trainKey, m.item.Number, p.baseSHA)
}

// logWriteBack emits the one line per attempt: member, base SHA, outcome.
func (e *Engine) logWriteBack(p trialParams, m trainMember, outcome string) {
	e.logf(m.item.Number, "merge-train", "write-back: #%d base %s — %s\n", m.item.Number, p.baseSHA, outcome)
}

// tryWriteBackAndContinue is assembleTrialBranch's step after a conflict for m was
// resolved in the trial worktree wtDir (HEAD is the resolved merge commit; preMergeHEAD is
// the trial's HEAD before the member was merged). It returns the member to carry forward
// (with the written-back head only when the trial was rebuilt on it) and whether it was.
// It never returns an error: every failure leaves the trial
// worktree exactly as the resolution left it.
func (e *Engine) tryWriteBackAndContinue(ctx context.Context, p trialParams, batchLen int, m trainMember, wtDir, preMergeHEAD string) (trainMember, bool) {
	if !e.writeBackGateOpen(p, batchLen, m) {
		return m, false
	}
	var out writeBackOutcome
	// In flight from before the push until the marker is posted: the head moves at the
	// push and the SHA-invalidation scan must not read it as external in between.
	endPush := e.beginCatchUpPush(p.owner, p.repo, m.prNum)
	defer endPush()
	if e.trainWriteBackGitFn != nil {
		out = e.trainWriteBackGitFn(ctx, p, m)
	} else {
		out = e.runWriteBackGit(ctx, p, m)
	}
	switch out.kind {
	case wbSkipped:
		e.logWriteBack(p, m, "skipped: "+out.reason+"; trial continues unchanged, nothing charged")
		return m, false
	case wbRejected:
		e.logWriteBack(p, m, "push rejected: "+out.reason+"; trial continues unchanged, nothing charged")
		if out.policyReason != "" && e.recordCatchUpPolicyRejection(p.repoKey(), out.policyReason) {
			e.logf(m.item.Number, "merge-train", "write-back: %s rejects pushes to member branches by repository policy (%s) — skipping write-back and singleton catch-up on this repo for %s; set singleton_catch_up: off (--singleton-catch-up / FABRIK_SINGLETON_CATCH_UP) to stop probing permanently\n", p.repoKey(), out.policyReason, catchUpPolicyMemoTTL)
		}
		return m, false
	}

	// Written.
	updated := m
	updated.headSHA = out.newHead
	e.postWriteBackMarker(p, updated, m.headSHA)
	endPush()

	adopted, why := e.continueTrialFromHead(wtDir, preMergeHEAD, out.newHead)
	if adopted {
		e.logWriteBack(p, m, fmt.Sprintf("written back (member head %s -> %s); trial continues from the new head", shortSHA(m.headSHA), shortSHA(out.newHead)))
		return updated, true
	}
	e.logWriteBack(p, m, fmt.Sprintf("written back (member head %s -> %s); trial keeps the original resolution (%s)", shortSHA(m.headSHA), shortSHA(out.newHead), why))
	// The trial still contains the member's original head, so that is the head the
	// survivor keeps: the recorded head describes the trial's content. The member's live PR
	// head is now newHead, so the recorded head is stale against it. That is the fail-closed
	// direction everywhere it is compared: the singleton fast path refuses a head differing
	// from its snapshot (singletonFastPathEligible), restart adoption discards a run whose
	// member head moved (adoptTrainRun, to the existing reconstruct routes, uncharged), and
	// the prefix cache is keyed on heads so a later trial misses naturally. Landing is by the
	// trial's integration PR, never by the member's own head.
	return m, false
}

// continueTrialFromHead rebuilds the trial's last merge on the member's new head when the
// result is byte-identical to the resolution already in the trial; otherwise it restores
// the resolved commit. The trial's content is invariant either way.
func (e *Engine) continueTrialFromHead(wtDir, preMergeHEAD, newHead string) (bool, string) {
	resolvedSHA, err := gitRevParse(wtDir, "HEAD")
	if err != nil {
		return false, "could not read the resolved trial commit: " + err.Error()
	}
	restore := func() {
		for _, args := range [][]string{{"merge", "--abort"}, {"reset", "--hard", resolvedSHA}, {"clean", "-fd"}} {
			if out, rerr := gitOutputIn(wtDir, args...); rerr != nil && args[0] != "merge" {
				e.logf(0, "merge-train", "warn: write-back trial restore step %v failed: %s\n", args, out)
			}
		}
	}
	resolvedTree, err := gitRevParse(wtDir, resolvedSHA+"^{tree}")
	if err != nil {
		return false, "could not read the resolved trial tree: " + err.Error()
	}
	if out, rerr := gitOutputIn(wtDir, "reset", "--hard", preMergeHEAD); rerr != nil {
		restore()
		return false, "could not rewind the trial: " + out
	}
	if out, merr := gitCombined(wtDir, "merge", "--no-ff", "--no-edit", newHead); merr != nil {
		restore()
		return false, "merging the new member head conflicted: " + strings.TrimSpace(out)
	}
	tree, err := gitRevParse(wtDir, "HEAD^{tree}")
	if err != nil || tree != resolvedTree {
		restore()
		return false, "the trial tree would differ from the resolved one"
	}
	return true, ""
}

// runWriteBackGit is the real git half: ready the member's own worktree, merge the pinned
// base into it, finish the merge with a replayed resolution only (never Claude), verify
// the shape, stamp the audit trailer and push under a lease. Every failure path restores
// the worktree to the member's head so a later poll finds it clean and equal to the remote.
func (e *Engine) runWriteBackGit(ctx context.Context, p trialParams, m trainMember) writeBackOutcome {
	n := m.item.Number
	// Ownership, first check: no worker, no fabrik:editing, readable labels.
	if why := e.catchUpBusyReason(p, m); why != "" {
		return writeBackOutcome{kind: wbSkipped, reason: why}
	}
	wtDir, err := p.wm.PrepareCatchUp(n, p.baseBranch, m.headSHA)
	if err != nil {
		return writeBackOutcome{kind: wbSkipped, reason: err.Error()}
	}
	restore := func() {
		for _, args := range [][]string{{"merge", "--abort"}, {"reset", "--hard", m.headSHA}, {"clean", "-fd"}} {
			if out, rerr := gitOutputIn(wtDir, args...); rerr != nil && args[0] != "merge" {
				e.logf(n, "merge-train", "warn: write-back restore step %v failed: %s\n", args, out)
			}
		}
	}

	mergeMsg := fmt.Sprintf("chore(merge-train): write back the conflict resolution for #%d with %s", n, shortSHA(p.baseSHA))
	mergeOut, mergeErr := gitCombined(wtDir, "merge", "--no-ff", "-m", mergeMsg, p.baseSHA)
	if mergeErr == nil {
		// The base merges cleanly into the member: the trial conflict was between members,
		// so a base merge carries no resolution worth writing back.
		restore()
		return writeBackOutcome{kind: wbSkipped, reason: "the base merges cleanly into the member's branch (the trial conflict was not against the base), so there is no resolution to write back"}
	}
	opts := InvokeOptions{BaseBranch: p.baseBranch, MaxTurnsOverride: p.maxTurnsOverride, FabrikRoot: e.fabrikDir, FabrikRepo: e.defaultRepo(), MaxResumeFailures: e.cfg.MaxResumeFailures, NoResume: true, CatchUpBaseSHA: p.baseSHA, noClaude: true, trainEpisode: p.episode}
	resolved, _, resolveErr := e.resolveTrainConflict(ctx, p.repoKey(), m.item, wtDir, p.holdingStg, p.baseSHA, m.headSHA, mergeOut, opts)
	if resolveErr != nil {
		restore()
		if errors.Is(resolveErr, errWriteBackNeedsClaude) {
			return writeBackOutcome{kind: wbSkipped, reason: "the member-versus-base conflict is not fully replayable by rerere (it would need a new Claude resolution)"}
		}
		return writeBackOutcome{kind: wbSkipped, reason: fmt.Sprintf("conflict resolution unavailable: %v", resolveErr)}
	}
	if !resolved {
		restore()
		return writeBackOutcome{kind: wbSkipped, reason: "the replayed resolution did not complete"}
	}

	parents, perr := gitOutputIn(wtDir, "rev-list", "--parents", "-n", "1", "HEAD")
	fields := strings.Fields(parents)
	if perr != nil || len(fields) != 3 || fields[1] != m.headSHA || fields[2] != p.baseSHA {
		restore()
		return writeBackOutcome{kind: wbSkipped, reason: fmt.Sprintf("the write-back did not produce the expected merge commit (rev-list: %q)", parents)}
	}
	if err := stampCatchUpTrailer(wtDir, p.baseSHA); err != nil {
		restore()
		return writeBackOutcome{kind: wbSkipped, reason: err.Error()}
	}
	// Ownership, second check: the merge can take a while.
	if why := e.catchUpBusyReason(p, m); why != "" {
		restore()
		return writeBackOutcome{kind: wbSkipped, reason: why}
	}
	newHead, herr := gitRevParse(wtDir, "HEAD")
	if herr != nil {
		restore()
		return writeBackOutcome{kind: wbSkipped, reason: fmt.Sprintf("could not read the write-back commit: %v", herr)}
	}
	if perr := p.wm.PushCatchUp(n, m.headSHA); perr != nil {
		restore()
		var pushErr *CatchUpPushError
		if errors.As(perr, &pushErr) {
			if reason, policy := classifyCatchUpPushRejection(pushErr.Output); policy {
				return writeBackOutcome{kind: wbRejected, reason: catchUpPolicyAttemptReason, policyReason: reason}
			}
		}
		return writeBackOutcome{kind: wbRejected, reason: perr.Error()}
	}
	return writeBackOutcome{kind: wbWritten, newHead: newHead}
}

// postWriteBackMarker posts the authenticated record on the member's PR: a human
// explanation plus the machine marker (pure=false, kind=writeback). The marker never
// suppresses review feedback (a resolved merge is reviewed as usual); it lets the
// SHA-invalidation scan recognise the head as the engine's own push (headIsTrainWriteBack).
// A failed post is logged and leaves the head unrecognised — the fail-closed direction.
func (e *Engine) postWriteBackMarker(p trialParams, updated trainMember, previousHead string) {
	mk := catchUpMarker{Head: updated.headSHA, Base: p.baseSHA, Kind: catchUpKindWriteBack}
	defer e.forgetCatchUpMarker(p.owner, p.repo, updated.prNum, updated.headSHA)
	body := fmt.Sprintf("🏭 **Fabrik merge-train** — conflict resolution written back\n\n"+
		"While assembling a merge-train trial, merging this PR conflicted with the pinned base `%s`. "+
		"Fabrik replayed that resolution as a merge commit on this branch (`%s`, previously `%s`; a merge commit, "+
		"so no rebase or force-push) so it survives an abandoned or rebuilt trial. This PR still lands through the "+
		"train's trial; the merge resolved conflicts, so reviews of this push are actionable as usual.\n\n%s",
		shortSHA(p.baseSHA), shortSHA(updated.headSHA), shortSHA(previousHead), formatCatchUpMarker(mk))
	if !e.addCommentWithRetry(p.owner, p.repo, updated.item.Number, updated.prNum, body, "the write-back marker") {
		e.logf(updated.item.Number, "merge-train", "warn: no write-back marker on PR #%d — the written-back head will not be recognised by the Validate SHA scan\n", updated.prNum)
	}
}
