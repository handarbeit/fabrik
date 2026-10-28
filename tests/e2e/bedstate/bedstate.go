// Package bedstate is the one definition of the engine decision state a live e2e
// bed carries between engine runs that the gate and the live harness must clear
// (#2482, ADR-2482).
//
// Since #2349 (ADR 2349) the engine persists its merge-train caps — the runaway
// guard's trial window, per-member ejection counts and auto-repair attempts — to
// <bed>/.fabrik/state/engine-decisions/train-caps.json, so a real restart does
// not reset them. A gate leg switch is such a restart, so one leg's deliberate
// guard trip would otherwise pause an unrelated member in the next leg. The
// gate (leg switch, reset) and the harness (TestSwitchTrainMode, the guard test)
// therefore remove exactly that file while the bed's engine is stopped.
//
// Only the train-caps file is ever removed. usage-limit.json is account state,
// not test state, and stays. The engine is not changed: it reads a missing file
// as absent (ADR 2349).
//
// The package is untagged and stdlib-only, like tests/e2e/bedtimeout: tests/gate
// carries no e2e build tag and cannot import the tagged harness.
package bedstate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// TrainCapsFile is the engine's train-caps file name
	// (engine/decision_state.go trainCapsFileName; pinned by a drift test).
	TrainCapsFile = "train-caps.json"
	// UsageLimitFile is the account-wide usage-limit hold file. It is named here
	// only so tests can assert it is left alone.
	UsageLimitFile = "usage-limit.json"
)

// DecisionsDir is the bed's engine-decisions state directory.
func DecisionsDir(bedDir string) string {
	return filepath.Join(bedDir, ".fabrik", "state", "engine-decisions")
}

// TrainCapsPath is the bed's train-caps file.
func TrainCapsPath(bedDir string) string {
	return filepath.Join(DecisionsDir(bedDir), TrainCapsFile)
}

// ClearTrainCaps removes the bed's train-caps file and reports whether one was
// removed. It must only be called while the bed's engine is stopped: a running
// engine rewrites the file from memory. A missing file (or directory) is
// success; any other failure is returned. It uses os.Remove, never RemoveAll, so
// a directory in the file's place is reported rather than silently deleted.
func ClearTrainCaps(bedDir string) (bool, error) {
	path := TrainCapsPath(bedDir)
	err := os.Remove(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("removing %s: %w", path, err)
}
