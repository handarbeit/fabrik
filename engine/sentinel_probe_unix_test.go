//go:build !windows

package engine

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// startSentinelHelper re-execs this test binary in FABRIK_TEST_SENTINEL_HELPER
// mode (see TestMain in process_item_test.go) with the given extra argv
// tokens appended, so a real, ps-visible process exists carrying whatever
// tokens the caller wants to embed — without depending on an external binary
// like `sleep` accepting arbitrary trailing arguments. The helper blocks
// forever until killed; callers must kill it (t.Cleanup below handles this).
func startSentinelHelper(t *testing.T, extraArgs ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], extraArgs...)
	cmd.Env = append(os.Environ(), "FABRIK_TEST_SENTINEL_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sentinel helper process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// paddingArgs returns n filler argv tokens, mirroring the shape of a real
// claude invocation's long argument list (many flags before --name near the
// end) so the probe is exercised against a realistic argv, not a two-token one.
func paddingArgs(n int) []string {
	args := make([]string, n)
	for i := range args {
		args[i] = fmt.Sprintf("--argpad%02d", i)
	}
	return args
}

func TestProbeSentinelLive_FindsExactMatch(t *testing.T) {
	sentinel := "fabrik:owner/repo#2966:Implement"
	args := append(paddingArgs(20), "--name", sentinel)
	cmd := startSentinelHelper(t, args...)
	pid := cmd.Process.Pid

	// Give the OS a moment to make the new process visible to `ps`.
	var result sentinelProbeResult
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		result = probeSentinelLive(sentinel)
		if result.Live {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if result.Err != nil {
		t.Fatalf("probeSentinelLive returned an error: %v", result.Err)
	}
	if !result.Live {
		t.Fatal("expected sentinel to be found live")
	}
	if result.PID != pid {
		t.Errorf("PID = %d, want %d", result.PID, pid)
	}
}

// TestProbeSentinelLive_RejectsSubstringMatch is the Acceptance 6 regression:
// a live process carrying "#29660" as its sentinel must NOT be matched when
// probing for "#2966" — exact-token equality only, never substring/Contains.
func TestProbeSentinelLive_RejectsSubstringMatch(t *testing.T) {
	decoySentinel := "fabrik:owner/repo#29660:Implement"
	probedSentinel := "fabrik:owner/repo#2966:Implement"
	args := append(paddingArgs(20), "--name", decoySentinel)
	startSentinelHelper(t, args...)

	// Poll briefly to let the decoy process become ps-visible, then assert
	// it is never matched by the (deliberately different) probed sentinel.
	time.Sleep(200 * time.Millisecond)
	result := probeSentinelLive(probedSentinel)
	if result.Err != nil {
		t.Fatalf("probeSentinelLive returned an error: %v", result.Err)
	}
	if result.Live {
		t.Errorf("probeSentinelLive matched %q against a decoy carrying %q — substring match, want exact-token only", probedSentinel, decoySentinel)
	}
}

func TestProbeSentinelLive_NotFoundWhenNoMatchingProcess(t *testing.T) {
	result := probeSentinelLive("fabrik:owner/repo#999999999:NoSuchStage")
	if result.Err != nil {
		t.Fatalf("probeSentinelLive returned an error: %v", result.Err)
	}
	if result.Live {
		t.Error("expected no live process to carry a never-used sentinel")
	}
}

func TestProbeSentinelLive_EmptySentinelErrors(t *testing.T) {
	result := probeSentinelLive("")
	if result.Err == nil {
		t.Error("expected an error for an empty sentinel")
	}
	if result.Live {
		t.Error("expected Live == false for an empty sentinel")
	}
}
