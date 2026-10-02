package gate

import (
	"reflect"
	"strings"
	"testing"
)

func TestRetryLogPath(t *testing.T) {
	if got := retryLogPath("/a/go-test.json", 2); got != "/a/go-test.retry-2.json" {
		t.Errorf("got %q", got)
	}
	if got := retryLogPath("/tmp/fabrik-e2e-pat-off-1.json", 1); got != "/tmp/fabrik-e2e-pat-off-1.retry-1.json" {
		t.Errorf("got %q", got)
	}
}

func ev(action, test string) Event { return Event{Action: action, Test: test} }

func TestMergeAttemptsLastAttemptWinsPerTest(t *testing.T) {
	base := []Event{ev("pass", "TestA"), ev("skip", "TestB"), ev("skip", "TestB/sub"), ev("pass", "TestC"), {Action: "fail", Package: "p"}}
	retry := []Event{ev("run", "TestB"), ev("pass", "TestB/sub"), ev("pass", "TestB"), {Action: "pass", Package: "p"}}
	merged := mergeAttempts(base, retry)
	c := Classify(merged)
	if !reflect.DeepEqual(c.Pass, []string{"TestA", "TestB", "TestC"}) || len(c.Skip) != 0 {
		t.Fatalf("merged classification = %+v", c)
	}
	// The retry's package-level event is dropped; the first attempt's stays.
	var pkg []string
	for _, e := range merged {
		if e.Test == "" {
			pkg = append(pkg, e.Action)
		}
	}
	if !reflect.DeepEqual(pkg, []string{"fail"}) {
		t.Errorf("package-level events = %v", pkg)
	}
}

func TestSummarizeRetriesAndSummaryText(t *testing.T) {
	first := []string{"TestA", "TestB", "TestC", "TestD"}
	final := Classification{Pass: []string{"TestA"}, Fail: []string{"TestB"}, Inconclusive: []string{"TestC", "TestD"}}
	o := summarizeRetries(first, final, 2)
	if !reflect.DeepEqual(o.PassedOnRetry, []string{"TestA"}) || !reflect.DeepEqual(o.FailedOnRetry, []string{"TestB"}) || !reflect.DeepEqual(o.Still, []string{"TestC", "TestD"}) {
		t.Fatalf("outcome = %+v", o)
	}
	s := o.Summary("pat/off", 3)
	for _, want := range []string{"4 on the first attempt: TestA, TestB, TestC, TestD", "passed on retry: TestA", "failed on retry: TestB", "UNCOVERED, not failed): TestC, TestD", "WARNING", "#1974"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
	// At the threshold exactly there is no warning; with none there is no output.
	if s := o.Summary("pat/off", 4); strings.Contains(s, "WARNING") {
		t.Errorf("no warning at the threshold:\n%s", s)
	}
	if s := summarizeRetries(nil, Classification{}, 0).Summary("x", 3); s != "" {
		t.Errorf("no inconclusives, no summary: %q", s)
	}
}
