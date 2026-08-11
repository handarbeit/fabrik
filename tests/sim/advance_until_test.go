package sim

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// advanceUntilHelperEnvVar selects the inner (deliberately-failing) half of
// TestAdvanceUntil_ExhaustionDiagnostics when re-exec'd as a subprocess —
// same pattern as main_test.go's TestMain_Help (os.Args[0] re-exec with
// -test.run, gated on an env var).
const advanceUntilHelperEnvVar = "SIM_ADVANCE_UNTIL_HELPER"

// TestAdvanceUntil_ExhaustionDiagnostics is AC7: a deliberately unsatisfiable
// condition must make AdvanceUntil fail with a message containing both the
// board state and the recent mutation log, so a failure is diagnosable from
// CI output alone (R4).
//
// Verifying this requires observing the actual text t.Fatalf produces, which
// the testing package does not expose to the calling test — so, as
// main_test.go's TestMain_Help does for main()'s exit behavior, this test
// re-execs its own test binary as a subprocess, running only the inner half
// below, and asserts against its captured combined output.
func TestAdvanceUntil_ExhaustionDiagnostics(t *testing.T) {
	if os.Getenv(advanceUntilHelperEnvVar) == "1" {
		// Inner half: runs in the subprocess. Deliberately unsatisfiable
		// condition with a small maxPolls, so this fails fast via t.Fatalf.
		//
		// The dispatched worker is scripted to never complete
		// (TurnLimitExhausted — incomplete, no marker, StageRetryIncremented
		// applies but the 10s real retry cooldown vastly outlasts this test's
		// 3-poll window) so the item deterministically stays in "Specify" for
		// the assertions below, regardless of how quickly RunPoll's worker-
		// quiescence wait lets the single dispatched attempt actually finish.
		// An earlier version of this test relied on a fixed-duration
		// per-poll sleep leaving the worker still asleep mid-dispatch by the
		// time 3 polls ran out — exactly the incidental real-time coupling
		// RunPoll's quiescence wait (#1450 follow-up) exists to remove, and
		// which made the item advance and archive before this test's
		// diagnostics fired once that coupling was fixed.
		env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})
		env.Claude.ForStage("Specify", simclaude.TurnLimitExhausted(50))
		FileIssue(t, env, "AC7 exhaustion probe", "body", "Specify")
		AdvanceUntil(t, env, func(env *Env) bool { return false }, 3)
		return
	}

	skipIfNoGit(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestAdvanceUntil_ExhaustionDiagnostics$", "-test.v")
	cmd.Env = append(os.Environ(), advanceUntilHelperEnvVar+"=1")
	out, err := cmd.CombinedOutput()
	output := string(out)

	if err == nil {
		t.Fatalf("expected the inner AdvanceUntil call to fail (unsatisfiable condition), but the subprocess exited 0:\n%s", output)
	}
	if !strings.Contains(output, "AdvanceUntil: condition not met after 3 polls") {
		t.Errorf("failure output missing the expected AdvanceUntil exhaustion message:\n%s", output)
	}
	if !strings.Contains(output, "=== board state ===") {
		t.Errorf("failure output missing the board-state section (R4/AC7):\n%s", output)
	}
	if !strings.Contains(output, "#1 Specify") {
		t.Errorf("failure output's board-state section should list the filed issue's number and column:\n%s", output)
	}
	if !strings.Contains(output, "=== mutation log (most recent) ===") {
		t.Errorf("failure output missing the mutation-log section (R4/AC7):\n%s", output)
	}
	if !strings.Contains(output, "FetchProjectBoard") {
		t.Errorf("failure output's mutation-log section should list actual intercepted calls:\n%s", output)
	}
}
