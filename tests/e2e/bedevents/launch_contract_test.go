package bedevents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The e2e-tagged harness cannot be compiled by plain `go test`, so the half of
// the launch contract that lives there (tests/e2e/lifecycle.go's
// StartFabrikTestBed and bedPollSeconds) is pinned by reading the source; the
// gate runner's half is pinned behaviourally by tests/gate's
// TestBedStartCmdContracts.
func TestHarnessLaunchInjectsTheLegsEventSource(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "lifecycle.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	for _, want := range []string{
		"bedevents.StripKeys()",
		"bedevents.Env(bedEventSource())...",
		"bedevents.PollSeconds(bedEventSource())",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("tests/e2e/lifecycle.go must contain %q (#2408)", want)
		}
	}
	// Strip first, inject after: an inject followed by a strip would drop the value.
	if strings.Index(src, "bedevents.StripKeys()") > strings.Index(src, "bedevents.Env(bedEventSource())...") {
		t.Error("tests/e2e/lifecycle.go must strip the ambient event-source variables before injecting bedevents.Env")
	}
	if got := strings.Count(src, "bedevents.Env("); got != 1 {
		t.Errorf("bedevents.Env must be injected exactly once in StartFabrikTestBed, found %d", got)
	}
}
