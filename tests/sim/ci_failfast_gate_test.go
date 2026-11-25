package sim

import (
	"strings"
	"testing"
)

// #2614: the stage-gate twin of mergetrain_failfast_test.go. A cancelled check run whose
// Actions run has a failed step is a CI failure: the CI-fix worker is dispatched with
// regression framing and the failing step named, not told the run was merely cancelled.

func TestCIFailFast_CancelledWithFailedStepDispatchesCIFixAsRegression(t *testing.T) {
	t.Parallel()
	env, num, _, bodies := cancelledGateEnv(t, cancelledGateOpts{rerunConclusion: "cancelled", jobs: failFastJobs(true)})

	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 120)
	WaitForIssueClosed(t, env, num, 120)

	got := bodies()
	if len(got) < 1 {
		t.Fatal("no CI-fix worker was dispatched for a fail-fast-cancelled run with a failed step")
	}
	for _, want := range []string{"NEW REGRESSION", "Run tests", "fail-fast"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("CI-fix context missing %q:\n%s", want, got[0])
		}
	}
	for _, bad := range []string{"not a test failure", "fresh run is needed"} {
		if strings.Contains(got[0], bad) {
			t.Errorf("CI-fix context must not carry cancellation framing %q:\n%s", bad, got[0])
		}
	}
}

// Non-vacuity: with the promotion neutralised the same state keeps the ADR 2296
// cancellation framing.
func TestCIFailFast_NeutralisedKeepsCancellationFraming(t *testing.T) {
	t.Parallel()
	env, num, _, bodies := cancelledGateEnv(t, cancelledGateOpts{rerunConclusion: "cancelled", jobs: failFastJobs(true)})
	defer env.Engine.SetFailFastPromotionDisabledForTest()()

	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 120)
	WaitForIssueClosed(t, env, num, 120)

	got := bodies()
	if len(got) < 1 {
		t.Fatal("no CI-fix worker was dispatched")
	}
	if strings.Contains(got[0], "NEW REGRESSION") || !strings.Contains(got[0], "fresh run is needed") {
		t.Errorf("neutralised run must keep the cancellation framing:\n%s", got[0])
	}
}

// No failed step: unchanged cancellation framing (R4).
func TestCIFailFast_CancelledWithoutFailedStepKeepsCancellationFraming(t *testing.T) {
	t.Parallel()
	env, num, _, bodies := cancelledGateEnv(t, cancelledGateOpts{rerunConclusion: "cancelled", jobs: failFastJobs(false)})

	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 120)
	WaitForIssueClosed(t, env, num, 120)

	got := bodies()
	if len(got) < 1 {
		t.Fatal("no CI-fix worker was dispatched")
	}
	if strings.Contains(got[0], "NEW REGRESSION") || !strings.Contains(got[0], "fresh run is needed") {
		t.Errorf("a run with no failed step must keep the cancellation framing:\n%s", got[0])
	}
}
