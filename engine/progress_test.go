package engine

import (
	"testing"
)

func TestCountCheckedTasks(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"empty", "", 0},
		{"no tasks", "Some text\nMore text", 0},
		{"unchecked only", "- [ ] Task 1\n- [ ] Task 2", 0},
		{"one checked", "- [x] Task 1\n- [ ] Task 2", 1},
		{"all checked", "- [x] Task 1\n- [x] Task 2", 2},
		{"uppercase X", "- [X] Task 1\n- [x] Task 2", 2},
		{"indented", "  - [x] Task 1\n    - [x] Task 2", 2},
		{"mixed content", "## Plan\n- [x] Done\n- [ ] Todo\nSome text\n- [x] Also done", 2},
		{"not a task", "[x] not a task\n-[x] no space", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countCheckedTasks(tt.body)
			if got != tt.want {
				t.Errorf("countCheckedTasks() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDetectProgress(t *testing.T) {
	tests := []struct {
		name              string
		before            progressBaseline
		worktreeHeadAfter string
		remoteHeadAfter   string
		checkedTasksAfter int
		wantProgress      bool
		wantNewCommits    int
		wantNewTasks      int
	}{
		{
			name:              "no change",
			before:            progressBaseline{worktreeHead: "abc123", remoteHead: "abc123", checkedTasks: 2},
			worktreeHeadAfter: "abc123",
			remoteHeadAfter:   "abc123",
			checkedTasksAfter: 2,
			wantProgress:      false,
		},
		{
			name:              "worktree HEAD changed",
			before:            progressBaseline{worktreeHead: "abc123", remoteHead: "abc123", checkedTasks: 0},
			worktreeHeadAfter: "def456",
			remoteHeadAfter:   "abc123",
			checkedTasksAfter: 0,
			wantProgress:      true,
			wantNewCommits:    1,
		},
		{
			name:              "remote HEAD changed",
			before:            progressBaseline{worktreeHead: "abc123", remoteHead: "abc123", checkedTasks: 0},
			worktreeHeadAfter: "abc123",
			remoteHeadAfter:   "def456",
			checkedTasksAfter: 0,
			wantProgress:      true,
			wantNewCommits:    1,
		},
		{
			name:              "new tasks checked",
			before:            progressBaseline{worktreeHead: "abc123", remoteHead: "abc123", checkedTasks: 1},
			worktreeHeadAfter: "abc123",
			remoteHeadAfter:   "abc123",
			checkedTasksAfter: 3,
			wantProgress:      true,
			wantNewTasks:      2,
		},
		{
			name:              "both commits and tasks",
			before:            progressBaseline{worktreeHead: "abc123", remoteHead: "abc123", checkedTasks: 0},
			worktreeHeadAfter: "def456",
			remoteHeadAfter:   "abc123",
			checkedTasksAfter: 1,
			wantProgress:      true,
			wantNewCommits:    1,
			wantNewTasks:      1,
		},
		{
			name:              "empty before SHAs",
			before:            progressBaseline{worktreeHead: "", remoteHead: "", checkedTasks: 0},
			worktreeHeadAfter: "abc123",
			remoteHeadAfter:   "abc123",
			checkedTasksAfter: 0,
			wantProgress:      false,
		},
		{
			name:              "tasks decreased (no progress)",
			before:            progressBaseline{worktreeHead: "abc123", remoteHead: "abc123", checkedTasks: 3},
			worktreeHeadAfter: "abc123",
			remoteHeadAfter:   "abc123",
			checkedTasksAfter: 2,
			wantProgress:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := detectProgress(tt.before, tt.worktreeHeadAfter, tt.remoteHeadAfter, tt.checkedTasksAfter)
			if result.hasProgress != tt.wantProgress {
				t.Errorf("hasProgress = %v, want %v", result.hasProgress, tt.wantProgress)
			}
			if result.newCommits != tt.wantNewCommits {
				t.Errorf("newCommits = %d, want %d", result.newCommits, tt.wantNewCommits)
			}
			if result.newTasks != tt.wantNewTasks {
				t.Errorf("newTasks = %d, want %d", result.newTasks, tt.wantNewTasks)
			}
			if result.hasProgress && result.detail == "" {
				t.Error("expected non-empty detail when progress detected")
			}
			if !result.hasProgress && result.detail != "" {
				t.Errorf("expected empty detail when no progress, got %q", result.detail)
			}
		})
	}
}

