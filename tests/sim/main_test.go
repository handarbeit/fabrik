package sim

import (
	"os"
	"testing"

	"github.com/handarbeit/fabrik/internal/testenv"
)

// TestMain keeps operator-warnings recorders (#2135) out of the package
// directory: the engine under test writes the warnings panel file.
func TestMain(m *testing.M) {
	cleanupWarnings := testenv.IsolateWarningsProcess()
	code := m.Run()
	cleanupWarnings()
	os.Exit(code)
}
