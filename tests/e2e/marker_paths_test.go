//go:build e2e

package e2e

import "testing"

// TestMarkerPathsAreUnique statically enumerates markerPaths and fails on any
// duplicate or empty value (handarbeit/fabrik#1394 AC1). It requires no live
// bed:
//
//	go test -tags e2e -run TestMarkerPathsAreUnique -v ./tests/e2e/
//
// TestConvergenceRace is deliberately absent from markerPaths — see the
// comment above the map in harness.go — so its intentional collision on
// README.md is not itself a bug this test should ever flag.
func TestMarkerPathsAreUnique(t *testing.T) {
	seen := make(map[string]string, len(markerPaths))
	for scenario, path := range markerPaths {
		if path == "" {
			t.Fatalf("markerPaths[%q] is empty", scenario)
		}
		if other, ok := seen[path]; ok {
			t.Fatalf("marker path %q is used by both %q and %q — each scenario must have a unique path", path, other, scenario)
		}
		seen[path] = scenario
	}
}
