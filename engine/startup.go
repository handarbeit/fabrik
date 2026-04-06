package engine

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// checkStageColumnAlignment validates that every configured non-cleanup stage
// has a matching column in the project board's Status field. It is called once
// from Run() before the first poll.
//
// Non-fatal errors (network failures, missing Status field) are logged as
// warnings and the check is skipped — only a successful fetch with genuine
// name mismatches is fatal. This prevents transient network errors from
// blocking startup.
//
// On success the fetched StatusField is stored in e.statusField so poll()'s
// lazy guard skips the redundant second FetchStatusField call.
func (e *Engine) checkStageColumnAlignment(ctx context.Context) error {
	board, err := e.client.FetchProjectBoard(e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType)
	if err != nil {
		e.logf(0, "startup", "warning: could not fetch project board for startup check: %v\n", err)
		return nil
	}
	if board.ProjectID == "" {
		e.logf(0, "startup", "warning: project board has no ID — skipping startup check\n")
		return nil
	}

	sf, err := e.client.FetchStatusField(board.ProjectID)
	if err != nil {
		e.logf(0, "startup", "warning: could not fetch status field for startup check: %v\n", err)
		return nil
	}

	// Store the StatusField so poll()'s lazy fetch is skipped.
	e.mu.Lock()
	e.statusField = sf
	e.mu.Unlock()

	// Build the set of non-cleanup stage names.
	var checkStages []*stageNameOrder
	for _, s := range e.cfg.Stages {
		if !s.CleanupWorktree {
			checkStages = append(checkStages, &stageNameOrder{name: s.Name, order: s.Order})
		}
	}
	// Sort by Order for deterministic mismatch report output.
	sort.Slice(checkStages, func(i, j int) bool {
		return checkStages[i].order < checkStages[j].order
	})

	// Find missing stages (stage name not in board options).
	var missing []*stageNameOrder
	for _, s := range checkStages {
		if _, ok := sf.Options[s.name]; !ok {
			missing = append(missing, s)
		}
	}

	// Find extra board columns (column not matching any non-cleanup stage).
	stageNames := make(map[string]bool, len(checkStages))
	for _, s := range checkStages {
		stageNames[s.name] = true
	}
	var extra []string
	for colName := range sf.Options {
		if !stageNames[colName] {
			extra = append(extra, colName)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		e.logf(0, "startup", "warning: board has columns with no matching stage: %s\n", strings.Join(extra, ", "))
	}

	if len(missing) == 0 {
		return nil
	}

	// Build the list of all board column names for the error report.
	allCols := make([]string, 0, len(sf.Options))
	for colName := range sf.Options {
		allCols = append(allCols, colName)
	}
	sort.Strings(allCols)

	fmt.Fprintf(os.Stderr, "Fabrik startup check failed: stage/board column mismatch\n\n")
	fmt.Fprintf(os.Stderr, "Configured stages not found on board:\n")
	for _, s := range missing {
		fmt.Fprintf(os.Stderr, "  - %s (order %d)\n", s.name, s.order)
	}
	fmt.Fprintf(os.Stderr, "\nBoard columns found:\n  %s\n\n", strings.Join(allCols, ", "))
	fmt.Fprintf(os.Stderr, "Fix: add the missing columns to your GitHub Project board, or update\n")
	fmt.Fprintf(os.Stderr, ".fabrik/stages/ to match your board column names (case-sensitive).\n")

	return fmt.Errorf("startup check failed: stage/board column mismatch")
}

// stageNameOrder is a helper for sorting and reporting stage names.
type stageNameOrder struct {
	name  string
	order int
}
