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
