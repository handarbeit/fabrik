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

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

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

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

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

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

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

// TestInvokeClaude_ExtendTurnsScalesWallTime verifies #1206: a fabrik:extend-turns
// first-invocation pre-grant (opts.MaxTurnsOverride = 2x stage.MaxTurns) scales the
// max_wall_time deadline by the same 2x factor, so an invocation that runs past the
// *unscaled* deadline but within the *scaled* one is not killed.
func TestInvokeClaude_ExtendTurnsScalesWallTime(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	// Sleeps past the unscaled 800ms deadline but well within the scaled 1600ms one,
	// then emits a valid completion. Margins (400ms on each side) are kept generous so
	// this doesn't flake under CPU contention from sibling subprocess tests.
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"sleep 1.2\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"done\\nFABRIK_STAGE_COMPLETE\\n\",\"session_id\":\"sess_scaled\",\"num_turns\":150,\"total_cost_usd\":0.001,\"is_error\":false}'\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	origDelay := claudeWaitDelay
	claudeWaitDelay = 1 * time.Second
	defer func() { claudeWaitDelay = origDelay }()

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:        "Implement",
		Prompt:      "Do implement",
		MaxTurns:    100,
		MaxWallTime: 800 * time.Millisecond,
	}
	issue := gh.ProjectItem{Number: 99, Title: "ExtendTurnsScalesWallTime"}
	opts := InvokeOptions{MaxTurnsOverride: 200} // 2x stage.MaxTurns, as the extend-turns pre-grant sets

	type result struct {
		output    string
		completed bool
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		output, completed, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, opts)
		ch <- result{output, completed, err}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("InvokeClaude: %v (expected the scaled 1600ms deadline to cover the 1.2s sleep)", res.err)
		}
		if !res.completed {
			t.Errorf("expected completed=true; output=%q", res.output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("InvokeClaude did not return within 10s")
	}
}

// TestInvokeClaude_ExtendTurnsStillKilledAtScaledDeadline verifies #1206's "proportionate,
// not unlimited" guardrail: an extend-turns invocation is still killed once it runs past
// its *scaled* deadline, not just the unscaled one.
func TestInvokeClaude_ExtendTurnsStillKilledAtScaledDeadline(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	// Never completes — sleeps well past the scaled 1200ms deadline.
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"sleep 60\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	origDelay := claudeWaitDelay
	claudeWaitDelay = 1 * time.Second
	defer func() { claudeWaitDelay = origDelay }()

	// killProcGroupGraceful sleeps out the full grace window before re-probing
	// liveness, regardless of how quickly the signalled process actually exits —
	// so the default 10s/10s production grace windows would swamp the timing
	// assertions below. Shrink them to isolate the max_wall_time deadline itself.
	origSigInt := claudeKillGraceSigInt
	origSigTerm := claudeKillGraceSigTerm
	claudeKillGraceSigInt = 100 * time.Millisecond
	claudeKillGraceSigTerm = 100 * time.Millisecond
	defer func() {
		claudeKillGraceSigInt = origSigInt
		claudeKillGraceSigTerm = origSigTerm
	}()

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:        "Implement",
		Prompt:      "Do implement",
		MaxTurns:    100,
		MaxWallTime: 800 * time.Millisecond,
	}
	issue := gh.ProjectItem{Number: 99, Title: "ExtendTurnsStillKilledAtScaledDeadline"}
	opts := InvokeOptions{MaxTurnsOverride: 200} // 2x stage.MaxTurns -> scaled deadline ~1600ms

	start := time.Now()
	type result struct {
		output    string
		completed bool
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		output, completed, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, opts)
		ch <- result{output, completed, err}
	}()

	select {
	case res := <-ch:
		elapsed := time.Since(start)
		if res.completed {
			t.Errorf("expected completed=false (no FABRIK_STAGE_COMPLETE); output=%q", res.output)
		}
		// Must run past the unscaled 800ms deadline (proves scaling applied), but must
		// still be killed well short of the 60s sleep (proves the deadline is a real,
		// proportionate bound, not unlimited).
		if elapsed < 900*time.Millisecond {
			t.Errorf("killed too early (elapsed=%v) — scaled deadline (~1600ms) was not honored", elapsed)
		}
		if elapsed > 6*time.Second {
			t.Errorf("killed too late (elapsed=%v) — deadline scaling may be unbounded", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("InvokeClaude did not return within 15s after max_wall_time kill")
	}
}

// TestInvokeClaude_NoExtensionStillKilledAtUnscaledDeadline verifies that an invocation
// whose MaxTurnsOverride equals the stage's base budget (i.e. no extension in effect,
// as with a genuine runaway carrying no fabrik:extend-turns label) is killed at the
// unscaled deadline — scaling must not accidentally widen the ordinary-case bound.
func TestInvokeClaude_NoExtensionStillKilledAtUnscaledDeadline(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"sleep 60\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	origDelay := claudeWaitDelay
	claudeWaitDelay = 1 * time.Second
	defer func() { claudeWaitDelay = origDelay }()

	origSigInt := claudeKillGraceSigInt
	origSigTerm := claudeKillGraceSigTerm
	claudeKillGraceSigInt = 100 * time.Millisecond
	claudeKillGraceSigTerm = 100 * time.Millisecond
	defer func() {
		claudeKillGraceSigInt = origSigInt
		claudeKillGraceSigTerm = origSigTerm
	}()

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:        "Implement",
		Prompt:      "Do implement",
		MaxTurns:    100,
		MaxWallTime: 500 * time.Millisecond,
	}
	issue := gh.ProjectItem{Number: 99, Title: "NoExtensionStillKilledAtUnscaledDeadline"}
	opts := InvokeOptions{MaxTurnsOverride: 100} // == stage.MaxTurns: no extension in effect

	start := time.Now()
	type result struct {
		output    string
		completed bool
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		output, completed, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, opts)
		ch <- result{output, completed, err}
	}()

	select {
	case res := <-ch:
		elapsed := time.Since(start)
		if res.completed {
			t.Errorf("expected completed=false (no FABRIK_STAGE_COMPLETE); output=%q", res.output)
		}
		if elapsed > 3*time.Second {
			t.Errorf("killed too late (elapsed=%v) — expected the unscaled ~500ms deadline, not a scaled one", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("InvokeClaude did not return within 15s after max_wall_time kill")
	}
}

// TestInvokeClaudeForComments_ExtendTurnsScalesWallTime is the comment-processing-path
// sibling of TestInvokeClaude_ExtendTurnsScalesWallTime — engine/comments.go's
// runCommentExtensionLoop pre-grants the identical 2x turn budget on its first
// invocation, via the same InvokeOptions.MaxTurnsOverride -> InvokeClaudeForComments
// -> runClaude path, so it must scale max_wall_time the same way.
func TestInvokeClaudeForComments_ExtendTurnsScalesWallTime(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	// Sleeps past the unscaled 800ms deadline but well within the scaled 1600ms one —
	// see the identical margin rationale in TestInvokeClaude_ExtendTurnsScalesWallTime.
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"sleep 1.2\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"comment done\",\"session_id\":\"sess_cmt_scaled\",\"num_turns\":90,\"total_cost_usd\":0.001,\"is_error\":false}'\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	origDelay := claudeWaitDelay
	claudeWaitDelay = 1 * time.Second
	defer func() { claudeWaitDelay = origDelay }()

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:        "Implement",
		Prompt:      "Do implement",
		MaxTurns:    50,
		MaxWallTime: 800 * time.Millisecond,
	}
	issue := gh.ProjectItem{Number: 99, Title: "CommentsExtendTurnsScalesWallTime"}
	comments := []gh.Comment{{Author: "user", Body: "please continue", CreatedAt: time.Now()}}
	opts := InvokeOptions{MaxTurnsOverride: 100} // 2x commentMaxTurns(stage) (== stage.MaxTurns here)

	type result struct {
		output    string
		completed bool
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		output, completed, _, err := InvokeClaudeForComments(context.Background(), stage, issue, comments, workDir, opts)
		ch <- result{output, completed, err}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("InvokeClaudeForComments: %v (expected the scaled 1600ms deadline to cover the 1.2s sleep)", res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("InvokeClaudeForComments did not return within 10s")
	}
}

// TestInvokeClaudeForComments_ExtendTurnsStillKilledAtScaledDeadline is the
// comment-processing-path sibling of TestInvokeClaude_ExtendTurnsStillKilledAtScaledDeadline.
func TestInvokeClaudeForComments_ExtendTurnsStillKilledAtScaledDeadline(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"sleep 60\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	origDelay := claudeWaitDelay
	claudeWaitDelay = 1 * time.Second
	defer func() { claudeWaitDelay = origDelay }()

	origSigInt := claudeKillGraceSigInt
	origSigTerm := claudeKillGraceSigTerm
	claudeKillGraceSigInt = 100 * time.Millisecond
	claudeKillGraceSigTerm = 100 * time.Millisecond
	defer func() {
		claudeKillGraceSigInt = origSigInt
		claudeKillGraceSigTerm = origSigTerm
	}()

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:        "Implement",
		Prompt:      "Do implement",
		MaxTurns:    50,
		MaxWallTime: 800 * time.Millisecond,
	}
	issue := gh.ProjectItem{Number: 99, Title: "CommentsExtendTurnsStillKilledAtScaledDeadline"}
	comments := []gh.Comment{{Author: "user", Body: "please continue", CreatedAt: time.Now()}}
	opts := InvokeOptions{MaxTurnsOverride: 100} // 2x commentMaxTurns(stage) -> scaled deadline ~1600ms

	start := time.Now()
	type result struct {
		completed bool
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		_, completed, _, err := InvokeClaudeForComments(context.Background(), stage, issue, comments, workDir, opts)
		ch <- result{completed, err}
	}()

	select {
	case res := <-ch:
		elapsed := time.Since(start)
		if res.completed {
			t.Errorf("expected completed=false (no FABRIK_STAGE_COMPLETE)")
		}
		if elapsed < 900*time.Millisecond {
			t.Errorf("killed too early (elapsed=%v) — scaled deadline (~1600ms) was not honored", elapsed)
		}
		if elapsed > 6*time.Second {
			t.Errorf("killed too late (elapsed=%v) — deadline scaling may be unbounded", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("InvokeClaudeForComments did not return within 15s after max_wall_time kill")
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

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

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

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

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

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

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
