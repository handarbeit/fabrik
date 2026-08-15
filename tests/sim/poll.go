package sim

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// workerYield is a small, fixed real-wall-clock pause used only when a poll
// cycle dispatched no worker at all (see RunPoll). It is what
// retryCooldownPolls (failure_shapes_test.go) and any other real-time-
// anchored wait (e.g. processItem's retry-cooldown check, deliberately
// outside the Clock seam) still rely on: a sequence of "nothing to do"
// polls must keep eating real wall-clock time for a real cooldown to ever
// elapse, the same way it would in production. When a worker *was*
// dispatched, RunPoll waits for its actual completion instead (see
// waitForWorkerQuiescence) — this constant plays no part in that path.
const workerYield = 100 * time.Millisecond

// workerQuiescencePollInterval is how often RunPoll re-checks
// Engine.HasInFlightWorker while waiting for a dispatched worker to reach
// quiescence. Small relative to any real-time-bound step a worker might be
// sleeping through (see workerQuiescenceTimeout's doc comment), so a worker
// that finishes quickly — the common case, since simclaude's scripted
// responses return synchronously — is observed almost immediately rather
// than waiting out a fixed interval regardless of actual completion time.
const workerQuiescencePollInterval = 5 * time.Millisecond

// workerQuiescenceTimeout bounds how long RunPoll will wait for
// Engine.HasInFlightWorker to report quiescence before failing the test
// loudly. Generous relative to every known real-time-bound step a single
// dispatch can pass through — engine/item.go's lockVerifyDelay (2s) is the
// largest under normal conditions — so a legitimate, if slow, dispatch is
// never mistaken for a hang, while a genuinely stuck worker still fails
// well within a single package's -timeout budget instead of hanging the
// suite. Set to 60s rather than something closer to lockVerifyDelay's 2s:
// an initial 15s value, chosen against an idle machine, itself produced two
// spurious timeouts (`TestReviewAuthorityReinvokesOnChangesRequested`,
// `TestReviewAuthorityCycleLimitPauses`) when the full package's ~30
// t.Parallel() scenarios ran concurrently under -race on a machine already
// under severe external CPU load (this package's own goroutines contending
// for the same starved CPU that motivated this fix in the first place) —
// both tests passed cleanly in isolation and on a second full-package run
// moments later, confirming genuine scheduling contention rather than a
// logic bug. A "generous" timeout (per #1450's own request) has to be
// generous enough to survive the condition it exists to be robust against.
const workerQuiescenceTimeout = 60 * time.Second

// RunPoll advances Clock by env.PollInterval, drives exactly one engine poll
// cycle via the Engine.PollOnce test seam (ADR-1449), fails the test on
// error, and then waits for the cycle's real-time cost to actually elapse:
// waitForWorkerQuiescence if this cycle dispatched a worker, or a fixed
// workerYield pause if it didn't (see each constant's doc comment for why
// the two cases need different treatment).
//
// Advancing the clock here — not just before the poll loop starts — is what
// keeps itemstate.CooldownAt-based dispatch suppression (R3's engine-local
// group) from becoming permanently stuck: a cooldown is stamped as
// e.now()+duration, so without this a scenario's Clock would sit frozen at
// its start time forever and no cooldown would ever expire, deadlocking
// dispatch exactly as it would if a real poll loop's wall-clock cadence
// simply stopped.
func RunPoll(t *testing.T, env *Env) {
	t.Helper()
	if env.PollInterval > 0 {
		env.Clock.Advance(env.PollInterval)
	}
	if err := env.Engine.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if env.Engine.HasInFlightWorker() {
		waitForWorkerQuiescence(t, env)
	} else {
		time.Sleep(workerYield)
	}
}

// waitForWorkerQuiescence blocks until Engine.HasInFlightWorker reports no
// worker in flight, polling at workerQuiescencePollInterval, and fails the
// test loudly if workerQuiescenceTimeout elapses first. Only called when
// RunPoll's own PollOnce call dispatched at least one worker (checked
// immediately after PollOnce returns — WorkerEntered is applied
// synchronously before the goroutine starts, so this read cannot race a
// dispatch that just happened).
//
// This is a deliberate, narrow exception to R4's "a scenario must never
// depend on real sleeping" — that requirement targets the live-bed anti-
// pattern of sleeping past a GitHub-side async event (a review arriving, CI
// finishing) that the scenario cannot otherwise observe. It says nothing
// about the engine's own dispatch concurrency: poll() hands each item's work
// to a background goroutine and returns immediately, and part of that
// goroutine's real, production behavior is genuinely real-time-bound —
// acquireLockAndVerify's multi-instance lock-verify step (engine/item.go's
// lockVerifyDelay, 2s) sleeps on the real clock, not the injected one,
// because it exists to let a *different Fabrik process* observe a competing
// lock, which the Clock seam has no way to represent.
//
// This replaces an earlier design where every poll — dispatched or not —
// paid the same fixed workerYield sleep, on the theory that it gave a
// dispatched goroutine *some* wall-clock time to progress before returning
// control to AdvanceUntil's poll-count loop, relying on enough such cycles
// eventually covering however long the worker really took. That assumption
// broke under CPU contention: a CI runner failure (#1450, PR #1538, run
// 31459112834) showed TestCIFixReinvokeCycleLimit exhausting an 80-poll
// AdvanceUntil bound while passing reliably in local runs, because a
// starved worker goroutine needed more real time per poll than the fixed
// sleep assumed, and a poll-count bound has no way to detect that. Waiting
// for the dispatched worker's *actual* completion — via
// Engine.HasInFlightWorker, the same liveness signal production's own
// idle-upgrade and shutdown-pause logic treat as authoritative — removes
// the guess entirely for the case that actually needs it: RunPoll now
// returns as soon as a dispatched worker is done, whether that took 5ms or
// several seconds, so a poll-count bound means what it says regardless of
// runner load. A poll that dispatched nothing still takes the old fixed
// workerYield path (see its own doc comment) — that case was never the
// source of the reported flake, and several existing waits
// (retryCooldownPolls, cfg-level real-time cooldowns) depend on its
// wall-clock floor to make a real cooldown elapse across "nothing to do"
// polls, the same way it would in production.
func waitForWorkerQuiescence(t *testing.T, env *Env) {
	t.Helper()
	deadline := time.Now().Add(workerQuiescenceTimeout)
	for env.Engine.HasInFlightWorker() {
		if time.Now().After(deadline) {
			t.Fatalf("RunPoll: a dispatched worker was still in-flight after %s — either a genuinely hung invocation or workerQuiescenceTimeout needs raising\n\n%s",
				workerQuiescenceTimeout, diagnostics(env))
		}
		time.Sleep(workerQuiescencePollInterval)
	}
}

// RunPolls drives n poll cycles in sequence.
func RunPolls(t *testing.T, env *Env, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		RunPoll(t, env)
	}
}

// AdvanceUntil drives poll cycles, calling cond after each, until cond
// returns true or maxPolls is reached — whichever comes first. Returns
// normally when cond becomes true (including immediately, before any poll,
// if cond already holds). On exhaustion it fails the test with a diagnostic
// containing the board's current mutation log — R4/AC7: a failing assertion
// must print enough to diagnose without a re-run.
//
// cond is called with env so it can inspect any part of the harness's
// state (labels, PR state, board status) it needs.
//
// Also checked after every poll: clonePauseDetected (below) — a genuinely
// unsatisfiable state, not merely a slow one, and worth aborting on
// immediately rather than burning the rest of maxPolls doing nothing but
// reads. See its own doc comment for why this check is scoped narrowly to
// the repo-clone-failure pause specifically, not "any paused member."
func AdvanceUntil(t *testing.T, env *Env, cond func(*Env) bool, maxPolls int) {
	t.Helper()
	if cond(env) {
		return
	}
	if num, reason, found := clonePauseDetected(t, env); found {
		t.Fatalf("AdvanceUntil: #%d is paused by a failed repo clone before this wait even began — polling further cannot resolve this\n\npause reason:\n%s\n\n%s",
			num, reason, diagnostics(env))
	}
	for i := 0; i < maxPolls; i++ {
		RunPoll(t, env)
		if cond(env) {
			return
		}
		if num, reason, found := clonePauseDetected(t, env); found {
			t.Fatalf("AdvanceUntil: #%d is paused by a failed repo clone (after %d poll(s)) — polling further cannot resolve this\n\npause reason:\n%s\n\n%s",
				num, i+1, reason, diagnostics(env))
		}
	}
	t.Fatalf("AdvanceUntil: condition not met after %d polls\n\n%s", maxPolls, diagnostics(env))
}

// clonePauseMarker is the exact comment prefix ensureRepoReady (engine/engine.go)
// posts when a real `git clone` fails and the owning item is paused as a
// result (fabrik:paused + fabrik:awaiting-input). It is a stable, narrow
// signal: no scenario in this package ever scripts or expects a clone
// failure, so its presence always means the underlying git operation was
// genuinely interrupted — most plausibly by this package's own heavy
// parallel real-git load (tests/sim and tests/sim/simgh both drive real git
// hard; merge-train scenarios in particular add repeated merge/push/gc work
// on top) contending for the same clone/filesystem resources, not an engine
// logic defect. See #1452's review-comment finding.
const clonePauseMarker = "🏭 **Fabrik — cannot clone repo**"

// clonePauseDetected scans env's board for an item paused specifically by a
// failed repo clone. Deliberately narrower than "any fabrik:paused member":
// several scenarios in this package (merge-train ejection, the runaway
// guard, the red-singleton reroute) deliberately wait through or for an
// *ordinary* pause, each with its own distinct comment wording — those are
// legitimate awaited states, not unsatisfiable ones, and a blanket "any
// pause aborts the wait" check would misfire on every one of them. A
// clone-failure pause is different in kind: nothing in the ordinary poll
// loop ever clears it (it requires a human, or a retrying caller matching
// the original owner, per ensureRepoReady's own retry-boundary comment), so
// once one appears, no scenario waiting on anything else can make progress —
// AdvanceUntil's mutation log would otherwise show 20 polls of nothing but
// reads before timing out, exactly the confusing shape this check replaces
// with a direct diagnosis.
//
// gh.ProjectItem.Comments is already populated by FetchProjectBoard in this
// harness (unlike production's shallow board query), so this needs no
// separate per-item FetchIssueComments round trip.
func clonePauseDetected(t *testing.T, env *Env) (issueNumber int, reason string, found bool) {
	t.Helper()
	board, err := env.Sim.FetchProjectBoard(env.Owner, env.Repo, env.ProjectNum, "User")
	if err != nil {
		return 0, "", false // a fetch failure here is reported by diagnostics() at the actual failure site
	}
	for _, item := range board.Items {
		if !hasLabel(item.Labels, "fabrik:paused") || !hasLabel(item.Labels, "fabrik:awaiting-input") {
			continue
		}
		for _, c := range item.Comments {
			if strings.Contains(c.Body, clonePauseMarker) {
				return item.Number, c.Body, true
			}
		}
	}
	return 0, "", false
}

// diagnostics renders the harness's current state for a failing assertion —
// the board state (every item's status and labels), the most recent pause
// comment for every paused item (see pausedItemReasons below), and the
// mutation log's most recent entries (via Instrumented.Log().Dump) — so a
// test failure is diagnosable from CI output alone, without a re-run
// (R4/AC7).
func diagnostics(env *Env) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== board state ===\n%s\n", boardSummary(env))
	if reasons := pausedItemReasons(env); reasons != "" {
		fmt.Fprintf(&b, "=== pause reasons ===\n%s\n", reasons)
	}
	fmt.Fprintf(&b, "=== mutation log (most recent) ===\n%s\n", env.Sim.Log().Dump(50))
	return b.String()
}

// pausedItemReasons renders, for every board item carrying fabrik:paused,
// its most recent comment — whatever the pause cause. clonePauseDetected
// above deliberately narrows its *abort* to one specific cause (a failed
// repo clone) so it never misfires on a scenario legitimately waiting
// through an ordinary pause; that narrowness is correct for aborting early,
// but it left diagnostics blind to every other pause cause, which is exactly
// what turned a known "#1/#3 paused, fabrik:awaiting-input" board dump into
// two days of guessing rather than a direct answer (see #1452 comment
// history). This function has no such scoping concern: it only renders
// information for a human or the next investigation to read, never decides
// whether to abort, so it can safely cover every paused item unconditionally.
//
// The most recent comment is taken by CreatedAt, not by position in
// item.Comments — comments are expected to already arrive in creation order
// from FetchProjectBoard, but sorting explicitly here is one line and does
// not depend on that assumption holding.
func pausedItemReasons(env *Env) string {
	board, err := env.Sim.FetchProjectBoard(env.Owner, env.Repo, env.ProjectNum, "User")
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, item := range board.Items {
		if !hasLabel(item.Labels, "fabrik:paused") {
			continue
		}
		if len(item.Comments) == 0 {
			fmt.Fprintf(&b, "#%d: fabrik:paused with no comments at all\n", item.Number)
			continue
		}
		latest := item.Comments[0]
		for _, c := range item.Comments[1:] {
			if c.CreatedAt.After(latest.CreatedAt) {
				latest = c
			}
		}
		fmt.Fprintf(&b, "#%d most recent comment (by %s):\n%s\n", item.Number, latest.Author, latest.Body)
	}
	return b.String()
}

// boardSummary renders every item's number, status column, and labels.
func boardSummary(env *Env) string {
	board, err := env.Sim.FetchProjectBoard(env.Owner, env.Repo, env.ProjectNum, "User")
	if err != nil {
		return fmt.Sprintf("(could not fetch board: %v)", err)
	}
	var b strings.Builder
	for _, item := range board.Items {
		fmt.Fprintf(&b, "#%d %-20s labels=%v closed=%v\n", item.Number, item.Status, item.Labels, item.IsClosed)
	}
	if len(board.Items) == 0 {
		b.WriteString("(no items)\n")
	}
	return b.String()
}
