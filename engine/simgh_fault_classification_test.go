package engine

// This file satisfies acceptance criterion 5 of issue #1457: an injected
// rate-limit error must be classified by *the engine's own* classifier as a
// rate limit, asserted against the real predicate rather than against the
// injector's intent. An injected error the engine does not recognise proves
// nothing — it exercises the escalation branch while the scenario believes it
// is exercising the defer branch.
//
// # Why the test lives here, in `package engine`
//
// isTransientAPIError and isTransientError are unexported. Their only
// production consumer is engine/ci_settle.go's settleAwaitingCIScan, itself
// unexported and reachable from outside only through Engine.Run(), which needs
// the harness issue #1457 puts out of scope. So neither an external
// `package engine_test` (which cannot see unexported identifiers) nor a
// simgh-side test (tests/sim/simgh imports engine, so engine importing it back
// is a cycle) can make this assertion. An internal test file that imports only
// the stdlib-only leaf package tests/sim/simgh/ghfault can — and does so
// without touching any production code under engine/.
//
// # What this test does not cover
//
// This is a classification assertion, not a behavioural one. The stronger
// form — inject the same number of FetchItemDetails failures across
// MaxRetries+ settle passes, once with a rate-limit-shaped error and once with
// a generic one, and assert that only the generic one escalates the item to
// fabrik:paused — needs a driveable Engine.Run(), which arrives with the
// harness issue. It belongs there, not here.

import (
	"errors"
	"testing"

	"github.com/handarbeit/fabrik/tests/sim/simgh/ghfault"
)

// TestInjectedRateLimitErrorsAreClassifiedAsTransientAPI is the AC5
// assertion proper: every rate-limit shape simgh can inject is recognised by
// isTransientAPIError, the predicate settleAwaitingCIScan uses to decide
// whether a failure defers indefinitely (#1313) or consumes the item's
// escalation budget.
func TestInjectedRateLimitErrorsAreClassifiedAsTransientAPI(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"RateLimit", ghfault.RateLimit()},
		{"GraphQLRateLimit", ghfault.GraphQLRateLimit()},
		{"SecondaryRateLimit", ghfault.SecondaryRateLimit()},
		{"AbuseDetection", ghfault.AbuseDetection()},
		{"TooManyRequests", ghfault.TooManyRequests()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !isTransientAPIError(tc.err) {
				t.Errorf("isTransientAPIError(ghfault.%s()) = false, want true\n  error: %v",
					tc.name, tc.err)
			}
		})
	}
}

// The transport-layer shapes must satisfy the *narrower* predicate too.
// isTransientError governs bounded retry; isTransientAPIError governs
// indefinite deferral. A constructor that only reached the wider one would
// silently skip every bounded-retry path a scenario aimed it at.
func TestInjectedTransportErrorsAreClassifiedByBothPredicates(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"ServerError", ghfault.ServerError()},
		{"ConnectionReset", ghfault.ConnectionReset()},
		{"IOTimeout", ghfault.IOTimeout()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !isTransientError(tc.err) {
				t.Errorf("isTransientError(ghfault.%s()) = false, want true\n  error: %v", tc.name, tc.err)
			}
			if !isTransientAPIError(tc.err) {
				t.Errorf("isTransientAPIError(ghfault.%s()) = false, want true\n  error: %v", tc.name, tc.err)
			}
		})
	}
}

// The rate-limit shapes must NOT satisfy isTransientError. The two predicates
// route to different engine behaviour, and a constructor that tripped both
// would make a scenario aimed at the defer-indefinitely path indistinguishable
// from one aimed at bounded retry.
func TestRateLimitShapesAreNotBoundedRetryTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"RateLimit", ghfault.RateLimit()},
		{"GraphQLRateLimit", ghfault.GraphQLRateLimit()},
		{"SecondaryRateLimit", ghfault.SecondaryRateLimit()},
		{"AbuseDetection", ghfault.AbuseDetection()},
		{"TooManyRequests", ghfault.TooManyRequests()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if isTransientError(tc.err) {
				t.Errorf("isTransientError(ghfault.%s()) = true, want false — the rate-limit "+
					"shapes belong to isTransientAPIError only\n  error: %v", tc.name, tc.err)
			}
		})
	}
}

// The negative half of AC5. Without this, every assertion above would still
// pass against a classifier that returned true unconditionally.
func TestUnrecognisedErrorsAreNotClassifiedAsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"plain error", errors.New("boom")},
		{"ghfault.NotFound", ghfault.NotFound()},
		{"wrapped sentinel", ghfault.Wrap(errors.New("not mergeable"), "simgh: merging PR #7")},
		{"nil", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if isTransientAPIError(tc.err) {
				t.Errorf("isTransientAPIError(%v) = true, want false", tc.err)
			}
			if isTransientError(tc.err) {
				t.Errorf("isTransientError(%v) = true, want false", tc.err)
			}
		})
	}
}
