package stages

import (
	"os"
	"testing"

	"github.com/handarbeit/fabrik/internal/testenv"
)

// TestMain keeps operator-warnings recorders (#2135) out of the package
// directory.
func TestMain(m *testing.M) {
	cleanupWarnings := testenv.IsolateWarningsProcess()
	code := m.Run()
	cleanupWarnings()
	os.Exit(code)
}
