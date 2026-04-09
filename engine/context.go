// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// findStageComment returns the most recent comment in item.Comments whose body
// starts with the Fabrik stage header for the given stage name. Returns nil if
// no match is found.
//
// NOTE: The header prefix matched here must stay in sync with formatOutputComment
// in engine/pr.go. If that format ever changes, update this function too.
func findStageComment(comments []gh.Comment, stageName string) *gh.Comment {
	prefix := fmt.Sprintf("🏭 **Fabrik — stage: %s**", stageName)
	var found *gh.Comment
	for i := range comments {
		if strings.HasPrefix(comments[i].Body, prefix) {
			found = &comments[i]
		}
	}
	return found
}

// parseMainSHA extracts the main: SHA from a stage comment's metadata line.
// The metadata line is the second line of the comment, formatted as:
//
//	*branch: {branch} | commit: {commit} | main: {sha} | {timestamp}*
//
// Returns empty string if the comment has no metadata line or no main: field
// (e.g., comments written before this feature was added).
func parseMainSHA(commentBody string) string {
	lines := strings.SplitN(commentBody, "\n", 3)
	if len(lines) < 2 {
		return ""
	}
	metaLine := lines[1]
	for _, segment := range strings.Split(metaLine, "|") {
		segment = strings.TrimSpace(segment)
		if strings.HasPrefix(segment, "main:") {
			sha := strings.TrimSpace(strings.TrimPrefix(segment, "main:"))
			// Strip trailing '*' from the last field edge case
			sha = strings.TrimSuffix(sha, "*")
			return sha
		}
	}
	return ""
}

// writeContextFiles writes context files to .fabrik/ in the worktree so Claude
// can read them on demand without relying solely on inline prompt content.
//
// Files written:
//   - .fabrik/issue.md — the issue body (always)
//   - .fabrik/stage-{Name}.md — body of the most recent Fabrik comment for each stage
//   - .fabrik/pr-description.md — linked PR body, for post_to_pr stage invocations only
//
// For stage invocations (isCommentProcessing=false): writes context for stages
// with Order strictly less than currentStage.Order (prior stages only).
// For comment processing (isCommentProcessing=true): writes context for all stages
// including the current one.
//
// Errors are non-fatal — Claude can still run without the files; log and continue.
func (e *Engine) writeContextFiles(item gh.ProjectItem, currentStage *stages.Stage, workDir string, isCommentProcessing bool) {
	// Use .fabrik-context/ instead of .fabrik/ to avoid colliding with
	// the tracked .fabrik/ directory (which contains stages/ and plugin/).
	fabrikDir := filepath.Join(workDir, ".fabrik-context")
	if err := os.MkdirAll(fabrikDir, 0755); err != nil {
		e.logf(item.Number, "warn", "could not create .fabrik-context dir: %v\n", err)
		return
	}

	// Write a .gitignore that excludes everything in this directory.
	// This prevents untracked context files from being staged by git add -A
	// (whether run by Fabrik or by Claude). Must be written before any other
	// files so it is in place even if a subsequent write fails early.
	if err := os.WriteFile(filepath.Join(fabrikDir, ".gitignore"), []byte("*\n"), 0644); err != nil {
		e.logf(item.Number, "warn", "could not write .fabrik-context/.gitignore: %v\n", err)
	}

	// Always write the issue body.
	if err := os.WriteFile(filepath.Join(fabrikDir, "issue.md"), []byte(item.Body), 0644); err != nil {
		e.logf(item.Number, "warn", "could not write .fabrik/issue.md: %v\n", err)
	}

	// Always write project identity so Claude can form gh CLI commands.
	// The Plan stage uses this to run `gh issue create`, `gh project item-add`, etc.
	projectMD := fmt.Sprintf("# Project Context\n\nOwner: %s\nRepo: %s\nProjectNum: %d\nOwnerType: %s\n",
		e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType)
	if err := os.WriteFile(filepath.Join(fabrikDir, "project.md"), []byte(projectMD), 0644); err != nil {
		e.logf(item.Number, "warn", "could not write .fabrik-context/project.md: %v\n", err)
	}

	// Write context files for each stage that has a comment.
	for _, stage := range e.cfg.Stages {
		// For stage invocations, only include prior stages (Order < current).
		// For comment processing, include all stages including current.
		if !isCommentProcessing && stage.Order >= currentStage.Order {
			continue
		}
		comment := findStageComment(item.Comments, stage.Name)
		if comment == nil {
			continue
		}
		// Sanitize stage name: reject names with path separators to prevent
		// context files from being written outside .fabrik/.
		safeName := stage.Name
		if strings.ContainsAny(safeName, "/\\") {
			e.logf(item.Number, "warn", "stage name %q contains path separator, skipping context file\n", safeName)
			continue
		}
		filename := fmt.Sprintf("stage-%s.md", safeName)
		if err := os.WriteFile(filepath.Join(fabrikDir, filename), []byte(comment.Body), 0644); err != nil {
			e.logf(item.Number, "warn", "could not write .fabrik/%s: %v\n", filename, err)
		}
	}

	// Generate codebase-changes.md for stage transitions (not comment processing).
	if !isCommentProcessing {
		e.writeCodebaseChanges(item, currentStage, workDir, fabrikDir)
	}

	// Write PR description for post_to_pr stage invocations.
	if !isCommentProcessing && currentStage.PostToPR {
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		prNumber, err := e.client.FindPRForIssue(owner, repo, item.Number)
		if err != nil || prNumber == 0 {
			return
		}
		prBody, err := e.client.GetIssueBody(owner, repo, prNumber)
		if err != nil {
			e.logf(item.Number, "warn", "could not fetch PR body for context file: %v\n", err)
			return
		}
		if err := os.WriteFile(filepath.Join(fabrikDir, "pr-description.md"), []byte(prBody), 0644); err != nil {
			e.logf(item.Number, "warn", "could not write .fabrik/pr-description.md: %v\n", err)
		}
	}
}

// writeCodebaseChanges generates .fabrik-context/codebase-changes.md showing
// what changed on origin/main since the most recent prior stage that recorded
// a main: SHA. Skips gracefully when no prior SHA exists or SHAs match.
func (e *Engine) writeCodebaseChanges(item gh.ProjectItem, currentStage *stages.Stage, workDir, fabrikDir string) {
	const maxFiles = 100

	// Find the most recent completed prior stage's main SHA.
	var priorSHA, priorStageName string
	for i := len(e.cfg.Stages) - 1; i >= 0; i-- {
		s := e.cfg.Stages[i]
		if s.Order >= currentStage.Order {
			continue
		}
		comment := findStageComment(item.Comments, s.Name)
		if comment == nil {
			continue
		}
		sha := parseMainSHA(comment.Body)
		if sha != "" {
			priorSHA = sha
			priorStageName = s.Name
			break
		}
	}

	if priorSHA == "" {
		return // no prior SHA — first stage or pre-feature comments
	}

	// Resolve current origin/{baseBranch} HEAD.
	wm := e.worktreesFor(item.Repo)
	baseBranch := wm.DefaultBaseBranch()
	currentSHA, err := gitRevParse(workDir, "origin/"+baseBranch)
	if err != nil {
		e.logf(item.Number, "warn", "could not resolve origin/%s for codebase changes: %v\n", baseBranch, err)
		return
	}
	// Use short SHA for display.
	currentShort := currentSHA
	if len(currentShort) > 8 {
		currentShort = currentShort[:8]
	}

	if strings.HasPrefix(currentSHA, priorSHA) || strings.HasPrefix(priorSHA, currentSHA) {
		return // SHAs match, no changes
	}

	// Count commits.
	countCmd := exec.Command("git", "rev-list", "--count", priorSHA+"..."+currentSHA)
	countCmd.Dir = workDir
	countOut, err := countCmd.Output()
	if err != nil {
		e.logf(item.Number, "warn", "could not count commits for codebase changes: %v\n", err)
		return
	}
	commitCount := strings.TrimSpace(string(countOut))

	// Get file-level changes.
	diffCmd := exec.Command("git", "diff", "--name-status", priorSHA+"..."+currentSHA)
	diffCmd.Dir = workDir
	diffOut, err := diffCmd.Output()
	if err != nil {
		e.logf(item.Number, "warn", "could not generate diff for codebase changes: %v\n", err)
		return
	}

	diffLines := strings.Split(strings.TrimSpace(string(diffOut)), "\n")
	if len(diffLines) == 1 && diffLines[0] == "" {
		return // no file changes
	}

	// Abbreviate SHAs for display in the markdown file.
	priorShort := priorSHA
	if len(priorShort) > 8 {
		priorShort = priorShort[:8]
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("# Codebase Changes Since %s\n\n", priorStageName))
	b.WriteString(fmt.Sprintf("Changes on `origin/%s` since the %s stage (%s → %s): **%s commit(s)**\n\n",
		baseBranch, priorStageName, priorShort, currentShort, commitCount))
	b.WriteString("| Status | File |\n")
	b.WriteString("|--------|------|\n")

	// Parse valid file entries first, so truncation count is accurate.
	type fileEntry struct {
		status string
		file   string
	}
	var entries []fileEntry
	for _, line := range diffLines {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		status := changeType(parts[0])
		// Renames/copies have 3 fields: R100\told\tnew — show as "old → new".
		var file string
		if len(parts) >= 3 {
			file = parts[1] + " → " + parts[2]
		} else {
			file = parts[1]
		}
		entries = append(entries, fileEntry{status, file})
	}

	for i, entry := range entries {
		if i >= maxFiles {
			b.WriteString(fmt.Sprintf("\n(and %d more files)\n", len(entries)-maxFiles))
			break
		}
		b.WriteString(fmt.Sprintf("| %s | `%s` |\n", entry.status, entry.file))
	}

	outPath := filepath.Join(fabrikDir, "codebase-changes.md")
	if err := os.WriteFile(outPath, []byte(b.String()), 0644); err != nil {
		e.logf(item.Number, "warn", "could not write codebase-changes.md: %v\n", err)
	}
}

// changeType converts git diff --name-status codes to human-readable labels.
func changeType(code string) string {
	switch {
	case strings.HasPrefix(code, "R"):
		return "Renamed"
	case strings.HasPrefix(code, "C"):
		return "Copied"
	case code == "M":
		return "Modified"
	case code == "A":
		return "New"
	case code == "D":
		return "Deleted"
	default:
		return code
	}
}
