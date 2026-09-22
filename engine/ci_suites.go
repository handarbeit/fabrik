package engine

import (
	"fmt"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// defaultPostPushDwell is the fallback for Config.PostPushDwell.
const defaultPostPushDwell = 90 * time.Second

// checkSuiteReader is the one method the suite hold needs. Both
// GitHubClient (e.client, used by the merge-train workers) and
// boardcache.ReadClient (e.readClient, used by the settle path) satisfy it, and
// both are live pass-throughs — the check_suite webhook is a documented no-op,
// so there is no cached suite state to serve.
type checkSuiteReader interface {
	FetchCheckSuites(owner, repo, sha string) ([]gh.CheckSuite, error)
}

// postPushDwell returns the configured post-push dwell, defaulting to 90s. It
// bounds how long a zero-run check suite is still assumed to be about to
// register its first job (#1822), as well as the pre-existing zero-check-run
// dwell in settlePRMergeState.
func (e *Engine) postPushDwell() time.Duration {
	if e.cfg.PostPushDwell > 0 {
		return e.cfg.PostPushDwell
	}
	return defaultPostPushDwell
}

// ciSuiteHold reports whether a head SHA has check-suite work outstanding that
// its check-run set does not yet reflect (#1822). It is the single primitive
// every "green and complete" judgement for a PR head consults — the wait_for_ci
// settle (rules 9/18/19), classifyLandingCI, and pollTrainCI — so the gates
// cannot disagree on one SHA.
//
// A check run exists only once its job has been scheduled onto a runner, so a
// needs:-dependent job (or one queued for a runner) is invisible to
// FetchCheckRuns; an all-green prefix reads as a complete pass. The check
// suite is created at queue time and stays non-completed until every job has
// run, which is exactly the signal missing from the run set. The discriminator
// (gh.OutstandingCheckSuites) counts a non-completed suite that has produced
// runs, or a run-less one still younger than the post-push dwell, and ignores
// an old run-less suite — an inert installed App must never deadlock the gate.
//
// Fail-safe: a read error holds (hold=true) with the error in detail, matching
// the review gate's "blocks conservatively when a fetch errors" idiom. The
// caller's own backstop (CIBackstopTimeout / the train's CI timeout) bounds the
// wait, so a persistent error escalates rather than hanging.
//
// Callers must only consult this on a path that would otherwise return green:
// a confirmed check-run failure or pending run already decides the verdict, so
// the extra read is spent once per clear attempt, not on every poll of a red
// or pending SHA.
func (e *Engine) ciSuiteHold(reader checkSuiteReader, owner, repo, sha string) (hold bool, detail string) {
	suites, err := reader.FetchCheckSuites(owner, repo, sha)
	if err != nil {
		return true, fmt.Sprintf("check-suite read failed: %v", err)
	}
	out := gh.OutstandingCheckSuites(suites, e.now(), e.postPushDwell())
	if len(out) == 0 {
		return false, ""
	}
	return true, describeOutstandingSuites(out)
}

// describeOutstandingSuites renders outstanding suites for a Reason or comment,
// e.g. "check suite(s) still running: github-actions (in_progress, 5 runs)".
func describeOutstandingSuites(out []gh.CheckSuite) string {
	parts := make([]string, 0, len(out))
	for _, s := range out {
		app := s.AppSlug
		if app == "" {
			app = "unknown app"
		}
		parts = append(parts, fmt.Sprintf("%s (%s, %d runs)", app, s.Status, s.LatestCheckRunsCount))
	}
	return "check suite(s) still running: " + strings.Join(parts, ", ")
}
