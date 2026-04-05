package engine

import (
	"strconv"
	"strings"
)

// countCheckedTasks counts the number of checked Markdown task list items
// (lines matching "- [x]") in the given text.
func countCheckedTasks(body string) int {
	count := 0
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- [x]") || strings.HasPrefix(trimmed, "- [X]") {
			count++
		}
	}
	return count
}

// progressBaseline holds the state captured before Claude runs, used to detect
// progress after an incomplete stage attempt.
type progressBaseline struct {
	worktreeHead string // git rev-parse HEAD in the worktree
	remoteHead   string // git rev-parse origin/<branch> (empty if no remote ref)
	checkedTasks int    // count of "- [x]" items in PR body
	prNumber     int    // PR number used for task count (0 if no PR)
}

// progressResult holds the outcome of comparing before/after state.
type progressResult struct {
	hasProgress bool
	newCommits  int    // estimated new commits (0 or 1+ based on SHA change)
	newTasks    int    // newly checked tasks
	detail      string // human-readable summary for comments
}

// detectProgress compares a baseline with the current state to determine
// whether meaningful progress was made during the session.
func detectProgress(before progressBaseline, worktreeHeadAfter, remoteHeadAfter string, checkedTasksAfter int) progressResult {
	result := progressResult{}

	// Check for commit-based progress: either worktree HEAD or remote HEAD changed
	commitProgress := false
	if before.worktreeHead != "" && worktreeHeadAfter != "" && before.worktreeHead != worktreeHeadAfter {
		commitProgress = true
		result.newCommits = 1 // at least 1; we can't cheaply count exact commits
	}
	if !commitProgress && before.remoteHead != "" && remoteHeadAfter != "" && before.remoteHead != remoteHeadAfter {
		commitProgress = true
		result.newCommits = 1
	}

	// Check for task-based progress: more checked items in PR body
	if checkedTasksAfter > before.checkedTasks {
		result.newTasks = checkedTasksAfter - before.checkedTasks
	}

	result.hasProgress = commitProgress || result.newTasks > 0

	// Build human-readable detail
	if result.hasProgress {
		parts := []string{}
		if commitProgress {
			parts = append(parts, "new commits detected")
		}
		if result.newTasks > 0 {
			if result.newTasks == 1 {
				parts = append(parts, "1 new task completed")
			} else {
				parts = append(parts, strconv.Itoa(result.newTasks)+" new tasks completed")
			}
		}
		result.detail = strings.Join(parts, ", ")
	}

	return result
}

