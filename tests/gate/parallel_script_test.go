package gate

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestDefaultRaceParallelScript runs scripts/lib/parallel_test.sh (#2228), the
// per-OS table test for default_race_parallel. CI's workflow cannot be edited
// by the engine, and the script is not run by any other step, so this keeps it
// inside plain `go test ./...` where every PR runs it.
func TestDefaultRaceParallelScript(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "lib", "parallel_test.sh"))
	if err != nil {
		t.Fatalf("resolving script path: %v", err)
	}
	out, err := exec.Command(bash, script).CombinedOutput()
	if err != nil {
		t.Fatalf("parallel_test.sh failed: %v\n%s", err, out)
	}
}
