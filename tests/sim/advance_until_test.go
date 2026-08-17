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

// clonePauseHelperEnvVar selects the inner half of
// TestAdvanceUntil_ClonePauseFailsFast when re-exec'd as a subprocess — same
// pattern as advanceUntilHelperEnvVar above.
const clonePauseHelperEnvVar = "SIM_ADVANCE_UNTIL_CLONE_PAUSE_HELPER"

// TestAdvanceUntil_ClonePauseFailsFast is this issue's own regression guard
// for the fail-fast check clonePauseDetected adds to AdvanceUntil (#1452
// review-comment finding): a member already paused specifically by a failed
// repo clone (ensureRepoReady's "cannot clone repo" comment) must abort the
// wait immediately — before spending any of maxPolls — rather than burning
// the full budget doing nothing but reads. This is exactly the shape that
// produced a confusing "condition not met after 20 polls" timeout for three
// genuinely-correct merge-train scenarios under CI load: real-git clone
// contention transiently paused an unrelated member before the train ever
// ran, and the old exhaustion message gave no hint why.
//
// maxPolls is set to 20 specifically, matching the poll budget the three
// affected merge-train scenarios actually used — proving the fix would have
// turned their exact confusing timeout into a direct diagnosis, not just
// some other, smaller bound.
func TestAdvanceUntil_ClonePauseFailsFast(t *testing.T) {
	if os.Getenv(clonePauseHelperEnvVar) == "1" {
		// Inner half: runs in the subprocess. Seeds a member already paused
		// by a failed clone (the exact shape ensureRepoReady leaves behind —
		// engine/engine.go's pauseIssue call with awaitingInput: true, plus
		// its "cannot clone repo" comment) and then waits on a condition that
		// can never become true, so the only way this call returns is via
		// clonePauseDetected's fast-fail path or the old 20-poll exhaustion.
		env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})
		num := FileIssue(t, env, "clone-pause probe", "body", "Specify",
			"fabrik:paused", "fabrik:awaiting-input")
		env.Sim.Sim().SeedComment(env.OwnerRepo, num, "fabrik-bot",
			clonePauseMarker+"\n\nFailed to clone `acme/widgets`:\n```\nsimulated: exit status 128\n```\nHuman intervention required. Fix the clone issue and remove `fabrik:paused` to retry.")
		if err := env.Sim.Sim().Err(); err != nil {
			t.Fatalf("seeding clone-pause fixture: %v", err)
		}
		AdvanceUntil(t, env, func(env *Env) bool { return false }, 20)
		return
	}

	skipIfNoGit(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestAdvanceUntil_ClonePauseFailsFast$", "-test.v")
	cmd.Env = append(os.Environ(), clonePauseHelperEnvVar+"=1")
	out, err := cmd.CombinedOutput()
	output := string(out)

	if err == nil {
		t.Fatalf("expected the inner AdvanceUntil call to fail (clone-paused member), but the subprocess exited 0:\n%s", output)
	}
	if !strings.Contains(output, "is paused by a failed repo clone before this wait even began") {
		t.Errorf("failure output missing the clone-pause fast-fail message:\n%s", output)
	}
	if !strings.Contains(output, "pause reason:") {
		t.Errorf("failure output missing the pause reason section:\n%s", output)
	}
	if !strings.Contains(output, clonePauseMarker) {
		t.Errorf("failure output's pause reason should quote the actual pause comment:\n%s", output)
	}
	// Non-vacuity: the old exhaustion path (which this fixes) would have
	// produced this exact message instead — its absence proves the fast-fail
	// check fired first, not that AdvanceUntil merely failed for some other
	// reason.
	if strings.Contains(output, "AdvanceUntil: condition not met after 20 polls") {
		t.Errorf("expected the fast-fail path to preempt the 20-poll exhaustion message entirely, but both appear:\n%s", output)
	}
}
