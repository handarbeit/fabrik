package simgh

import (
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/tests/sim/simgh/ghfault"
)

// This file is the fault injector: the mechanism that makes a chosen
// engine.GitHubClient method fail on demand.
//
// # Why it exists
//
// The five settle scans (fabrik:awaiting-done, fabrik:awaiting-ci,
// fabrik:awaiting-member-close, fabrik:awaiting-close,
// fabrik:awaiting-placement — ADR-060, ADR-061, ADR-062, ADR-1097, ADR-1270)
// each retry every poll while a specific call fails and escalate after
// MaxRetries sustained failures. Their retry loops and escalation cutovers are
// reachable *only* by injecting a repeatedly-failing call. That machinery is
// the least-tested in the engine precisely because nothing has ever been able
// to provoke it.
//
// # The error shape is part of the test
//
// A fault is registered with a caller-chosen error, and the choice is
// load-bearing rather than cosmetic: the engine classifies rate-limit and
// abuse-detection failures as transient-and-deferrable and everything else as
// escalation-worthy (engine/item.go's isTransientAPIError). Injecting a
// generic error where a rate limit was meant exercises the opposite branch
// while the scenario believes otherwise. Use the ghfault constructors
// re-exported at the bottom of this file; engine/simgh_fault_classification_test.go
// pins them to the real classifier.
//
// # RateLimitStats cannot be failed
//
// It is the one interface method with no error return (engine/interfaces.go),
// so a fault registered against it could never fire. Registering one panics
// rather than sitting silently inert — an inert fault is a false-pass
// generator, and this layer exists to remove those. The controllability that
// surface actually needs is scripting the *budget* it returns, since
// engine/backoff.go consumes it as ratios rather than as a failing call; that
// is SeedRateLimits. Recorded in FIDELITY.md.
//
// # Locking
//
// FaultTable owns its own mutex and never holds it across a call into Sim.
// Like MutationLog it is a leaf in the package's lock ordering.

// Always is the repeat count meaning "for every matching call, without limit".
const Always = -1

// unfaultableMethods are interface methods with no error return, so no fault
// registered against them could ever fire.
var unfaultableMethods = map[string]bool{
	"RateLimitStats": true,
}

// interfaceMethodSet is every method name on engine.GitHubClient, derived by
// reflection so the set maintains itself as the interface grows. A fault
// registered against a name not in here is a typo, and a typo that silently
// registers nothing is a test that passes because it tested nothing.
var interfaceMethodSet = func() map[string]bool {
	t := reflect.TypeOf((*engine.GitHubClient)(nil)).Elem()
	out := make(map[string]bool, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		out[t.Method(i).Name] = true
	}
	return out
}()

// InterfaceMethods returns every engine.GitHubClient method name, sorted.
// Exported for the reflection-driven completeness tests, which assert that
// every one of them is both instrumented and (bar RateLimitStats) faultable.
func InterfaceMethods() []string {
	out := make([]string, 0, len(interfaceMethodSet))
	for name := range interfaceMethodSet {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// faultRule is one registered fault.
type faultRule struct {
	err error
	// remaining is how many more matching calls this rule fails: a positive
	// count, or Always.
	remaining int
	// onCall, when positive, restricts the rule to exactly the Kth call to the
	// method (1-based, counted across every call since the table was created,
	// including ones made before the rule was registered).
	onCall int
	// match, when non-nil, further narrows the rule to calls whose arguments
	// satisfy it.
	match func(Args) bool
	// fired counts how many times this rule has produced an error.
	fired int
}

// FaultTable holds the registered faults for one Instrumented client.
type FaultTable struct {
	mu sync.Mutex
	// rules is keyed by method name; within a method they are consulted in
	// registration order and the first eligible one wins.
	rules map[string][]*faultRule
	// calls counts every intercepted call per method, whether it failed or
	// not. This is what FailOnCall's K counts.
	calls map[string]int
}

// NewFaultTable returns an empty table.
func NewFaultTable() *FaultTable {
	return &FaultTable{
		rules: make(map[string][]*faultRule),
		calls: make(map[string]int),
	}
}

// validateMethod panics on a name that could never fire a fault. Both cases
// are programming errors in the scenario, not runtime conditions, so failing
// loudly at registration is the only way they surface at all.
func validateMethod(method string) {
	if !interfaceMethodSet[method] {
		panic(fmt.Sprintf("simgh: no engine.GitHubClient method named %q; "+
			"a fault on an unknown method would never fire", method))
	}
	if unfaultableMethods[method] {
		panic(fmt.Sprintf("simgh: %s has no error return, so a fault on it could never fire; "+
			"script its returned budget with SeedRateLimits instead", method))
	}
}

// add registers a rule, validating the method name first.
func (f *FaultTable) add(method string, r *faultRule) {
	validateMethod(method)
	if r.err == nil {
		panic(fmt.Sprintf("simgh: fault on %s registered with a nil error; "+
			"a nil error is indistinguishable from success", method))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules[method] = append(f.rules[method], r)
}

// FailOnce makes the next call to method fail with err.
func (f *FaultTable) FailOnce(method string, err error) {
	f.add(method, &faultRule{err: err, remaining: 1})
}

// FailNTimes makes the next n calls to method fail with err, after which it
// succeeds again. n must be positive; use FailAlways for an unbounded fault.
func (f *FaultTable) FailNTimes(method string, n int, err error) {
	if n < 1 {
		panic(fmt.Sprintf("simgh: FailNTimes(%s, %d): count must be positive; use FailAlways for an unbounded fault", method, n))
	}
	f.add(method, &faultRule{err: err, remaining: n})
}

// FailAlways makes every call to method fail with err until the rule is
// cleared.
func (f *FaultTable) FailAlways(method string, err error) {
	f.add(method, &faultRule{err: err, remaining: Always})
}

// FailOnCall makes only the kth call to method fail with err, counting from 1
// across every call since the table was created — including calls made before
// this rule was registered. Earlier and later calls succeed.
func (f *FaultTable) FailOnCall(method string, k int, err error) {
	if k < 1 {
		panic(fmt.Sprintf("simgh: FailOnCall(%s, %d): call number must be 1-based and positive", method, k))
	}
	f.add(method, &faultRule{err: err, remaining: Always, onCall: k})
}

// FailWhen makes the next times calls to method whose arguments satisfy match
// fail with err. times is a positive count or Always.
//
// The narrowing is what makes the settle scans testable. Each of them retries
// a *specific* call for a *specific* issue while MaxConcurrent workers are
// touching other items through the same methods; a method-name-only injector
// would fail all of them at once and could not express the scenario at all.
func (f *FaultTable) FailWhen(method string, match func(Args) bool, times int, err error) {
	if match == nil {
		panic(fmt.Sprintf("simgh: FailWhen(%s): nil match; use FailNTimes or FailAlways for an unnarrowed fault", method))
	}
	if times < 1 && times != Always {
		panic(fmt.Sprintf("simgh: FailWhen(%s, …, %d): count must be positive or simgh.Always", method, times))
	}
	f.add(method, &faultRule{err: err, remaining: times, match: match})
}

// Clear removes every rule registered against method. The call counter is left
// alone, so a subsequent FailOnCall still counts from the table's creation.
func (f *FaultTable) Clear(method string) {
	validateMethod(method)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rules, method)
}

// ClearAll removes every rule. Call counters are left alone.
func (f *FaultTable) ClearAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = make(map[string][]*faultRule)
}

// CallCount reports how many calls to method have been intercepted, whether
// they failed or not.
func (f *FaultTable) CallCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// FiredCount reports how many injected failures method has produced.
func (f *FaultTable) FiredCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, r := range f.rules[method] {
		total += r.fired
	}
	return total
}

// next records a call against method and returns the error to fail it with, or
// nil to let it through. Every intercepted call goes through here exactly
// once, before the underlying Sim call.
func (f *FaultTable) next(method string, args Args) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls[method]++
	n := f.calls[method]

	for _, r := range f.rules[method] {
		if r.match != nil && !r.match(args) {
			continue
		}
		if r.onCall > 0 {
			if n != r.onCall {
				continue
			}
			r.fired++
			return r.err
		}
		if r.remaining == 0 {
			continue
		}
		if r.remaining > 0 {
			r.remaining--
		}
		r.fired++
		return r.err
	}
	return nil
}

// snapshotRules deep-copies the table's state so a Sim snapshot can carry it.
// The match closures are shared rather than copied — they are stateless
// predicates supplied by the scenario, and a scenario that hid mutable state
// in one has already broken determinism for reasons this package cannot fix.
func (f *FaultTable) snapshotRules() (map[string][]*faultRule, map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rules := make(map[string][]*faultRule, len(f.rules))
	for method, list := range f.rules {
		copies := make([]*faultRule, 0, len(list))
		for _, r := range list {
			dup := *r
			copies = append(copies, &dup)
		}
		rules[method] = copies
	}
	calls := make(map[string]int, len(f.calls))
	for k, v := range f.calls {
		calls[k] = v
	}
	return rules, calls
}

// restoreRules replaces the table's state with a snapshot's, deep-copying
// again so the snapshot stays reusable for a second restore.
func (f *FaultTable) restoreRules(rules map[string][]*faultRule, calls map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = make(map[string][]*faultRule, len(rules))
	for method, list := range rules {
		copies := make([]*faultRule, 0, len(list))
		for _, r := range list {
			dup := *r
			copies = append(copies, &dup)
		}
		f.rules[method] = copies
	}
	f.calls = make(map[string]int, len(calls))
	for k, v := range calls {
		f.calls[k] = v
	}
}

// --- ghfault re-exports ----------------------------------------------------
//
// Re-exported so a scenario needs one import rather than two, while the
// constructors themselves stay in a stdlib-only leaf package that `engine` can
// import from a test without a cycle. See ghfault's package doc.

// ErrRateLimit returns GitHub's REST primary rate-limit exhaustion error.
func ErrRateLimit() error { return ghfault.RateLimit() }

// ErrGraphQLRateLimit returns GitHub's GraphQL primary rate-limit exhaustion error.
func ErrGraphQLRateLimit() error { return ghfault.GraphQLRateLimit() }

// ErrSecondaryRateLimit returns GitHub's secondary rate-limit error.
func ErrSecondaryRateLimit() error { return ghfault.SecondaryRateLimit() }

// ErrAbuseDetection returns GitHub's abuse-detection error.
func ErrAbuseDetection() error { return ghfault.AbuseDetection() }

// ErrTooManyRequests returns a bare HTTP 429 in the engine's REST framing.
func ErrTooManyRequests() error { return ghfault.TooManyRequests() }

// ErrServerError returns a GitHub 5xx in the engine's REST framing.
func ErrServerError() error { return ghfault.ServerError() }

// ErrConnectionReset returns a transport-layer connection reset.
func ErrConnectionReset() error { return ghfault.ConnectionReset() }

// ErrIOTimeout returns a transport-layer read timeout.
func ErrIOTimeout() error { return ghfault.IOTimeout() }

// ErrNotFound returns a GitHub 404 — the non-transient control case.
func ErrNotFound() error { return ghfault.NotFound() }

// WrapErr produces an error carrying sentinel in its chain, for the engine's
// errors.Is matches against gh.ErrNotMergeable, gh.ErrNotMergeableCI and
// gh.ErrNotFound.
func WrapErr(sentinel error, msg string) error { return ghfault.Wrap(sentinel, msg) }
