// Package bedtimeout is the one definition of the engine wait timeouts a live
// e2e bed runs with (#2417, ADR-2417), shared by the two bed launch sites
// (tests/gate/bed.go and tests/e2e/lifecycle.go) and by the tests that wait one
// of them out.
//
// The launch sites inject Env() into the bed engine's process environment. The
// engine's .env loading never overrides an already-set process variable
// (cmd/root.go, config.LoadDotenv), so the launch value wins over anything an
// operator put in the bed's .env — a bed and its tests cannot disagree, and no
// preflight mismatch check is needed. Tests read ReviewWait() instead of parsing
// the bed's .env and falling back to a harness default.
//
// The package is untagged and stdlib-only, like internal/pollctl and
// tests/e2e/pollhold: tests/gate carries no e2e build tag and so cannot import
// the tagged harness, and CI only compiles that harness, so the window logic
// below is covered by plain `go test ./...`.
package bedtimeout

import (
	"fmt"
	"time"
)

const (
	// ReviewEnvVar and CIEnvVar are the engine's variables (cmd/root.go).
	ReviewEnvVar = "FABRIK_REVIEW_WAIT_TIMEOUT"
	CIEnvVar     = "FABRIK_CI_WAIT_TIMEOUT"

	// ReviewMinutes is the bed's review wait timeout (engine default: 15).
	// 10 is the lowest value at which TestConjunctiveCIReviewGate still runs (it
	// skips below 10 with a reviewer token) and leaves the bed's slowest
	// legitimate review waits — the cycle-limit race of README item 24a, the
	// Yolo block-persist window and Pruefer's latency on the full-pipeline
	// tests (Phase 2 pauses at 2x this) — their margin. A single constant: raise
	// it here if a gate run shows a loss.
	ReviewMinutes = 10

	// CIMinutes is the bed's CI wait timeout, pinned at the engine's own
	// effective default (30). It is a liveness-stall dwell, not a bound on CI
	// duration (ADR 1410), and no live test waits it out; the 25-minute
	// slow-gate check has no interim transitions, which is exactly the shape
	// that dwell cannot tell from a stall. Shortening it would buy no wall-clock
	// and risks pausing the slow-gate items, so it is made explicit and shared
	// but not shortened.
	CIMinutes = 30
)

// Env returns the NAME=value entries both launch sites add to the bed's
// environment, after anything inherited from the operator's shell.
func Env() []string {
	return []string{
		fmt.Sprintf("%s=%d", ReviewEnvVar, ReviewMinutes),
		fmt.Sprintf("%s=%d", CIEnvVar, CIMinutes),
	}
}

// ReviewWait is the review wait timeout the bed was launched with.
func ReviewWait() time.Duration { return ReviewMinutes * time.Minute }

// CIWait is the CI wait timeout the bed was launched with.
func CIWait() time.Duration { return CIMinutes * time.Minute }

// ReviewTimeoutLogFmt is the engine log line (format, one %d for the issue
// number) written when the review wait timeout elapses on the mixed/human path
// (engine/reviews.go checkAwaitingReviewTimeout) — the positive observable that a
// post-timeout gate evaluation ran without taking the bot re-prompt path. Pinned
// to the engine source by TestReviewTimeoutLogLineMatchesEngine.
const ReviewTimeoutLogFmt = "[#%d warn] review wait timeout elapsed; pausing issue"

// PollMargins is how many bed polls a negative window waits past the point the
// bug could first show. One poll for the engine to reach its first evaluation
// after the timeout, one for the resulting label or log line to be written, and
// one for the harness to observe it.
const PollMargins = 3

// Window is a negative-observation window: "this must not happen" polled from
// the event that arms the bug.
type Window struct {
	// Armed is the event time that starts the bug's clock (for the review gate,
	// GitHub's time for the fabrik:awaiting-review application) — never a later
	// observation of it.
	Armed time.Time
	// Timeout is the engine timeout the bug fires after.
	Timeout time.Duration
	// Poll is the bed's engine poll interval.
	Poll time.Duration
	// WorkerAllowance covers a worker already in flight that delays the engine's
	// first gate evaluation past Armed+Timeout (a review-reinvoke is a multi-
	// minute Claude run). Zero when nothing can delay it.
	WorkerAllowance time.Duration
}

// Ceiling is the latest instant the window waits for the engine to act:
// Armed + Timeout + WorkerAllowance + PollMargins x Poll. It is a failure
// bound, reached only when the engine does nothing, not a sleep.
func (w Window) Ceiling() time.Time {
	return w.Armed.Add(w.Timeout + w.WorkerAllowance + PollMargins*w.Poll)
}

// Earliest reports the earliest instant the bug can show.
func (w Window) Earliest() time.Time { return w.Armed.Add(w.Timeout) }

// Observation is one probe of the world.
type Observation struct {
	// Bug is set when the forbidden event happened; Detail says how.
	Bug    bool
	Detail string
	// Settled is set when the engine took its correct escalation — a positive
	// observable at the same engine instant the bug would have fired, which
	// proves at least one post-timeout gate evaluation ran without taking the
	// bug's path.
	Settled bool
}

// Outcome is how Watch ended.
type Outcome int

const (
	// OutcomeSettled: the correct escalation was observed after Earliest().
	OutcomeSettled Outcome = iota
	// OutcomeBug: the forbidden event happened.
	OutcomeBug
	// OutcomeCeiling: Ceiling() passed with neither. The bug did not show, but
	// no post-timeout evaluation was positively confirmed either.
	OutcomeCeiling
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSettled:
		return "settled"
	case OutcomeBug:
		return "bug"
	default:
		return "ceiling"
	}
}

// Watch probes every Poll until the forbidden event, the correct escalation
// (only credited once Earliest() has passed — an escalation seen earlier cannot
// have come from this timeout), or the ceiling. now and sleep are injectable.
// A Bug observation ends the window at once, at any time.
func Watch(w Window, now func() time.Time, sleep func(time.Duration), probe func() Observation) (Outcome, Observation) {
	ceiling := w.Ceiling()
	for {
		obs := probe()
		if obs.Bug {
			return OutcomeBug, obs
		}
		t := now()
		if obs.Settled && !t.Before(w.Earliest()) {
			return OutcomeSettled, obs
		}
		if !t.Before(ceiling) {
			return OutcomeCeiling, obs
		}
		sleep(w.Poll)
	}
}
