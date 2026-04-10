package engine

import (
	"os/exec"
	"strings"
	"testing"
)

func TestFormatOutputComment(t *testing.T) {
	comment := formatOutputComment("Research", "Hello world", "", "main", "abc12345", "", "2026-04-01 14:30 UTC")
	if !strings.Contains(comment, "🏭 **Fabrik — stage: Research**") {
		t.Errorf("comment = %q", comment)
	}
	if !strings.Contains(comment, "Hello world") {
		t.Error("comment missing output")
	}
	if !strings.Contains(comment, "*branch: main | commit: abc12345 | 2026-04-01 14:30 UTC*") {
		t.Errorf("comment missing metadata line: %q", comment)
	}
}

func TestFormatOutputComment_WithMainSHA(t *testing.T) {
	comment := formatOutputComment("Research", "Hello", "", "main", "abc12345", "def56789", "2026-04-01 14:30 UTC")
	expected := "*branch: main | commit: abc12345 | main: def56789 | 2026-04-01 14:30 UTC*"
	if !strings.Contains(comment, expected) {
		t.Errorf("comment missing main SHA metadata line.\ngot: %q\nwant substring: %q", comment, expected)
	}
}

func TestFormatOutputComment_Truncation(t *testing.T) {
	longOutput := strings.Repeat("x", 70000)
	comment := formatOutputComment("Test", longOutput, "", "main", "abc12345", "", "2026-04-01 14:30 UTC")
	if len(comment) > 61000 {
		t.Errorf("comment should be truncated, len = %d", len(comment))
	}
	if !strings.Contains(comment, "... (truncated)") {
		t.Error("truncated comment missing truncation notice")
	}
}

func TestFormatOutputComment_FooterSurvivesTruncation(t *testing.T) {
	// Output exceeds the limit; footer must appear after the truncation notice, not be cut off.
	longOutput := strings.Repeat("x", 65000)
	footer := "\n\n---\nUsed 30/30 turns, 47k input / 8k output tokens. Stage incomplete."
	comment := formatOutputComment("Test", longOutput, footer, "main", "abc12345", "", "2026-04-01 14:30 UTC")
	if !strings.Contains(comment, "... (truncated)") {
		t.Error("expected truncation notice")
	}
	if !strings.Contains(comment, "30/30 turns") {
		t.Error("stats footer must appear after truncation, not be cut off")
	}
}

func TestFormatPRSummaryComment(t *testing.T) {
	output := "FABRIK_SUMMARY_BEGIN\nDid some work.\nFABRIK_SUMMARY_END\n"
	comment := formatPRSummaryComment("Plan", 42, output, "fabrik/issue-5", "deadbeef", "", "2026-04-01 14:30 UTC")
	if !strings.Contains(comment, "🏭 **Fabrik — stage: Plan**") {
		t.Errorf("missing header: %q", comment)
	}
	if !strings.Contains(comment, "*branch: fabrik/issue-5 | commit: deadbeef | 2026-04-01 14:30 UTC*") {
		t.Errorf("missing metadata line: %q", comment)
	}
	if !strings.Contains(comment, "PR #42") {
		t.Errorf("missing PR reference: %q", comment)
	}
}

func TestFormatPRSummaryComment_WithMainSHA(t *testing.T) {
	output := "FABRIK_SUMMARY_BEGIN\nDid some work.\nFABRIK_SUMMARY_END\n"
	comment := formatPRSummaryComment("Plan", 42, output, "fabrik/issue-5", "deadbeef", "aaa11111", "2026-04-01 14:30 UTC")
	expected := "*branch: fabrik/issue-5 | commit: deadbeef | main: aaa11111 | 2026-04-01 14:30 UTC*"
	if !strings.Contains(comment, expected) {
		t.Errorf("missing main SHA metadata.\ngot: %q\nwant substring: %q", comment, expected)
	}
}

func TestCaptureGitMeta_EmptyWorkDir(t *testing.T) {
	branch, commit, mainSHA, timestamp := captureGitMeta("", "")
	if branch != "unknown" {
		t.Errorf("expected branch=unknown, got %q", branch)
	}
	if commit != "unknown" {
		t.Errorf("expected commit=unknown, got %q", commit)
	}
	if mainSHA != "" {
		t.Errorf("expected empty mainSHA, got %q", mainSHA)
	}
	if timestamp == "" {
		t.Error("expected non-empty timestamp")
	}
}

func TestCaptureGitMeta_ValidDir(t *testing.T) {
	// Use the current repo root — it definitely has commits and a branch
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skip("not in a git repo")
	}
	repoRoot := strings.TrimSpace(string(out))
	branch, commit, _, timestamp := captureGitMeta(repoRoot, "main")
	if branch == "unknown" {
		t.Errorf("expected real branch, got %q", branch)
	}
	if commit == "unknown" || len(commit) != 8 {
		t.Errorf("expected 8-char commit SHA, got %q", commit)
	}
	if timestamp == "" {
		t.Error("expected non-empty timestamp")
	}
}
