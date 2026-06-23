package engine

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/handarbeit/fabrik/tui"
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
	board, err := e.readClient.FetchProjectBoard(e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType)
	if err != nil {
		e.logf(0, "startup", "warning: could not fetch project board for startup check: %v\n", err)
		return nil
	}
	if board.ProjectID == "" {
		e.logf(0, "startup", "warning: project board has no ID — skipping startup check\n")
		return nil
	}

	// Emit project metadata to the TUI so the footer can display the board title.
	if board.Title != "" {
		ownerSegment := "orgs"
		if board.OwnerType == "user" {
			ownerSegment = "users"
		}
		boardURL := fmt.Sprintf("https://github.com/%s/%s/projects/%d", ownerSegment, e.cfg.Owner, e.cfg.ProjectNum)
		e.emitStructural(tui.ProjectMetaEvent{BoardTitle: board.Title, BoardURL: boardURL})
	}

	sf, err := e.readClient.FetchStatusField(board.ProjectID)
	if err != nil {
		e.logf(0, "startup", "warning: could not fetch status field for startup check: %v\n", err)
		return nil
	}

	// Store the StatusField so poll()'s lazy fetch is skipped.
	e.mu.Lock()
	e.statusField = sf
	e.mu.Unlock()

	// Build the required stage set for column validation.
	// Cleanup stages are always excluded (they have no board column requirement).
	// Holding stages are excluded when merge_train is off — they only require a
	// board column when merge_train is on (the operator must add the Queued column).
	var checkStages []*stageNameOrder
	for _, s := range e.cfg.Stages {
		if s.CleanupWorktree {
			continue
		}
		if s.HoldingStage && e.cfg.MergeTrain != "on" {
			continue
		}
		checkStages = append(checkStages, &stageNameOrder{name: s.Name, order: s.Order, holdingStage: s.HoldingStage})
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

	// Drift scan: warn about items whose board column doesn't match their
	// cleanup-stage complete label. Cleanup stages are terminal — board drift
	// for them cannot self-heal and each mismatch is a regression signal.
	// Non-cleanup stage mismatches are still in-flight and are ignored here.
	cleanupStageNames := make(map[string]bool, len(e.cfg.Stages))
	for _, s := range e.cfg.Stages {
		if s.CleanupWorktree {
			cleanupStageNames[s.Name] = true
		}
	}
	for _, item := range board.Items {
		for _, label := range item.Labels {
			if !strings.HasPrefix(label, "stage:") || !strings.HasSuffix(label, ":complete") {
				continue
			}
			stageName := strings.TrimSuffix(strings.TrimPrefix(label, "stage:"), ":complete")
			if !cleanupStageNames[stageName] {
				continue
			}
			if item.Status != stageName {
				e.logf(item.Number, "startup", "warning: item #%d has label stage:%s:complete but board column is %q — board drift detected\n", item.Number, stageName, item.Status)
			}
		}
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
	for _, s := range missing {
		if s.holdingStage {
			fmt.Fprintf(os.Stderr, "\nNote: %q is a holding stage required by merge_train: on.\n", s.name)
			fmt.Fprintf(os.Stderr, "Add a `%s` column to your GitHub Project board between `Validate` and `Done`,\n", s.name)
			fmt.Fprintf(os.Stderr, "then restart. See docs/state-machine.md for setup steps.\n")
			fmt.Fprintf(os.Stderr, "If you copied queued.yaml from a new Fabrik installation, ensure the column\n")
			fmt.Fprintf(os.Stderr, "name on the board matches the 'name' field in the YAML (case-sensitive).\n")
		}
	}

	return fmt.Errorf("startup check failed: stage/board column mismatch")
}

// stageNameOrder is a helper for sorting and reporting stage names.
type stageNameOrder struct {
	name         string
	order        int
	holdingStage bool
}
