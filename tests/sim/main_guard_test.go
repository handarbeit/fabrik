package sim

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/testlife"
)

const guardProbeEnvVar = "SIM_GUARD_PROBE"

// TestMain_InstallsTestLifeGuard is #2128 acceptance 3 on the real sim binary: a
// sim test binary forced past its ceiling exits with a message naming the
// ceiling. The inner half idles; the outer half re-execs this binary with a 1s
// ceiling. Non-vacuity is built in: were TestMain not installing the Guard, the
// helper would idle until its 30s -test.timeout and die with "test timed out"
// instead, and the assertions below would fail.
func TestMain_InstallsTestLifeGuard(t *testing.T) {
	if os.Getenv(guardProbeEnvVar) == "1" {
		time.Sleep(time.Hour)
		return
	}
	cmd, cancel := testlife.ReExecCommand(30*time.Second, "-test.run=^TestMain_InstallsTestLifeGuard$")
	defer cancel()
	cmd.Env = append(cmd.Env, guardProbeEnvVar+"=1", testlife.CeilingEnv+"=1s")
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the probe to be ended by the ceiling, but it exited 0:\n%s", out)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("ceiling of 1s took %s to fire", took)
	}
	if !strings.Contains(string(out), "tests/sim: test binary exceeded its hard wall-time ceiling of 1s") {
		t.Errorf("exit message does not name the ceiling:\n%s", out)
	}
	if strings.Contains(string(out), "test timed out") {
		t.Errorf("the probe was ended by -test.timeout, not the ceiling:\n%s", out)
	}
}
