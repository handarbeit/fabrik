//go:build !windows

package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestInvokeClaude_GrandchildHoldsPipe verifies that when a grandchild process
// spawned by the Claude subprocess holds the stdout pipe open after Claude exits,
// InvokeClaude returns within a bounded time (via WaitDelay) and correctly
// processes the buffered output including FABRIK_STAGE_COMPLETE.
func TestInvokeClaude_GrandchildHoldsPipe(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	// The script emits valid NDJSON then backgrounds sleep to hold the pipe open,
	// simulating a grandchild process (e.g. tail -f from the Monitor tool) that
	// outlives the Claude process.
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"stage output\\nFABRIK_STAGE_COMPLETE\\n\",\"session_id\":\"sess_gchild\",\"num_turns\":3,\"total_cost_usd\":0.001,\"is_error\":false}'\n" +
		"sleep 60 &\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	// Short WaitDelay so the test completes quickly (1s vs the production default of 30s).
	origDelay := claudeWaitDelay
	claudeWaitDelay = 1 * time.Second
	defer func() { claudeWaitDelay = origDelay }()

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:   "Research",
		Prompt: "Do research",
	}
	issue := gh.ProjectItem{Number: 99, Title: "GrandchildHoldsPipe"}

	type result struct {
		output    string
		completed bool
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		output, completed, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, InvokeOptions{})
		ch <- result{output, completed, err}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("InvokeClaude: %v", res.err)
		}
		if !res.completed {
			t.Errorf("expected completed=true; output=%q", res.output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("InvokeClaude did not return within 10s — likely stuck waiting for grandchild to close pipe")
	}
}

// TestInvokeClaude_MaxWallTimeKillsWithComplete verifies that when a Claude process
// emits FABRIK_STAGE_COMPLETE in an assistant turn and then hangs, a max_wall_time
// kill still surfaces completed=true by scanning the streamed assistant output.
func TestInvokeClaude_MaxWallTimeKillsWithComplete(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	// Print a valid assistant NDJSON line with FABRIK_STAGE_COMPLETE, then hang.
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"printf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"Done!\\nFABRIK_STAGE_COMPLETE\\n\"}]}}'\n" +
		"sleep 60\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	origDelay := claudeWaitDelay
	claudeWaitDelay = 1 * time.Second
	defer func() { claudeWaitDelay = origDelay }()

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:        "Review",
		Prompt:      "Do review",
		MaxWallTime: 500 * time.Millisecond,
	}
	issue := gh.ProjectItem{Number: 99, Title: "MaxWallTimeWithComplete"}

	type result struct {
		output    string
		completed bool
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		output, completed, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, InvokeOptions{})
		ch <- result{output, completed, err}
	}()

	select {
	case res := <-ch:
		if !res.completed {
			t.Errorf("expected completed=true (FABRIK_STAGE_COMPLETE in stream); err=%v; output=%q", res.err, res.output)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("InvokeClaude did not return within 15s after max_wall_time kill")
	}
}

// TestInvokeClaude_MaxWallTimeKillsWithoutComplete verifies that when a Claude
// process hangs without emitting FABRIK_STAGE_COMPLETE, a max_wall_time kill
// results in completed=false.
func TestInvokeClaude_MaxWallTimeKillsWithoutComplete(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	// Just hang — no output.
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"sleep 60\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	origDelay := claudeWaitDelay
	claudeWaitDelay = 1 * time.Second
	defer func() { claudeWaitDelay = origDelay }()

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:        "Review",
		Prompt:      "Do review",
		MaxWallTime: 500 * time.Millisecond,
	}
	issue := gh.ProjectItem{Number: 99, Title: "MaxWallTimeWithoutComplete"}

	type result struct {
		output    string
		completed bool
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		output, completed, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, InvokeOptions{})
		ch <- result{output, completed, err}
	}()

	select {
	case res := <-ch:
		if res.completed {
			t.Errorf("expected completed=false (no FABRIK_STAGE_COMPLETE); output=%q", res.output)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("InvokeClaude did not return within 15s after max_wall_time kill")
	}
}

// TestKillProcGroupGraceful_StructuredLog (SC-1) verifies that the kill escalation
// sequence emits structured log lines for each signal sent to the process group,
// with the correct signal name and reason code (max_wall_time in this case).
func TestKillProcGroupGraceful_StructuredLog(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	// Script ignores SIGINT and SIGTERM so all three signals are sent before SIGKILL
	// terminates the loop. This ensures we get log lines for the full escalation.
	script := "#!/bin/sh\n" +
		"trap '' INT TERM\n" +
		"cat >/dev/null\n" +
		"while true; do sleep 1; done\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	origDelay := claudeWaitDelay
	claudeWaitDelay = 500 * time.Millisecond
	defer func() { claudeWaitDelay = origDelay }()

	origSigInt := claudeKillGraceSigInt
	origSigTerm := claudeKillGraceSigTerm
	claudeKillGraceSigInt = 200 * time.Millisecond
	claudeKillGraceSigTerm = 200 * time.Millisecond
	defer func() {
		claudeKillGraceSigInt = origSigInt
		claudeKillGraceSigTerm = origSigTerm
	}()

	var logLines []string
	var logMu sync.Mutex
	origLogf := claudeLogf
	claudeLogf = func(issueNumber int, tag, format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		logMu.Lock()
		logLines = append(logLines, fmt.Sprintf("[#%d %s] %s", issueNumber, tag, msg))
		logMu.Unlock()
	}
	defer func() { claudeLogf = origLogf }()

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:        "Validate",
		Prompt:      "Do validate",
		MaxWallTime: 300 * time.Millisecond,
	}
	issue := gh.ProjectItem{Number: 42, Title: "KillStructuredLog"}

	InvokeClaude(context.Background(), stage, issue, nil, false, workDir, InvokeOptions{})

	logMu.Lock()
	captured := append([]string(nil), logLines...)
	logMu.Unlock()

	var hasSIGINT, hasSIGTERM, hasSIGKILL bool
	for _, line := range captured {
		if strings.Contains(line, "kill]") && strings.Contains(line, "SIGINT") && strings.Contains(line, "reason=max_wall_time") {
			hasSIGINT = true
		}
		if strings.Contains(line, "kill]") && strings.Contains(line, "SIGTERM") && strings.Contains(line, "reason=max_wall_time") {
			hasSIGTERM = true
		}
		if strings.Contains(line, "kill]") && strings.Contains(line, "SIGKILL") && strings.Contains(line, "reason=max_wall_time") {
			hasSIGKILL = true
		}
	}
	t.Logf("kill log lines: SIGINT=%v SIGTERM=%v SIGKILL=%v; all captured: %v", hasSIGINT, hasSIGTERM, hasSIGKILL, captured)
	if !hasSIGINT {
		t.Error("expected SIGINT log line with reason=max_wall_time")
	}
	if !hasSIGTERM {
		t.Error("expected SIGTERM log line with reason=max_wall_time")
	}
	if !hasSIGKILL {
		t.Error("expected SIGKILL log line with reason=max_wall_time")
	}
}

// TestKillProcGroupGraceful_SIGINTGraceWindow (SC-2) verifies that a child process
// that handles SIGINT can write a sentinel file within the SIGINT grace window before
// SIGTERM arrives. This simulates a test-runner wrapper that catches SIGINT to post
// a final Commit Status before being terminated.
func TestKillProcGroupGraceful_SIGINTGraceWindow(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")

	sentinelFile := filepath.Join(t.TempDir(), "sentinel")

	// Use the test binary itself as the fake claude subprocess: shell traps are
	// unreliable on macOS (bash re-raises SIGINT after a foreground command exits
	// rather than running the trap). Go's signal.Notify is reliable. TestMain
	// detects FABRIK_TEST_SIGINT_SENTINEL and enters subprocess mode: drains stdin,
	// waits for SIGINT, writes the sentinel, exits 0.
	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// Shell wrapper: exec replaces the shell with the test binary so the test binary
	// IS the process in the Claude process group and receives SIGINT directly.
	script := "#!/bin/sh\n" +
		"FABRIK_TEST_SIGINT_SENTINEL='" + sentinelFile + "' exec '" + testBin + "' -test.run='^$'\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	origDelay := claudeWaitDelay
	claudeWaitDelay = 500 * time.Millisecond
	defer func() { claudeWaitDelay = origDelay }()

	// 3s SIGINT grace gives the child ample time to write the sentinel.
	origSigInt := claudeKillGraceSigInt
	origSigTerm := claudeKillGraceSigTerm
	claudeKillGraceSigInt = 3 * time.Second
	claudeKillGraceSigTerm = 200 * time.Millisecond
	defer func() {
		claudeKillGraceSigInt = origSigInt
		claudeKillGraceSigTerm = origSigTerm
	}()

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:   "Validate",
		Prompt: "Do validate",
		// 2s gives the Go test-binary subprocess enough time to start up and install
		// signal.Notify before SIGINT arrives. 100ms was too short: SIGINT landed
		// before the runtime reached signal.Notify, triggering the default handler.
		MaxWallTime: 2 * time.Second,
	}
	issue := gh.ProjectItem{Number: 43, Title: "SIGINTGraceWindow"}

	InvokeClaude(context.Background(), stage, issue, nil, false, workDir, InvokeOptions{})

	if _, err := os.Stat(sentinelFile); err != nil {
		t.Errorf("sentinel file not created: child SIGINT handler did not run before SIGTERM landed; path=%q", sentinelFile)
	}
}

// TestInvokeClaude_InactivityTimeout verifies that when no streamed output is
// received for claudeInactivityTimeout, the process is killed and completed=false.
func TestInvokeClaude_InactivityTimeout(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	// Hang with no output.
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"sleep 60\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	origDelay := claudeWaitDelay
	claudeWaitDelay = 1 * time.Second
	defer func() { claudeWaitDelay = origDelay }()

	// Override inactivity timeout to 2s so the test completes quickly.
	origInactivity := claudeInactivityTimeout
	claudeInactivityTimeout = 2 * time.Second
	defer func() { claudeInactivityTimeout = origInactivity }()

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:   "Review",
		Prompt: "Do review",
		// No MaxWallTime — only the inactivity timeout applies.
	}
	issue := gh.ProjectItem{Number: 99, Title: "InactivityTimeout"}

	type result struct {
		output    string
		completed bool
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		output, completed, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, InvokeOptions{})
		ch <- result{output, completed, err}
	}()

	select {
	case res := <-ch:
		if res.completed {
			t.Errorf("expected completed=false (inactivity kill); output=%q", res.output)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("InvokeClaude did not return within 20s after inactivity timeout")
	}
}
