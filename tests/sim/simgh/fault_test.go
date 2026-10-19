package simgh

import (
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestFaultFailsNTimesThenSucceeds is AC4.
//
// Two claims, and the second is the one that matters. The first is that the
// injector counts: exactly N failures, then success. The second is that the
// *log shows it* — N failed attempts followed by the success. A log recording
// only mutations that happened could not, which is why its contract is "every
// intercepted call, with its outcome" rather than what its name suggests.
func TestFaultFailsNTimesThenSucceeds(t *testing.T) {
	const n = 3
	in, _ := newInstrumented(t)
	in.Faults().FailNTimes("AddLabelToIssue", n, ErrRateLimit())

	for i := 0; i < n; i++ {
		if err := in.AddLabelToIssue("acme", "widgets", 7, "fabrik:awaiting-done"); err == nil {
			t.Fatalf("attempt %d succeeded, want the injected failure", i+1)
		}
	}
	if err := in.AddLabelToIssue("acme", "widgets", 7, "fabrik:awaiting-done"); err != nil {
		t.Fatalf("attempt %d: %v — want success once the fault is exhausted", n+1, err)
	}

	entries := in.Log().ByMethod("AddLabelToIssue")
	if len(entries) != n+1 {
		t.Fatalf("log has %d AddLabelToIssue entries, want %d\n%s", len(entries), n+1, in.Log().Dump(20))
	}
	for i := 0; i < n; i++ {
		if !entries[i].Failed() {
			t.Errorf("log entry %d records a success, want the injected failure", i)
		}
		if !entries[i].Injected {
			t.Errorf("log entry %d does not mark the failure as injected", i)
		}
	}
	if entries[n].Failed() {
		t.Errorf("log entry %d records a failure, want the success: %s", n, entries[n])
	}

	// The label must genuinely have been applied only by the successful call:
	// a fault that logged a failure but let the mutation through would satisfy
	// every assertion above.
	labels, err := in.Sim().FetchLabels("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if !contains(labels, "fabrik:awaiting-done") {
		t.Error("label absent after the successful attempt")
	}
	if in.Faults().FiredCount("AddLabelToIssue") != n {
		t.Errorf("FiredCount = %d, want %d", in.Faults().FiredCount("AddLabelToIssue"), n)
	}
}

// A faulted call must not reach the model at all. Logging a failure while
// letting the mutation through would make every settle-scan scenario pass
// against a GitHub that never actually failed.
func TestAFaultedCallDoesNotReachTheModel(t *testing.T) {
	in, _ := newInstrumented(t)
	in.Faults().FailAlways("AddLabelToIssue", ErrRateLimit())

	if err := in.AddLabelToIssue("acme", "widgets", 7, "fabrik:paused"); err == nil {
		t.Fatal("AddLabelToIssue succeeded despite FailAlways")
	}
	labels, err := in.Sim().FetchLabels("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if contains(labels, "fabrik:paused") {
		t.Error("the faulted call still mutated the model")
	}
}

func TestFailOnce(t *testing.T) {
	in, _ := newInstrumented(t)
	in.Faults().FailOnce("FetchLabels", ErrServerError())

	if _, err := in.FetchLabels("acme", "widgets", 7); err == nil {
		t.Fatal("first call succeeded, want the injected failure")
	}
	if _, err := in.FetchLabels("acme", "widgets", 7); err != nil {
		t.Fatalf("second call failed: %v — FailOnce must fire exactly once", err)
	}
}

func TestFailOnCallFiresOnlyOnTheKthCall(t *testing.T) {
	in, _ := newInstrumented(t)
	in.Faults().FailOnCall("FetchLabels", 3, ErrTooManyRequests())

	var failedAt []int
	for i := 1; i <= 5; i++ {
		if _, err := in.FetchLabels("acme", "widgets", 7); err != nil {
			failedAt = append(failedAt, i)
		}
	}
	if len(failedAt) != 1 || failedAt[0] != 3 {
		t.Errorf("calls failed at %v, want only call 3", failedAt)
	}
	if got := in.Faults().CallCount("FetchLabels"); got != 5 {
		t.Errorf("CallCount = %d, want 5 — every intercepted call counts, faulted or not", got)
	}
}

// FailWhen's narrowing is what makes the settle scans testable: each retries a
// specific call for a specific issue while other workers touch other items
// through the same method.
func TestFailWhenNarrowsByArguments(t *testing.T) {
	in, _ := newInstrumented(t)
	in.Sim().SeedIssue("acme/widgets", IssueSeed{Number: 8, Title: "other", Status: "Implement"})
	if err := in.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	in.Faults().FailWhen("AddLabelToIssue",
		func(a Args) bool { return a.Number == 7 && a.Label == "fabrik:awaiting-done" },
		Always, ErrRateLimit())

	if err := in.AddLabelToIssue("acme", "widgets", 7, "fabrik:awaiting-done"); err == nil {
		t.Error("the targeted call succeeded")
	}
	if err := in.AddLabelToIssue("acme", "widgets", 8, "fabrik:awaiting-done"); err != nil {
		t.Errorf("a different issue was caught by the narrowed fault: %v", err)
	}
	if err := in.AddLabelToIssue("acme", "widgets", 7, "fabrik:paused"); err != nil {
		t.Errorf("a different label on the same issue was caught by the narrowed fault: %v", err)
	}
}

func TestClearAndClearAll(t *testing.T) {
	in, _ := newInstrumented(t)
	in.Faults().FailAlways("FetchLabels", ErrRateLimit())
	in.Faults().FailAlways("GetIssueBody", ErrRateLimit())

	in.Faults().Clear("FetchLabels")
	if _, err := in.FetchLabels("acme", "widgets", 7); err != nil {
		t.Errorf("FetchLabels still faulted after Clear: %v", err)
	}
	if _, err := in.GetIssueBody("acme", "widgets", 7); err == nil {
		t.Error("Clear removed a rule on a different method")
	}

	in.Faults().ClearAll()
	if _, err := in.GetIssueBody("acme", "widgets", 7); err != nil {
		t.Errorf("GetIssueBody still faulted after ClearAll: %v", err)
	}
}

// The injected error must be usable with errors.Is, so a scenario can inject
// exactly the sentinel a production path matches on.
func TestInjectedSentinelsSurviveWrapping(t *testing.T) {
	in, _ := newInstrumented(t)
	in.Faults().FailOnce("MergePRAtHeadSHA", WrapErr(gh.ErrNotMergeable, "simgh: injected"))

	err := in.MergePRAtHeadSHA("acme", "widgets", 1, "x")
	if !errors.Is(err, gh.ErrNotMergeable) {
		t.Fatalf("MergePRAtHeadSHA err = %v, want errors.Is(err, gh.ErrNotMergeable)", err)
	}
}

func TestRegisteringAFaultOnAnUnknownMethodPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("no panic for an unknown method name")
		}
		if !strings.Contains(toString(r), "FetchNonsense") {
			t.Errorf("panic message %v does not name the offending method", r)
		}
	}()
	NewFaultTable().FailOnce("FetchNonsense", ErrRateLimit())
}

// RateLimitStats has no error return, so a fault on it could never fire. A
// silently-inert fault is a false-pass generator: the scenario believes it has
// made the call fail and asserts against a path that was never taken.
func TestRegisteringAFaultOnRateLimitStatsPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("no panic for a fault on RateLimitStats")
		}
		if !strings.Contains(toString(r), "SeedRateLimits") {
			t.Errorf("panic message %v does not point at the supported alternative", r)
		}
	}()
	NewFaultTable().FailAlways("RateLimitStats", ErrRateLimit())
}

func TestRegisteringAFaultWithANilErrorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic for a nil injected error — it is indistinguishable from success")
		}
	}()
	NewFaultTable().FailOnce("FetchLabels", nil)
}

func TestFaultCountValidation(t *testing.T) {
	cases := []struct {
		name string
		call func(*FaultTable)
	}{
		{"FailNTimes(0)", func(f *FaultTable) { f.FailNTimes("FetchLabels", 0, ErrRateLimit()) }},
		{"FailNTimes(-1)", func(f *FaultTable) { f.FailNTimes("FetchLabels", -1, ErrRateLimit()) }},
		{"FailOnCall(0)", func(f *FaultTable) { f.FailOnCall("FetchLabels", 0, ErrRateLimit()) }},
		{"FailWhen(nil match)", func(f *FaultTable) { f.FailWhen("FetchLabels", nil, 1, ErrRateLimit()) }},
		{"FailWhen(0)", func(f *FaultTable) {
			f.FailWhen("FetchLabels", func(Args) bool { return true }, 0, ErrRateLimit())
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s did not panic", tc.name)
				}
			}()
			tc.call(NewFaultTable())
		})
	}
}

// TestEveryInterfaceMethodIsFaultable is the fault table's drift detector: a
// wrapper that delegated without consulting the table would make a fault
// registered against it silently inert. Every method but RateLimitStats — the
// one with no error return — must be reachable.
func TestEveryInterfaceMethodIsFaultable(t *testing.T) {
	for _, name := range InterfaceMethods() {
		if unfaultableMethods[name] {
			continue
		}
		t.Run(name, func(t *testing.T) {
			in, _ := newInstrumented(t)
			in.Faults().FailAlways(name, ErrRateLimit())
			callInterfaceMethod(t, in, name)

			entries := in.Log().Entries()
			if len(entries) != 1 {
				t.Fatalf("calling %s produced %d log entries, want 1", name, len(entries))
			}
			if !entries[0].Injected {
				t.Errorf("%s is not faultable: the registered fault never fired\n  %s", name, entries[0])
			}
			if in.Faults().FiredCount(name) != 1 {
				t.Errorf("%s: FiredCount = %d, want 1", name, in.Faults().FiredCount(name))
			}
		})
	}
}

// The unfaultable set must stay exactly the methods with no error return —
// otherwise a method could be excused from fault injection by being added to
// the map rather than by being genuinely unfaultable.
func TestUnfaultableSetIsExactlyTheErrorlessMethods(t *testing.T) {
	for name := range unfaultableMethods {
		if !interfaceMethodSet[name] {
			t.Errorf("unfaultableMethods lists %q, which is not an engine.GitHubClient method", name)
		}
	}
	if len(unfaultableMethods) != 1 || !unfaultableMethods["RateLimitStats"] {
		t.Errorf("unfaultableMethods = %v, want exactly {RateLimitStats}; every other method returns an error",
			unfaultableMethods)
	}
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if err, ok := v.(error); ok {
		return err.Error()
	}
	return ""
}

// TestBeforeCallRunsBeforeTheCallAndTheFault pins the #2344 seam: the hook runs
// on the call that is about to be made, before the fault table is consulted and
// before the model sees it, so a scenario can land a change at an exact point (the fault still fires after it).
func TestBeforeCallRunsBeforeTheCallAndTheFault(t *testing.T) {
	in, _ := newInstrumented(t)
	var order []string
	in.Faults().BeforeCall("AddLabelToIssue", func(a Args) {
		order = append(order, "hook")
	})
	in.Faults().FailOnce("AddLabelToIssue", ErrRateLimit())

	if err := in.AddLabelToIssue("acme", "widgets", 7, "x"); err == nil {
		t.Fatal("injected failure did not fire after the hook")
	}
	if err := in.AddLabelToIssue("acme", "widgets", 7, "x"); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 {
		t.Errorf("hook ran %d times, want once per call (2)", len(order))
	}
}

func TestBeforeCallOnAnUnknownMethodPanics(t *testing.T) {
	in, _ := newInstrumented(t)
	defer func() {
		if recover() == nil {
			t.Error("BeforeCall on an unknown method did not panic")
		}
	}()
	in.Faults().BeforeCall("NoSuchMethod", func(Args) {})
}

// FailAllWrites fails every mutating call and leaves reads alone (#2393).
func TestFailAllWritesFailsOnlyMutations(t *testing.T) {
	in, _ := newInstrumented(t)
	in.Faults().FailAllWrites(ErrDeadlineExceeded())

	if err := in.AddLabelToIssue("acme", "widgets", 7, "fabrik:paused"); err == nil {
		t.Fatal("a write succeeded under FailAllWrites")
	}
	if _, err := in.FetchLabels("acme", "widgets", 7); err != nil {
		t.Fatalf("a read failed under FailAllWrites: %v", err)
	}
	labels, _ := in.Sim().FetchLabels("acme", "widgets", 7)
	if contains(labels, "fabrik:paused") {
		t.Error("the faulted write reached the model")
	}

	in.Faults().ClearBlanket()
	if err := in.AddLabelToIssue("acme", "widgets", 7, "fabrik:paused"); err != nil {
		t.Fatalf("write still failing after ClearBlanket: %v", err)
	}
}

// FailAll fails reads too, until cleared.
func TestFailAllFailsEverythingUntilCleared(t *testing.T) {
	in, _ := newInstrumented(t)
	in.Faults().FailAll(ErrDeadlineExceeded())
	if _, err := in.FetchLabels("acme", "widgets", 7); err == nil {
		t.Fatal("a read succeeded under FailAll")
	}
	if err := in.AddLabelToIssue("acme", "widgets", 7, "x"); err == nil {
		t.Fatal("a write succeeded under FailAll")
	}
	in.Faults().ClearBlanket()
	if _, err := in.FetchLabels("acme", "widgets", 7); err != nil {
		t.Fatalf("read still failing after ClearBlanket: %v", err)
	}
}

// The result observer is told each call's outcome: the injected error, or nil.
func TestResultObserverSeesEveryOutcome(t *testing.T) {
	in, _ := newInstrumented(t)
	var got []error
	in.Faults().SetResultObserver(func(err error) { got = append(got, err) })
	in.Faults().FailOnce("AddLabelToIssue", ErrServerError())

	_ = in.AddLabelToIssue("acme", "widgets", 7, "a")
	_ = in.AddLabelToIssue("acme", "widgets", 7, "a")
	if len(got) != 2 || got[0] == nil || got[1] != nil {
		t.Fatalf("observer saw %v, want [injected error, nil]", got)
	}
}

// Every method is either a read or a write for the blanket faults: FailAllWrites
// covers exactly the methods the instrumented layer flags as mutations, so a new
// interface method cannot silently escape it.
func TestBlanketFaultsCoverEveryFaultableMethod(t *testing.T) {
	for _, m := range InterfaceMethods() {
		if unfaultableMethods[m] {
			continue
		}
		validateMethod(m) // panics on a method the blanket faults could never reach
	}
}
