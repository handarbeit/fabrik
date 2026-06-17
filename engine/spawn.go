package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// SpawnBlock represents one child issue declared in a Plan's FABRIK_SPAWN_CHILD_BEGIN/END block.
type SpawnBlock struct {
	Repo  string // "owner/repo"
	Title string
	Body  string
}

// ParseSpawnBlocks scans body for all FABRIK_SPAWN_CHILD_BEGIN/END pairs and
// returns the parsed spawn blocks in order. Malformed or incomplete pairs are
// silently skipped. The BEGIN marker must be followed by the target repo on
// the same line: "FABRIK_SPAWN_CHILD_BEGIN owner/repo". The first non-empty
// line after BEGIN is the TITLE: line; the body starts after the blank line
// following the title.
func ParseSpawnBlocks(body string) []SpawnBlock {
	const beginPrefix = "FABRIK_SPAWN_CHILD_BEGIN"
	const endMarker = "FABRIK_SPAWN_CHILD_END"

	var blocks []SpawnBlock
	remaining := body
	for {
		beginIdx := strings.Index(remaining, beginPrefix)
		if beginIdx == -1 {
			break
		}

		// Extract the rest of the BEGIN line to get the repo argument.
		lineEnd := strings.IndexByte(remaining[beginIdx:], '\n')
		var beginLine, afterBegin string
		if lineEnd == -1 {
			beginLine = remaining[beginIdx:]
			afterBegin = ""
		} else {
			beginLine = remaining[beginIdx : beginIdx+lineEnd]
			afterBegin = remaining[beginIdx+lineEnd+1:]
		}

		// BEGIN line must be exactly "FABRIK_SPAWN_CHILD_BEGIN owner/repo".
		// Strip trailing \r if present (CRLF files).
		beginLine = strings.TrimRight(beginLine, "\r")
		repo := ""
		if parts := strings.Fields(strings.TrimPrefix(beginLine, beginPrefix)); len(parts) > 0 {
			repo = parts[0]
		}

		if repo == "" || !strings.Contains(repo, "/") {
			// Malformed — advance past this BEGIN and keep scanning.
			remaining = remaining[beginIdx+len(beginPrefix):]
			continue
		}

		endIdx := strings.Index(afterBegin, endMarker)
		if endIdx == -1 {
			// No matching END — stop scanning.
			break
		}

		blockContent := afterBegin[:endIdx]

		// Advance remaining past this full block.
		remaining = afterBegin[endIdx+len(endMarker):]

		// Parse TITLE: from the first non-empty line.
		title, blockBody := parseTitleAndBody(blockContent)
		if title == "" {
			continue // malformed block
		}

		blocks = append(blocks, SpawnBlock{
			Repo:  repo,
			Title: title,
			Body:  blockBody,
		})
	}
	return blocks
}

// parseTitleAndBody extracts the title (from the "TITLE: ..." line) and the
// remaining body content from the inside of a FABRIK_SPAWN_CHILD_BEGIN/END block.
func parseTitleAndBody(content string) (title, body string) {
	lines := strings.Split(content, "\n")
	titleIdx := -1
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "TITLE:") {
			title = strings.TrimSpace(strings.TrimPrefix(trimmed, "TITLE:"))
			titleIdx = i
			break
		}
		// First non-empty line that isn't a TITLE: prefix — malformed.
		return "", ""
	}
	if title == "" || titleIdx == -1 {
		return "", ""
	}

	// Body is everything after the TITLE: line, trimmed.
	bodyLines := lines[titleIdx+1:]
	body = strings.TrimSpace(strings.Join(bodyLines, "\n"))
	return title, body
}

// childFooter returns the engine-appended back-reference footer for a spawned
// child issue per FR-011.
func childFooter(parentOwner, parentRepo string, parentNumber int) string {
	return fmt.Sprintf("\n---\n\n*Spawned by Fabrik from parent issue %s/%s#%d as a multi-issue decomposition. The parent's plan is at the link above.*",
		parentOwner, parentRepo, parentNumber)
}

// resolveSpecifyOptionID returns the project Status option ID for the "Specify"
// column, or the first non-Backlog, non-terminal column as a fallback. Returns
// "" when no suitable option exists or sf is nil (caller skips the status-set).
func resolveSpecifyOptionID(sf *gh.StatusField) string {
	if sf == nil {
		return ""
	}
	// Exact match on "Specify".
	if id, ok := sf.Options["Specify"]; ok {
		return id
	}
	// Fallback: first option that is not "Backlog" and not the last column.
	names := sf.OrderedOptionNames
	if len(names) < 2 {
		return ""
	}
	last := names[len(names)-1]
	for _, name := range names {
		if name == "Backlog" || name == last {
			continue
		}
		return sf.Options[name]
	}
	return ""
}

// preImplement runs the pre-Implement step for stage "Implement". It parses
// the parent's Plan comment for FABRIK_SPAWN_CHILD_BEGIN/END blocks and, when
// found, creates the child issues on GitHub, adds them to the project board,
// links them as blockedBy dependencies of the parent, and marks the parent
// with fabrik:children-spawned.
//
// Returns (true, nil) when children were spawned — the Implement Claude
// invocation must be skipped in this case; checkDependencies will block the
// parent on its next evaluation cycle.
// Returns (false, nil) when there is nothing to do (no blocks, or already spawned).
// Returns (false, err) on any fatal error; the parent is paused before returning.
func (e *Engine) preImplement(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem) (bool, error) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Idempotency guard: if children have already been spawned, skip.
	if hasLabel(item, "fabrik:children-spawned") {
		return false, nil
	}

	// Find the most recent Plan stage comment.
	planComment := findStageComment(item.Comments, "Plan")
	if planComment == nil {
		return false, nil
	}

	blocks := ParseSpawnBlocks(planComment.Body)
	if len(blocks) == 0 {
		return false, nil
	}

	e.logf(item.Number, "spawn", "pre-Implement: found %d child(ren) to spawn\n", len(blocks))

	// Ensure all target repos are initialized (bare-cloned) before any mutation.
	// On-demand clone via singleflight — no prior processing of an issue from the
	// target repo is required. Error comment and labels are posted by
	// ensureSpawnTargetReady on failure.
	uniqueRepos := make(map[string]struct{})
	for _, b := range blocks {
		uniqueRepos[b.Repo] = struct{}{}
	}
	for targetRepo := range uniqueRepos {
		targetOwner, targetRepoName, ok := parseOwnerRepoStr(targetRepo)
		if !ok {
			// Malformed repo string — the per-block loop below will catch and report it.
			continue
		}
		if err := e.ensureSpawnTargetReady(ctx, targetOwner, targetRepoName, item); err != nil {
			return false, fmt.Errorf("pre-implement: initializing spawn target %s: %w", targetRepo, err)
		}
	}

	// Snapshot statusField once before the loop (stable once set; avoids holding the mutex during network calls).
	e.mu.Lock()
	sf := e.statusField
	e.mu.Unlock()

	// Spawn children in order.
	var spawned []string
	for i, block := range blocks {
		childOwner, childRepo, ok := parseOwnerRepoStr(block.Repo)
		if !ok {
			msg := fmt.Sprintf("🏭 **Fabrik — pre-Implement spawn failed**\n\nInvalid repo in spawn block #%d: `%s`. Created so far: %s\n\nRemove `fabrik:paused` after fixing the Plan output to retry.",
				i+1, block.Repo, formatSpawnedList(spawned))
			if dbID, commentErr := e.client.AddComment(owner, repo, item.Number, msg); commentErr != nil {
				e.logf(item.Number, "warn", "could not post spawn error comment: %v\n", commentErr)
			} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
					DatabaseID: dbID, Body: msg, Author: e.cfg.User, CreatedAt: time.Now(),
				})
			}
			e.addPausedLabelToItem(owner, repo, item)
			return false, fmt.Errorf("pre-implement: invalid repo %q in block %d", block.Repo, i+1)
		}

		fullBody := block.Body + childFooter(owner, repo, item.Number)
		childNumber, childNodeID, err := e.client.CreateIssue(childOwner, childRepo, block.Title, fullBody)
		if err != nil {
			msg := fmt.Sprintf("🏭 **Fabrik — pre-Implement spawn failed**\n\nFailed to create child issue %d/%d in `%s`: `%v`\n\nCreated so far: %s\n\nManually close any orphaned children, remove `fabrik:paused`, then re-advance to retry.",
				i+1, len(blocks), block.Repo, err, formatSpawnedList(spawned))
			if dbID, commentErr := e.client.AddComment(owner, repo, item.Number, msg); commentErr != nil {
				e.logf(item.Number, "warn", "could not post spawn error comment: %v\n", commentErr)
			} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
					DatabaseID: dbID, Body: msg, Author: e.cfg.User, CreatedAt: time.Now(),
				})
			}
			e.addPausedLabelToItem(owner, repo, item)
			return false, fmt.Errorf("pre-implement: creating child %d: %w", i+1, err)
		}
		e.logf(item.Number, "spawn", "created child %s/%s#%d\n", childOwner, childRepo, childNumber)
		spawned = append(spawned, fmt.Sprintf("%s#%d", block.Repo, childNumber))

		// Add child to the project board.
		childItemID, err := e.client.AddProjectV2ItemById(board.ProjectID, childNodeID)
		if err != nil {
			msg := fmt.Sprintf("🏭 **Fabrik — pre-Implement spawn failed**\n\nFailed to add child %s/%s#%d to project board: `%v`\n\nCreated so far: %s\n\nManually close any orphaned children, remove `fabrik:paused`, then re-advance to retry.",
				childOwner, childRepo, childNumber, err, formatSpawnedList(spawned))
			if dbID, commentErr := e.client.AddComment(owner, repo, item.Number, msg); commentErr != nil {
				e.logf(item.Number, "warn", "could not post spawn error comment: %v\n", commentErr)
			} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
					DatabaseID: dbID, Body: msg, Author: e.cfg.User, CreatedAt: time.Now(),
				})
			}
			e.addPausedLabelToItem(owner, repo, item)
			return false, fmt.Errorf("pre-implement: adding child %s#%d to project: %w", block.Repo, childNumber, err)
		}

		// Link child as a blockedBy dependency of the parent.
		// item.ID is the parent issue's GraphQL node ID.
		if err := e.client.AddBlockedByIssue(item.ID, childNodeID); err != nil {
			msg := fmt.Sprintf("🏭 **Fabrik — pre-Implement spawn failed**\n\nFailed to link child %s/%s#%d as blocked-by of parent: `%v`\n\nCreated so far: %s\n\nManually close any orphaned children, remove `fabrik:paused`, then re-advance to retry.",
				childOwner, childRepo, childNumber, err, formatSpawnedList(spawned))
			if dbID, commentErr := e.client.AddComment(owner, repo, item.Number, msg); commentErr != nil {
				e.logf(item.Number, "warn", "could not post spawn error comment: %v\n", commentErr)
			} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
					DatabaseID: dbID, Body: msg, Author: e.cfg.User, CreatedAt: time.Now(),
				})
			}
			e.addPausedLabelToItem(owner, repo, item)
			return false, fmt.Errorf("pre-implement: linking child %s#%d as blocked-by: %w", block.Repo, childNumber, err)
		}

		// Apply fabrik:sub-issue label to child (for human-visible filtering; no engine semantics).
		if err := e.client.AddLabelToIssue(childOwner, childRepo, childNumber, "fabrik:sub-issue"); err != nil {
			e.logf(item.Number, "warn", "could not add fabrik:sub-issue to %s#%d: %v\n", block.Repo, childNumber, err)
		}

		// Set child's project Status to Specify (or first processing stage) when statusField is available.
		if optionID := resolveSpecifyOptionID(sf); optionID != "" {
			if err := e.client.UpdateProjectItemStatus(board.ProjectID, childItemID, sf.FieldID, optionID); err != nil {
				e.logf(item.Number, "warn", "could not set project status on %s#%d: %v\n", block.Repo, childNumber, err)
			} else {
				e.store.Apply(itemstate.StatusUpdateRecorded{Repo: block.Repo, Number: childNumber, At: time.Now()})
			}
		} else if sf == nil {
			e.logf(item.Number, "warn", "project status field unavailable for %s#%d; child lands in Backlog\n", block.Repo, childNumber)
		} else {
			e.logf(item.Number, "warn", "no Specify/processing status option found for %s#%d; child lands in Backlog\n", block.Repo, childNumber)
		}

		// Inherit fabrik:yolo and fabrik:cruise from parent (enables autonomous child pipeline).
		if hasLabel(item, "fabrik:yolo") {
			if err := e.client.AddLabelToIssue(childOwner, childRepo, childNumber, "fabrik:yolo"); err != nil {
				e.logf(item.Number, "warn", "could not add fabrik:yolo to %s#%d: %v\n", block.Repo, childNumber, err)
			}
		}
		if hasLabel(item, "fabrik:cruise") {
			if err := e.client.AddLabelToIssue(childOwner, childRepo, childNumber, "fabrik:cruise"); err != nil {
				e.logf(item.Number, "warn", "could not add fabrik:cruise to %s#%d: %v\n", block.Repo, childNumber, err)
			}
		}
	}

	// All children spawned successfully — mark parent with idempotency guard.
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:children-spawned"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:children-spawned: %v\n", err)
	} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
		cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:children-spawned")
	}

	e.logf(item.Number, "spawn", "pre-Implement: spawned %d child(ren); parent will be gated until all close\n", len(blocks))
	return true, nil
}

// addPausedLabelToItem adds fabrik:paused to the given item, with cache write-through.
func (e *Engine) addPausedLabelToItem(owner, repo string, item gh.ProjectItem) {
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); err != nil {
		e.logf(item.Number, "warn", "could not add fabrik:paused: %v\n", err)
	} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
		cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:paused")
	}
}

// parseOwnerRepoStr splits "owner/repo" into owner and repo. Returns false if
// the string does not contain exactly one "/" with non-empty parts on each side.
func parseOwnerRepoStr(s string) (owner, repo string, ok bool) {
	idx := strings.Index(s, "/")
	if idx <= 0 || idx == len(s)-1 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// formatSpawnedList formats the list of already-spawned children for error messages.
func formatSpawnedList(spawned []string) string {
	if len(spawned) == 0 {
		return "none"
	}
	return strings.Join(spawned, ", ")
}
