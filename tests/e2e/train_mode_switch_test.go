//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSwitchTrainMode restarts the shared test bed with FABRIK_MERGE_TRAIN
// set to the mode named by E2E_TRAIN_MODE, satisfying FR-5: mode is read at
// Fabrik startup, so switching it requires a restart — never an in-run flip
// while other scenarios' t.Parallel() invocations might be in flight.
//
// This is deliberately its own `go test` invocation (see scripts/e2e/run.sh),
// not folded into the main suite run: a dedicated invocation whose only test
// is this one completes — bed fully back up in the new mode — before the
// suite invocation that follows it even starts. That makes "the restart
// completes before any scenario starts" structural rather than dependent on
// Go's "non-parallel tests run before parallel ones" ordering interacting
// correctly with whatever -run pattern the caller passed to a combined
// invocation.
//
// Gated on E2E_TRAIN_SWITCH=1 in addition to requiring E2E_TRAIN_MODE, so a
// broad -run pattern (or a plain full-suite invocation with E2E_TRAIN_MODE
// set) doesn't accidentally sweep this test into the main suite run and
// restart the bed a second, redundant time. scripts/e2e/run.sh's dedicated
// switch step is the only caller that sets it.
func TestSwitchTrainMode(t *testing.T) {
	if os.Getenv("E2E_TRAIN_SWITCH") != "1" {
		t.Skip("only runs when E2E_TRAIN_SWITCH=1 (set by scripts/e2e/run.sh's mode-switch step)")
	}

	rawMode := os.Getenv("E2E_TRAIN_MODE")
	mode, err := normalizeTrainMode(rawMode)
	if err != nil {
		t.Fatalf("E2E_TRAIN_MODE=%q is invalid (must be on or off)", rawMode)
	}

	env := LoadEnv(t)

	// Mirrors RestartFabrikTestBed (lifecycle.go): register the bring-back-up
	// before stopping, so a failure between Stop and Start (e.g. writeEnvFileValue
	// erroring, or StartFabrikTestBed itself failing) still leaves the bed running
	// instead of down for whoever/whatever uses it next. StartFabrikTestBed is a
	// no-op if the bed is already up, so this is harmless on the successful path.
	t.Cleanup(func() { StartFabrikTestBed(t, env) })

	t.Logf("switching test bed to FABRIK_MERGE_TRAIN=%s", mode)
	StopFabrikTestBed(t, env)

	envFile := filepath.Join(env.FabrikTestDir, ".env")
	if err := writeEnvFileValue(envFile, "FABRIK_MERGE_TRAIN", mode); err != nil {
		t.Fatalf("write FABRIK_MERGE_TRAIN=%s to %s: %v", mode, envFile, err)
	}

	StartFabrikTestBed(t, env)

	// Postcondition: assert the bed's .env still holds the requested mode
	// after the restart, per this issue's explicit acceptance criteria.
	// StartFabrikTestBed above already t.Fatalf's if the bed doesn't come
	// up, so "is it running" is already covered by the time we get here.
	// This only re-reads the file writeEnvFileValue just wrote a few lines
	// up (whose own write error is already checked) — it can't observe
	// whether the live Fabrik process actually resolved FABRIK_MERGE_TRAIN
	// from it, which would need new instrumentation on the bed and is out
	// of scope here. It does still guard against the .env state being
	// silently reverted or corrupted between that write and this read.
	got, err := readEnvFileValue(envFile, "FABRIK_MERGE_TRAIN")
	if err != nil {
		t.Fatalf("postcondition failed: read FABRIK_MERGE_TRAIN from %s: %v", envFile, err)
	}
	if got != mode {
		t.Fatalf("postcondition failed: %s holds FABRIK_MERGE_TRAIN=%q, want %q", envFile, got, mode)
	}

	t.Logf("test bed restarted with FABRIK_MERGE_TRAIN=%s", mode)
}
