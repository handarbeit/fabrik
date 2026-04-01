package engine

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

type Config struct {
	Owner       string
	Repo        string
	ProjectNum  int
	User        string
	Token       string
	Yolo        bool
	PollSeconds int
	Stages      []*stages.Stage
}

type Engine struct {
	cfg          Config
	client       *gh.Client
	statusField  *gh.StatusField
	processedSet map[string]time.Time // track what we've processed: "issue#-commentID" -> timestamp
}

func New(cfg Config) *Engine {
	return &Engine{
		cfg:          cfg,
		client:       gh.NewClient(cfg.Token),
		processedSet: make(map[string]time.Time),
	}
}

func (e *Engine) Run() error {
	// Set up graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("\nFabrik is running. Press Ctrl+C to stop.\n")

	// Main poll loop
	ticker := time.NewTicker(time.Duration(e.cfg.PollSeconds) * time.Second)
	defer ticker.Stop()

	// Run immediately on start, then on tick
	if err := e.poll(); err != nil {
		fmt.Printf("  [warn] poll error: %v\n", err)
	}

	for {
		select {
		case <-sigCh:
			fmt.Println("\nShutting down...")
			return nil
		case <-ticker.C:
			if err := e.poll(); err != nil {
				fmt.Printf("  [warn] poll error: %v\n", err)
			}
		}
	}
}

func (e *Engine) poll() error {
	fmt.Printf("[poll] fetching project board %s/%s#%d\n", e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum)

	board, err := e.client.FetchProjectBoard(e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum)
	if err != nil {
		return err
	}

	// Fetch status field metadata (for mutations) on first poll
	if e.statusField == nil && board.ProjectID != "" {
		sf, err := e.client.FetchStatusField(board.ProjectID)
		if err != nil {
			fmt.Printf("  [warn] could not fetch status field: %v\n", err)
		} else {
			e.statusField = sf
		}
	}

	fmt.Printf("[poll] found %d items on board\n", len(board.Items))

	for _, item := range board.Items {
		if err := e.processItem(board, item); err != nil {
			fmt.Printf("  [error] issue #%d: %v\n", item.Number, err)
		}
	}

	return nil
}

func (e *Engine) processItem(board *gh.ProjectBoard, item gh.ProjectItem) error {
	// Find the stage config for this item's current status
	stage := stages.FindStage(e.cfg.Stages, item.Status)
	if stage == nil {
		// Item is in a column we don't have a stage config for — skip
		return nil
	}

	// Check if this issue is locked by another driver instance
	lockLabel := fmt.Sprintf("fabrik:locked:%s", e.cfg.User)
	otherLockPrefix := "fabrik:locked:"
	for _, label := range item.Labels {
		if strings.HasPrefix(label, otherLockPrefix) && label != lockLabel {
			fmt.Printf("  [skip] issue #%d locked by another user\n", item.Number)
			return nil
		}
	}

	// Check for stage completion label — already done
	completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
	for _, label := range item.Labels {
		if label == completeLabel {
			return nil
		}
	}

	// Determine if there are new comments from our user to process
	newComments := e.findNewComments(item)

	// Determine if we need to process this item
	itemKey := fmt.Sprintf("%d-%s", item.Number, stage.Name)
	_, alreadyProcessed := e.processedSet[itemKey]

	if alreadyProcessed && len(newComments) == 0 {
		return nil
	}

	fmt.Printf("\n[process] issue #%d %q — stage: %s\n", item.Number, item.Title, stage.Name)

	// Acquire lock
	if err := e.client.AddLabelToIssue(e.cfg.Owner, e.cfg.Repo, item.Number, lockLabel); err != nil {
		fmt.Printf("  [warn] could not add lock label: %v\n", err)
	}

	// Invoke Claude Code
	resume := alreadyProcessed // resume session if we've processed this before
	output, completed, err := InvokeClaude(stage, item, newComments, resume)
	if err != nil {
		fmt.Printf("  [warn] claude invocation issue: %v\n", err)
	}

	// Post Claude's output as a comment (truncated if very long)
	if output != "" {
		comment := formatOutputComment(stage.Name, output)
		if err := e.client.AddComment(e.cfg.Owner, e.cfg.Repo, item.Number, comment); err != nil {
			fmt.Printf("  [warn] could not post comment: %v\n", err)
		}
	}

	e.processedSet[itemKey] = time.Now()

	if completed {
		fmt.Printf("  [done] stage %q complete for issue #%d\n", stage.Name, item.Number)

		// Add completion label
		if err := e.client.AddLabelToIssue(e.cfg.Owner, e.cfg.Repo, item.Number, completeLabel); err != nil {
			fmt.Printf("  [warn] could not add completion label: %v\n", err)
		}

		// Auto-advance if yolo mode (or stage-specific override)
		shouldAdvance := e.cfg.Yolo
		if stage.AutoAdvance != nil {
			shouldAdvance = *stage.AutoAdvance
		}

		if shouldAdvance {
			if err := e.advanceToNextStage(board, item, stage); err != nil {
				fmt.Printf("  [warn] could not advance: %v\n", err)
			}
		} else {
			fmt.Printf("  [wait] waiting for human to advance issue #%d\n", item.Number)
		}
	}

	return nil
}

func (e *Engine) findNewComments(item gh.ProjectItem) []gh.Comment {
	var newComments []gh.Comment
	for _, c := range item.Comments {
		// Only process comments from the configured user
		if c.Author != e.cfg.User {
			continue
		}
		// Skip comments we've already processed
		key := fmt.Sprintf("%d-comment-%s", item.Number, c.ID)
		if _, seen := e.processedSet[key]; seen {
			continue
		}
		// Skip comments that look like Fabrik output
		if strings.HasPrefix(c.Body, "🏭 **Fabrik") {
			continue
		}
		newComments = append(newComments, c)
		e.processedSet[key] = time.Now()
	}
	return newComments
}

func (e *Engine) advanceToNextStage(board *gh.ProjectBoard, item gh.ProjectItem, currentStage *stages.Stage) error {
	next := stages.NextStage(e.cfg.Stages, currentStage.Name)
	if next == nil {
		fmt.Printf("  [info] issue #%d has completed all stages\n", item.Number)
		return nil
	}

	if e.statusField == nil {
		return fmt.Errorf("status field metadata not available")
	}

	optionID, ok := e.statusField.Options[next.Name]
	if !ok {
		return fmt.Errorf("no status option %q found on project board (available: %v)",
			next.Name, mapKeys(e.statusField.Options))
	}

	fmt.Printf("  [advance] moving issue #%d to stage %q\n", item.Number, next.Name)
	return e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, e.statusField.FieldID, optionID)
}

func formatOutputComment(stageName, output string) string {
	// Truncate very long output
	const maxLen = 60000
	if len(output) > maxLen {
		output = output[:maxLen] + "\n\n... (truncated)"
	}

	return fmt.Sprintf("🏭 **Fabrik — stage: %s**\n\n%s", stageName, output)
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
