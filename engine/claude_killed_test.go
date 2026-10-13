package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/claudeerr"
	"github.com/handarbeit/fabrik/stages"
)

func TestWrapKilledError(t *testing.T) {
	base := errors.New("claude exited with error: signal: killed")
	cases := []struct {
		name                          string
		err                           error
		completed, timedOut, inactive bool
		wantCause                     string // "" = unwrapped
	}{
		{"wall-time", base, false, true, false, claudeerr.KillWallTime},
		{"inactivity", base, false, true, true, claudeerr.KillInactivity},
		{"not timed out", base, false, false, false, ""},
		{"completed before the kill", base, true, true, false, ""},
		{"no error", nil, false, true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapKilledError(tc.err, tc.completed, tc.timedOut, tc.inactive)
			var ke *claudeerr.KilledError
			if tc.wantCause == "" {
				if errors.As(got, &ke) {
					t.Fatalf("must not wrap, got %v", got)
				}
				if got != tc.err {
					t.Fatalf("err changed: %v", got)
				}
				return
			}
			if !errors.As(got, &ke) || ke.Cause != tc.wantCause {
				t.Fatalf("got %v, want KilledError cause %q", got, tc.wantCause)
			}
			if !errors.Is(got, base) || got.Error() != base.Error() {
				t.Fatalf("wrapper must be transparent: %v", got)
			}
		})
	}
}

// A wrapped kill still classifies exactly like the unwrapped error: a turn-limit
// error inside a KilledError is still a turn limit (retry accounting is keyed on
// errors.As, #2170).
func TestKilledError_PreservesTypedClassification(t *testing.T) {
	inner := &claudeTurnLimitError{TerminalReason: "error_max_turns", NumTurns: 9}
	err := wrapKilledError(inner, false, true, false)
	var tl *claudeTurnLimitError
	if !errors.As(err, &tl) || tl != inner {
		t.Fatal("turn-limit classification lost through the kill wrapper")
	}
}

// End to end through the real runClaude timeout path: a hung worker killed by
// max_wall_time returns a wall-time KilledError.
func TestInvokeClaude_WallTimeKillReturnsKilledError(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\nsleep 60\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	origDelay := claudeWaitDelay
	claudeWaitDelay = 1 * time.Second
	defer func() { claudeWaitDelay = origDelay }()

	stage := &stages.Stage{Name: "Review", Prompt: "Do review", MaxWallTime: 500 * time.Millisecond}
	issue := gh.ProjectItem{Number: 98, Title: "KilledError"}
	type result struct {
		completed bool
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		_, completed, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, t.TempDir(), InvokeOptions{})
		ch <- result{completed, err}
	}()
	select {
	case res := <-ch:
		var ke *claudeerr.KilledError
		if res.completed || !errors.As(res.err, &ke) || ke.Cause != claudeerr.KillWallTime {
			t.Fatalf("completed=%v err=%v, want a wall-time-kill KilledError", res.completed, res.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("InvokeClaude did not return within 15s")
	}
}
