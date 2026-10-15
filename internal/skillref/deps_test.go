package skillref

import (
	"os/exec"
	"strings"
	"testing"
)

// The package is shared with Pruefer, which must not import engine (ADR 1113).
// Keep the dependency set to the standard library plus the module's two leaves.
func TestNoEngineSideDependencies(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/handarbeit/fabrik/internal/skillref").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if !strings.HasPrefix(dep, "github.com/handarbeit/fabrik/") {
			continue
		}
		switch dep {
		case "github.com/handarbeit/fabrik/internal/skillref",
			"github.com/handarbeit/fabrik/plugin",
			"github.com/handarbeit/fabrik/internal/claudeerr":
		default:
			t.Errorf("internal/skillref depends on %s", dep)
		}
	}
}
