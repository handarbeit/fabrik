package sim

import (
	"os"
	"testing"

	"github.com/handarbeit/fabrik/internal/testenv"
	"github.com/handarbeit/fabrik/internal/testlife"
)

// TestMain bounds the sim test binary's lifetime (#2128, ADR-2128) and keeps
// operator-warnings recorders (#2135) out of the package directory.
//
// A sim.test started under a Claude worker used to be able to outlive the run
// that started it — one ran 32 hours — because nothing below `go test` itself
// bounded it: no -test.timeout on a binary run directly or re-exec'd, and no
// owner.
//
//   - testlife.Guard installs the hard wall-time ceiling (SIM_TEST_CEILING,
//     default 30m, ignoring -test.timeout), the owner watch (exit once the worker
//     named by FABRIK_WORKER_OWNER is provably gone; a live worker with its
//     launching shell long gone is left alone) and, for a helper started through
//     testlife.ReExecCommand, the parent watch.
//   - testenv.ScrubProcess keeps the suite hermetic against a worker's inherited
//     credentials (#2027), as the cmd and engine packages' TestMain do.
//   - testenv.IsolateWarningsProcess: the engine under test writes the warnings
//     panel file, which must not land in the package directory.
func TestMain(m *testing.M) {
	testlife.Guard(testlife.Options{Name: "tests/sim"})
	testenv.ScrubProcess()
	cleanupWarnings := testenv.IsolateWarningsProcess()
	code := m.Run()
	cleanupWarnings()
	os.Exit(code)
}
