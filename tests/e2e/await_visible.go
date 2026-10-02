//go:build e2e

package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tests/e2e/awaitvisible"
)

// The awaitVisible family (#1974, ADR-1974).
//
// GitHub is eventually consistent, and the engine reads through paths that lag
// independently. A harness write (a board add, a PR opened with "Closes #N", a
// Status move) is not visible to every reader the moment the mutation returns.
// Exposing the item to the engine before then makes the scenario either never
// reach the state it tests or fail for a reason that is not the engine's.
//
// Each helper here blocks until the write is visible THROUGH THE SURFACE THE
// ENGINE CONSULTS FOR THAT FACT, within an explicit per-call timeout:
//
//	board item / Status   ProjectV2 items(first:100) GraphQL connection (fetchBoardItems)
//	closing linkage       issue.closedByPullRequestsReferences (the engine's read)
//	                      AND pullRequest.closingIssuesReferences (the computed PR side)
//	mergeable             REST /pulls/N .mergeable_state (what the engine reads)
//	label                 REST issue read (the engine's live label read)
//	PR for a branch       REST /pulls?head=owner:branch (FetchLinkedPR)
//
// All of them share ONE polling loop, awaitvisible.Poll.
//
// Classification rule (ADR-1974). Waiting for a HARNESS write to become visible
// is lag: a timeout is Inconclusive (#1973) — the precondition never arose — and
// is never a silent pass. Waiting for the ENGINE to do something (WaitForProject-
// Status after the engine moves an item, WaitForIssueLabel for an engine-applied
// label, WaitForCheckConclusion, the waitForLogMatch family) is an assertion
// about engine behaviour and stays t.Fatalf: retrying it into green would mask a
// regression. A state that will not resolve by waiting (mergeable_state "dirty")
// is a scenario assertion and also stays t.Fatalf.
//
// Inconclusive must end a TOP-LEVEL test from its own goroutine; called from a
// subtest it is a loud t.Fatalf (see Inconclusive). Seed in the top-level test.

// Test seams. The loop itself is awaitvisible.Poll; the wrappers read the clock
// and sleep through these so the wrapper tests need no real time.
var (
	awaitNow   = time.Now
	awaitSleep = time.Sleep
)

// Per-read-path intervals. GraphQL reads cost ~1 point each against a budget the
// suite and the bed engine share (#1695), so they are polled no faster than the
// pre-existing 5–10s loops; REST reads are free.
const (
	awaitGraphQLInterval = 10 * time.Second
	awaitRESTInterval    = 5 * time.Second
)

// awaitSeedTimeout is the bound the seed helpers use for a harness write's
// visibility. It is deliberately generous: the 2026-09-30 partial outage
// stretched board-listing lag past five minutes, and a timeout here costs only
// an Inconclusive retry, never a false failure.
const awaitSeedTimeout = 10 * time.Minute

func awaitSpec(t testing.TB, what string, timeout, interval time.Duration) awaitvisible.Spec {
	return awaitvisible.Spec{
		What: what, Timeout: timeout, Interval: interval,
		Now: awaitNow, Sleep: awaitSleep,
		OnRetry: func(err error) { t.Logf("awaitVisible(%s): transient read error (will retry): %v", what, err) },
	}
}

// finishAwait ends the calling test according to a Poll result: visible → return;
// aborted (a state that will not resolve by waiting, or a misconfigured wait) →
// t.Fatalf; timed out → Inconclusive.
func finishAwait(t testing.TB, res awaitvisible.Result) {
	t.Helper()
	switch {
	case res.Visible:
		if res.Attempts > 1 {
			t.Logf("awaitVisible: %s visible after %d reads (%s)", res.What, res.Attempts, res.Elapsed.Round(time.Second))
		}
	case res.Aborted != nil:
		t.Fatalf("awaitVisible: %s: %v", res.What, res.Aborted)
	default:
		Inconclusive(t, "%s", res.Reason())
	}
}

// AwaitBoardItemVisible blocks until the issue appears in the test board's
// ProjectV2 item listing — the connection the engine's bootstrap fetch lists.
// AddIssueToProject returns when the mutation returns; the listing can lag it by
// minutes, and an item the listing does not show yet is "discovered as new" by
// the engine instead of being part of the state the scenario seeded.
func AwaitBoardItemVisible(t testing.TB, env *Env, repo string, issueNumber int, timeout time.Duration) {
	t.Helper()
	finishAwait(t, awaitBoardItem(t, env, repo, issueNumber, "", timeout))
}

// AwaitStatusVisible is AwaitBoardItemVisible and additionally requires the
// item's Status in the listing to equal status — the read-your-write check after
// SetIssueStatus.
//
// Use it ONLY while the bed is down, or for an item the engine will not act on in
// that Status. With a running bed the engine can move the item on within a poll;
// a lagging first read then sees the old Status, and the item never shows status
// again, so the wait would burn the whole timeout and end Inconclusive for a
// legitimate engine action — breaking the "harness-write lag only" rule. Against a
// running bed use AwaitBoardItemVisible: an item that has appeared in the listing
// stays in it, whatever the engine does next.
func AwaitStatusVisible(t testing.TB, env *Env, repo string, issueNumber int, status string, timeout time.Duration) {
	t.Helper()
	finishAwait(t, awaitBoardItem(t, env, repo, issueNumber, status, timeout))
}

func awaitBoardItem(t testing.TB, env *Env, repo string, issueNumber int, status string, timeout time.Duration) awaitvisible.Result {
	what := fmt.Sprintf("board item %s#%d in the ProjectV2 listing", repo, issueNumber)
	if status != "" {
		what = fmt.Sprintf("board item %s#%d with Status %q in the ProjectV2 listing", repo, issueNumber, status)
	}
	return awaitvisible.Poll(awaitSpec(t, what, timeout, awaitGraphQLInterval), func() (bool, string, error) {
		items, err := fetchBoardItems(env)
		if err != nil {
			return false, "", err
		}
		it, ok := findBoardItem(items, repo, issueNumber)
		if !ok {
			return false, fmt.Sprintf("listing had %d issue item(s), none was #%d", len(items), issueNumber), nil
		}
		if status != "" && it.Status != status {
			return false, fmt.Sprintf("item listed with Status %q", it.Status), nil
		}
		return true, fmt.Sprintf("item listed with Status %q", it.Status), nil
	})
}

// AwaitClosingLinkage blocks until GitHub shows the PR's "Closes #N" linkage on
// both sides: the issue's closedByPullRequestsReferences (the field the engine's
// board fetch and broken-linkage check read) and the PR's closingIssuesReferences.
// Exposing the item earlier lets the review gate's broken-linkage check pause it
// (#1962). Only meaningful for a default-base PR: GitHub creates no link for a
// PR targeting a non-default base, so callers must not wait on one.
func AwaitClosingLinkage(t testing.TB, env *Env, repo string, issueNum, prNum int, timeout time.Duration) {
	t.Helper()
	finishAwait(t, awaitClosingLinkage(t, env, repo, issueNum, prNum, timeout))
}

func awaitClosingLinkage(t testing.TB, env *Env, repo string, issueNum, prNum int, timeout time.Duration) awaitvisible.Result {
	what := fmt.Sprintf("PR #%d's Closes #%d linkage on %s (issue.closedByPullRequestsReferences and pullRequest.closingIssuesReferences)", prNum, issueNum, repo)
	owner, name, ok := splitRepo(repo)
	if !ok {
		return awaitvisible.Result{What: what, Timeout: timeout, Aborted: fmt.Errorf("bad repo: %q", repo)}
	}
	return awaitvisible.Poll(awaitSpec(t, what, timeout, awaitGraphQLInterval), func() (bool, string, error) {
		out, err := ghOutput(env, "api", "graphql", "-f", "query="+awaitvisible.ClosingLinkageQuery,
			"-f", "owner="+owner, "-f", "name="+name,
			"-F", "issue="+strconv.Itoa(issueNum), "-F", "pr="+strconv.Itoa(prNum))
		if err != nil {
			return false, "", fmt.Errorf("closing-linkage read: %w\n%s", err, strings.TrimSpace(out))
		}
		link, perr := awaitvisible.ParseClosingLinkage([]byte(out), issueNum, prNum)
		if perr != nil {
			return false, "", perr
		}
		return link.Both(), link.String(), nil
	})
}

// AwaitPRMergeableComputed blocks until GitHub has computed the PR's
// mergeability (a non-null `mergeable`, i.e. mergeable_state other than
// "unknown"). It is the primitive; AwaitPRMergeableSettled layers #1982's
// verdict on it.
func AwaitPRMergeableComputed(t testing.TB, env *Env, repo string, prNumber int, timeout time.Duration) {
	t.Helper()
	finishAwait(t, awaitPRMergeable(t, env, repo, prNumber, timeout, false))
}

// AwaitPRMergeableSettled blocks until GitHub has computed the PR's mergeability
// AND it is one the engine's merge gate can clear on: "clean", or "unstable" (the
// gate then classifies the individual checks). A scenario that needs the engine's
// landing decision to run on its first look at an item calls this before exposing
// the item: while the state is "unknown" the merge gate claims the item and
// defers, so the landing decision never runs in that poll. "blocked" can still
// clear (CI or reviews settling) and is waited out. "dirty" and "behind" will not
// resolve by themselves in a scenario, so they fail immediately — that is an
// assertion about the scenario, not lag, and stays t.Fatalf. Only the timeout is
// Inconclusive. This narrows the window in which a concurrent merge can move the
// base; it does not close it.
func AwaitPRMergeableSettled(t testing.TB, env *Env, repo string, prNumber int, timeout time.Duration) {
	t.Helper()
	finishAwait(t, awaitPRMergeable(t, env, repo, prNumber, timeout, true))
}

func awaitPRMergeable(t testing.TB, env *Env, repo string, prNumber int, timeout time.Duration, settled bool) awaitvisible.Result {
	what := fmt.Sprintf("PR %s#%d mergeability computed (mergeable_state != unknown)", repo, prNumber)
	if settled {
		what = fmt.Sprintf("PR %s#%d mergeable_state clean/unstable", repo, prNumber)
	}
	return awaitvisible.Poll(awaitSpec(t, what, timeout, awaitRESTInterval), func() (bool, string, error) {
		state, err := tryPRMergeableState(env, repo, prNumber)
		if err != nil {
			return false, "", err
		}
		detail := fmt.Sprintf("mergeable_state %q", state)
		if !settled {
			return awaitvisible.MergeableComputed(state), detail, nil
		}
		switch awaitvisible.ClassifyMergeable(state) {
		case awaitvisible.MergeableSettled:
			return true, detail, nil
		case awaitvisible.MergeableFatal:
			return false, detail, awaitvisible.Abort(fmt.Errorf("mergeable_state is %q — it will not settle by itself; the scenario cannot place its item", state))
		}
		return false, detail, nil
	})
}

// AwaitLabelVisible blocks until a fresh read of the issue shows label. Use it
// after the HARNESS applies a label the engine will later act on; a label the
// engine applies is waited for with WaitForIssueLabel (an engine effect).
func AwaitLabelVisible(t testing.TB, env *Env, repo string, issueNumber int, label string, timeout time.Duration) {
	t.Helper()
	what := fmt.Sprintf("label %q on %s#%d", label, repo, issueNumber)
	finishAwait(t, awaitvisible.Poll(awaitSpec(t, what, timeout, awaitRESTInterval), func() (bool, string, error) {
		labels, err := tryIssueLabels(env, repo, issueNumber)
		if err != nil {
			return false, "", err
		}
		return awaitvisible.HasLabel(labels, label), fmt.Sprintf("labels %v", labels), nil
	}))
}

// AwaitLabelGone is AwaitLabelVisible's counterpart for a label the HARNESS
// removed: it blocks until a fresh read of the issue no longer shows label. Use it
// before a poll is expected to observe the removal (#1978's BatchCap waits on the
// REST issue read after unpausing, then triggers one poll).
func AwaitLabelGone(t testing.TB, env *Env, repo string, issueNumber int, label string, timeout time.Duration) {
	t.Helper()
	what := fmt.Sprintf("label %q gone from %s#%d", label, repo, issueNumber)
	finishAwait(t, awaitvisible.Poll(awaitSpec(t, what, timeout, awaitRESTInterval), func() (bool, string, error) {
		labels, err := tryIssueLabels(env, repo, issueNumber)
		if err != nil {
			return false, "", err
		}
		return !awaitvisible.HasLabel(labels, label), fmt.Sprintf("labels %v", labels), nil
	}))
}

// AwaitPRForBranchVisible blocks until the PR the HARNESS opened on
// fabrik/issue-<N> is resolvable by the engine's own convention
// (FetchLinkedPR: GET /pulls?head=<owner>:fabrik/issue-N). It returns the PR
// number. LinkedPRNumber is the engine-effect counterpart: it waits for a PR the
// ENGINE creates, and stays t.Fatalf.
func AwaitPRForBranchVisible(t testing.TB, env *Env, repo string, issueNumber int, timeout time.Duration) int {
	t.Helper()
	pr, res := awaitPRForBranch(t, env, repo, issueNumber, timeout)
	finishAwait(t, res)
	return pr
}

func awaitPRForBranch(t testing.TB, env *Env, repo string, issueNumber int, timeout time.Duration) (int, awaitvisible.Result) {
	branch := fmt.Sprintf("fabrik/issue-%d", issueNumber)
	what := fmt.Sprintf("a PR with head %s on %s (GET /pulls?head=…)", branch, repo)
	owner, name, ok := splitRepo(repo)
	if !ok {
		return 0, awaitvisible.Result{What: what, Timeout: timeout, Aborted: fmt.Errorf("bad repo: %q", repo)}
	}
	var pr int
	res := awaitvisible.Poll(awaitSpec(t, what, timeout, awaitRESTInterval), func() (bool, string, error) {
		out, err := ghOutput(env, "api",
			fmt.Sprintf("repos/%s/%s/pulls?head=%s:%s&state=all&per_page=1", owner, name, owner, branch),
			"--jq", ".[0].number")
		if err != nil {
			return false, "", fmt.Errorf("list PRs for %s: %w\n%s", branch, err, strings.TrimSpace(out))
		}
		out = strings.TrimSpace(out)
		if out == "" || out == "null" {
			return false, "no PR listed for the branch", nil
		}
		n, err := strconv.Atoi(out)
		if err != nil {
			return false, "", fmt.Errorf("parse PR number %q: %w", out, err)
		}
		pr = n
		return true, fmt.Sprintf("PR #%d", n), nil
	})
	return pr, res
}
