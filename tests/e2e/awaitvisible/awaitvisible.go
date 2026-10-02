// Package awaitvisible is the polling and timeout core of the live e2e harness's
// awaitVisible family (#1974, ADR-1974): block until GitHub reflects a harness
// write through the same read path the engine uses, within an explicit bound.
//
// GitHub is eventually consistent. A harness that writes (adds a board item,
// opens a PR with "Closes #N", moves a Status) and carries on as if every reader
// can already see it races the engine's own, differently-lagging reads — and the
// scenario then either never reaches the state it tests (a vacuous pass) or
// fails for a reason that has nothing to do with the engine. Every such wait goes
// through Poll, so there is exactly one polling loop, one timeout implementation
// and one way to describe what was being waited for.
//
// The package is deliberately untagged. The harness (tests/e2e, build tag e2e) is
// only COMPILED by CI, so a unit test placed there would never run on a PR; the
// loop and the pure classifiers live here so plain `go test ./...` covers them.
// tests/e2e/inconclusive and tests/e2e/registry are the precedent.
//
// Poll never decides what a timeout MEANS. The harness maps a timed-out Result to
// the Inconclusive outcome (#1973) — a harness-state wait that expires says the
// precondition never arose, not that the engine misbehaved.
package awaitvisible

import (
	"errors"
	"fmt"
	"time"
)

// Spec describes one wait. Timeout and Interval are explicit per call: the
// family hides no default, because the right bound differs by read path (a
// GraphQL listing is polled slowly to spare the shared budget, a REST read can
// be polled faster).
type Spec struct {
	// What names the fact being waited for, in words a log reader can act on
	// ("board item handarbeit/e2e-alpha#42 in the ProjectV2 listing"). It appears
	// verbatim in the timeout reason.
	What     string
	Timeout  time.Duration
	Interval time.Duration

	// OnRetry, when non-nil, is called with each probe error that is being
	// retried (never with "not visible yet").
	OnRetry func(err error)

	// Now and Sleep are test seams; nil means time.Now and time.Sleep.
	Now   func() time.Time
	Sleep func(time.Duration)
}

// Probe makes one read. visible reports whether the awaited fact is now visible;
// detail is a short description of what was observed (kept for the timeout
// reason); a non-nil err is a failed READ — retried, never counted as "not
// visible". Wrap an error with Abort to stop polling at once.
type Probe func() (visible bool, detail string, err error)

type abortError struct{ err error }

func (e abortError) Error() string { return e.err.Error() }
func (e abortError) Unwrap() error { return e.err }

// Abort wraps err so Poll stops immediately and reports it in Result.Aborted
// instead of retrying. It is for an observation that will not resolve by waiting
// and is an assertion about the scenario rather than lag (a PR that is
// mergeable_state "dirty"); the caller turns Aborted into a real failure.
func Abort(err error) error { return abortError{err} }

// Result is the outcome of one Poll.
type Result struct {
	What     string
	Visible  bool
	Attempts int
	Elapsed  time.Duration
	Timeout  time.Duration

	// LastDetail is the last observation a probe reported without error.
	LastDetail string
	// LastErr is the last read error a probe returned (retried).
	LastErr error
	// Aborted is set when a probe returned an Abort error; polling stopped.
	Aborted error
}

// TimedOut reports whether the wait expired without the fact becoming visible
// and without being aborted or misconfigured.
func (r Result) TimedOut() bool { return !r.Visible && r.Aborted == nil }

// Reason is a one-line explanation of a timed-out Result, naming what was waited
// for, for how long, and the last thing seen.
func (r Result) Reason() string {
	msg := fmt.Sprintf("%s never became visible within %s (%d read(s))", r.What, r.Timeout, r.Attempts)
	if r.LastDetail != "" {
		msg += fmt.Sprintf("; last observed: %s", r.LastDetail)
	}
	if r.LastErr != nil {
		msg += fmt.Sprintf("; last read error: %v", r.LastErr)
	}
	return msg
}

// Poll runs probe until it reports visible, returns an Abort error, or the
// spec's Timeout elapses. The probe always runs at least once, and a final read
// is made at the deadline, so a fact that became visible during the last sleep is
// not missed.
func Poll(spec Spec, probe Probe) Result {
	res := Result{What: spec.What, Timeout: spec.Timeout}
	if spec.Timeout <= 0 || spec.Interval <= 0 {
		res.Aborted = fmt.Errorf("awaitvisible: %q needs an explicit positive Timeout and Interval (got %s, %s)", spec.What, spec.Timeout, spec.Interval)
		return res
	}
	now, sleep := spec.Now, spec.Sleep
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	start := now()
	deadline := start.Add(spec.Timeout)
	for {
		res.Attempts++
		visible, detail, err := probe()
		res.Elapsed = now().Sub(start)
		var ab abortError
		switch {
		case errors.As(err, &ab):
			res.Aborted = ab.err
			return res
		case err != nil:
			res.LastErr = err
			if spec.OnRetry != nil {
				spec.OnRetry(err)
			}
		default:
			res.LastErr = nil
			res.LastDetail = detail
			if visible {
				res.Visible = true
				return res
			}
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return res
		}
		wait := spec.Interval
		if wait > remaining {
			wait = remaining
		}
		sleep(wait)
	}
}
