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
	in.Faults().FailOnce("MergePR", WrapErr(gh.ErrNotMergeable, "simgh: injected"))

	err := in.MergePR("acme", "widgets", 1)
	if !errors.Is(err, gh.ErrNotMergeable) {
		t.Fatalf("MergePR err = %v, want errors.Is(err, gh.ErrNotMergeable)", err)
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
