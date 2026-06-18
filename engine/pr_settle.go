package engine

import (
	"fmt"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// PRMergeStatus classifies the PR merge/CI state returned by settlePRMergeState.
type PRMergeStatus int

const (
	// PRMergeNoPR is returned when there is no linked PR (gate clears for both gates).
	PRMergeNoPR PRMergeStatus = iota
	// PRMergeUnsettled indicates a transient state; both gates block and re-evaluate
	// on the next poll without label churn (ADR-032 R10c).
	PRMergeUnsettled
	// PRMergeReady indicates CI has passed (or there is no CI); the merge gate clears.
	PRMergeReady
	// PRMergeConflicting indicates the PR cannot be merged due to a base-branch
	// conflict; checkMergeabilityGate applies fabrik:rebase-needed.
	PRMergeConflicting
	// PRMergeBlocked indicates CI checks have failed.
	PRMergeBlocked
	// PRMergeTerminal indicates the PR is already merged or closed; both gates clear.
	PRMergeTerminal
)

// PRSettleResult is the output of settlePRMergeState.
type PRSettleResult struct {
	Status         PRMergeStatus
	Reason         string
	MergeableState string // raw mergeable_state; used by checkCIGate R3 timeout path
	CheckRuns      []gh.CheckRun
	PR             *gh.PRDetails
}

// settlePRMergeState fetches all PR merge/CI state in a single pass and returns
// a typed PRSettleResult. Called once per poll cycle from poll.go; both
// checkMergeabilityGate and checkCIGate receive the result, ensuring they see
// identical GitHub state within a single poll cycle.
//
// The stage parameter is accepted for API consistency (callers already have it)
// but is not used by the primitive itself; gating on WaitForCI is the caller's
// responsibility.
func (e *Engine) settlePRMergeState(item gh.ProjectItem, _ *stages.Stage) PRSettleResult {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	itemRepo := itemOwnerRepoString(item, e.defaultRepo())

	pr, err := e.readClient.FetchLinkedPR(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "settle", "could not fetch linked PR: %v — treating as unsettled\n", err)
		return PRSettleResult{Status: PRMergeUnsettled, Reason: fmt.Sprintf("FetchLinkedPR error: %v", err)}
	}
	if pr == nil || pr.Number == 0 {
		return PRSettleResult{Status: PRMergeNoPR, Reason: "no linked PR"}
	}

	if pr.Merged {
		return PRSettleResult{Status: PRMergeTerminal, Reason: fmt.Sprintf("PR #%d already merged", pr.Number), PR: pr}
	}
	if pr.State == "closed" {
		return PRSettleResult{Status: PRMergeTerminal, Reason: fmt.Sprintf("PR #%d closed without merging", pr.Number), PR: pr}
	}

	mergeable, mergeableState, err := e.readClient.FetchPRMergeableFields(owner, repo, pr.Number)
	if err != nil {
		e.logf(item.Number, "settle", "could not fetch PR #%d mergeable fields: %v — treating as unsettled\n", pr.Number, err)
		return PRSettleResult{Status: PRMergeUnsettled, Reason: fmt.Sprintf("FetchPRMergeableFields error: %v", err), PR: pr}
	}

	if mergeable == nil {
		e.logf(item.Number, "settle", "PR #%d mergeable=null — GitHub still computing\n", pr.Number)
		return PRSettleResult{Status: PRMergeUnsettled, Reason: "mergeable=null (GitHub computing)", MergeableState: mergeableState, PR: pr}
	}
	if mergeableState == "unknown" {
		e.logf(item.Number, "settle", "PR #%d mergeable_state=unknown — transient\n", pr.Number)
		return PRSettleResult{Status: PRMergeUnsettled, Reason: "mergeable_state=unknown", MergeableState: mergeableState, PR: pr}
	}
	if !*mergeable {
		e.logf(item.Number, "settle", "PR #%d not mergeable (base conflict)\n", pr.Number)
		return PRSettleResult{Status: PRMergeConflicting, Reason: "mergeable=false", MergeableState: mergeableState, PR: pr}
	}

	// ADR-033: mergeable_state ∈ {clean, unstable} → branch protection satisfied;
	// skip per-check classification to avoid blocking on non-required workflows.
	if gh.MergeableStateAccepted(mergeableState) {
		e.logf(item.Number, "settle", "PR #%d mergeable_state=%q — ready\n", pr.Number, mergeableState)
		return PRSettleResult{Status: PRMergeReady, Reason: fmt.Sprintf("mergeable_state=%q", mergeableState), MergeableState: mergeableState, PR: pr}
	}

	if pr.HeadSHA == "" {
		e.logf(item.Number, "settle", "PR #%d HeadSHA empty — treating as unsettled\n", pr.Number)
		return PRSettleResult{Status: PRMergeUnsettled, Reason: "HeadSHA empty", PR: pr}
	}

	checkRuns, err := e.readClient.FetchCheckRuns(owner, repo, pr.HeadSHA)
	if err != nil {
		e.logf(item.Number, "settle", "could not fetch check runs for SHA %s: %v — treating as unsettled\n",
			pr.HeadSHA[:min(8, len(pr.HeadSHA))], err)
		return PRSettleResult{Status: PRMergeUnsettled, Reason: fmt.Sprintf("FetchCheckRuns error: %v", err), MergeableState: mergeableState, PR: pr}
	}

	if len(checkRuns) > 0 {
		e.store.Apply(itemstate.PRChecksObserved{
			Repo:   itemRepo,
			Number: item.Number,
		})
	}

	if len(checkRuns) == 0 {
		var hadChecks bool
		var lpr *itemstate.LinkedPRState
		if snap, snapErr := e.store.Get(itemRepo, item.Number); snapErr == nil {
			lpr = snap.LinkedPR()
			if lpr != nil {
				hadChecks = lpr.HasHadChecks
			}
		}

		if hadChecks {
			e.logf(item.Number, "settle", "no check runs for SHA %s — post-push registration delay\n",
				pr.HeadSHA[:min(8, len(pr.HeadSHA))])
			// MergeableState intentionally omitted: checkCIGate uses MergeableState to
			// detect R3 (OPEN+BLOCKED+never-had-checks). This case is hadChecks=true,
			// so R3 must NOT fire regardless of the raw mergeable_state value.
			return PRSettleResult{Status: PRMergeUnsettled, Reason: "post-push registration delay (hadChecks)", PR: pr}
		}

		if lpr != nil && !lpr.LastHeadSHAUpdate.IsZero() {
			dwell := e.cfg.PostPushDwell
			if dwell <= 0 {
				dwell = 90 * time.Second
			}
			if elapsed := time.Since(lpr.LastHeadSHAUpdate); elapsed < dwell {
				e.logf(item.Number, "settle", "no check runs for SHA %s — post-push dwell active (%.0fs remaining)\n",
					pr.HeadSHA[:min(8, len(pr.HeadSHA))], (dwell-elapsed).Seconds())
				// MergeableState intentionally omitted: post-push dwell is not an R3 case.
				return PRSettleResult{Status: PRMergeUnsettled, Reason: "post-push dwell active", PR: pr}
			}
		}

		// R3 (BLOCKED+no-checks) and non-check branch-protection signals: return
		// Unsettled so checkCIGate can apply its timeout escalation using MergeableState.
		if mergeableState != "" && mergeableState != "unknown" {
			e.logf(item.Number, "settle", "no check runs — mergeable_state=%q (R3 / branch-protection signal)\n", mergeableState)
			return PRSettleResult{Status: PRMergeUnsettled, Reason: fmt.Sprintf("no check runs, mergeable_state=%q", mergeableState), MergeableState: mergeableState, PR: pr}
		}

		e.logf(item.Number, "settle", "no check runs for SHA %s — no CI configured\n",
			pr.HeadSHA[:min(8, len(pr.HeadSHA))])
		return PRSettleResult{Status: PRMergeReady, Reason: "no CI configured", MergeableState: mergeableState, PR: pr}
	}

	// Classify check runs: pending → Unsettled, any failed → Blocked, all green → Ready.
	var hasPending, hasFailed bool
	for _, cr := range checkRuns {
		switch cr.Status {
		case "queued", "in_progress":
			hasPending = true
		case "completed":
			switch cr.Conclusion {
			case "failure", "timed_out", "action_required":
				hasFailed = true
			}
		}
	}

	if hasFailed {
		return PRSettleResult{Status: PRMergeBlocked, Reason: "CI checks failed", MergeableState: mergeableState, CheckRuns: checkRuns, PR: pr}
	}
	if hasPending {
		return PRSettleResult{Status: PRMergeUnsettled, Reason: "CI checks pending", MergeableState: mergeableState, CheckRuns: checkRuns, PR: pr}
	}
	return PRSettleResult{Status: PRMergeReady, Reason: "all CI checks passed", MergeableState: mergeableState, CheckRuns: checkRuns, PR: pr}
}
