package engine

import (
	"strings"
	"testing"
)

// TestSanitizeToolsDeniedCommand covers #1775's R2/AC3: a denied command
// rendered into a GitHub comment or a single-line log message must never
// carry a raw newline (would break a one-line sentence) or a raw backtick
// (would prematurely close an inline code span), and must be truncated
// rather than allowed to balloon the comment.
func TestSanitizeToolsDeniedCommand(t *testing.T) {
	t.Run("empty command returns empty", func(t *testing.T) {
		if got := sanitizeToolsDeniedCommand(""); got != "" {
			t.Errorf("sanitizeToolsDeniedCommand(\"\") = %q, want \"\"", got)
		}
	})

	t.Run("short command passes through unchanged", func(t *testing.T) {
		if got, want := sanitizeToolsDeniedCommand("git status"), "git status"; got != want {
			t.Errorf("sanitizeToolsDeniedCommand = %q, want %q", got, want)
		}
	})

	t.Run("embedded newlines are collapsed, not left raw", func(t *testing.T) {
		got := sanitizeToolsDeniedCommand("echo one\necho two\r\necho three")
		if strings.Contains(got, "\n") || strings.Contains(got, "\r") {
			t.Errorf("expected no raw newline in output, got: %q", got)
		}
		if !strings.Contains(got, "echo one") || !strings.Contains(got, "echo three") {
			t.Errorf("expected surrounding content preserved, got: %q", got)
		}
	})

	t.Run("embedded backticks are replaced, not left raw", func(t *testing.T) {
		got := sanitizeToolsDeniedCommand("echo `whoami`")
		if strings.Contains(got, "`") {
			t.Errorf("expected no raw backtick in output, got: %q", got)
		}
	})

	t.Run("long command is truncated", func(t *testing.T) {
		long := strings.Repeat("a", 2000)
		got := sanitizeToolsDeniedCommand(long)
		if len(got) >= len(long) {
			t.Errorf("expected truncation, got length %d (input length %d)", len(got), len(long))
		}
		if strings.Contains(got, "\n") {
			t.Errorf("truncated output must remain single-line, got: %q", got)
		}
	})

	t.Run("pathological command (long, newlines, backticks) is safe", func(t *testing.T) {
		pathological := strings.Repeat("x`\ny`\r\n", 500)
		got := sanitizeToolsDeniedCommand(pathological)
		if strings.ContainsAny(got, "`\n\r") {
			t.Errorf("expected no backtick/newline survivors, got: %q", got)
		}
		if len(got) > toolsDeniedCommandMaxLen+64 {
			// Generous slack for the omission marker's own text; the point is
			// that it can't grow unboundedly with the input.
			t.Errorf("expected bounded output length, got length %d", len(got))
		}
	})
}

// TestFirstToolsDeniedCommand covers #1775's R2/AC2: the first denial
// carrying a usable command wins; a nil/empty denials list, or a list whose
// every entry has an empty command, degrades to ok=false rather than
// fabricating a command.
func TestFirstToolsDeniedCommand(t *testing.T) {
	t.Run("nil denials degrades to not-ok", func(t *testing.T) {
		_, _, ok := firstToolsDeniedCommand(nil)
		if ok {
			t.Error("expected ok=false for nil denials")
		}
	})

	t.Run("all-empty commands degrades to not-ok", func(t *testing.T) {
		_, _, ok := firstToolsDeniedCommand([]toolDenial{
			{ToolName: "Write", Command: ""},
			{ToolName: "Edit", Command: ""},
		})
		if ok {
			t.Error("expected ok=false when no denial carries a command")
		}
	})

	t.Run("first denial with a command wins", func(t *testing.T) {
		toolName, command, ok := firstToolsDeniedCommand([]toolDenial{
			{ToolName: "Write", Command: ""},
			{ToolName: "Bash", Command: "git status"},
			{ToolName: "Bash", Command: "gh pr view"},
		})
		if !ok {
			t.Fatal("expected ok=true")
		}
		if toolName != "Bash" {
			t.Errorf("toolName = %q, want Bash", toolName)
		}
		if command != "git status" {
			t.Errorf("command = %q, want %q", command, "git status")
		}
	})
}
