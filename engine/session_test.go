package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveSessionIDDirect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.session")

	saveSessionIDDirect(path, "sess_abc123")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading session file: %v", err)
	}
	if string(data) != "sess_abc123" {
		t.Errorf("session ID = %q, want sess_abc123", string(data))
	}
}

func TestSaveSessionIDDirect_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.session")

	saveSessionIDDirect(path, "")

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("session file should not exist for empty session ID")
	}
}

func TestSessionDir(t *testing.T) {
	dir := SessionDir(42)
	if !strings.Contains(dir, "issue-42") {
		t.Errorf("SessionDir(42) = %q, expected to contain issue-42", dir)
	}
	if !strings.Contains(dir, ".fabrik/sessions") {
		t.Errorf("SessionDir(42) = %q, expected to contain .fabrik/sessions", dir)
	}
}

func TestLogDir(t *testing.T) {
	dir := LogDir(42)
	if !strings.Contains(dir, "issue-42") {
		t.Errorf("LogDir(42) = %q, expected to contain issue-42", dir)
	}
	if !strings.Contains(dir, ".fabrik/logs") {
		t.Errorf("LogDir(42) = %q, expected to contain .fabrik/logs", dir)
	}
}

func TestFormatStatsFooter(t *testing.T) {
	tests := []struct {
		name      string
		stats     TokenUsage
		completed bool
		wantEmpty bool
		wantSubs  []string
	}{
		{
			name:      "zero stats returns empty",
			stats:     TokenUsage{},
			completed: true,
			wantEmpty: true,
		},
		{
			name:      "with turns and tokens, completed",
			stats:     TokenUsage{TurnsUsed: 15, MaxTurns: 30, InputTokens: 47000, OutputTokens: 8000},
			completed: true,
			wantSubs:  []string{"15/30 turns", "47k input", "8k output"},
		},
		{
			name:      "with turns and tokens, incomplete",
			stats:     TokenUsage{TurnsUsed: 30, MaxTurns: 30, InputTokens: 47000, OutputTokens: 8000},
			completed: false,
			wantSubs:  []string{"30/30 turns", "Stage incomplete."},
		},
		{
			name:      "no max turns",
			stats:     TokenUsage{TurnsUsed: 10, InputTokens: 5000, OutputTokens: 1000},
			completed: true,
			wantSubs:  []string{"10 turns", "5k input", "1k output"},
		},
		{
			name:      "only input tokens",
			stats:     TokenUsage{InputTokens: 5000},
			completed: true,
			wantEmpty: false,
			wantSubs:  []string{"5k input"},
		},
		{
			name:      "only output tokens",
			stats:     TokenUsage{OutputTokens: 2000},
			completed: true,
			wantEmpty: false,
			wantSubs:  []string{"2k output"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatStatsFooter(tt.stats, tt.completed)
			if tt.wantEmpty {
				if got != "" {
					t.Errorf("expected empty footer, got %q", got)
				}
				return
			}
			for _, sub := range tt.wantSubs {
				if !strings.Contains(got, sub) {
					t.Errorf("footer %q missing %q", got, sub)
				}
			}
		})
	}
}

func TestSessionFile(t *testing.T) {
	path := sessionFile(42, "Research")
	if !strings.HasSuffix(path, "Research.session") {
		t.Errorf("sessionFile = %q, expected to end with Research.session", path)
	}
}

