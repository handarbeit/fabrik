package engine

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// TestInterpretClaudeResult_ToolsDenied_ProgressWording covers #1743's
// R7-R9/AC10-AC11: the log line interpretClaudeResult emits for a
// tools-denied exit must never assert "did not make progress" (a claim it
// never measured), and must upgrade to a measured commit count whenever the
// caller (runClaude) supplied one, degrading cleanly to completion-only
// wording when it didn't.
func TestInterpretClaudeResult_ToolsDenied_ProgressWording(t *testing.T) {
	raw := []byte(`{"result":"denied","is_error":false,"subtype":"success","terminal_reason":"completed","permission_denials":[{"tool_name":"Bash","tool_input":{"command":"git status"}}]}`)

	tests := []struct {
		name           string
		commitsPushed  int
		wantSubstrings []string
		wantAbsent     []string
	}{
		{
			name:          "not measured (-1) degrades to completion-only wording",
			commitsPushed: -1,
			wantSubstrings: []string{
				"stage did not signal completion",
			},
			wantAbsent: []string{"commit(s) pushed", "0 commit(s)"},
		},
		{
			name:          "zero commits degrades to completion-only wording, never 0 commit(s)",
			commitsPushed: 0,
			wantSubstrings: []string{
				"stage did not signal completion",
			},
			wantAbsent: []string{"commit(s) pushed", "0 commit(s)"},
		},
		{
			name:          "measured commit count is reported",
			commitsPushed: 3,
			wantSubstrings: []string{
				"3 commit(s) pushed, stage did not signal completion",
			},
			wantAbsent: []string{"0 commit(s)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logLines []string
			origLogf := claudeLogf
			claudeLogf = func(issueNumber int, tag, format string, args ...any) {
				logLines = append(logLines, fmt.Sprintf(format, args...))
			}
			defer func() { claudeLogf = origLogf }()

			_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, nil, false, t.TempDir()+"/sess", t.TempDir(), "", 2, tt.commitsPushed)
			if err == nil {
				t.Fatalf("expected a tools-denied error, got nil")
			}
			if completed {
				t.Errorf("expected completed=false")
			}

			joined := strings.Join(logLines, "\n")
			// AC10: this exact phrase must never appear, in any variant.
			if strings.Contains(joined, "did not make progress") {
				t.Errorf("log output must never assert \"did not make progress\", got: %s", joined)
			}
			for _, want := range tt.wantSubstrings {
				if !strings.Contains(joined, want) {
					t.Errorf("expected log output to contain %q, got: %s", want, joined)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(joined, absent) {
					t.Errorf("expected log output NOT to contain %q, got: %s", absent, joined)
				}
			}
		})
	}
}

// TestGitCommitCountBetween covers #1743/R8's measurement primitive directly
// against a real git repo: a genuine range with N commits reports N: an
// empty range (before == after, or no path between them the way an amended
// history could produce) reports 0, never an error; and an invalid ref
// reports an error rather than a fabricated count.
func TestGitCommitCountBetween(t *testing.T) {
	skipIfNoGit(t)
	dir := initBareRepo(t)

	before, err := gitHeadSHA(dir)
	if err != nil {
		t.Fatalf("gitHeadSHA (before): %v", err)
	}

	for i := 0; i < 2; i++ {
		cmd := exec.Command("git", "commit", "--allow-empty", "-m", "extra commit")
		cmd.Dir = dir
		if out, cerr := cmd.CombinedOutput(); cerr != nil {
			t.Fatalf("git commit: %s: %v", out, cerr)
		}
	}

	after, err := gitHeadSHA(dir)
	if err != nil {
		t.Fatalf("gitHeadSHA (after): %v", err)
	}

	n, err := gitCommitCountBetween(dir, before, after)
	if err != nil {
		t.Fatalf("gitCommitCountBetween: %v", err)
	}
	if n != 2 {
		t.Errorf("gitCommitCountBetween = %d, want 2", n)
	}

	// Same SHA on both ends: an empty range, not an error.
	n0, err := gitCommitCountBetween(dir, after, after)
	if err != nil {
		t.Fatalf("gitCommitCountBetween (empty range): %v", err)
	}
	if n0 != 0 {
		t.Errorf("gitCommitCountBetween (empty range) = %d, want 0", n0)
	}

	// An invalid ref must return an error, not a fabricated count.
	if _, err := gitCommitCountBetween(dir, "not-a-real-sha", after); err == nil {
		t.Error("expected an error for an invalid before ref, got nil")
	}
}
