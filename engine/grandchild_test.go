//go:build !windows

package engine

import (
	"context"
	"os"
	"path/filepath"
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
