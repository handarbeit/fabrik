package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
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

	// Always write the issue body.
	if err := os.WriteFile(filepath.Join(fabrikDir, "issue.md"), []byte(item.Body), 0644); err != nil {
		e.logf(item.Number, "warn", "could not write .fabrik/issue.md: %v\n", err)
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

	// Write PR description for post_to_pr stage invocations.
	if !isCommentProcessing && currentStage.PostToPR {
		prNumber, err := e.client.FindPRForIssue(e.cfg.Owner, e.cfg.Repo, item.Number)
		if err != nil || prNumber == 0 {
			return
		}
		prBody, err := e.client.GetIssueBody(e.cfg.Owner, e.cfg.Repo, prNumber)
		if err != nil {
			e.logf(item.Number, "warn", "could not fetch PR body for context file: %v\n", err)
			return
		}
		if err := os.WriteFile(filepath.Join(fabrikDir, "pr-description.md"), []byte(prBody), 0644); err != nil {
			e.logf(item.Number, "warn", "could not write .fabrik/pr-description.md: %v\n", err)
		}
	}
}
